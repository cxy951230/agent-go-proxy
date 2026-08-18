package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// outlookRefreshConcurrency 是批量刷新的并发账号数。刷新要连微软的 authorize/token 端点,
// 并发太高既容易被限流,也可能把本地出站代理打满,取 3 折中。
const outlookRefreshConcurrency = 3

// startRefreshAllOutlookTokens 异步刷新所有「有 access token」的账号,等价于逐行点「刷新 Token」。
// 返回本次要刷新的账号数;已有任务在跑时返回 (0, false)。任务状态见 batch_refresh.go。
func (p *proxyServer) startRefreshAllOutlookTokens(ctx context.Context) (int, bool, error) {
	ids, err := p.store.ListOutlookRefreshableIDs(ctx)
	if err != nil {
		return 0, false, err
	}
	// 单账号 90s 上限(refreshOutlookToken 内部另有 45s),失败原因带上邮箱好定位。
	total, started := startBatchRefresh(p.outlookJob, ids, outlookRefreshConcurrency, 90*time.Second,
		func(runCtx context.Context, id int64) error {
			email, _ := p.store.GetOutlookAccountEmail(runCtx, id)
			if _, rerr := p.refreshOutlookToken(runCtx, id); rerr != nil {
				return fmt.Errorf("%s：%w", orDefaultStr(email, "?"), rerr)
			}
			return nil
		})
	return total, started, nil
}

// outlookAutoRefreshInterval 是定时刷新全部 Outlook token 的间隔。
const outlookAutoRefreshInterval = 10 * time.Minute

// settingOutlookAutoRefresh 是「定时刷新 Outlook token 是否开启」的 app_settings 键("1"/"0")。
const settingOutlookAutoRefresh = "outlook_token_auto_refresh"

// runOutlookAutoRefresh 后台每 outlookAutoRefreshInterval 刷新一次全部账号的 token,直到 ctx 取消。
// goroutine 常驻,每次 tick 先看开关(p.outlookAutoRefresh):关着就跳过本次、只保留心跳,
// 这样开关一点就生效、不用重启。与手动「刷新全部 Token」共用一个任务实例(p.outlookJob),
// 撞上手动任务在跑就跳过本次(started=false)。
func (p *proxyServer) runOutlookAutoRefresh(ctx context.Context) {
	ticker := time.NewTicker(outlookAutoRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if p.outlookAutoRefresh.Load() {
			if _, started, err := p.startRefreshAllOutlookTokens(ctx); err != nil {
				log.Printf("定时刷新 Outlook token 失败: %v", err)
			} else if !started {
				log.Printf("定时刷新 Outlook token: 已有刷新任务在进行中，跳过本次")
			}
		}
	}
}

// handleAPIOutlookAutoRefresh 返回定时刷新 token 开关的当前状态。
func (p *proxyServer) handleAPIOutlookAutoRefresh(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"enabled": p.outlookAutoRefresh.Load()}, nil)
}

// handleAPIOutlookAutoRefreshToggle 设置定时刷新 token 开关(body {enabled:bool}),持久化到 app_settings。
func (p *proxyServer) handleAPIOutlookAutoRefreshToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	p.outlookAutoRefresh.Store(in.Enabled)
	val := "0"
	if in.Enabled {
		val = "1"
	}
	if err := p.store.SetSetting(r.Context(), settingOutlookAutoRefresh, val); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": in.Enabled}, nil)
}

// Outlook Web(consumer)静默刷新用到的常量,与 outlook-login-automation skill 保持一致。
const (
	outlookClientID     = "9199bf20-a13f-4107-85dc-02114787ef48"
	outlookRedirectURI  = "https://outlook.live.com/mail/oauthRedirect.html"
	outlookScope        = "service::outlook.office.com::MBI_SSL openid profile offline_access"
	outlookAuthorizeURL = "https://login.microsoftonline.com/consumers/oauth2/v2.0/authorize"
	outlookTokenURL     = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
	outlookClaims       = `{"access_token":{"xms_cc":{"values":["CP1"]}}}`
)

