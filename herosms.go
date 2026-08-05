package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HeroSMS 接码集成:把 gpt-login-automation skill 里用到的接码功能搬进代理,
// 页面可配置 API Key、查余额、看 dr(OpenAI)优惠、买号、查验证码、取消/完成激活。
//
// 两套上游 base:
//   - handler_api.php(SMS-Activate 兼容):纯文本返回(ACCESS_NUMBER:.. / STATUS_OK:.. / ACCESS_BALANCE:..)
//   - /api/v1(REST):JSON,鉴权头 Authorization: ApiKey <key>
const (
	herosmsHandlerAPI = "https://hero-sms.com/stubs/handler_api.php"
	herosmsOffersAPI  = "https://hero-sms.com/api/v1/activations/offers"
	// herosmsDefaultKey 先从 gpt-login-automation skill 直接拿过来兜底;页面可覆盖。
	herosmsDefaultKey               = "076c10b118b7b1917f40b20b8fAb2A7b"
	herosmsService                  = "dr" // OpenAI
	settingHeroSMSKey               = "herosms_api_key"
	settingHeroSMSGPTLoginMaxPrice  = "herosms_gpt_login_max_price"
	settingHeroSMSGPTLoginMinCount  = "herosms_gpt_login_min_count"
	settingHeroSMSGPTLoginCountries = "herosms_gpt_login_countries"
)

// ---- 买过号码的后台自动取消监控 ----
//
// login 自动化买号后不再同步等 2 分钟取消(skill 老做法),而是把号登记进这个跟踪器。
// 后台每 30s 扫一遍:凡是「未用上、未取消、买入已满 125s」的号就异步取消一次。
// HeroSMS 有 120s 最小激活期(提前取消 EARLY_CANCEL_DENIED),所以卡 125s。
// 用上验证码的号会被 MarkUsed 标记,不会被取消。

const herosmsCancelAfter = 125 * time.Second

type herosmsActivation struct {
	boughtAt time.Time
	used     bool
	canceled bool
}

type herosmsTracker struct {
	mu    sync.Mutex
	items map[string]*herosmsActivation
}

// Register 登记一个刚买到的激活(login 流程买号后调)。
func (p *proxyServer) herosmsRegister(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	p.herosmsTrack.mu.Lock()
	defer p.herosmsTrack.mu.Unlock()
	if p.herosmsTrack.items == nil {
		p.herosmsTrack.items = make(map[string]*herosmsActivation)
	}
	p.herosmsTrack.items[id] = &herosmsActivation{boughtAt: time.Now()}
}

// herosmsMarkUsed 标记号码已用上验证码(finishActivation 之后调),避免被自动取消。
func (p *proxyServer) herosmsMarkUsed(id string) {
	p.herosmsTrack.mu.Lock()
	defer p.herosmsTrack.mu.Unlock()
	if a := p.herosmsTrack.items[id]; a != nil {
		a.used = true
	}
	if p.store != nil {
		_ = p.store.UpdateHeroSMSActivationStatus(context.Background(), id, "finished", "", "finishActivation")
	}
}

// runHeroSMSCanceler 后台常驻:超 125s 未用上的登记号自动取消。ctx 取消即退出。
func (p *proxyServer) runHeroSMSCanceler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		key := p.herosmsKey(ctx)
		p.herosmsTrack.mu.Lock()
		var due []string
		for id, a := range p.herosmsTrack.items {
			if a.used || a.canceled {
				// 已用/已取消的清理掉,别让 map 无限涨。
				if time.Since(a.boughtAt) > 10*time.Minute {
					delete(p.herosmsTrack.items, id)
				}
				continue
			}
			if time.Since(a.boughtAt) >= herosmsCancelAfter {
				a.canceled = true
				due = append(due, id)
			}
		}
		p.herosmsTrack.mu.Unlock()
		for _, id := range due {
			text, err := p.herosmsCancelActivation(ctx, key, id)
			if err != nil {
				log.Printf("HeroSMS 自动取消 %s 失败: %v (%s)", id, err, text)
			} else if p.store != nil {
				_ = p.store.UpdateHeroSMSActivationStatus(ctx, id, "cancelled", "", text)
			}
		}
		// 同时扫 MySQL 持久化记录。旧逻辑/同步逻辑写进库但不在内存 herosmsTrack 里的号码，
		// 到 125s 后也要自动取消，否则页面能看到但后台永远不处理。
		if p.store != nil {
			items, err := p.store.DueHeroSMSActivations(ctx, herosmsCancelAfter)
			if err != nil {
				log.Printf("HeroSMS 查询到期激活失败: %v", err)
			} else {
				for _, item := range items {
					if item.ID == "" {
						continue
					}
					text, err := p.herosmsCancelActivation(ctx, key, item.ID)
					if err != nil {
						log.Printf("HeroSMS 数据库自动取消 %s 失败: %v (%s)", item.ID, err, text)
						// 已经尝试过自动取消但仍失败的记录标成 cancel_failed，避免每 30 秒重复处理刷屏。
						// 如果后续需要人工处理，可以在 HeroSMS 页面 include_done 查看 last_raw。
						_ = p.store.UpdateHeroSMSActivationStatus(ctx, item.ID, "cancel_failed", "", fallback(text, err.Error()))
						continue
					}
					_ = p.store.UpdateHeroSMSActivationStatus(ctx, item.ID, "cancelled", "", text)
				}
			}
		}
	}
}

