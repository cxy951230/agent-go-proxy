package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// Outlook 自动登录:用 CDP 驱动我们自己拉起的那个 Chrome 窗口,按 DOM 状态机一步步填表。
//
// 状态判定与交互策略移植自 outlook-login-automation skill 的 Python 实现——那些规则
// (判定顺序、三级点击降级、不重复输入)都是踩坑踩出来的,这里逐条对齐,不自创。
// 没有移植的是它的指纹伪装(独立 583 行)与诊断产物(log.jsonl / DOM 快照 / 截图):
// 前者按需求不做,后者不落任何文件,出问题只把原因回报给页面。
//
// 因为少了指纹伪装,更容易撞上人机验证/账号保护。所以这里不像 Python 版那样直接退出,
// 而是把这类状态回报成「需要人工接管」,窗口留着,由调用方回落到手动登录的轮询。

// outlookHelperJS 注入页面的取数助手。相对 skill 的版本裁掉了只服务于诊断的
// links / iframes / candidates 三个字段(candidates 一次要序列化 350 个节点),
// 其余与原版逐字一致——状态机只用到 href/title/ready/headings/inputs/buttons/bodyText。
const outlookHelperJS = `
(() => {
  window.__outlookLoginBot = {
    vis: e => !!e && e.offsetParent !== null && !e.disabled && getComputedStyle(e).visibility !== 'hidden' && getComputedStyle(e).display !== 'none' && e.getBoundingClientRect().width > 0 && e.getBoundingClientRect().height > 0,
    meta: (e, i) => { const r = e.getBoundingClientRect(); return {
      idx:i, tag:e.tagName.toLowerCase(), id:e.id||'', name:e.getAttribute('name')||'', type:e.getAttribute('type')||'', role:e.getAttribute('role')||'', aria:e.getAttribute('aria-label')||'', placeholder:e.getAttribute('placeholder')||'',
      value:e.value||'', text:(e.innerText||e.value||e.getAttribute('aria-label')||e.getAttribute('placeholder')||'').trim().slice(0,500),
      x:r.left+r.width/2, y:r.top+r.height/2, visible: window.__outlookLoginBot.vis(e)
    }; },
    snapshot: () => ({
      href: location.href, title: document.title, ready: document.readyState,
      headings: [...document.querySelectorAll('h1,h2,[role=heading]')].filter(window.__outlookLoginBot.vis).map(window.__outlookLoginBot.meta),
      inputs: [...document.querySelectorAll('input,textarea')].filter(window.__outlookLoginBot.vis).map(window.__outlookLoginBot.meta),
      buttons: [...document.querySelectorAll('button,input[type=submit],input[type=button],[role=button]')].filter(window.__outlookLoginBot.vis).map(window.__outlookLoginBot.meta),
      errors: [...document.querySelectorAll('[role=alert],[aria-live=assertive],[id$="Error"],.alert-error')].filter(window.__outlookLoginBot.vis).map(window.__outlookLoginBot.meta),
      bodyText: (document.body ? document.body.innerText : '').slice(0,2500)
    }),
    input: pats => { for (const e of [...document.querySelectorAll('input,textarea')].filter(window.__outlookLoginBot.vis)) { const s=[e.id,e.name,e.type,e.placeholder,e.getAttribute('aria-label'),e.autocomplete].join(' ').toLowerCase(); if (pats.some(p => s.includes(p))) return window.__outlookLoginBot.meta(e,0); } return null; },
    button: pats => { for (const e of [...document.querySelectorAll('button,input[type=submit],input[type=button],[role=button]')].filter(window.__outlookLoginBot.vis)) { const s=(e.innerText||e.value||e.getAttribute('aria-label')||'').toLowerCase(); if (pats.some(p => s.includes(p))) return window.__outlookLoginBot.meta(e,0); } return null; },
    buttonExact: labels => { const lower = labels.map(x => x.toLowerCase()); for (const e of [...document.querySelectorAll('button,input[type=submit],input[type=button],[role=button]')].filter(window.__outlookLoginBot.vis)) { const s=(e.innerText||e.value||e.getAttribute('aria-label')||'').trim().toLowerCase(); if (lower.includes(s)) return window.__outlookLoginBot.meta(e,0); } return null; }
  };
  return true;
})();
`