// refreshOutlookToken 用库里保存的登录会话 cookie 跑 authorize?prompt=none 的 PKCE 静默授权,
// 换取全新的 access / refresh / id token,并把重定向链里被微软续期的 cookie 一并写回库
// (滑动会话窗口才能真正往后滚)。不打开浏览器、不依赖 refresh_token(该 token 24h 固定窗口,
// 滚动刷新不延长),纯 Go 实现,不调用任何外部脚本。
func (p *proxyServer) refreshOutlookToken(ctx context.Context, id int64) (OutlookAccount, error) {
	account, err := p.refreshOutlookTokenOnce(ctx, id)
	if err != nil {
		// 任何一条失败路径都要落状态,不能只标记「静默授权失败」那一种。
		// 最常见的其实是「没有已保存的 cookie」,以前走的是直接 return,页面上
		// 刷新状态一直停在 `-`,看不出点过刷新、也看不出为什么没成。
		// 用独立的 ctx:请求可能已经取消/超时,但失败原因照样要记下来。
		markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.store.MarkOutlookRefreshError(markCtx, id, err.Error())
		cancel()
	}
	return account, err
}

func (p *proxyServer) refreshOutlookTokenOnce(ctx context.Context, id int64) (OutlookAccount, error) {
	email, cookiesJSON, userAgent, _, err := p.store.GetOutlookRefreshInput(ctx, id)
	if err != nil {
		return OutlookAccount{}, err
	}
	if strings.TrimSpace(cookiesJSON) == "" {
		return OutlookAccount{}, fmt.Errorf("账号 %s 没有已保存的 cookie/会话,无法刷新,请重新登录", email)
	}
	var rawCookies []map[string]any
	if err := json.Unmarshal([]byte(cookiesJSON), &rawCookies); err != nil {
		return OutlookAccount{}, fmt.Errorf("解析已保存 cookie 失败: %w", err)
	}
	if userAgent == "" {
		userAgent = "Mozilla/5.0"
	}
	store := newOutlookCookieStore(rawCookies)

	runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client := &http.Client{
		Transport: p.proxiedTransport, // OUTLOOK 菜单请求走固定本地代理 127.0.0.1:7899
		// 手动跟重定向:需要读 Location 里的 #code= 片段,并逐跳收集 Set-Cookie。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       40 * time.Second,
	}

	tj, err := silentAuthorizeOutlook(runCtx, client, store, userAgent)
	if err != nil {
		return OutlookAccount{}, err
	}

	upd, err := outlookTokenUpdateFromResponse(tj, userAgent, store.export())
	if err != nil {
		return OutlookAccount{}, err
	}

	if err := p.store.UpdateOutlookTokens(ctx, id, upd); err != nil {
		return OutlookAccount{}, err
	}
	return p.store.GetOutlookAccountByID(ctx, id)
}

