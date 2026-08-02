package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	herosmsDefaultKey = "076c10b118b7b1917f40b20b8fAb2A7b"
	herosmsService    = "dr" // OpenAI
	settingHeroSMSKey = "herosms_api_key"
)

// herosmsKey 取当前生效的 API Key:优先 app_settings 里配置的,为空回落 skill 内置的默认值。
func (p *proxyServer) herosmsKey(ctx context.Context) string {
	v, _ := p.store.GetSetting(ctx, settingHeroSMSKey, herosmsDefaultKey)
	if v = strings.TrimSpace(v); v == "" {
		return herosmsDefaultKey
	}
	return v
}

func (p *proxyServer) herosmsClient() *http.Client {
	return &http.Client{Transport: p.client.Transport, Timeout: 30 * time.Second}
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
	resp, err := p.herosmsClient().Do(req)
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
		"api_key":    key,
		"is_default": strings.TrimSpace(stored) == "",
		"service":    herosmsService,
	}, nil)
}

// POST /api/herosms/config {api_key} —— 保存 key(留空=清掉、回落默认)。
func (p *proxyServer) handleAPIHeroSMSConfigSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := p.store.SetSetting(r.Context(), settingHeroSMSKey, strings.TrimSpace(in.APIKey)); err != nil {
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
}

// GET /api/herosms/offers?max_price=&min_count= —— dr(OpenAI)优惠,按价格升序。
// 默认口径对齐 skill:max_price=0.2、min_count=2000。
func (p *proxyServer) handleAPIHeroSMSOffers(w http.ResponseWriter, r *http.Request) {
	key := p.herosmsKey(r.Context())
	service := herosmsReqService(r.URL.Query().Get("service"))
	maxPrice := 0.2
	if v, err := strconv.ParseFloat(r.URL.Query().Get("max_price"), 64); err == nil && v > 0 {
		maxPrice = v
	}
	minCount := 2000
	if v, err := strconv.Atoi(r.URL.Query().Get("min_count")); err == nil && v >= 0 {
		minCount = v
	}

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, herosmsOffersAPI+"?services="+url.QueryEscape(service), nil)
	req.Header.Set("Authorization", "ApiKey "+key)
	resp, err := p.herosmsClient().Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("HeroSMS offers HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)), http.StatusBadGateway)
		return
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
		http.Error(w, "解析 offers 失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	names := p.herosmsCountryNames(r.Context(), key)
	rows := make([]herosmsOfferRow, 0, 64)
	for cid, info := range parsed.Data[service] {
		for priceStr, count := range info.Map {
			price, perr := strconv.ParseFloat(priceStr, 64)
			if perr != nil || price > maxPrice || count <= minCount {
				continue
			}
			rows = append(rows, herosmsOfferRow{
				CountryID:   cid,
				CountryName: names[cid],
				Price:       price,
				Count:       count,
				Total:       info.Counts.Total,
			})
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
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), "getNumber", map[string]string{
		"service":    herosmsReqService(in.Service),
		"country":    in.Country,
		"maxPrice":   fmt.Sprintf("%.4f", in.MaxPrice),
		"fixedPrice": "true",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !strings.HasPrefix(text, "ACCESS_NUMBER:") {
		// 买号失败(余额不足 / 无号 / 价格不匹配等)HeroSMS 也回 200 + 文本码。
		http.Error(w, "买号未成功: "+text, http.StatusBadGateway)
		return
	}
	parts := strings.SplitN(text, ":", 3)
	writeJSON(w, map[string]any{"ok": true, "activation_id": parts[1], "phone": parts[2], "raw": text}, nil)
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
	code := ""
	if strings.HasPrefix(text, "STATUS_OK:") {
		code = strings.TrimPrefix(text, "STATUS_OK:")
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
	text, err := p.herosmsGet(r.Context(), p.herosmsKey(r.Context()), action, map[string]string{"id": in.ID})
	if err != nil {
		http.Error(w, err.Error()+" | "+text, http.StatusBadGateway)
		return
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
