package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 极简 Chrome DevTools Protocol 客户端,走 `--remote-debugging-pipe`:
// Chrome 从 fd3 读命令、往 fd4 写应答,消息是 NUL 分隔的 JSON。
//
// 选管道而不是 `--remote-debugging-port` 的原因:端口模式的命令通道是 websocket,
// 纯 Go 实现要么引依赖要么手写 RFC6455;管道模式只是两个 os.Pipe,几十行就够。
//
// 这里只用**浏览器级**方法(Browser.* / Target.* / Storage.*),不 attach 页面 session,
// 手动登录场景需要的「看当前页面 URL」和「取全部 cookie」都在这个范围内。

type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    any             `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("CDP 错误 %d: %s", e.Code, e.Message) }

// chromeSession 是一个由本进程拉起、带独立 user-data-dir 的 Chrome 实例。
type chromeSession struct {
	cmd     *exec.Cmd
	in      *os.File // 本进程写 → Chrome 的 fd3
	out     *os.File // Chrome 的 fd4 → 本进程读
	profile string

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpMessage
	closed  bool
	done    chan struct{} // 读循环结束(Chrome 退出或管道断开)时关闭
}

// chromeTarget 是 Target.getTargets 返回的一个目标(标签页/worker 等)。
type chromeTarget struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// chromeCookie 对应 Storage.getCookies 返回的一条 cookie。
type chromeCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // unix 秒;会话 cookie 是 -1
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

// chromeExecutable 定位 Chrome 可执行文件,可用 CHROME_PATH 覆盖。
func chromeExecutable() string {
	if value := strings.TrimSpace(os.Getenv("CHROME_PATH")); value != "" {
		return value
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	if runtime.GOOS != "darwin" {
		candidates = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	}
	for _, candidate := range candidates {
		if strings.ContainsRune(candidate, os.PathSeparator) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

// launchChrome 用独立的临时 profile 启动一个可见的 Chrome 窗口并接上 CDP 管道(OUTLOOK 登录用)。
func launchChrome(startURL string) (*chromeSession, error) {
	return launchChromeWith(startURL, "agent-go-proxy-outlook-login-", []string{
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--window-size=1120,860",
	})
}

// launchChromeWith 是 launchChrome 的可定制版本:调用方给中间那段启动参数,pipe/profile/startURL 仍由本函数管。
// profile 目录 Close 时删除。GPT 注册/登录流程用它 + 反检测参数(见 gpt_automation.go)。
func launchChromeWith(startURL, profilePrefix string, flags []string) (*chromeSession, error) {
	binary := chromeExecutable()
	if binary == "" {
		return nil, errors.New("没找到 Chrome,可用环境变量 CHROME_PATH 指定可执行文件路径")
	}
	profile, err := os.MkdirTemp("", profilePrefix)
	if err != nil {
		return nil, err
	}
	// fd3:Chrome 读端;fd4:Chrome 写端。ExtraFiles[0] 就是子进程的 fd3。
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(profile)
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		inRead.Close()
		inWrite.Close()
		_ = os.RemoveAll(profile)
		return nil, err
	}

	args := append([]string{"--remote-debugging-pipe", "--user-data-dir=" + profile}, flags...)
	args = append(args, startURL)
	cmd := exec.Command(binary, args...)
	cmd.Env = withoutProxyEnv(os.Environ())
	cmd.ExtraFiles = []*os.File{inRead, outWrite}
	if err := cmd.Start(); err != nil {
		inRead.Close()
		inWrite.Close()
		outRead.Close()
		outWrite.Close()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	// 子进程已经继承了这两端,父进程留着会让读端永远等不到 EOF。
	inRead.Close()
	outWrite.Close()

	session := &chromeSession{
		cmd:     cmd,
		in:      inWrite,
		out:     outRead,
		profile: profile,
		pending: make(map[int]chan cdpMessage),
		done:    make(chan struct{}),
	}
	go session.readLoop()
	return session, nil
}

func (s *chromeSession) readLoop() {
	defer close(s.done)
	reader := bufio.NewReaderSize(s.out, 1<<20)
	for {
		raw, err := reader.ReadBytes(0)
		if err != nil {
			return
		}
		if len(raw) <= 1 {
			continue
		}
		var m cdpMessage
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil || m.ID == 0 {
			continue // 解析不了的、或没有 id 的(事件)一律忽略
		}
		s.mu.Lock()
		ch := s.pending[m.ID]
		delete(s.pending, m.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// call 发一条浏览器级 CDP 命令并等应答;out 非 nil 时把 result 反序列化进去。
func (s *chromeSession) call(ctx context.Context, method string, params any, out any) error {
	return s.callSession(ctx, "", method, params, out)
}

// callSession 与 call 相同,但把命令投递到指定页面会话(sessionID 非空时)。
// 应答仍按全局自增的 id 路由,所以这里不需要按 sessionId 分发。
func (s *chromeSession) callSession(ctx context.Context, sessionID, method string, params any, out any) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Chrome 会话已关闭")
	}
	s.nextID++
	id := s.nextID
	ch := make(chan cdpMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	raw, err := json.Marshal(cdpMessage{ID: id, SessionID: sessionID, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := s.in.Write(append(raw, 0)); err != nil {
		return fmt.Errorf("写 CDP 管道失败: %w", err)
	}

	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-s.done:
		return errors.New("Chrome 已退出")
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return ctx.Err()
	}
}

// Alive 判断 Chrome 是否还活着(用户手动关窗口时读循环会结束)。
func (s *chromeSession) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *chromeSession) Targets(ctx context.Context) ([]chromeTarget, error) {
	var result struct {
		TargetInfos []chromeTarget `json:"targetInfos"`
	}
	if err := s.call(ctx, "Target.getTargets", nil, &result); err != nil {
		return nil, err
	}
	return result.TargetInfos, nil
}

// PageURLs 只返回标签页类型目标的 URL,用于判断用户登录到哪一步了。
func (s *chromeSession) PageURLs(ctx context.Context) ([]string, error) {
	targets, err := s.Targets(ctx)
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.Type == "page" {
			urls = append(urls, target.URL)
		}
	}
	return urls, nil
}

func (s *chromeSession) OpenTab(ctx context.Context, url string) error {
	return s.call(ctx, "Target.createTarget", map[string]any{"url": url}, nil)
}

// Cookies 取整个浏览器上下文的 cookie。走浏览器级的 Storage.getCookies,
// 不需要 attach 页面,拿到的是全域 cookie(含 HttpOnly 的登录会话 cookie)。
func (s *chromeSession) Cookies(ctx context.Context) ([]chromeCookie, error) {
	var result struct {
		Cookies []chromeCookie `json:"cookies"`
	}
	if err := s.call(ctx, "Storage.getCookies", nil, &result); err != nil {
		return nil, err
	}
	return result.Cookies, nil
}

// ActivePageTarget 挑一个标签页作为操作对象:优先微软登录/邮箱相关的页面,否则取第一个。
func (s *chromeSession) ActivePageTarget(ctx context.Context) (chromeTarget, bool, error) {
	targets, err := s.Targets(ctx)
	if err != nil {
		return chromeTarget{}, false, err
	}
	var first *chromeTarget
	for i := range targets {
		if targets[i].Type != "page" {
			continue
		}
		url := strings.ToLower(targets[i].URL)
		if strings.Contains(url, "login.live.com") || strings.Contains(url, "login.microsoftonline.com") ||
			strings.Contains(url, "outlook.live.com") || strings.Contains(url, "account.microsoft.com") {
			return targets[i], true, nil
		}
		if first == nil {
			first = &targets[i]
		}
	}
	if first != nil {
		return *first, true, nil
	}
	return chromeTarget{}, false, nil
}

// cdpPage 是附着在某个标签页上的会话。页面级的 Runtime / Input / Page 命令都要带 sessionId。
type cdpPage struct {
	session   *chromeSession
	sessionID string
	targetID  string
}

// AttachPage 附着到指定标签页。flatten=true 让应答走同一条管道并带 sessionId,
// 不需要额外的消息通道。
func (s *chromeSession) AttachPage(ctx context.Context, targetID string) (*cdpPage, error) {
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &result); err != nil {
		return nil, err
	}
	if result.SessionID == "" {
		return nil, errors.New("attachToTarget 没有返回 sessionId")
	}
	page := &cdpPage{session: s, sessionID: result.SessionID, targetID: targetID}
	// Page/Runtime 域显式 enable 后 bringToFront 等命令才可用;失败不致命。
	_ = page.call(ctx, "Page.enable", nil, nil)
	_ = page.call(ctx, "Runtime.enable", nil, nil)
	return page, nil
}

func (p *cdpPage) call(ctx context.Context, method string, params any, out any) error {
	return p.session.callSession(ctx, p.sessionID, method, params, out)
}

// Evaluate 在页面里执行 JS,按值取回结果(out 为 nil 时忽略返回值)。
func (p *cdpPage) Evaluate(ctx context.Context, expression string, out any) error {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":      expression,
		"awaitPromise":    true,
		"returnByValue":   true,
		"timeout":         10000,
		"userGesture":     true,
		"replMode":        false,
		"generatePreview": false,
	}, &result); err != nil {
		return err
	}
	if result.ExceptionDetails != nil {
		return fmt.Errorf("页面脚本异常: %s", result.ExceptionDetails.Text)
	}
	if out != nil && len(result.Result.Value) > 0 {
		return json.Unmarshal(result.Result.Value, out)
	}
	return nil
}

// InsertText 模拟输入一段文本(按字符逐次调用才像真人打字)。
func (p *cdpPage) InsertText(ctx context.Context, text string) error {
	return p.call(ctx, "Input.insertText", map[string]any{"text": text}, nil)
}

// Key 发一个键盘事件。kind 取 keyDown / keyUp。
func (p *cdpPage) Key(ctx context.Context, kind, key, code string, modifiers int) error {
	params := map[string]any{"type": kind, "key": key, "code": code}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	return p.call(ctx, "Input.dispatchKeyEvent", params, nil)
}

// Mouse 发一个鼠标事件。kind 取 mouseMoved / mousePressed / mouseReleased。
func (p *cdpPage) Mouse(ctx context.Context, kind string, x, y float64, button string, clickCount int) error {
	params := map[string]any{"type": kind, "x": x, "y": y}
	if button != "" {
		params["button"] = button
		params["clickCount"] = clickCount
	}
	return p.call(ctx, "Input.dispatchMouseEvent", params, nil)
}

func (p *cdpPage) BringToFront(ctx context.Context) error {
	return p.call(ctx, "Page.bringToFront", nil, nil)
}

func (s *chromeSession) UserAgent(ctx context.Context) (string, error) {
	var result struct {
		UserAgent string `json:"userAgent"`
	}
	if err := s.call(ctx, "Browser.getVersion", nil, &result); err != nil {
		return "", err
	}
	return result.UserAgent, nil
}

// Close 关掉 Chrome 并清理临时 profile。先礼后兵:Browser.close 不成再 Kill。
func (s *chromeSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	if s.Alive() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.call(ctx, "Browser.close", nil, nil)
		cancel()
	}
	exited := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-exited
	}
	s.in.Close()
	s.out.Close()
	if s.profile != "" {
		_ = os.RemoveAll(s.profile)
	}
}