// outlookTokenUpdateFromResponse 把 token 端点返回的 JSON + 当前 cookie 快照,
// 拼成要写回 outlook_login_tokens 的一组字段。静默刷新与手动登录抓取共用这一份逻辑,
// 保证两条路径落库的字段口径完全一致。
func outlookTokenUpdateFromResponse(tj map[string]any, userAgent string, cookies []map[string]any) (outlookTokenUpdate, error) {
	now := time.Now()
	ci := decodeOutlookClientInfo(asString(tj["client_info"]))
	upd := outlookTokenUpdate{
		TokenType:            asString(tj["token_type"]),
		Scope:                firstNonEmpty(asString(tj["scope"]), outlookScope),
		AccessToken:          asString(tj["access_token"]),
		RefreshToken:         asString(tj["refresh_token"]),
		IDToken:              asString(tj["id_token"]),
		ClientInfo:           asString(tj["client_info"]),
		UserAgent:            userAgent,
		DisplayName:          asString(ci["name"]),
		TenantID:             firstNonEmpty(asString(ci["tid"]), asString(ci["utid"])),
		AccountOID:           firstNonEmpty(asString(ci["oid"]), asString(ci["uid"])),
		TokenIssuedAt:        now,
		AccessTokenExpiresAt: now,
	}
	if uid, utid := asString(ci["uid"]), asString(ci["utid"]); uid != "" && utid != "" {
		upd.HomeAccountID = uid + "." + utid
	}
	if v, ok := jsonInt(tj["expires_in"]); ok {
		upd.ExpiresIn = &v
		upd.AccessTokenExpiresAt = now.Add(time.Duration(v) * time.Second)
	}
	if v, ok := jsonInt(tj["ext_expires_in"]); ok {
		upd.ExtExpiresIn = &v
	}
	if v, ok := jsonInt(tj["refresh_token_expires_in"]); ok {
		upd.RefreshTokenExpiresIn = &v
		t := now.Add(time.Duration(v) * time.Second)
		upd.RefreshTokenExpiresAt = &t
	}
	cookiesOut, err := json.Marshal(cookies)
	if err != nil {
		return upd, err
	}
	upd.CookiesJSON = string(cookiesOut)
	upd.CookieCount = len(cookies)
	return upd, nil
}

