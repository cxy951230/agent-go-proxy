package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// OUTLOOK 页的「手动登录」:代理拉起一个独立 profile 的 Chrome 窗口,用户自己在里面
// 输账号密码/过验证,代理只负责轮询登录是否完成,完成后把浏览器里的 cookie 抓出来,
// 复用已有的 silentAuthorizeOutlook 换 token,再落库。
//
// 为什么必须自己拉浏览器:这行记录的价值在 cookies_json(静默刷新全靠它),而用户日常
// Chrome 的 cookie 本进程读不到;而且 Chrome 已在运行时再传 --remote-debugging-* 参数
// 会被直接忽略(新进程只是把请求转给已有实例),Chrome 136+ 还禁止默认 profile 开调试通道。
const (
	outlookLoginStartURL = "https://login.live.com/?mkt=en-US&lc=1033"
	outlookMailboxURL    = "https://outlook.live.com/mail/0/"
	// 手动登录要留足时间输密码、过验证码、收验证邮件。
	outlookLoginTimeout   = 10 * time.Minute
	outlookLoginPollEvery = 1500 * time.Millisecond
	// 登录成功后等邮箱页加载完再抓 cookie,拿到 outlook.live.com 域的会话。
	outlookMailboxWait = 45 * time.Second
)

type outlookLoginStatus struct {
	ID          string    `json:"id"`
	State       string    `json:"state"` // waiting / capturing / completed / failed / cancelled
	Email       string    `json:"email,omitempty"`
	Message     string    `json:"message,omitempty"`
	Error       string    `json:"error,omitempty"`
	AccountID   int64     `json:"account_id,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func (s outlookLoginStatus) done() bool {
	return s.State == "completed" || s.State == "failed" || s.State == "cancelled"
}

type outlookLoginProcess struct {
	status  outlookLoginStatus
	cancel  context.CancelFunc
	session *chromeSession
}

type outlookLoginManager struct {
	mu     sync.Mutex
	logins map[string]*outlookLoginProcess
	store  *Store
	// transport 复用代理的出站配置(系统代理 / TLS),与静默刷新走同一条出口。
	transport http.RoundTripper
	// wg 让 Close 能等登录任务真正收尾:只发取消信号就返回的话,进程会先退出,
	// 拉起的 Chrome 变成孤儿进程、临时 profile 也留在磁盘上。
	wg sync.WaitGroup
}

func newOutlookLoginManager(store *Store, transport http.RoundTripper) *outlookLoginManager {
	return &outlookLoginManager{
		logins:    make(map[string]*outlookLoginProcess),
		store:     store,
		transport: transport,
	}
}

// Start 拉起一个登录任务。auto=true 时先跑自动填表流程,卡住了再转人工;
// auto=false 就是纯手动。同一时刻只允许一个:多开 Chrome 窗口只会让用户困惑,
// 而且两个窗口登不同账号时抓 cookie 容易串。已有任务在跑时返回 error。
func (m *outlookLoginManager) Start(email, password string, auto bool) (outlookLoginStatus, error) {
	id, err := randomLoginID()
	if err != nil {
		return outlookLoginStatus{}, err
	}
	email = strings.TrimSpace(email)

	m.mu.Lock()
	for existingID, process := range m.logins {
		if !process.status.done() {
			if process.session != nil && !process.session.Alive() {
				process.status.State = "cancelled"
				process.status.Message = "Chrome 窗口已关闭"
				process.status.CompletedAt = time.Now()
				process.cancel()
			} else {
				m.mu.Unlock()
				return outlookLoginStatus{}, fmt.Errorf("已有一个登录任务在进行中(%s),请先完成或取消", existingID)
			}
		}
		// 顺手清掉早就结束的任务,避免 map 无限增长。
		if time.Since(process.status.CompletedAt) > 30*time.Minute {
			delete(m.logins, existingID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), outlookLoginTimeout)
	message := "已打开 Chrome 窗口，请在窗口里完成 Microsoft 登录"
	if auto {
		message = "正在打开 Chrome 窗口，准备自动登录…"
	}
	process := &outlookLoginProcess{
		status: outlookLoginStatus{
			ID:        id,
			State:     "waiting",
			Email:     email,
			Message:   message,
			StartedAt: time.Now(),
		},
		cancel: cancel,
	}
	m.logins[id] = process
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(ctx, id, email, password, auto)
	}()
	return process.status, nil
}

func (m *outlookLoginManager) Status(id string) (outlookLoginStatus, bool) {
	m.mu.Lock()
	process, ok := m.logins[id]
	if !ok {
		m.mu.Unlock()
		return outlookLoginStatus{}, false
	}
	if !process.status.done() && process.session != nil && !process.session.Alive() {
		process.status.State = "cancelled"
		process.status.Message = "Chrome 窗口已关闭"
		process.status.CompletedAt = time.Now()
		process.cancel()
	}
	st := process.status
	m.mu.Unlock()
	return st, true
}

func (m *outlookLoginManager) Cancel(id string) error {
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
	process.status.State = "cancelled"
	process.status.Message = "已取消"
	process.status.CompletedAt = time.Now()
	cancel := process.cancel
	session := process.session
	m.mu.Unlock()
	cancel()
	if session != nil {
		session.Close()
	}
	return nil
}

// Close 取消所有进行中的登录并等它们收尾。必须等:run 的 defer 里才会关掉 Chrome、
// 删掉临时 profile,只发信号就返回会把 Chrome 留成孤儿进程。
func (m *outlookLoginManager) Close() {
	m.mu.Lock()
	for _, process := range m.logins {
		if !process.status.done() {
			process.cancel()
		}
	}
	m.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		log.Printf("outlook login: 等待登录任务收尾超时，可能残留 Chrome 进程")
	}
}

// setState 更新进行中任务的状态;已被取消的任务不再改写,让「取消」是终态。
func (m *outlookLoginManager) setState(id, state, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.logins[id]
	if process == nil || process.status.State == "cancelled" {
		return
	}
	process.status.State = state
	process.status.Message = message
}

func (m *outlookLoginManager) complete(id, state, email string, accountID int64, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.logins[id]
	if process == nil || process.status.State == "cancelled" {
		return
	}
	process.status.State = state
	process.status.CompletedAt = time.Now()
	process.status.AccountID = accountID
	if email != "" {
		process.status.Email = email
	}
	if state == "failed" {
		process.status.Error = message
		process.status.Message = ""
		return
	}
	process.status.Message = message
}

func (m *outlookLoginManager) attachSession(id string, session *chromeSession) {
	m.mu.Lock()
	process := m.logins[id]
	if process != nil {
		process.session = session
	}
	m.mu.Unlock()
}

func (m *outlookLoginManager) cancelled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.logins[id]
	return process != nil && process.status.State == "cancelled"
}

func (m *outlookLoginManager) run(ctx context.Context, id, email, password string, auto bool) {
	session, err := launchChrome(outlookLoginStartURL)
	if err != nil {
		m.complete(id, "failed", "", 0, err.Error())
		return
	}
	m.attachSession(id, session)
	defer session.Close()

	// 自动模式:先按 DOM 状态机填表。撞到人机验证/两步验证/未识别页面时不算失败,
	// 把原因显示到页面上、窗口留着,下面照常回落到手动等待,由用户接管完成。
	autoLoggedIn := false
	if auto {
		m.setState(id, "waiting", "正在自动登录…")
		result, autoErr := startOutlookAutoLogin(ctx, session, email, password, func(message string) {
			m.setState(id, "waiting", message)
		})
		switch {
		case autoErr != nil:
			m.setState(id, "waiting", "自动登录无法启动（"+autoErr.Error()+"），请在窗口里手动完成登录")
		case result.Fatal:
			m.complete(id, "failed", "", 0, result.Message)
			return
		case result.NeedsHuman:
			m.setState(id, "waiting", result.Message)
		default:
			autoLoggedIn = result.State == "success"
		}
	}

	if !autoLoggedIn {
		if err := waitForOutlookLogin(ctx, session); err != nil {
			m.complete(id, "failed", "", 0, err.Error())
			return
		}
	}
	if m.cancelled(id) {
		return
	}
	m.setState(id, "capturing", "登录成功，正在抓取 cookie 与 token…")

	// 打开一次邮箱页:让 Outlook Web 自己走完会话建立,顺带拿到 outlook.live.com 域的
	// cookie(与 skill 的行为一致)。失败不致命,静默授权只依赖 login.* 域的会话 cookie。
	openCtx, cancelOpen := context.WithTimeout(ctx, 15*time.Second)
	if err := session.OpenTab(openCtx, outlookMailboxURL); err != nil {
		log.Printf("outlook login: 打开邮箱页失败(忽略): %v", err)
	}
	cancelOpen()
	waitForOutlookURL(ctx, session, outlookMailboxWait, func(url string) bool {
		return strings.Contains(strings.ToLower(url), "outlook.live.com/mail")
	})

	callCtx, cancelCall := context.WithTimeout(ctx, 20*time.Second)
	userAgent, uaErr := session.UserAgent(callCtx)
	if uaErr != nil {
		log.Printf("outlook login: 取 User-Agent 失败(用默认值): %v", uaErr)
	}
	cookies, err := session.Cookies(callCtx)
	cancelCall()
	if err != nil {
		m.complete(id, "failed", "", 0, "抓取浏览器 cookie 失败: "+err.Error())
		return
	}
	stored := chromeCookiesToStore(cookies)
	if len(stored) == 0 {
		m.complete(id, "failed", "", 0, "没有抓到任何 cookie，登录可能没真正完成")
		return
	}

	cookieStore := newOutlookCookieStore(stored)
	client := &http.Client{
		Transport: m.transport,
		// 与静默刷新一致:手动跟重定向,才能读到 Location 里的 #code=。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       40 * time.Second,
	}
	authCtx, cancelAuth := context.WithTimeout(ctx, 60*time.Second)
	tokens, err := silentAuthorizeOutlook(authCtx, client, cookieStore, orDefaultStr(userAgent, "Mozilla/5.0"))
	cancelAuth()
	if err != nil {
		m.complete(id, "failed", "", 0, "用登录后的 cookie 换 token 失败: "+err.Error())
		return
	}

	update, err := outlookTokenUpdateFromResponse(tokens, orDefaultStr(userAgent, "Mozilla/5.0"), cookieStore.export())
	if err != nil {
		m.complete(id, "failed", "", 0, err.Error())
		return
	}
	resolvedEmail := outlookEmailFromLogin(tokens, email)
	if m.cancelled(id) {
		return
	}
	// token 已经拿到手,落库不能再受登录 ctx(可能已接近 10 分钟上限)影响。
	saveCtx, cancelSave := context.WithTimeout(context.Background(), 15*time.Second)
	accountID, err := m.store.SaveOutlookLogin(saveCtx, resolvedEmail, password, update)
	cancelSave()
	if err != nil {
		m.complete(id, "failed", resolvedEmail, 0, "保存登录结果失败: "+err.Error())
		return
	}
	m.complete(id, "completed", resolvedEmail, accountID, "登录成功，token 已保存")
}

// waitForOutlookLogin 轮询标签页 URL,等用户在窗口里登录完成。
func waitForOutlookLogin(ctx context.Context, session *chromeSession) error {
	if err := waitForOutlookURL(ctx, session, outlookLoginTimeout, outlookLoginSucceeded); err != nil {
		return err
	}
	return nil
}

// waitForOutlookURL 每隔 outlookLoginPollEvery 拉一次标签页列表,直到有 URL 命中 match。
// 用户手动关掉窗口时 Chrome 退出,这里立即返回错误,不干等到超时。
func waitForOutlookURL(ctx context.Context, session *chromeSession, timeout time.Duration, match func(string) bool) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(outlookLoginPollEvery)
	defer ticker.Stop()
	for {
		if !session.Alive() {
			return errors.New("Chrome 窗口已关闭，登录未完成")
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		urls, err := session.PageURLs(callCtx)
		cancel()
		if err == nil {
			for _, url := range urls {
				if match(url) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("登录已取消或超时")
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return errors.New("等待登录超时")
		}
	}
}

// outlookLoginSucceeded 判断当前 URL 是否表示登录已完成。判据与 skill 的状态机一致:
// 进到 Outlook 信箱,或被重定向到已登录的 Microsoft 账户主页。
func outlookLoginSucceeded(url string) bool {
	u := strings.ToLower(url)
	return strings.Contains(u, "outlook.live.com/mail") ||
		strings.Contains(u, "outlook.office.com/mail") ||
		strings.Contains(u, "account.microsoft.com")
}

// chromeCookiesToStore 把 CDP 的 cookie 转成 outlookCookieStore / cookies_json 用的形状,
// 字段名与 skill 写入的保持一致,这样静默刷新读哪一边都一样。
func chromeCookiesToStore(cookies []chromeCookie) []map[string]any {
	out := make([]map[string]any, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Domain == "" {
			continue
		}
		item := map[string]any{
			"name":     cookie.Name,
			"value":    cookie.Value,
			"domain":   cookie.Domain,
			"path":     orDefaultStr(cookie.Path, "/"),
			"secure":   cookie.Secure,
			"httpOnly": cookie.HTTPOnly,
		}
		if cookie.Expires > 0 {
			item["expires"] = cookie.Expires
		}
		if cookie.SameSite != "" {
			item["sameSite"] = cookie.SameSite
		}
		out = append(out, item)
	}
	return out
}

// outlookEmailFromLogin 决定这条记录用哪个邮箱:弹窗里填了就用填的,
// 否则从 id_token 的 claim 里解(手动登录时代理并不知道用户登的是哪个号)。
func outlookEmailFromLogin(tokens map[string]any, provided string) string {
	if email := strings.TrimSpace(provided); email != "" {
		return email
	}
	claims := jwtClaims(asString(tokens["id_token"]))
	for _, key := range []string{"email", "preferred_username", "upn", "unique_name", "login_hint"} {
		value := strings.TrimSpace(asString(claims[key]))
		if strings.Contains(value, "@") {
			return value
		}
	}
	return ""
}

// jwtClaims 解 JWT 的 payload 段(纯本地 base64 解码,不验签,只用于读 email 这类声明)。
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

// ---- HTTP 处理函数 ----

func (p *proxyServer) handleAPIOutlookLoginStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(input.Email) > 320 || len(input.Password) > 512 {
		http.Error(w, "邮箱或密码过长", http.StatusBadRequest)
		return
	}
	status, err := p.outlookLogins.Start(input.Email, input.Password, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, status, nil)
}

// handleAPIOutlookAccountAutoLogin 行内「登录」按钮:用该行存的邮箱 + 明文密码自动登录。
// 没存密码就直接拒绝——自动流程没有密码可填,不如让用户先去「修改」里补上。
func (p *proxyServer) handleAPIOutlookAccountAutoLogin(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	email, password, err := p.store.GetOutlookCredentials(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(password) == "" {
		http.Error(w, "该账号没有保存密码，请先用「修改」填写密码", http.StatusBadRequest)
		return
	}
	status, err := p.outlookLogins.Start(email, password, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, status, nil)
}

func (p *proxyServer) handleAPIOutlookLoginStatus(w http.ResponseWriter, r *http.Request) {
	status, ok := p.outlookLogins.Status(chi.URLParam(r, "id"))
	if !ok {
		http.Error(w, "login not found", http.StatusNotFound)
		return
	}
	writeJSON(w, status, nil)
}

func (p *proxyServer) handleAPIOutlookLoginCancel(w http.ResponseWriter, r *http.Request) {
	if err := p.outlookLogins.Cancel(chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}