// herosmsKey 取当前生效的 API Key:优先 app_settings 里配置的,为空回落 skill 内置的默认值。
func (p *proxyServer) herosmsKey(ctx context.Context) string {
	v, _ := p.store.GetSetting(ctx, settingHeroSMSKey, herosmsDefaultKey)
	if v = strings.TrimSpace(v); v == "" {
		return herosmsDefaultKey
	}
	return v
}

// herosmsGPTLoginMaxPrice 是 OUTLOOK 页「登录 Codex」自动买号的单号价格上限。
func (p *proxyServer) herosmsGPTLoginMaxPrice(ctx context.Context) float64 {
	v, _ := p.store.GetSetting(ctx, settingHeroSMSGPTLoginMaxPrice, "")
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
		return f
	}
	return gptLoginDefaultMaxPrice
}

// herosmsGPTLoginMinCount 是 OUTLOOK 页「登录 Codex」自动买号的库存门槛。HeroSMS offers
// 查询里保留原口径 count > min_count；默认 2000，现在可在 HeroSMS 页面配置。
func (p *proxyServer) herosmsGPTLoginMinCount(ctx context.Context) int {
	v, _ := p.store.GetSetting(ctx, settingHeroSMSGPTLoginMinCount, "")
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return n
	}
	return gptLoginDefaultMinCount
}

// herosmsGPTLoginCountries 是 OUTLOOK 页「登录 Codex」自动买号国家白名单。
// 为空表示沿用原逻辑:按 HeroSMS 优惠列表和黑名单自动选择。非空时按逗号分割的顺序只尝试这些国家。
func (p *proxyServer) herosmsGPTLoginCountries(ctx context.Context) string {
	v, _ := p.store.GetSetting(ctx, settingHeroSMSGPTLoginCountries, "")
	return normalizeHeroSMSCountries(v)
}

func normalizeHeroSMSCountries(v string) string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, part)
	}
	return strings.Join(out, ",")
}

func (p *proxyServer) herosmsClient() *http.Client {
	return &http.Client{Transport: p.client.Transport, Timeout: 30 * time.Second}
}

func (p *proxyServer) herosmsClientForContext(ctx context.Context) *http.Client {
	if use, _ := ctx.Value(scopedProxyContextKey{}).(bool); use {
		return &http.Client{Transport: p.proxiedTransport, Timeout: 30 * time.Second}
	}
	return p.herosmsClient()
}

// herosmsGet 打 handler_api.php(SMS-Activate 兼容),返回去空白的纯文本响应。
func (p *proxyServer) herosmsGet(ctx context.Context, key, action string, params map[string]string) (string, error) {
	q := url.Values{}
	q.Set("api_key", key)
	q.Set("action", action)
	for k, v := range params {
		q.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, herosmsHandlerAPI+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := p.herosmsClientForContext(ctx).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("HeroSMS %s HTTP %d: %s", action, resp.StatusCode, truncate(text, 200))
	}
	return text, nil
}