// silentAuthorizeOutlook 执行 authorize?prompt=none → 换 code → token 两步,返回 token 端点的 JSON。
// 过程中的 Set-Cookie 逐跳并入 store,供调用方写回库。
func silentAuthorizeOutlook(ctx context.Context, client *http.Client, store *outlookCookieStore, ua string) (map[string]any, error) {
	verifier, challenge := genPKCE()
	params := url.Values{}
	params.Set("client_id", outlookClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", outlookRedirectURI)
	params.Set("scope", outlookScope)
	params.Set("response_mode", "fragment")
	params.Set("client_info", "1")
	params.Set("prompt", "none")
	params.Set("nonce", b64urand(16))
	params.Set("state", b64urand(24))
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("x-client-SKU", "msal.js.browser")
	params.Set("x-client-VER", "3.28.1")
	params.Set("client-request-id", randUUID())
	params.Set("claims", outlookClaims)

	loc := outlookAuthorizeURL + "?" + params.Encode()
	var final string
	var lastStatus int
	for i := 0; i < 15; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		applyOutlookHeaders(req, ua, store)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("authorize 请求失败: %w", err)
		}
		store.apply(resp.Cookies(), req.URL.Hostname())
		location := resp.Header.Get("Location")
		lastStatus = resp.StatusCode
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if outlookHasCodeOrError(location) {
			final = location
			break
		}
		if location == "" {
			final = loc
			break
		}
		if ref, err := url.Parse(location); err == nil {
			loc = req.URL.ResolveReference(ref).String()
		} else {
			loc = location
		}
	}

	code, autherr := extractOutlookCode(final)
	if autherr != "" {
		return nil, fmt.Errorf("silent authorize 被拒绝(会话可能已失效,需重新登录): %s", autherr)
	}
	if code == "" {
		return nil, fmt.Errorf("prompt=none 未拿到授权码(last_status=%d),会话可能已失效,需重新登录", lastStatus)
	}

	form := url.Values{}
	form.Set("client_id", outlookClientID)
	form.Set("redirect_uri", outlookRedirectURI)
	form.Set("scope", outlookScope)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("client_info", "1")
	form.Set("claims", outlookClaims)

	tokURL := outlookTokenURL + "?client-request-id=" + randUUID()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	applyOutlookHeaders(req, ua, store)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token 请求失败: %w", err)
	}
	store.apply(resp.Cookies(), req.URL.Hostname())
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var tj map[string]any
	if err := json.Unmarshal(body, &tj); err != nil {
		return nil, fmt.Errorf("token 端点返回非 JSON(status=%d): %s", resp.StatusCode, truncate(string(body), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token 端点 status=%d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return tj, nil
}

func applyOutlookHeaders(req *http.Request, ua string, store *outlookCookieStore) {
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://outlook.live.com/")
	req.Header.Set("Origin", "https://outlook.live.com")
	if cookie := store.header(req.URL.Hostname(), req.URL.Path); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

// -------- 手动 cookie 存储(按域名/路径匹配发送,逐跳并入 Set-Cookie 续期,保留原富字段) --------

type outlookCookieStore struct {
	items []map[string]any
	idx   map[string]int
}

func newOutlookCookieStore(raw []map[string]any) *outlookCookieStore {
	s := &outlookCookieStore{idx: map[string]int{}}
	for _, c := range raw {
		if c == nil || asString(c["name"]) == "" {
			continue
		}
		item := make(map[string]any, len(c))
		for k, v := range c {
			item[k] = v
		}
		s.items = append(s.items, item)
		s.idx[cookieKey(asString(item["name"]), asString(item["domain"]))] = len(s.items) - 1
	}
	return s
}

func cookieKey(name, domain string) string {
	return name + "\x00" + strings.ToLower(strings.TrimPrefix(domain, "."))
}

// header 拼出发往指定 host/path 的 Cookie 头:域名后缀匹配 + 路径前缀匹配,复刻 requests 的行为。
func (s *outlookCookieStore) header(host, path string) string {
	host = strings.ToLower(host)
	if path == "" {
		path = "/"
	}
	var parts []string
	for _, c := range s.items {
		cd := strings.ToLower(strings.TrimPrefix(asString(c["domain"]), "."))
		if cd == "" {
			continue
		}
		if host != cd && !strings.HasSuffix(host, "."+cd) {
			continue
		}
		cp := asString(c["path"])
		if cp == "" {
			cp = "/"
		}
		if !strings.HasPrefix(path, cp) {
			continue
		}
		parts = append(parts, asString(c["name"])+"="+asString(c["value"]))
	}
	return strings.Join(parts, "; ")
}

// apply 把一跳响应的 Set-Cookie 并入存储:同名同域就地更新 value/expires(保留其余富字段),
// 新 cookie 追加。这样续期后的会话 cookie 才能落库。
func (s *outlookCookieStore) apply(cookies []*http.Cookie, reqHost string) {
	for _, c := range cookies {
		domain := c.Domain
		if domain == "" {
			domain = reqHost // host-only cookie
		}
		key := cookieKey(c.Name, domain)
		if i, ok := s.idx[key]; ok {
			s.items[i]["value"] = c.Value
			if !c.Expires.IsZero() {
				s.items[i]["expires"] = float64(c.Expires.Unix())
			}
			continue
		}
		item := map[string]any{"name": c.Name, "value": c.Value, "domain": domain, "path": orDefaultStr(c.Path, "/"), "secure": c.Secure}
		if !c.Expires.IsZero() {
			item["expires"] = float64(c.Expires.Unix())
		}
		s.items = append(s.items, item)
		s.idx[key] = len(s.items) - 1
	}
}

func (s *outlookCookieStore) export() []map[string]any { return s.items }

// -------- 小工具 --------

func genPKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func b64urand(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func outlookHasCodeOrError(loc string) bool {
	return strings.Contains(loc, "#code=") || strings.Contains(loc, "?code=") ||
		strings.Contains(loc, "#error=") || strings.Contains(loc, "?error=")
}

// extractOutlookCode 优先从片段(#...)取 code/error,没有再看查询串,复刻 Python 的 parse_qs(fragment or query)。
func extractOutlookCode(loc string) (code, autherr string) {
	if loc == "" {
		return "", ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", ""
	}
	raw := u.Fragment
	if raw == "" {
		raw = u.RawQuery
	}
	vals, _ := url.ParseQuery(raw)
	if e := vals.Get("error"); e != "" {
		return "", strings.TrimSpace(e + " " + vals.Get("error_description"))
	}
	return vals.Get("code"), ""
}

func decodeOutlookClientInfo(s string) map[string]any {
	if s == "" {
		return map[string]any{}
	}
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func jsonInt(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case int64:
		return t, true
	case int:
		return int64(t), true
	default:
		return 0, false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
