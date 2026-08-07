package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"
)

type openAILoginStatus struct {
	ID           string    `json:"id"`
	State        string    `json:"state"`
	AuthURL      string    `json:"auth_url,omitempty"`
	CallbackPort uint16    `json:"callback_port,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	Email        string    `json:"email,omitempty"`
	PlanType     string    `json:"plan_type,omitempty"`
	Error        string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
}

func (s openAILoginStatus) done() bool {
	switch s.State {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

type bridgeLoginStarted struct {
	AuthURL      string `json:"auth_url"`
	CallbackPort uint16 `json:"callback_port"`
}

type openAILoginProcess struct {
	status      openAILoginStatus
	displayName string
	home        string
	bridge      *nativeBridge
	session     unsafe.Pointer
	browser     *chromeSession
}

type openAILoginManager struct {
	mu sync.Mutex
	// refreshMu 串行化 token 刷新:上游刷新会轮换 refresh_token,
	// 并发刷新会让其中一次拿着已作废的旧 token 去请求。
	refreshMu sync.Mutex
	bridge    *nativeBridge
	store     *Store
	proxyURL  string
	logins    map[string]*openAILoginProcess
	baseURL   string
	// Responses 请求体的基线默认值,取自 Bridge 的 JSON Schema。只取一次:schema 是静态的。
	schemaOnce     sync.Once
	schemaDefaults map[string]json.RawMessage
}

func newOpenAILoginManager(bridgePath string, store *Store, proxyURL string) *openAILoginManager {
	return &openAILoginManager{
		bridge:   newNativeBridge(bridgePath),
		store:    store,
		proxyURL: proxyURL,
		logins:   make(map[string]*openAILoginProcess),
		baseURL:  envOrDefault("CODEX_STATUS_BASE_URL", "https://chatgpt.com/backend-api"),
	}
}

// 进程环境变量在启动时已永久指向 scopedLocalProxyURL(见 main.go),Bridge(C 库)每次调用
// 直接从 env 读代理,因此这里不再每次加锁改 env——去掉全局串行化,批量刷额度/拉模型才能真正并发。
func (m *openAILoginManager) withProxyEnv(fn func() error) error {
	return fn()
}

func (m *openAILoginManager) bridgeCallLoginStart(config []byte) (unsafe.Pointer, string, error) {
	var session unsafe.Pointer
	var out string
	err := m.withProxyEnv(func() error {
		var err error
		session, out, err = m.bridge.loginStart(config)
		return err
	})
	return session, out, err
}

func (p *openAILoginProcess) bridgeCallLoginWait(session unsafe.Pointer) (string, error) {
	var out string
	err := bridgeProxyEnvMuLocked(p.bridge, func() error {
		var err error
		out, err = p.bridge.loginWait(session)
		return err
	})
	return out, err
}

func bridgeProxyEnvMuLocked(bridge *nativeBridge, fn func() error) error {
	return fn()
}

func (m *openAILoginManager) bridgeCallStatus(auth, baseURL []byte) (string, error) {
	var out string
	err := m.withProxyEnv(func() error {
		var err error
		out, err = m.bridge.status(auth, baseURL)
		return err
	})
	return out, err
}

func (m *openAILoginManager) bridgeCallTokenRefresh(auth []byte) (string, error) {
	var out string
	err := m.withProxyEnv(func() error {
		var err error
		out, err = m.bridge.tokenRefresh(auth)
		return err
	})
	return out, err
}

func (m *openAILoginManager) bridgeCallModelsList(auth []byte) (string, error) {
	var out string
	err := m.withProxyEnv(func() error {
		var err error
		out, err = m.bridge.modelsList(auth)
		return err
	})
	return out, err
}

func (m *openAILoginManager) bridgeCallResponsesSchema() (string, error) {
	var out string
	err := m.withProxyEnv(func() error {
		var err error
		out, err = m.bridge.responsesSchema()
		return err
	})
	return out, err
}

func (m *openAILoginManager) Start(displayName string) (openAILoginStatus, error) {
	return m.start(displayName, false)
}

func (m *openAILoginManager) StartWithBrowser(displayName string) (openAILoginStatus, error) {
	// 页面刷新后前端会丢失旧任务 id；旧的浏览器登录如果仍停在 waiting，会继续占用
	// Bridge 回调端口，导致再次点击只开出 about:blank/无响应。新开手动登录前先清理
	// 这类由 OPENAI 页面启动的旧浏览器任务；不碰 browser=nil 的 Bridge 会话(GPT 登录流程在用)。
	m.cancelWaitingBrowserLogins()
	return m.start(displayName, true)
}

func (m *openAILoginManager) cancelWaitingBrowserLogins() {
	type item struct {
		id      string
		browser *chromeSession
		session unsafe.Pointer
	}
	var items []item
	m.mu.Lock()
	for id, process := range m.logins {
		if process != nil && process.browser != nil && process.status.State == "waiting" {
			process.status.State = "cancelled"
			process.status.CompletedAt = time.Now()
			items = append(items, item{id: id, browser: process.browser, session: process.session})
		}
	}
	m.mu.Unlock()
	for _, it := range items {
		it.browser.Close()
		_ = m.bridge.loginCancel(it.session)
	}
}

func (m *openAILoginManager) start(displayName string, openBrowser bool) (openAILoginStatus, error) {
	id, err := randomLoginID()
	if err != nil {
		return openAILoginStatus{}, err
	}
	home, err := os.MkdirTemp("", "agent-go-proxy-openai-login-")
	if err != nil {
		return openAILoginStatus{}, err
	}
	config, err := json.Marshal(map[string]any{
		"codex_home": home, "open_browser": false, "port": 1455, "clear_existing": false,
	})
	if err != nil {
		_ = os.RemoveAll(home)
		return openAILoginStatus{}, err
	}
	session, startJSON, err := m.bridgeCallLoginStart(config)
	if err != nil {
		_ = os.RemoveAll(home)
		return openAILoginStatus{}, fmt.Errorf("启动 Codex Bridge 登录失败: %w", err)
	}
	var started bridgeLoginStarted
	if err := json.Unmarshal([]byte(startJSON), &started); err != nil || started.AuthURL == "" {
		m.bridge.sessionFree(session)
		_ = os.RemoveAll(home)
		return openAILoginStatus{}, errors.New("Codex Bridge 返回了无效的登录信息")
	}
	process := &openAILoginProcess{
		status: openAILoginStatus{
			ID: id, State: "waiting", AuthURL: started.AuthURL,
			CallbackPort: started.CallbackPort, StartedAt: time.Now(),
		},
		displayName: strings.TrimSpace(displayName), home: home,
		bridge: m.bridge, session: session,
	}
	if openBrowser {
		// OPENAI 页「+ 登录 GPT 账号」由本进程启动一个独立 Chrome,并指定 7899 代理。
		// OUTLOOK 页「登录 Codex」只需要这里返回 auth_url,它自己会启动自动化 Chrome,不能重复开。
		browser, browserErr := launchChromeWith(started.AuthURL, "agent-go-proxy-openai-login-browser-", []string{
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-background-networking",
			"--window-size=1120,860",
			"--proxy-server=" + scopedLocalProxyURL,
		})
		if browserErr != nil {
			m.bridge.sessionFree(session)
			_ = os.RemoveAll(home)
			return openAILoginStatus{}, fmt.Errorf("启动代理 Chrome 失败: %w", browserErr)
		}
		process.browser = browser
	}
	m.mu.Lock()
	m.logins[id] = process
	m.mu.Unlock()
	go m.finish(process)
	return process.status, nil
}

func (m *openAILoginManager) finish(process *openAILoginProcess) {
	defer os.RemoveAll(process.home)
	defer process.bridge.sessionFree(process.session)
	defer func() {
		if process.browser != nil {
			process.browser.Close()
		}
	}()
	credentialsJSON, err := process.bridgeCallLoginWait(process.session)
	if err != nil {
		m.complete(process.status.ID, "failed", nil, err.Error())
		return
	}
	account := accountFromCredentials(process.displayName, credentialsJSON)
	if account.AccountID == "" {
		m.complete(process.status.ID, "failed", nil, "登录凭证中没有 ChatGPT Account ID")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accountID, err := m.store.UpsertOpenAIAccount(ctx, account)
	if err != nil {
		m.complete(process.status.ID, "failed", nil, "保存 GPT 账号失败: "+err.Error())
		return
	}
	m.complete(process.status.ID, "completed", &account, "")
	go func() { _ = m.Refresh(accountID) }()
}

func accountFromCredentials(name, credentialsJSON string) OpenAIAccount {
	var envelope struct {
		Auth json.RawMessage `json:"auth"`
	}
	if json.Unmarshal([]byte(credentialsJSON), &envelope) != nil {
		return OpenAIAccount{Name: name}
	}
	var auth struct {
		Tokens *struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(envelope.Auth, &auth) != nil || auth.Tokens == nil {
		return OpenAIAccount{Name: name, AuthJSON: string(envelope.Auth)}
	}
	expiresAt, _ := accessTokenExpiry(envelopeAccessToken(envelope.Auth))
	email, userID, accountID, plan := decodeIDToken(auth.Tokens.IDToken)
	if accountID == "" {
		accountID = auth.Tokens.AccountID
	}
	if accountID == "" {
		accountID = userID
	}
	return OpenAIAccount{Name: name, Email: email, AccountID: accountID, PlanType: plan, AuthJSON: string(envelope.Auth), TokenExpiresAt: expiresAt}
}

func decodeIDToken(token string) (email, userID, accountID, plan string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", ""
	}
	var claims struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			UserID           string `json:"user_id"`
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			Plan             string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", "", ""
	}
	email = claims.Email
	if email == "" {
		email = claims.Profile.Email
	}
	userID = claims.Auth.ChatGPTUserID
	if userID == "" {
		userID = claims.Auth.UserID
	}
	return email, userID, claims.Auth.ChatGPTAccountID, claims.Auth.Plan
}

// tokenRefreshWindow 与上游 Codex 保持一致:access_token 剩余不足 5 分钟就提前刷新。
const tokenRefreshWindow = 5 * time.Minute

// envelopeAccessToken 从 auth.json 里取 access_token。
func envelopeAccessToken(raw []byte) string {
	var parsed struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	return parsed.Tokens.AccessToken
}

// accessTokenExpiry 读 access_token(JWT)的 exp 声明,纯本地解析、不发网络请求。
// 与上游 codex-login 的 parse_jwt_expiration 等价。
func accessTokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// needsRefresh 判断该账号的 access_token 是否已过期或即将过期。
// 过期时间未知时保守地返回 false,交给 401 兜底,避免每个请求都去刷新。
func needsRefresh(account OpenAIAccount) bool {
	expiresAt := account.TokenExpiresAt
	if expiresAt.IsZero() {
		if parsed, ok := accessTokenExpiry(envelopeAccessToken([]byte(account.AuthJSON))); ok {
			expiresAt = parsed
		} else {
			return false
		}
	}
	return time.Now().Add(tokenRefreshWindow).After(expiresAt)
}

// EnsureFreshAuth 在直连转发前保证凭证可用:没到期直接返回;快到期则通过 Bridge 刷新,
// 并把轮换后的完整 auth.json 回写数据库。整个过程串行化,避免并发请求重复刷新——
// 上游刷新会轮换 refresh_token,并发刷新会让其中一次拿到已作废的旧 token。
func (m *openAILoginManager) EnsureFreshAuth(ctx context.Context, account OpenAIAccount) (OpenAIAccount, error) {
	if !needsRefresh(account) {
		return account, nil
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	// 取锁期间可能已被别的请求刷过,按 id 重新读一次再判断。
	if latest, err := m.store.GetOpenAIAccount(ctx, account.ID); err == nil {
		account = latest
		if !needsRefresh(account) {
			return account, nil
		}
	}
	return m.refreshAuth(ctx, account)
}

// ForceRefreshAuth 无视过期时间强制刷新一次,用于上游返回 401 的兜底
// (时钟偏差、服务端提前失效等主动刷新覆盖不到的情况)。
//
// staleAccessToken 是刚刚被上游拒绝的那个 token:取锁后如果发现库里的 token 已经不是它了,
// 说明并发的另一个请求已经刷过,直接用新的即可,不必再轮换一次 refresh_token。
func (m *openAILoginManager) ForceRefreshAuth(ctx context.Context, staleAccessToken string) (*openaiAccountAuth, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	account, ok, err := m.store.DefaultOpenAIAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("没有可用的 GPT 账号")
	}
	if current := envelopeAccessToken([]byte(account.AuthJSON)); current != "" && current != staleAccessToken {
		return newAccountAuth(account)
	}
	refreshed, err := m.refreshAuth(ctx, account)
	if err != nil {
		return nil, err
	}
	return newAccountAuth(refreshed)
}

// newAccountAuth 由账号记录构造直连所需凭证。
func newAccountAuth(account OpenAIAccount) (*openaiAccountAuth, error) {
	auth, err := accountAuthFromJSON(account.AuthJSON, account.AccountID)
	if err != nil {
		return nil, err
	}
	auth.Label = fallback(account.Name, account.AccountID)
	auth.AccountDBID = account.ID
	return auth, nil
}

// ForceRefreshAccount 强制刷新指定账号的凭证(按 openai_accounts.id),用于多账号直连路径下
// 某个账号返回 401 时的兜底:只刷新它自己,不影响别的账号。
func (m *openAILoginManager) ForceRefreshAccount(ctx context.Context, id int64) (*openaiAccountAuth, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	account, err := m.store.GetOpenAIAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	refreshed, err := m.refreshAuth(ctx, account)
	if err != nil {
		return nil, err
	}
	return newAccountAuth(refreshed)
}

// refreshAuth 调 Bridge 刷新一次并回写结果。调用方需已持有 refreshMu。
func (m *openAILoginManager) refreshAuth(ctx context.Context, account OpenAIAccount) (OpenAIAccount, error) {
	raw, err := m.bridgeCallTokenRefresh([]byte(account.AuthJSON))
	if err != nil {
		return account, fmt.Errorf("调用 Bridge 刷新凭证失败: %w", err)
	}
	var result struct {
		Refreshed bool            `json:"refreshed"`
		Permanent bool            `json:"permanent"`
		Message   string          `json:"message"`
		Auth      json.RawMessage `json:"auth"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return account, fmt.Errorf("解析刷新结果失败: %w", err)
	}
	if !result.Refreshed {
		_ = m.store.SetOpenAIAccountRefreshError(ctx, account.ID, result.Message)
		if result.Permanent {
			return account, fmt.Errorf("凭证已失效，需要重新登录该账号: %s", result.Message)
		}
		return account, fmt.Errorf("刷新凭证失败: %s", result.Message)
	}

	authJSON := string(result.Auth)
	expiresAt, _ := accessTokenExpiry(envelopeAccessToken(result.Auth))
	if err := m.store.UpdateOpenAIAccountAuth(ctx, account.ID, authJSON, expiresAt); err != nil {
		return account, fmt.Errorf("回写刷新后的凭证失败: %w", err)
	}
	account.AuthJSON = authJSON
	account.TokenExpiresAt = expiresAt
	account.RefreshError = ""
	log.Printf("openai account %d token refreshed, expires at %s", account.ID, expiresAt.Format(time.RFC3339))
	return account, nil
}