// herosmsCancelActivation 对 cancelActivation 做终态归一化。
// HeroSMS 对已经结束的激活可能返回 409 ACTIVATION_NOT_ACTIVE，也可能返回 204 空响应；
// 这些都表示本地不应再继续取消，统一当作已取消成功处理。
func (p *proxyServer) herosmsCancelActivation(ctx context.Context, key, id string) (string, error) {
	text, err := p.herosmsGet(ctx, key, "cancelActivation", map[string]string{"id": id})
	if err == nil {
		if strings.TrimSpace(text) == "" {
			return "cancelActivation: empty success response", nil
		}
		return text, nil
	}
	if heroSMSCancelTerminal(text, err) {
		if strings.TrimSpace(text) != "" {
			return text, nil
		}
		return err.Error(), nil
	}
	return text, err
}

func heroSMSCancelTerminal(text string, err error) bool {
	combined := strings.ToUpper(strings.TrimSpace(text))
	if err != nil {
		combined += "\n" + strings.ToUpper(err.Error())
	}
	return strings.Contains(combined, "STATUS_CANCEL") ||
		strings.Contains(combined, "ALREADY_CANCEL") ||
		strings.Contains(combined, "NO_ACTIVATION") ||
		strings.Contains(combined, "ACTIVATION_NOT_ACTIVE") ||
		strings.Contains(combined, "ACTIVATION IS TERMINATED") ||
		strings.Contains(combined, "HTTP 204")
}

// ---- 国家名缓存(getCountries 结果较大且极少变,缓存 1 小时)----

type herosmsCountryCache struct {
	mu      sync.Mutex
	names   map[string]string // country_id -> 中文名
	fetched time.Time
}

var herosmsCountries herosmsCountryCache

func (p *proxyServer) herosmsCountryNames(ctx context.Context, key string) map[string]string {
	herosmsCountries.mu.Lock()
	defer herosmsCountries.mu.Unlock()
	if herosmsCountries.names != nil && time.Since(herosmsCountries.fetched) < time.Hour {
		return herosmsCountries.names
	}
	text, err := p.herosmsGet(ctx, key, "getCountries", nil)
	if err != nil {
		if herosmsCountries.names != nil {
			return herosmsCountries.names // 拿旧的兜底
		}
		return map[string]string{}
	}
	var raw map[string]struct {
		Chn string `json:"chn"`
		Eng string `json:"eng"`
	}
	if json.Unmarshal([]byte(text), &raw) != nil {
		return map[string]string{}
	}
	names := make(map[string]string, len(raw))
	for id, c := range raw {
		names[id] = fallback(c.Chn, c.Eng)
	}
	herosmsCountries.names = names
	herosmsCountries.fetched = time.Now()
	return names
}

// ---- 处理函数 ----