// outlookActiveElementJS 读当前焦点元素,用于 Tab 键盘激活时判断有没有走到目标按钮。
const outlookActiveElementJS = `(() => { const e = document.activeElement; const r = e ? e.getBoundingClientRect() : {left:0,top:0,width:0,height:0}; return {tag:e && e.tagName, id:(e && e.id) || '', text:((e && (e.innerText || e.value || e.getAttribute('aria-label'))) || '').trim(), testid:(e && e.getAttribute('data-testid')) || '', x:r.left+r.width/2, y:r.top+r.height/2}; })()`

type domMeta struct {
	Tag         string  `json:"tag"`
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Role        string  `json:"role"`
	Aria        string  `json:"aria"`
	Placeholder string  `json:"placeholder"`
	Value       string  `json:"value"`
	Text        string  `json:"text"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Visible     bool    `json:"visible"`
}

type domSnapshot struct {
	Href     string    `json:"href"`
	Title    string    `json:"title"`
	Ready    string    `json:"ready"`
	Headings []domMeta `json:"headings"`
	Inputs   []domMeta `json:"inputs"`
	Buttons  []domMeta `json:"buttons"`
	// Errors 是页面上可见的报错块(role=alert / *Error 等)。bodyText 会被截断到 2500 字,
	// 单拎出来既能保证报错文案一定参与判定,也能原样回报给页面。
	Errors   []domMeta `json:"errors"`
	BodyText string    `json:"bodyText"`
}

type activeElement struct {
	Tag    string `json:"tag"`
	ID     string `json:"id"`
	Text   string `json:"text"`
	TestID string `json:"testid"`
}

// outlookCaptchaPatterns 必须用显式的挑战短语,不能用宽泛的 "prove you" 子串——
// cookie 横幅那句 "improve your experience" 里就含它,会把正常页面误判成验证码。
var outlookCaptchaPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bpress\s+and\s+hold\b`),
	regexp.MustCompile(`\bhuman\s+verification\b`),
	regexp.MustCompile(`\bprove\s+you(?:\s+are|['’]re)\s+(?:human|not\s+a\s+robot)\b`),
	regexp.MustCompile(`\bverify\s+you(?:\s+are|['’]re)\s+human\b`),
	regexp.MustCompile(`\bcaptcha\b`),
}

// outlookProblemPhrases 是「继续下去没有意义」的页面提示,命中即判 problem(直接失败)。
//
// 「找不到这个 Microsoft 账号」这一组是后补的:这类页面报错后**邮箱输入框还在**,
// 不认它就会被判回 email 状态,状态机于是一直重填重点,每步都要等满转场超时,
// 直到整个登录任务 10 分钟超时才结束——表现就是页面上卡住不动、也不报错。
var outlookProblemPhrases = []string{
	"incorrect",
	"doesn't exist",
	"does not exist",
	"couldn't find a microsoft account",
	"couldn't find an account",
	"no account found",
	"enter a valid email address",
	"temporarily blocked",
	"account has been locked",
	"too many times",
	"something went wrong",
	"password sign-in isn't available",
	"password sign in isn't available",
	"try another method",
	"we ran into a problem",
	"err_connection_closed",
	"err_connection_reset",
	"err_timed_out",
	"this site can't be reached",
	"this site can’t be reached",
	"unexpectedly closed the connection",
	"无法访问此网站",
	"意外终止了连接",
}

// classifyOutlookLoginState 把一次 DOM 快照归类成登录状态。**判定顺序不能改**:
//   - password_choice 必须排在 captcha 前面(「Verify your email」页同时含两类关键词);
//   - account.microsoft.com 只有在没有邮箱/密码输入框时才算已登录,否则可能只是落地页。
func classifyOutlookLoginState(s domSnapshot) string {
	body := strings.ToLower(s.BodyText)
	parts := make([]string, 0, len(s.Headings))
	for _, h := range s.Headings {
		parts = append(parts, h.Text)
	}
	headings := strings.ToLower(strings.Join(parts, " "))
	href := strings.ToLower(s.Href)
	all := strings.Join([]string{body, headings, href}, " ")

	// 空文档一律算没加载完,**不能看 readyState**:微软登录页跳转过程中会先给出一个
	// readyState 已经是 complete、但 DOM 里什么都没有的中间文档。skill 的判据带了
	// `ready != complete`,靠开工前固定睡 6 秒盖过去;这里改成按 DOM 判定就不能再这么写,
	// 否则空文档会一路落到 unknown、刚开始就误判成「未识别页面」。
	if len(s.Inputs) == 0 && len(s.Buttons) == 0 {
		return "loading"
	}
	if containsAnyString(href, "outlook.live.com/mail", "outlook.office.com/mail") ||
		containsAnyString(all, "inbox", "focused", "new mail", "message list") {
		return "success"
	}
	if strings.Contains(href, "account.microsoft.com") && !hasInputOfType(s.Inputs, "password", "email") {
		return "success"
	}
	if containsAnyString(all, "stay signed in", "stay signed-in", "reduce the number of times you are asked to sign in") {
		return "stay_signed_in"
	}
	if strings.Contains(all, "use your password") {
		return "password_choice"
	}
	for _, pattern := range outlookCaptchaPatterns {
		if pattern.MatchString(all) {
			return "captcha"
		}
	}
	if containsAnyString(all, "help us protect", "protect your account", "verify your identity",
		"enter code", "security code", "two-step", "authenticator", "approve sign in request") {
		return "mfa_or_protection"
	}
	// 报错块单独并进来:bodyText 截断到 2500 字,长页面可能把报错切掉。
	if containsAnyString(all+" "+strings.ToLower(outlookSnapshotError(s)), outlookProblemPhrases...) {
		return "problem"
	}
	if hasPasswordInput(s.Inputs) {
		return "password"
	}
	if hasEmailInput(s.Inputs) {
		return "email"
	}
	if containsAnyString(all, "pick an account", "choose an account", "use another account") {
		return "account_picker"
	}
	return "unknown"
}