// ResponsesDefaults 返回 Codex Responses 请求体各基线字段的默认值(键 → 默认值 JSON)。
// 取自 Bridge 的 JSON Schema 里每个 property 的 default。Bridge 不可用时返回 nil,调用方自行兜底。
func (m *openAILoginManager) ResponsesDefaults() map[string]json.RawMessage {
	m.schemaOnce.Do(func() {
		raw, err := m.bridgeCallResponsesSchema()
		if err != nil {
			log.Printf("load responses schema from bridge: %v", err)
			return
		}
		var schema struct {
			Properties map[string]struct {
				Default json.RawMessage `json:"default"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			log.Printf("parse responses schema: %v", err)
			return
		}
		defaults := make(map[string]json.RawMessage)
		for key, prop := range schema.Properties {
			if len(prop.Default) > 0 {
				defaults[key] = prop.Default
			}
		}
		m.schemaDefaults = defaults
	})
	return m.schemaDefaults
}

// RefreshModels 拉取该账号可用的模型目录并缓存到数据库。
// 目录按账号套餐过滤,且每个模型自带可选的推理强度与速度档位,所以换账号/换套餐后需重新拉取。
// 拉取前先保证凭证有效——过期 token 会让目录接口直接失败。
func (m *openAILoginManager) RefreshModels(ctx context.Context, id int64) error {
	account, err := m.store.GetOpenAIAccount(ctx, id)
	if err != nil {
		return err
	}
	if fresh, err := m.EnsureFreshAuth(ctx, account); err != nil {
		log.Printf("refresh auth before listing models (account %d): %v", id, err)
	} else {
		account = fresh
	}
	raw, err := m.bridgeCallModelsList([]byte(account.AuthJSON))
	if err != nil {
		return fmt.Errorf("拉取模型列表失败: %w", err)
	}
	if err := m.store.UpdateOpenAIAccountModels(ctx, id, raw); err != nil {
		return fmt.Errorf("保存模型列表失败: %w", err)
	}
	// 首次拉取(还没手动选过)时,把默认模型落库,避免页面下拉「显示了第一个但没存」的假象——
	// 否则直连匹配 selected_model 时查不到账号,报「没有账号配置了模型」。
	if strings.TrimSpace(account.SelectedModel) == "" {
		if model, effort := defaultModelFromCatalog(raw); model != "" {
			if err := m.store.UpdateOpenAIAccountSettings(ctx, id, model, effort, "default"); err != nil {
				log.Printf("persist default model for account %d: %v", id, err)
			}
		}
	}
	return nil
}

type catalogModel struct {
	Model                  string `json:"model"`
	IsDefault              bool   `json:"is_default"`
	ShowInPicker           *bool  `json:"show_in_picker"`
	DefaultReasoningEffort string `json:"default_reasoning_effort"`
}

// defaultModelFromCatalog 从 Bridge 模型目录里挑「默认选中」的模型,规则与前端 currentModel 一致:
// 只看 picker 可见项,优先 is_default,否则取第一项;返回模型 slug 与其默认推理强度。
func defaultModelFromCatalog(raw string) (string, string) {
	var catalog struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
		return "", ""
	}
	var first *catalogModel
	for i := range catalog.Models {
		mdl := &catalog.Models[i]
		if mdl.Model == "" || (mdl.ShowInPicker != nil && !*mdl.ShowInPicker) {
			continue
		}
		if mdl.IsDefault {
			return mdl.Model, mdl.DefaultReasoningEffort
		}
		if first == nil {
			first = mdl
		}
	}
	if first != nil {
		return first.Model, first.DefaultReasoningEffort
	}
	return "", ""
}

// Refresh 刷新单个账号:补齐 Token 过期时间 → 按需刷新凭证 → 查额度并写 status_json。
// 返回 error 只为批量刷新能统计失败条数,单账号接口仍忽略它(失败原因已写进 status_error 由页面展示)。
func (m *openAILoginManager) Refresh(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, err := m.store.GetOpenAIAccount(ctx, id)
	if err != nil {
		return err
	}
	// 先按已存的 access_token 本地补齐过期时间(历史记录该字段为空),
	// 再按需刷新凭证,最后才查额度。
	if expiresAt, ok := accessTokenExpiry(envelopeAccessToken([]byte(account.AuthJSON))); ok && !expiresAt.Equal(account.TokenExpiresAt) {
		if err := m.store.UpdateOpenAIAccountTokenExpiry(ctx, id, expiresAt); err != nil {
			log.Printf("backfill token expiry for account %d: %v", id, err)
		}
		account.TokenExpiresAt = expiresAt
	}
	if refreshed, err := m.EnsureFreshAuth(ctx, account); err != nil {
		log.Printf("refresh openai account %d: %v", id, err)
	} else {
		account = refreshed
	}

	statusJSON, err := m.bridgeCallStatus([]byte(account.AuthJSON), []byte(m.baseURL))
	if err != nil {
		_ = m.store.UpdateOpenAIAccountStatus(ctx, id, "", err.Error())
		return err
	}
	return m.store.UpdateOpenAIAccountStatus(ctx, id, statusJSON, "")
}

func (m *openAILoginManager) complete(id, state string, account *OpenAIAccount, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.logins[id]
	if process == nil || process.status.State == "cancelled" {
		return
	}
	process.status.State, process.status.Error, process.status.CompletedAt = state, message, time.Now()
	if account != nil {
		process.status.AccountID, process.status.Email, process.status.PlanType = account.AccountID, account.Email, account.PlanType
	}
}

func (m *openAILoginManager) Status(id string) (openAILoginStatus, bool) {
	var cancelSession unsafe.Pointer
	m.mu.Lock()
	process, ok := m.logins[id]
	if !ok {
		m.mu.Unlock()
		return openAILoginStatus{}, false
	}
	if process.status.State == "waiting" && process.browser != nil && !process.browser.Alive() {
		process.status.State = "cancelled"
		process.status.Error = "Chrome 窗口已关闭"
		process.status.CompletedAt = time.Now()
		cancelSession = process.session
	}
	st := process.status
	m.mu.Unlock()
	if cancelSession != nil {
		_ = m.bridge.loginCancel(cancelSession)
	}
	return st, true
}

func (m *openAILoginManager) Cancel(id string) error {
	m.mu.Lock()
	process := m.logins[id]
	if process == nil {
		m.mu.Unlock()
		return errors.New("登录任务不存在")
	}
	if process.status.done() {
		m.mu.Unlock()
		return nil
	}
	process.status.State, process.status.CompletedAt = "cancelled", time.Now()
	browser := process.browser
	session := process.session
	m.mu.Unlock()
	if browser != nil {
		browser.Close()
	}
	return process.bridge.loginCancel(session)
}

func (m *openAILoginManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, process := range m.logins {
		if process.status.State == "waiting" {
			if process.browser != nil {
				process.browser.Close()
			}
			_ = process.bridge.loginCancel(process.session)
		}
	}
}

func randomLoginID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func defaultBridgeLoginBin() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_BRIDGE_LIB")); value != "" {
		return value
	}
	extension := ".dylib"
	if runtime.GOOS == "linux" {
		extension = ".so"
	}
	candidates := []string{
		filepath.Join("..", "codex-bridge", "target", "release", "libcodex_bridge"+extension),
		filepath.Join(filepath.Dir(os.Args[0]), "libcodex_bridge"+extension),
	}
	for _, candidate := range candidates {
		if path, err := filepath.Abs(candidate); err == nil {
			if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
				return path
			}
		}
	}
	return candidates[0]
}