func (p *proxyServer) handleHeroSMS(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "herosms", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// GET /api/herosms/config —— 返回当前 key(明文,本地工具)与是否用的默认值。
func (p *proxyServer) handleAPIHeroSMSConfig(w http.ResponseWriter, r *http.Request) {
	key := p.herosmsKey(r.Context())
	stored, _ := p.store.GetSetting(r.Context(), settingHeroSMSKey, "")
	writeJSON(w, map[string]any{
		"api_key":             key,
		"is_default":          strings.TrimSpace(stored) == "",
		"service":             herosmsService,
		"gpt_login_max_price": p.herosmsGPTLoginMaxPrice(r.Context()),
		"gpt_login_min_count": p.herosmsGPTLoginMinCount(r.Context()),
		"gpt_login_countries": p.herosmsGPTLoginCountries(r.Context()),
	}, nil)
}

// POST /api/herosms/config —— 保存 key、Codex 登录买号单价与库存门槛。
func (p *proxyServer) handleAPIHeroSMSConfigSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey            string  `json:"api_key"`
		GPTLoginMaxPrice  float64 `json:"gpt_login_max_price"`
		GPTLoginMinCount  int     `json:"gpt_login_min_count"`
		GPTLoginCountries string  `json:"gpt_login_countries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := p.store.SetSetting(r.Context(), settingHeroSMSKey, strings.TrimSpace(in.APIKey)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	price := in.GPTLoginMaxPrice
	if price <= 0 {
		price = gptLoginDefaultMaxPrice
	}
	if err := p.store.SetSetting(r.Context(), settingHeroSMSGPTLoginMaxPrice, fmt.Sprintf("%.4f", price)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	minCount := in.GPTLoginMinCount
	if minCount < 0 {
		minCount = gptLoginDefaultMinCount
	}
	if err := p.store.SetSetting(r.Context(), settingHeroSMSGPTLoginMinCount, strconv.Itoa(minCount)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.store.SetSetting(r.Context(), settingHeroSMSGPTLoginCountries, normalizeHeroSMSCountries(in.GPTLoginCountries)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// GET /api/herosms/services —— 服务列表(getServicesList),供页面选择要接哪个服务(openai/google...)。
// 原样返回 {status, services:[{code,name}]}。
func (p *proxyServer) handleAPIHeroSMSServices(w http.ResponseWriter, r *http.Request) {
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), "getServicesList", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, text)
}

// herosmsReqService 从请求取 service(默认 dr=OpenAI)。
func herosmsReqService(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return herosmsService
}

// GET /api/herosms/balance —— 账户余额(ACCESS_BALANCE:<float>)。
func (p *proxyServer) handleAPIHeroSMSBalance(w http.ResponseWriter, r *http.Request) {
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), "getBalance", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var balance float64
	if strings.HasPrefix(text, "ACCESS_BALANCE:") {
		balance, _ = strconv.ParseFloat(strings.TrimPrefix(text, "ACCESS_BALANCE:"), 64)
	}
	writeJSON(w, map[string]any{"ok": true, "balance": balance, "raw": text}, nil)
}

type herosmsOfferRow struct {
	CountryID   string  `json:"country_id"`
	CountryName string  `json:"country_name"`
	Price       float64 `json:"price"`
	Count       int     `json:"count"`
	Total       int     `json:"total"`
	Blacklisted bool    `json:"blacklisted,omitempty"`
}

// herosmsOfferRows 拉某服务的优惠并过滤拉黑国家/排序(HTTP 处理器与 login 自动化共用)。
func (p *proxyServer) herosmsOfferRows(ctx context.Context, key, service string, maxPrice float64, minCount int) ([]herosmsOfferRow, error) {
	return p.herosmsOfferRowsWithBlacklist(ctx, key, service, maxPrice, minCount, false)
}

// herosmsOfferRowsWithBlacklist includeBlacklisted=true 时展示所有国家并标记黑名单，用于国家拉黑页。
func (p *proxyServer) herosmsOfferRowsWithBlacklist(ctx context.Context, key, service string, maxPrice float64, minCount int, includeBlacklisted bool) ([]herosmsOfferRow, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, herosmsOffersAPI+"?services="+url.QueryEscape(service), nil)
	req.Header.Set("Authorization", "ApiKey "+key)
	resp, err := p.herosmsClientForContext(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HeroSMS offers HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Data map[string]map[string]struct {
			Counts struct {
				Total int `json:"total"`
			} `json:"counts"`
			Map map[string]int `json:"map"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 offers 失败: %w", err)
	}
	names := p.herosmsCountryNames(ctx, key)
	blocked := map[string]bool{}
	if p.store != nil {
		if b, err := p.store.HeroSMSBlacklistedCountryIDs(ctx, service); err == nil {
			blocked = b
		}
	}
	rows := make([]herosmsOfferRow, 0, 64)
	for cid, info := range parsed.Data[service] {
		if blocked[cid] && !includeBlacklisted {
			continue
		}
		for priceStr, count := range info.Map {
			price, perr := strconv.ParseFloat(priceStr, 64)
			if perr != nil || price > maxPrice || count <= minCount {
				continue
			}
			rows = append(rows, herosmsOfferRow{CountryID: cid, CountryName: names[cid], Price: price, Count: count, Total: info.Counts.Total, Blacklisted: blocked[cid]})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Price != rows[j].Price {
			return rows[i].Price < rows[j].Price
		}
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].CountryID < rows[j].CountryID
	})
	return rows, nil
}