func containsAnyString(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func hasInputOfType(inputs []domMeta, types ...string) bool {
	for _, input := range inputs {
		for _, want := range types {
			if input.Type == want {
				return true
			}
		}
	}
	return false
}

func hasPasswordInput(inputs []domMeta) bool {
	for _, input := range inputs {
		if input.Type == "password" || input.Name == "passwd" ||
			strings.Contains(strings.ToLower(input.ID+input.Aria+input.Placeholder), "password") {
			return true
		}
	}
	return false
}

func hasEmailInput(inputs []domMeta) bool {
	for _, input := range inputs {
		if input.Type != "email" && input.Type != "text" {
			continue
		}
		hay := strings.ToLower(input.ID + input.Name + input.Aria + input.Placeholder)
		if containsAnyString(hay, "loginfmt", "email", "phone", "skype", "username") {
			return true
		}
	}
	return false
}

// 各步骤的节奏参数。转场超时与步数上限沿用 skill 的默认值;
// 「开工前的等待」没有沿用它的固定 6 秒,改成按 DOM 就绪度判定,见 waitReady。
const (
	outlookAutoTransitionTimeout = 35 * time.Second
	outlookAutoMaxSteps          = 25
	outlookAutoMaxTabs           = 12
	// waitReady 的轮询间隔与上限:页面就绪通常 1 秒内,给足 20 秒兜底冷启动/弱网。
	outlookAutoReadyPoll    = 250 * time.Millisecond
	outlookAutoReadyTimeout = 20 * time.Second
)

// outlookAutoMaxSameState 是「同一个状态连续出现多少次算推不动」。取 2 表示连续三次
// 判定都一样就收工:正常流程每一步都会换页,同一状态反复出现只可能是页面没接受我们的操作
// (通常是账号/密码有问题,页面把报错显示在原地)。
const outlookAutoMaxSameState = 2

// outlookStateLabels 给状态起个中文名,只用于回报给页面的文案。
var outlookStateLabels = map[string]string{
	"loading":         "页面加载",
	"email":           "填写邮箱",
	"password":        "填写密码",
	"password_choice": "选择验证方式",
	"stay_signed_in":  "保持登录状态",
	"account_picker":  "选择账号",
	"unknown":         "未识别页面",
}

// outlookAutoLogin 驱动一次自动登录。report 用来把进度回报给页面(不落任何文件)。
type outlookAutoLogin struct {
	// session 用于页面 target 失效后重新附着,见 snapshot。
	session  *chromeSession
	page     *cdpPage
	email    string
	password string
	report   func(message string)
}

// outlookSnapshotError 把页面上可见的报错块拼成一句话,用于判定失败原因和回报给页面。
func outlookSnapshotError(s domSnapshot) string {
	seen := make(map[string]bool, len(s.Errors))
	parts := make([]string, 0, len(s.Errors))
	for _, item := range s.Errors {
		text := strings.Join(strings.Fields(item.Text), " ")
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		parts = append(parts, text)
	}
	return limitString(strings.Join(parts, " / "), 300)
}

// outlookAutoResult 是自动流程的结局。
type outlookAutoResult struct {
	State string // 最后一次识别到的状态
	// NeedsHuman 为真表示流程停在需要人工处理的页面(验证码/两步验证/未识别),
	// 窗口应保留,由调用方回落到手动等待。
	NeedsHuman bool
	// Fatal 为真表示继续下去没有意义(账号被锁、密码错误等)。
	Fatal   bool
	Message string
}

func (a *outlookAutoLogin) note(format string, args ...any) {
	if a.report != nil {
		a.report(fmt.Sprintf(format, args...))
	}
}

// snapshot 拍一次 DOM 快照。附着的页面 target 没了(微软登录过程中会换 target,用户也可能
// 关掉标签页)时 CDP 回 "Session with given id not found.",这里重新附着到当前活动页再拍一次。
// 不这么做的话每次调用都拿同一个错误,状态机会一路空转到任务超时——线上就是这么卡住的。
func (a *outlookAutoLogin) snapshot(ctx context.Context) (domSnapshot, error) {
	snap, err := a.snapshotOnce(ctx)
	if err == nil {
		return snap, nil
	}
	if a.session == nil || ctx.Err() != nil || !a.reattach(ctx) {
		return domSnapshot{}, err
	}
	return a.snapshotOnce(ctx)
}

func (a *outlookAutoLogin) snapshotOnce(ctx context.Context) (domSnapshot, error) {
	if err := a.page.Evaluate(ctx, outlookHelperJS, nil); err != nil {
		return domSnapshot{}, err
	}
	var snap domSnapshot
	if err := a.page.Evaluate(ctx, "window.__outlookLoginBot.snapshot()", &snap); err != nil {
		return domSnapshot{}, err
	}
	return snap, nil
}

// reattach 重新附着到当前活动标签页。Chrome 已经退出时直接失败,由调用方判死。
func (a *outlookAutoLogin) reattach(ctx context.Context) bool {
	target, ok, err := a.session.ActivePageTarget(ctx)
	if err != nil || !ok {
		return false
	}
	page, err := a.session.AttachPage(ctx, target.TargetID)
	if err != nil {
		return false
	}
	a.page = page
	return true
}

func (a *outlookAutoLogin) findInput(ctx context.Context, patterns []string) *domMeta {
	return a.findMeta(ctx, "input", patterns)
}

func (a *outlookAutoLogin) findButton(ctx context.Context, patterns []string) *domMeta {
	return a.findMeta(ctx, "button", patterns)
}

func (a *outlookAutoLogin) findButtonExact(ctx context.Context, labels []string) *domMeta {
	return a.findMeta(ctx, "buttonExact", labels)
}

func (a *outlookAutoLogin) findMeta(ctx context.Context, fn string, args []string) *domMeta {
	if err := a.page.Evaluate(ctx, outlookHelperJS, nil); err != nil {
		return nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	var meta domMeta
	if err := a.page.Evaluate(ctx, "window.__outlookLoginBot."+fn+"("+string(encoded)+")", &meta); err != nil {
		return nil
	}
	if meta.Tag == "" {
		return nil
	}
	return &meta
}

// click 用鼠标事件点一个元素。点完的等待时长分两种:点按钮要留时间给页面反应,
// 只是把光标落进输入框则不需要,后者用 clickToFocus。
func (a *outlookAutoLogin) click(ctx context.Context, meta domMeta) error {
	return a.clickWithWait(ctx, meta, 700, 1300)
}

// clickToFocus 仅用于聚焦输入框:点完几乎立刻就能开始打字,不必等页面转场。
func (a *outlookAutoLogin) clickToFocus(ctx context.Context, meta domMeta) error {
	return a.clickWithWait(ctx, meta, 120, 260)
}

func (a *outlookAutoLogin) clickWithWait(ctx context.Context, meta domMeta, postMinMS, postMaxMS int) error {
	if err := a.page.Mouse(ctx, "mouseMoved", meta.X, meta.Y, "", 0); err != nil {
		return err
	}
	sleepJitter(ctx, 100, 250)
	if err := a.page.Mouse(ctx, "mousePressed", meta.X, meta.Y, "left", 1); err != nil {
		return err
	}
	sleepJitter(ctx, 60, 160)
	if err := a.page.Mouse(ctx, "mouseReleased", meta.X, meta.Y, "left", 1); err != nil {
		return err
	}
	sleepJitter(ctx, postMinMS, postMaxMS)
	return nil
}

// waitReady 用 DOM 就绪度代替固定等待:每 250ms 拍一次快照,直到 readyState=complete
// 且状态不是 loading,并且**连续两次判定一致**才算稳定。
//
// 只看单次快照不够:微软登录页是前端框架渲染的,输入框可能已经在 DOM 里、事件还没绑上,
// 这时候打字会打进空气。连续两次一致相当于「页面 250ms 内没再变」,够用又比睡 6 秒快得多。
func (a *outlookAutoLogin) waitReady(ctx context.Context, timeout time.Duration) (domSnapshot, string) {
	deadline := time.Now().Add(timeout)
	var last domSnapshot
	previous := ""
	for {
		snap, err := a.snapshot(ctx)
		if err == nil {
			last = snap
			state := classifyOutlookLoginState(snap)
			if state != "loading" && snap.Ready == "complete" {
				if state == previous {
					return snap, state
				}
				previous = state
			} else {
				previous = ""
			}
		}
		if time.Now().After(deadline) {
			return last, ""
		}
		if !sleepCtx(ctx, outlookAutoReadyPoll) {
			return last, ""
		}
	}
}

// typeText 逐字符输入,像真人打字。整串一次性 insertText 会被部分前端识别成粘贴。
func (a *outlookAutoLogin) typeText(ctx context.Context, text string) error {
	for _, ch := range text {
		if err := a.page.InsertText(ctx, string(ch)); err != nil {
			return err
		}
		sleepJitter(ctx, 35, 110)
	}
	return nil
}

// focusAndTypeIfNeeded 填一个输入框。已经是目标值就不重打(skill 的硬规则),
// 否则点击聚焦 → Cmd+A 全选 → 逐字符输入 → **回读核对**。
//
// 回读这步是我们比 skill 多做的:Input.insertText 要求元素已经拿到焦点,一旦点击没落到
// 输入框上(坐标失效/控件拦截),字符就打进了空气。不核对的话会拿一个空密码去撞登录,
// 微软照样回「密码不正确」,既误导人又白白消耗该账号的失败次数。
func (a *outlookAutoLogin) focusAndTypeIfNeeded(ctx context.Context, patterns []string, text string) bool {
	meta := a.findInput(ctx, patterns)
	if meta == nil {
		return false
	}
	if meta.Value == text {
		return true
	}
	if err := a.clickToFocus(ctx, *meta); err != nil {
		return false
	}
	_ = a.page.Key(ctx, "keyDown", "Meta", "MetaLeft", 4)
	_ = a.page.Key(ctx, "keyDown", "a", "KeyA", 4)
	_ = a.page.Key(ctx, "keyUp", "a", "KeyA", 4)
	_ = a.page.Key(ctx, "keyUp", "Meta", "MetaLeft", 0)
	sleepJitter(ctx, 100, 100)
	if err := a.typeText(ctx, text); err != nil {
		return false
	}
	sleepJitter(ctx, 500, 1000)
	// 只在「输入框仍然是空的」时判失败:这是确凿的没填进去。长度对不上可能是
	// 输入法/控件格式化导致的,不足以否定,放过去让微软自己判。
	if after := a.findInput(ctx, patterns); after != nil && after.Value == "" && text != "" {
		return false
	}
	return true
}

// keyboardActivate 用 Tab 走焦点直到命中目标按钮再回车。微软的 Fluent 控件
// 有一部分吃不到 CDP 的鼠标事件,只能靠键盘激活。
func (a *outlookAutoLogin) keyboardActivate(ctx context.Context, labels []string) bool {
	want := make([]string, 0, len(labels))
	for _, label := range labels {
		want = append(want, strings.ToLower(strings.TrimSpace(label)))
	}
	_ = a.page.BringToFront(ctx)
	for i := 0; i <= outlookAutoMaxTabs; i++ {
		var active activeElement
		if err := a.page.Evaluate(ctx, outlookActiveElementJS, &active); err == nil {
			text := strings.ToLower(strings.TrimSpace(active.Text))
			if matchesAnyLabel(text, want) {
				// data-testid 含 primarybutton 的控件响应空格而不是回车。
				key, code := "Enter", "Enter"
				if strings.Contains(strings.ToLower(active.TestID), "primarybutton") {
					key, code = " ", "Space"
				}
				_ = a.page.Key(ctx, "keyDown", key, code, 0)
				_ = a.page.Key(ctx, "keyUp", key, code, 0)
				sleepJitter(ctx, 800, 1400)
				return true
			}
		}
		_ = a.page.Key(ctx, "keyDown", "Tab", "Tab", 0)
		_ = a.page.Key(ctx, "keyUp", "Tab", "Tab", 0)
		sleepJitter(ctx, 250, 250)
	}
	return false
}

func matchesAnyLabel(text string, want []string) bool {
	if text == "" {
		return false
	}
	for _, w := range want {
		if text == w || strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// jsClickExact 在页面里直接用 JS 找精确文案的元素并 focus+click。
// 这是三级点击策略里最先尝试的一级:不受坐标/遮挡影响。
func (a *outlookAutoLogin) jsClickExact(ctx context.Context, labels []string) bool {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return false
	}
	expr := `((labels) => {
  const want = labels.map(x => String(x).trim().toLowerCase());
  const vis = e => !!e && e.offsetParent !== null && !e.disabled && getComputedStyle(e).visibility !== 'hidden' && getComputedStyle(e).display !== 'none';
  const nodes = [...document.querySelectorAll('button,input[type=submit],input[type=button],[role=button],span,a')].filter(vis);
  const e = nodes.find(e => want.includes((e.innerText || e.value || e.getAttribute('aria-label') || '').trim().toLowerCase()));
  if (!e) return false;
  e.focus();
  e.click();
  return true;
})(` + string(encoded) + `)`
	var hit bool
	if err := a.page.Evaluate(ctx, expr, &hit); err != nil || !hit {
		return false
	}
	sleepJitter(ctx, 800, 1400)
	return true
}

// activate 三级降级地激活一个按钮:JS 点击 → 键盘激活 → 鼠标事件。
func (a *outlookAutoLogin) activate(ctx context.Context, labels []string, patterns []string) bool {
	if a.jsClickExact(ctx, labels) {
		return true
	}
	if a.keyboardActivate(ctx, labels) {
		return true
	}
	if meta := a.findButton(ctx, patterns); meta != nil {
		return a.click(ctx, *meta) == nil
	}
	return false
}

// clickAndWaitChange 点按钮后等状态变化;超时后再用键盘激活兜底一次。
func (a *outlookAutoLogin) clickAndWaitChange(ctx context.Context, oldState string, labels, patterns []string) string {
	if !a.activate(ctx, labels, patterns) {
		return oldState
	}
	if next, ok := a.waitStateChange(ctx, oldState, outlookAutoTransitionTimeout); ok {
		return next
	}
	if a.keyboardActivate(ctx, labels) {
		if next, ok := a.waitStateChange(ctx, oldState, 12*time.Second); ok {
			return next
		}
	}
	return oldState
}

// waitStateChange 每 1.2s 拍一次快照,直到状态与 oldState 不同或命中终态。
func (a *outlookAutoLogin) waitStateChange(ctx context.Context, oldState string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !sleepCtx(ctx, 1200*time.Millisecond) {
			return oldState, false
		}
		snap, err := a.snapshot(ctx)
		if err != nil {
			continue
		}
		state := classifyOutlookLoginState(snap)
		if state != oldState || isOutlookTerminalState(state) {
			return state, true
		}
	}
	return oldState, false
}

func isOutlookTerminalState(state string) bool {
	switch state {
	case "success", "problem", "captcha", "mfa_or_protection", "password_choice":
		return true
	}
	return false
}

// Run 跑完整个自动登录状态机。返回结局由调用方决定后续:成功就直接抓 token,
// NeedsHuman 就把窗口留给用户接管,Fatal 就直接失败。
func (a *outlookAutoLogin) Run(ctx context.Context) outlookAutoResult {
	// 不再固定睡 6 秒:按 DOM 就绪度判定,页面稳定下来立刻开工(通常 1 秒内)。
	a.note("正在等待登录页加载…")
	if _, ready := a.waitReady(ctx, outlookAutoReadyTimeout); ready == "" && ctx.Err() != nil {
		return outlookAutoResult{State: "cancelled", Fatal: true, Message: "已取消"}
	}
	state := ""
	repeats := 0
	for step := 1; step <= outlookAutoMaxSteps; step++ {
		if ctx.Err() != nil {
			return outlookAutoResult{State: "cancelled", Fatal: true, Message: "已取消"}
		}
		snap, err := a.snapshot(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return outlookAutoResult{State: "cancelled", Fatal: true, Message: "已取消"}
			}
			// 重新附着都救不回来:窗口没了就直接判死,别再回落到手动等待干等到超时。
			if a.session != nil && !a.session.Alive() {
				return outlookAutoResult{State: "unknown", Fatal: true, Message: "Chrome 窗口已关闭，登录未完成"}
			}
			return outlookAutoResult{State: "unknown", NeedsHuman: true,
				Message: "读取页面失败（" + err.Error() + "），请在窗口里手动继续"}
		}
		next := classifyOutlookLoginState(snap)
		if next == state {
			repeats++
		} else {
			repeats = 0
		}
		state = next
		// 同一个状态反复出现说明操作没被页面接受,再走下去只是把每步的转场超时耗满。
		if repeats >= outlookAutoMaxSameState && !isOutlookTerminalState(state) {
			return a.stuckResult(state, snap)
		}
		switch state {
		case "loading":
			// 同样交给 DOM 判定,而不是傻等固定时长;它内部按 250ms 轮询。
			if _, ready := a.waitReady(ctx, outlookAutoReadyTimeout); ready == "" && ctx.Err() != nil {
				return outlookAutoResult{State: "cancelled", Fatal: true, Message: "已取消"}
			}
		case "success":
			return outlookAutoResult{State: state, Message: "自动登录成功"}
		case "problem":
			return outlookAutoResult{State: state, Fatal: true,
				Message: "登录被拒绝：" + outlookProblemReason(snap)}
		case "captcha":
			return outlookAutoResult{State: state, NeedsHuman: true,
				Message: "遇到人机验证，请在 Chrome 窗口里完成，完成后会自动继续"}
		case "mfa_or_protection":
			return outlookAutoResult{State: state, NeedsHuman: true,
				Message: "需要账号验证（验证码 / 两步验证），请在 Chrome 窗口里完成，完成后会自动继续"}
		case "password_choice":
			a.note("正在切换到密码登录…")
			if a.keyboardActivate(ctx, []string{"Use your password"}) ||
				a.jsClickExact(ctx, []string{"Use your password"}) {
				sleepJitter(ctx, 1500, 1500)
				continue
			}
			if meta := a.findButton(ctx, []string{"use your password"}); meta != nil {
				_ = a.click(ctx, *meta)
				continue
			}
			return outlookAutoResult{State: state, NeedsHuman: true,
				Message: "页面要求选择验证方式但找不到「Use your password」，请在窗口里手动继续"}
		case "account_picker":
			a.note("正在选择账号…")
			if a.activate(ctx, []string{"Use another account"}, []string{"use another account", strings.ToLower(a.email)}) {
				continue
			}
			return outlookAutoResult{State: state, NeedsHuman: true,
				Message: "账号选择页无法自动处理，请在窗口里手动继续"}
		case "email":
			a.note("正在填写邮箱…")
			if !a.focusAndTypeIfNeeded(ctx, []string{"loginfmt", "email", "phone", "skype", "username"}, a.email) {
				return outlookAutoResult{State: state, NeedsHuman: true,
					Message: "邮箱没能填进输入框（找不到或被页面拦截），请在窗口里手动继续"}
			}
			if next := a.clickAndWaitChange(ctx, "email", []string{"Next"}, []string{"next"}); next == "email" {
				a.clickAndWaitChange(ctx, "email", []string{"Next"}, []string{"next"})
			}
		case "password":
			a.note("正在填写密码…")
			if !a.focusAndTypeIfNeeded(ctx, []string{"passwd", "password"}, a.password) {
				return outlookAutoResult{State: state, NeedsHuman: true,
					Message: "密码没能填进输入框（找不到或被页面拦截），已停在这一步没有提交，请在窗口里手动继续"}
			}
			a.clickAndWaitChange(ctx, "password", []string{"Sign in", "Next"}, []string{"sign in", "next", "continue"})
		case "stay_signed_in":
			// 默认选 Yes:隔离 profile 要保住登录会话,cookie 才会持久化落盘,
			// 后续的静默刷新完全依赖这份 cookie。
			a.note("正在确认保持登录状态…")
			if !a.activate(ctx, []string{"Yes"}, []string{"yes"}) {
				return outlookAutoResult{State: state, NeedsHuman: true,
					Message: "「保持登录状态」页无法自动确认，请在窗口里手动继续"}
			}
			if next, ok := a.waitStateChange(ctx, "stay_signed_in", outlookAutoTransitionTimeout); ok && next == "success" {
				return outlookAutoResult{State: "success", Message: "自动登录成功"}
			}
		default: // unknown
			return outlookAutoResult{State: state, NeedsHuman: true,
				Message: "自动流程遇到未识别的页面，请在窗口里手动继续"}
		}
	}
	return outlookAutoResult{State: state, NeedsHuman: true,
		Message: "自动登录步数用尽，请在窗口里手动继续"}
}

// stuckResult 处理「页面一直停在同一步」的结局:页面上有报错就当确定失败(账号有问题,
// 留着窗口也没人能救),没有报错才转人工——那种情况可能只是控件没吃到我们的点击。
func (a *outlookAutoLogin) stuckResult(state string, snap domSnapshot) outlookAutoResult {
	label := fallback(outlookStateLabels[state], state)
	if reason := outlookSnapshotError(snap); reason != "" {
		return outlookAutoResult{State: state, Fatal: true,
			Message: fmt.Sprintf("登录停在「%s」步且页面报错：%s", label, reason)}
	}
	return outlookAutoResult{State: state, NeedsHuman: true,
		Message: fmt.Sprintf("自动登录停在「%s」步没有推进，请在窗口里手动继续", label)}
}

// outlookProblemReason 从页面报错块和正文里挑一句能解释失败的原因,回报给页面显示。
func outlookProblemReason(snap domSnapshot) string {
	pageError := outlookSnapshotError(snap)
	hay := strings.ToLower(snap.BodyText + " " + pageError)
	reasons := []struct{ key, text string }{
		{"account has been locked", "账号已被锁定"},
		{"temporarily blocked", "账号被临时封禁"},
		{"too many times", "尝试次数过多"},
		{"couldn't find a microsoft account", "账号不存在（微软找不到这个账号）"},
		{"couldn't find an account", "账号不存在"},
		{"no account found", "账号不存在"},
		{"doesn't exist", "账号不存在"},
		{"does not exist", "账号不存在"},
		{"enter a valid email address", "邮箱格式不被接受"},
		{"password sign-in isn't available", "此账号当前不允许密码登录，需改用其它验证方式"},
		{"password sign in isn't available", "此账号当前不允许密码登录，需改用其它验证方式"},
		{"try another method", "此账号当前不允许密码登录，需改用其它验证方式"},
		{"incorrect", "账号或密码不正确"},
		{"err_connection_closed", "登录页网络加载失败(ERR_CONNECTION_CLOSED)"},
		{"err_connection_reset", "登录页网络加载失败(ERR_CONNECTION_RESET)"},
		{"err_timed_out", "登录页网络加载超时(ERR_TIMED_OUT)"},
		{"无法访问此网站", "登录页网络加载失败：无法访问此网站"},
		{"意外终止了连接", "登录页网络加载失败：连接被意外终止"},
		{"unexpectedly closed the connection", "登录页网络加载失败：连接被意外终止"},
		{"this site can't be reached", "登录页网络加载失败：无法访问此网站"},
		{"this site can’t be reached", "登录页网络加载失败：无法访问此网站"},
		{"we ran into a problem", "微软登录页报错"},
		{"something went wrong", "微软登录页报错"},
	}
	for _, reason := range reasons {
		if strings.Contains(hay, reason.key) {
			return reason.text
		}
	}
	// 关键词都没命中就把页面原话带回去,总好过一句「未知原因」。
	return fallback(pageError, "未知原因")
}

// waitForPageTarget 等 Chrome 把第一个标签页建出来。冷启动要一两秒,不等的话首次
// getTargets 可能是空的,自动流程会被误判成「无法启动」而白白降级成手动。
func waitForPageTarget(ctx context.Context, session *chromeSession, timeout time.Duration) (chromeTarget, error) {
	deadline := time.Now().Add(timeout)
	for {
		target, ok, err := session.ActivePageTarget(ctx)
		if err == nil && ok {
			return target, nil
		}
		if !session.Alive() {
			return chromeTarget{}, errors.New("Chrome 已退出")
		}
		if time.Now().After(deadline) {
			return chromeTarget{}, errors.New("没找到可操作的标签页")
		}
		if !sleepCtx(ctx, 200*time.Millisecond) {
			return chromeTarget{}, errors.New("已取消")
		}
	}
}

// startOutlookAutoLogin 附着到当前标签页并跑自动登录。
func startOutlookAutoLogin(ctx context.Context, session *chromeSession, email, password string, report func(string)) (outlookAutoResult, error) {
	target, err := waitForPageTarget(ctx, session, 15*time.Second)
	if err != nil {
		return outlookAutoResult{}, err
	}
	page, err := session.AttachPage(ctx, target.TargetID)
	if err != nil {
		return outlookAutoResult{}, err
	}
	driver := &outlookAutoLogin{session: session, page: page, email: email, password: password, report: report}
	return driver.Run(ctx), nil
}

// sleepCtx 睡一段时间,ctx 取消时提前返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// sleepJitter 在 [minMS, maxMS] 之间随机睡一会儿,让节奏不那么机械。
func sleepJitter(ctx context.Context, minMS, maxMS int) {
	d := minMS
	if maxMS > minMS {
		d = minMS + rand.Intn(maxMS-minMS)
	}
	sleepCtx(ctx, time.Duration(d)*time.Millisecond)
}
