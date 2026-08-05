package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// GPT 账号注册自动化(移植自 gpt-account-signup skill 的 chatgpt_signup.py)。
// 用隔离 Chrome 打开 chatgpt.com/auth/login,DOM 状态机:登录页 → 邮箱验证 → about-you → all-set → done。
// 撞人机验证/账号停用/未知状态就干净失败,不做模型兜底。任务结束删临时 profile。

const signupLoginURL = "https://chatgpt.com/auth/login"
const signupEmailSelector = "input[type=email],input[name=email],input[autocomplete=email],input[placeholder*=Email],input[placeholder*=邮箱],input[placeholder*=郵箱],input[placeholder*=邮件],input[placeholder*=郵件]"

var (
	signupAccountDeactivatedPat = regexp.MustCompile(`(?i)account_deactivated|account (?:has been )?(?:deleted|deactivated|disabled)|(?:账户|帳戶).*(?:已被)?(?:删除|刪除|停用|禁用)`)
	signupMaxSteps              = 45
)

type gptSignupStatus struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	State       string    `json:"state"` // running / completed / human_verification / failed / cancelled
	Message     string    `json:"message,omitempty"`
	Error       string    `json:"error,omitempty"`
	FinalURL    string    `json:"final_url,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func (s gptSignupStatus) done() bool {
	switch s.State {
	case "completed", "human_verification", "failed", "cancelled":
		return true
	}
	return false
}

type gptSignupProcess struct {
	status gptSignupStatus
	cancel context.CancelFunc
}

// gptSignupManager:一次只跑一个注册任务,重复发起返回 409(与 outlookLoginManager 同构)。
type gptSignupManager struct {
	srv *proxyServer
	mu  sync.Mutex
	wg  sync.WaitGroup
	all map[string]*gptSignupProcess
}

func newGPTSignupManager(srv *proxyServer) *gptSignupManager {
	return &gptSignupManager{srv: srv, all: make(map[string]*gptSignupProcess)}
}

func (m *gptSignupManager) running() bool {
	for _, p := range m.all {
		if !p.status.done() {
			return true
		}
	}
	return false
}

// hasRunning 是给别的管理器(login)做互斥检查用的加锁版本。
func (m *gptSignupManager) hasRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running()
}

func (m *gptSignupManager) Start(email, name, age string) (gptSignupStatus, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return gptSignupStatus{}, errors.New("缺少邮箱")
	}
	// 与 login 互斥(两者都要开浏览器):先查对方,再锁自己。
	if m.srv.gptLogins != nil && m.srv.gptLogins.hasRunning() {
		return gptSignupStatus{}, errStartConflict
	}
	m.mu.Lock()
	if m.running() {
		m.mu.Unlock()
		return gptSignupStatus{}, errStartConflict
	}
	id, err := randomLoginID()
	if err != nil {
		m.mu.Unlock()
		return gptSignupStatus{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = deriveSignupName(email)
	}
	if strings.TrimSpace(age) == "" {
		age = "30"
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), scopedProxyContextKey{}, true))
	proc := &gptSignupProcess{
		status: gptSignupStatus{ID: id, Email: email, State: "running", Message: "启动浏览器…", StartedAt: time.Now()},
		cancel: cancel,
	}
	m.all[id] = proc
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.run(ctx, id, email, name, age)
	}()
	return proc.status, nil
}

func (m *gptSignupManager) set(id, state, message, errMsg, finalURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc := m.all[id]
	if proc == nil {
		return
	}
	proc.status.State = state
	if message != "" {
		proc.status.Message = message
	}
	if errMsg != "" {
		proc.status.Error = errMsg
	}
	if finalURL != "" {
		proc.status.FinalURL = finalURL
	}
	if proc.status.done() && proc.status.CompletedAt.IsZero() {
		proc.status.CompletedAt = time.Now()
	}
}

func (m *gptSignupManager) note(id, message string) { m.set(id, "running", message, "", "") }

func (m *gptSignupManager) Status(id string) (gptSignupStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc := m.all[id]
	if proc == nil {
		return gptSignupStatus{}, false
	}
	return proc.status, true
}

func (m *gptSignupManager) Latest() (gptSignupStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *gptSignupProcess
	for _, p := range m.all {
		if latest == nil || p.status.StartedAt.After(latest.status.StartedAt) {
			latest = p
		}
	}
	if latest == nil {
		return gptSignupStatus{}, false
	}
	return latest.status, true
}

func (m *gptSignupManager) Cancel(id string) {
	m.mu.Lock()
	proc := m.all[id]
	m.mu.Unlock()
	if proc != nil {
		proc.cancel()
	}
}

func (m *gptSignupManager) Close() {
	m.mu.Lock()
	for _, p := range m.all {
		p.cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

// run 是注册状态机主体,忠实移植自 chatgpt_signup.py 的 main 循环。
func (m *gptSignupManager) run(ctx context.Context, id, email, name, age string) {
	driver, err := newGPTDriver(ctx, signupLoginURL)
	if err != nil {
		m.set(id, "failed", "", "启动浏览器失败: "+err.Error(), "")
		return
	}
	defer driver.Close() // 任务结束(含失败/取消)删临时 profile

	m.note(id, "已打开 ChatGPT 登录页…")
	sleepCtx(ctx, 3*time.Second)

	emailSubmitAt := time.Time{}
	for step := 1; step <= signupMaxSteps; step++ {
		if ctx.Err() != nil {
			m.set(id, "cancelled", "已取消", "", "")
			return
		}
		if !driver.Alive() {
			m.set(id, "failed", "", "Chrome 窗口已关闭", "")
			return
		}
		st, err := driver.state(ctx)
		if err != nil {
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		if gptIsHuman(st) {
			m.set(id, "human_verification", "遇到人机验证,请在 Chrome 窗口里完成", "遇到人机验证", st.URL)
			return
		}
		url, text := st.URL, st.Text
		if signupAccountDeactivatedPat.MatchString(text) {
			m.set(id, "failed", "", "OpenAI 报告账号已停用(account_deactivated)", url)
			return
		}
		if reason := gptErrorPageReason(st); reason != "" {
			m.set(id, "failed", "", reason, url)
			return
		}

		switch {
		case strings.Contains(url, "auth/login") || strings.Contains(text, "Log in or sign up"):
			m.note(id, "登录页加载中，等待页面稳定…")
			loginReady := driver.waitUntilStable(ctx, signupLoginEmailReady, 30*time.Second, 500*time.Millisecond, 3)
			if !signupLoginEmailReady(loginReady) {
				m.note(id, "登录页还没稳定，继续等待邮箱输入框…")
				sleepCtx(ctx, 1500*time.Millisecond)
				continue
			}
			m.note(id, "填写邮箱…")
			if err := driver.insertTrusted(ctx, signupEmailSelector, email); err != nil {
				m.note(id, "邮箱输入框暂不可用，继续等待…")
				sleepCtx(ctx, 1500*time.Millisecond)
				continue
			}
			// 慢加载/水合可能在刚输入后刷新表单并清空值；回读确认还在，再提交。
			sleepCtx(ctx, 900*time.Millisecond)
			filled, ferr := driver.state(ctx)
			if ferr != nil || !signupEmailFilled(filled, email) {
				m.note(id, "邮箱被页面刷新清空，等待稳定后重填…")
				sleepCtx(ctx, 1500*time.Millisecond)
				continue
			}
			emailSubmitAt = time.Now().UTC()
			driver.submitForm(ctx, signupEmailSelector)
			// 等 DOM 真跳到验证码页再取码。
			ready := driver.waitUntil(ctx, signupEmailVerifyReady, 45*time.Second, 750*time.Millisecond)
			if !signupEmailVerifyReady(ready) {
				m.set(id, "failed", "", "提交邮箱后没跳到验证码页", ready.URL)
				return
			}
			m.note(id, "等待邮箱验证码…")
			code, cerr := m.srv.fetchFreshGPTCode(ctx, email, emailSubmitAt, 12)
			if cerr != nil {
				m.set(id, "failed", "", cerr.Error(), ready.URL)
				return
			}
			m.note(id, "填入验证码…")
			driver.insertTrusted(ctx, "input[name=code],input[autocomplete=one-time-code]", code)
			driver.clickContinueOrEnter(ctx)
			// 等页面真正离开验证码页再继续,避免导航延迟里误入下面的「重试」分支去取不存在的新码。
			driver.waitUntil(ctx, func(s gptState) bool { return !strings.Contains(s.URL, "email-verification") }, 15*time.Second, 750*time.Millisecond)

		case strings.Contains(url, "email-verification") || strings.Contains(text, "Check your inbox"):
			if !signupEmailVerifyReady(st) {
				sleepCtx(ctx, time.Second)
				continue
			}
			// 到这说明是「验证码错了重试」,重开新鲜边界再取。
			requestedAt := time.Now().UTC()
			m.note(id, "重取邮箱验证码…")
			code, cerr := m.srv.fetchFreshGPTCode(ctx, email, requestedAt, 12)
			if cerr != nil {
				m.set(id, "failed", "", cerr.Error(), url)
				return
			}
			driver.insertTrusted(ctx, "input[name=code],input[autocomplete=one-time-code]", code)
			driver.clickContinueOrEnter(ctx)
			driver.waitUntil(ctx, func(s gptState) bool { return !strings.Contains(s.URL, "email-verification") }, 15*time.Second, 750*time.Millisecond)

		case strings.Contains(url, "about-you") || strings.Contains(text, "How old are you"):
			m.note(id, "填写姓名/年龄…")
			driver.insertTrusted(ctx, "input[name=name]", name)
			driver.insertTrusted(ctx, "input[name=age]", age)
			driver.submitForm(ctx, "input[name=age]")
			sleepCtx(ctx, 8*time.Second)

		case signupAllSet(text):
			// 到「你已准备就绪 / You're all set」说明账号已建好,这一步是最后的引导。
			// 点一下「继续」收尾(best-effort),然后直接判成功——不强求跳到聊天首页
			// (那页文案按语言变,检测脆)。
			m.note(id, "完成引导…")
			driver.clickExact(ctx, "继续", "Continue", "続行")
			if err := m.markRegistered(email); err != nil {
				m.set(id, "failed", "", "注册成功但写入 registered_gpt_account 失败: "+err.Error(), url)
				return
			}
			m.set(id, "completed", "注册成功(已到「你已准备就绪」)", "", url)
			return

		case strings.HasPrefix(url, "https://chatgpt.com/") && signupDoneText(text):
			if err := m.markRegistered(email); err != nil {
				m.set(id, "failed", "", "注册成功但写入 registered_gpt_account 失败: "+err.Error(), url)
				return
			}
			m.set(id, "completed", "注册成功", "", url)
			return
		default:
			sleepCtx(ctx, 2*time.Second)
		}
	}
	m.set(id, "failed", "", "状态机超过最大步数仍未完成", "")
}

func (m *gptSignupManager) markRegistered(email string) error {
	if m == nil || m.srv == nil || m.srv.store == nil {
		return nil
	}
	return m.srv.store.MarkOutlookRegisteredGPTByEmail(context.Background(), email)
}

func signupLoginEmailReady(st gptState) bool {
	if st.Ready != "complete" {
		return false
	}
	if !(strings.Contains(st.URL, "auth/login") || strings.Contains(st.Text, "Log in or sign up") || strings.Contains(st.Text, "登录或注册") || strings.Contains(st.Text, "登入或註冊")) {
		return false
	}
	for _, in := range st.Inputs {
		if in.Disabled {
			continue
		}
		if signupLooksEmailInput(in) {
			return true
		}
	}
	return false
}

func signupLooksEmailInput(in gptInput) bool {
	hay := strings.ToLower(strings.TrimSpace(in.Type + " " + in.Name + " " + in.ID + " " + in.Placeholder + " " + in.Autocomplete))
	return strings.Contains(hay, "email") || strings.Contains(hay, "邮箱") || strings.Contains(hay, "郵箱") || strings.Contains(hay, "邮件") || strings.Contains(hay, "郵件")
}

func signupEmailFilled(st gptState, email string) bool {
	email = strings.TrimSpace(email)
	for _, in := range st.Inputs {
		if signupLooksEmailInput(in) {
			return strings.TrimSpace(in.Value) == email
		}
	}
	return false
}

// signupEmailVerifyReady 要求确实到了验证码输入页(不是提交后的短暂延迟)。
func signupEmailVerifyReady(st gptState) bool {
	if !(strings.Contains(st.URL, "email-verification") || strings.Contains(st.Text, "Check your inbox")) {
		return false
	}
	return st.HasCodeInput
}

// signupAllSet 判「你已准备就绪 / You're all set」引导终页(多语言)。
func signupAllSet(text string) bool {
	for _, s := range []string{"You're all set", "你已准备就绪", "你已準備就緒", "準備ができました"} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func signupDoneText(text string) bool {
	for _, s := range []string{
		"What’s on your mind", "Welcome,", "New chat", "何でも聞いて", "新しいチャット",
		"有什么可以帮忙", "有什麼可以幫忙", "新聊天", "新对话",
	} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// deriveSignupName 从邮箱本地部分猜一个可读姓名(移植 derive_name)。
func deriveSignupName(email string) string {
	local := strings.SplitN(email, "@", 2)[0]
	local = regexp.MustCompile(`\d+[a-z0-9]*$`).ReplaceAllString(local, "")
	local = strings.NewReplacer(".", " ", "_", " ").Replace(local)
	parts := regexp.MustCompile(`[a-zA-Z]+`).FindAllString(local, -1)
	if len(parts) >= 2 {
		return strings.Title(parts[0]) + " " + strings.Title(parts[1])
	}
	if len(parts) == 1 && len(parts[0]) > 5 {
		return strings.Title(parts[0])
	}
	return "ChatGPT User"
}

// ---- HTTP handlers ----

func (m *gptSignupManager) handleStart(w http.ResponseWriter, r *http.Request) {
	id, err := parseAccountID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	acc, err := m.srv.store.GetOutlookAccountByID(r.Context(), id)
	if err != nil {
		http.Error(w, "账号不存在", http.StatusNotFound)
		return
	}
	st, err := m.Start(acc.Email, "", "")
	if err != nil {
		if errors.Is(err, errStartConflict) {
			http.Error(w, "已有注册/登录任务在进行中", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, st, nil)
}

func (m *gptSignupManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	st, ok := m.Status(id)
	if !ok {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, st, nil)
}

func (m *gptSignupManager) handleCancel(w http.ResponseWriter, r *http.Request) {
	m.Cancel(chi.URLParam(r, "id"))
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// errStartConflict 是 signup/login 管理器共用的「已有任务在跑」哨兵错误。
var errStartConflict = errors.New("已有任务在进行中")

// parseAccountID 从 URL 取 {id}。
func parseAccountID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}