// herosmsBuyNumber 买一个号(getNumber, fixedPrice)。成功后写入 MySQL，页面手动买号和
// GPT 登录自动化买号统一从 herosms_activations 表展示。会真实扣费。
func (p *proxyServer) herosmsBuyNumber(ctx context.Context, key, service, country string, maxPrice float64, source string) (HeroSMSActivation, string, error) {
	if p.store != nil {
		if blocked, err := p.store.HeroSMSBlacklistedCountryIDs(ctx, service); err == nil && blocked[country] {
			return HeroSMSActivation{}, "", fmt.Errorf("国家 %s 已在业务 %s 的拉黑列表中", country, service)
		}
	}
	text, gerr := p.herosmsGet(ctx, key, "getNumber", map[string]string{
		"service": service, "country": country, "maxPrice": fmt.Sprintf("%.4f", maxPrice), "fixedPrice": "true",
	})
	if gerr != nil {
		return HeroSMSActivation{}, text, gerr
	}
	if !strings.HasPrefix(text, "ACCESS_NUMBER:") {
		return HeroSMSActivation{}, text, fmt.Errorf("买号未成功: %s", text)
	}
	parts := strings.SplitN(text, ":", 3)
	names := p.herosmsCountryNames(ctx, key)
	now := time.Now()
	a := HeroSMSActivation{
		ID:          parts[1],
		Phone:       parts[2],
		Service:     service,
		CountryID:   country,
		CountryName: fallback(names[country], country),
		Price:       maxPrice,
		Source:      fallback(strings.TrimSpace(source), "manual"),
		Status:      "waiting",
		LastRaw:     text,
		BoughtAt:    now,
		UpdatedAt:   now,
	}
	if p.store != nil {
		if err := p.store.SaveHeroSMSActivation(ctx, a); err != nil {
			return a, text, err
		}
	}
	p.herosmsRegister(a.ID)
	return a, text, nil
}

// herosmsCheckStatus 查激活状态;STATUS_OK:<code> 时返回 code。
func (p *proxyServer) herosmsCheckStatus(ctx context.Context, key, id string) (raw, code string, err error) {
	text, gerr := p.herosmsGet(ctx, key, "getStatus", map[string]string{"id": id})
	if gerr != nil {
		return text, "", gerr
	}
	if strings.HasPrefix(text, "STATUS_OK:") {
		return text, strings.TrimPrefix(text, "STATUS_OK:"), nil
	}
	return text, "", nil
}

// herosmsFinish 完成激活(收到并使用了验证码之后)。
func (p *proxyServer) herosmsFinish(ctx context.Context, key, id string) error {
	_, err := p.herosmsGet(ctx, key, "finishActivation", map[string]string{"id": id})
	return err
}

// GET /api/herosms/offers?max_price=&min_count= —— dr(OpenAI)优惠,按价格升序。
// 默认口径:max_price=0.2、min_count 从 HeroSMS 配置读取(默认 2000)。
func (p *proxyServer) handleAPIHeroSMSOffers(w http.ResponseWriter, r *http.Request) {
	key := p.herosmsKey(r.Context())
	service := herosmsReqService(r.URL.Query().Get("service"))
	maxPrice := 0.2
	if v, err := strconv.ParseFloat(r.URL.Query().Get("max_price"), 64); err == nil && v > 0 {
		maxPrice = v
	}
	minCount := p.herosmsGPTLoginMinCount(r.Context())
	if v, err := strconv.Atoi(r.URL.Query().Get("min_count")); err == nil && v >= 0 {
		minCount = v
	}
	rows, err := p.herosmsOfferRows(r.Context(), key, service, maxPrice, minCount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "service": service, "max_price": maxPrice, "min_count": minCount, "rows": rows}, nil)
}

