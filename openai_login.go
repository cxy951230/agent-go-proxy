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
}

type openAILoginManager struct {
	mu sync.Mutex
	// refreshMu 串行化 token 刷新:上游刷新会轮换 refresh_token,
	// 并发刷新会让其中一次拿着已作废的旧 token 去请求。
	refreshMu sync.Mutex
	bridge    *nativeBridge
	store     *Store
	logins    map[string]*openAILoginProcess
	baseURL   string
	// Responses 请求体的基线默认值,取自 Bridge 的 JSON Schema。只取一次:schema 是静态的。
	schemaOnce     sync.Once
	schemaDefaults map[string]json.RawMessage
}

func newOpenAILoginManager(bridgePath string, store *Store) *openAILoginManager {
	return &openAILoginManager{
		bridge:  newNativeBridge(bridgePath),
		store:   store,
		logins:  make(map[string]*openAILoginProcess),
		baseURL: envOrDefault("CODEX_STATUS_BASE_URL", "https://chatgpt.com/backend-api"),
	}
}

func (m *openAILoginManager) Start(displayName string) (openAILoginStatus, error) {
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
	session, startJSON, err := m.bridge.loginStart(config)
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
	m.mu.Lock()
	m.logins[id] = process
	m.mu.Unlock()
	go m.finish(process)
	return process.status, nil
}

func (m *openAILoginManager) finish(process *openAILoginProcess) {
	defer os.RemoveAll(process.home)
	defer process.bridge.sessionFree(process.session)
	credentialsJSON, err := process.bridge.loginWait(process.session)
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
	go m.Refresh(accountID)
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
	return auth, nil
}

// refreshAuth 调 Bridge 刷新一次并回写结果。调用方需已持有 refreshMu。
func (m *openAILoginManager) refreshAuth(ctx context.Context, account OpenAIAccount) (OpenAIAccount, error) {
	raw, err := m.bridge.tokenRefresh([]byte(account.AuthJSON))
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
		raw, err := m.bridge.responsesSchema()
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
	raw, err := m.bridge.modelsList([]byte(account.AuthJSON))
	if err != nil {
		return fmt.Errorf("拉取模型列表失败: %w", err)
	}
	if err := m.store.UpdateOpenAIAccountModels(ctx, id, raw); err != nil {
		return fmt.Errorf("保存模型列表失败: %w", err)
	}
	return nil
}

func (m *openAILoginManager) Refresh(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, err := m.store.GetOpenAIAccount(ctx, id)
	if err != nil {
		return
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

	statusJSON, err := m.bridge.status([]byte(account.AuthJSON), []byte(m.baseURL))
	if err != nil {
		_ = m.store.UpdateOpenAIAccountStatus(ctx, id, "", err.Error())
		return
	}
	_ = m.store.UpdateOpenAIAccountStatus(ctx, id, statusJSON, "")
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
	m.mu.Lock()
	defer m.mu.Unlock()
	process, ok := m.logins[id]
	if !ok {
		return openAILoginStatus{}, false
	}
	return process.status, true
}

func (m *openAILoginManager) Cancel(id string) error {
	m.mu.Lock()
	process := m.logins[id]
	if process == nil {
		m.mu.Unlock()
		return errors.New("登录任务不存在")
	}
	if process.status.State != "waiting" {
		m.mu.Unlock()
		return nil
	}
	process.status.State, process.status.CompletedAt = "cancelled", time.Now()
	m.mu.Unlock()
	return process.bridge.loginCancel(process.session)
}

func (m *openAILoginManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, process := range m.logins {
		if process.status.State == "waiting" {
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