// POST /api/herosms/number {country, max_price} —— 买号(getNumber, fixedPrice)。
// 返回 ACCESS_NUMBER:<activation_id>:<phone>。会真实扣费。
func (p *proxyServer) handleAPIHeroSMSBuyNumber(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service  string  `json:"service"`
		Country  string  `json:"country"`
		MaxPrice float64 `json:"max_price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Country) == "" {
		http.Error(w, "缺少 country", http.StatusBadRequest)
		return
	}
	if in.MaxPrice <= 0 {
		in.MaxPrice = 0.2
	}
	a, text, err := p.herosmsBuyNumber(r.Context(), p.herosmsKey(r.Context()), herosmsReqService(in.Service), in.Country, in.MaxPrice, "manual")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "activation_id": a.ID, "id": a.ID, "phone": a.Phone, "raw": text, "activation": a}, nil)
}

// GET /api/herosms/status?id= —— 查激活状态(getStatus)。STATUS_OK:<code> 表示收到验证码。
func (p *proxyServer) handleAPIHeroSMSStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), "getStatus", map[string]string{"id": id})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	status, code := heroSMSLocalStatus(text)
	if p.store != nil {
		_ = p.store.UpdateHeroSMSActivationStatus(r.Context(), id, status, code, text)
	}
	writeJSON(w, map[string]any{"ok": true, "raw": text, "code": code}, nil)
}

// setStatusAction 是 cancel/finish 共用的动作封装(cancelActivation / finishActivation)。
func (p *proxyServer) herosmsActivationAction(w http.ResponseWriter, r *http.Request, action string) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	var text string
	var err error
	if action == "cancelActivation" {
		text, err = p.herosmsCancelActivation(r.Context(), p.herosmsKey(r.Context()), in.ID)
	} else {
		text, err = p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), action, map[string]string{"id": in.ID})
	}
	if err != nil {
		http.Error(w, err.Error()+" | "+text, http.StatusBadGateway)
		return
	}
	if p.store != nil {
		status := "waiting"
		if action == "cancelActivation" {
			status = "cancelled"
		} else if action == "finishActivation" {
			status = "finished"
		}
		_ = p.store.UpdateHeroSMSActivationStatus(r.Context(), in.ID, status, "", text)
	}
	writeJSON(w, map[string]any{"ok": true, "raw": text}, nil)
}

// POST /api/herosms/cancel {id} —— 取消激活(有 120s 最小激活期,过早取消会 EARLY_CANCEL_DENIED)。
func (p *proxyServer) handleAPIHeroSMSCancel(w http.ResponseWriter, r *http.Request) {
	p.herosmsActivationAction(w, r, "cancelActivation")
}

// POST /api/herosms/finish {id} —— 完成激活(收到并使用了验证码之后)。
func (p *proxyServer) handleAPIHeroSMSFinish(w http.ResponseWriter, r *http.Request) {
	p.herosmsActivationAction(w, r, "finishActivation")
}

// GET /api/herosms/activations —— 本地数据库里的买号记录（包括页面手买和登录 Codex 自动化）。
func (p *proxyServer) handleAPIHeroSMSActivations(w http.ResponseWriter, r *http.Request) {
	includeDone := r.URL.Query().Get("include_done") == "1"
	_ = p.syncHeroSMSActiveActivations(r.Context())
	items, err := p.store.ListHeroSMSActivations(r.Context(), includeDone)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "items": items}, nil)
}

// DELETE /api/herosms/activations/done —— 清除本地已结束记录，不会调用 HeroSMS 上游。
func (p *proxyServer) handleAPIHeroSMSActivationsClearDone(w http.ResponseWriter, r *http.Request) {
	if err := p.store.ClearDoneHeroSMSActivations(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func heroSMSLocalStatus(raw string) (status, code string) {
	text := strings.TrimSpace(raw)
	upper := strings.ToUpper(text)
	switch {
	case strings.HasPrefix(upper, "STATUS_OK:"):
		return "code", strings.TrimSpace(text[strings.Index(text, ":")+1:])
	case strings.Contains(upper, "STATUS_CANCEL") || strings.Contains(upper, "CANCEL") || strings.Contains(upper, "NO_ACTIVATION"):
		return "cancelled", ""
	case strings.Contains(upper, "FINISH") || strings.Contains(upper, "DONE"):
		return "finished", ""
	default:
		return "waiting", ""
	}
}

// syncHeroSMSActiveActivations 从 HeroSMS 当前激活接口补录记录。用于修复旧逻辑下
// GPT 登录自动化已经买号但还没写本地表的情况；解析尽量宽松，兼容 row/rows/data 等形态。
func (p *proxyServer) syncHeroSMSActiveActivations(ctx context.Context) error {
	key := p.herosmsKey(ctx)
	text, err := p.herosmsGet(ctx, key, "getActiveActivations", nil)
	if err != nil || strings.TrimSpace(text) == "" {
		return err
	}
	var root any
	if json.Unmarshal([]byte(text), &root) != nil {
		return nil
	}
	var rows []map[string]any
	collectHeroSMSMaps(root, &rows)
	for _, m := range rows {
		id := firstHeroSMSString(m, "activation_id", "activationId", "activationID", "id", "ID")
		phone := firstHeroSMSString(m, "phone", "number", "phone_number", "phoneNumber", "tel")
		// 跳过容器/统计对象，只保存看起来像激活记录的对象。
		if id == "" || phone == "" {
			continue
		}
		service := firstHeroSMSString(m, "service", "service_code", "serviceCode")
		country := firstHeroSMSString(m, "country", "country_id", "countryId")
		price := firstHeroSMSFloat(m, "price", "cost", "sum")
		rawStatus := firstHeroSMSString(m, "status", "STATUS", "activation_status", "activationStatus", "last_status", "lastStatus")
		status, code := heroSMSLocalStatus(rawStatus)
		a := HeroSMSActivation{ID: id, Phone: phone, Service: fallback(service, herosmsService), CountryID: country, CountryName: country,
			Price: price, Source: "sync", Status: status, Code: code, LastRaw: fallback(rawStatus, "synced getActiveActivations"), BoughtAt: time.Now(), UpdatedAt: time.Now()}
		_ = p.store.SaveHeroSMSActivation(ctx, a)
		p.herosmsRegister(id)
	}
	return nil
}

func collectHeroSMSMaps(v any, out *[]map[string]any) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			collectHeroSMSMaps(item, out)
		}
	case map[string]any:
		*out = append(*out, x)
		for _, item := range x {
			collectHeroSMSMaps(item, out)
		}
	}
}

func firstHeroSMSString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s != "" && s != "<nil>" && s != "0" {
				return s
			}
		}
	}
	return ""
}

func firstHeroSMSFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			s := strings.TrimSpace(fmt.Sprint(v))
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// GET /api/herosms/attempt-logs?service=&limit= —— 登录/接码实际尝试过的号码记录。
func (p *proxyServer) handleAPIHeroSMSAttemptLogs(w http.ResponseWriter, r *http.Request) {
	limit := 300
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	items, err := p.store.ListHeroSMSAttemptLogs(r.Context(), strings.TrimSpace(r.URL.Query().Get("service")), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "items": items}, nil)
}

// GET /api/herosms/countries?service=&max_price=&min_count= —— 当前业务全部可用国家，包含已拉黑标记。
func (p *proxyServer) handleAPIHeroSMSCountries(w http.ResponseWriter, r *http.Request) {
	key := p.herosmsKey(r.Context())
	service := herosmsReqService(r.URL.Query().Get("service"))
	maxPrice := 9999.0
	if v, err := strconv.ParseFloat(r.URL.Query().Get("max_price"), 64); err == nil && v > 0 {
		maxPrice = v
	}
	minCount := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("min_count")); err == nil && v >= 0 {
		minCount = v
	}
	rows, err := p.herosmsOfferRowsWithBlacklist(r.Context(), key, service, maxPrice, minCount, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "service": service, "rows": rows}, nil)
}

// GET /api/herosms/blacklist?service= —— 国家拉黑列表。
func (p *proxyServer) handleAPIHeroSMSBlacklist(w http.ResponseWriter, r *http.Request) {
	items, err := p.store.ListHeroSMSCountryBlacklist(r.Context(), strings.TrimSpace(r.URL.Query().Get("service")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "items": items}, nil)
}

// POST /api/herosms/blacklist {service,country_id,country,reason} —— 拉黑某业务下的国家。
func (p *proxyServer) handleAPIHeroSMSBlacklistSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service   string `json:"service"`
		CountryID string `json:"country_id"`
		Country   string `json:"country"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	item := HeroSMSCountryBlacklist{Service: herosmsReqService(in.Service), CountryID: strings.TrimSpace(in.CountryID), CountryName: strings.TrimSpace(in.Country), Reason: strings.TrimSpace(in.Reason)}
	if item.CountryID == "" {
		http.Error(w, "缺少 country_id", http.StatusBadRequest)
		return
	}
	if err := p.store.UpsertHeroSMSCountryBlacklist(r.Context(), item); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// DELETE /api/herosms/blacklist?service=&country_id= —— 解除拉黑。
func (p *proxyServer) handleAPIHeroSMSBlacklistDelete(w http.ResponseWriter, r *http.Request) {
	service := herosmsReqService(r.URL.Query().Get("service"))
	cid := strings.TrimSpace(r.URL.Query().Get("country_id"))
	if cid == "" {
		http.Error(w, "缺少 country_id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteHeroSMSCountryBlacklist(r.Context(), service, cid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// GET /api/herosms/active —— 当前进行中的激活列表(getActiveActivations),原样透传 JSON。
func (p *proxyServer) handleAPIHeroSMSActive(w http.ResponseWriter, r *http.Request) {
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), "getActiveActivations", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, text)
}
