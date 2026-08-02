package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// GPT 账号自动化的共享 CDP 驱动:signup / login 两条流程共用。
// 架在 cdp.go 之上(launchChrome + AttachPage + cdpPage),DOM/CDP 驱动、不依赖截图/模型。
// 设计原则:撞到不认识的状态就干净失败(转人工),不做模型兜底。

// gptHumanPat 命中即判为人机验证,立刻停(不尝试破解)。与 signup skill 的 HUMAN_PAT 对齐。
var gptHumanPat = regexp.MustCompile(`(?i)captcha|verify you are human|human verification|press and hold|turnstile|hcaptcha|arkose|funcaptcha|challenge`)

type gptInput struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	Placeholder  string `json:"placeholder"`
	Autocomplete string `json:"autocomplete"`
	Value        string `json:"value"`
	Disabled     bool   `json:"disabled"`
}

type gptButton struct {
	Text     string `json:"text"`
	Disabled bool   `json:"disabled"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

// gptState 是一份页面快照,字段是 signup + login 两条流程需要的并集。
type gptState struct {
	URL          string      `json:"url"`
	Title        string      `json:"title"`
	Ready        string      `json:"ready"`
	Text         string      `json:"text"`
	Inputs       []gptInput  `json:"inputs"`
	Buttons      []gptButton `json:"buttons"`
	HasCodeInput bool        `json:"hasCodeInput"`
	Country      string      `json:"country"` // 手机页可见的国家按钮文案(含 "(+")
	Phone        string      `json:"phone"`   // input[name=phoneNumber] 的值
	Channel      string      `json:"channel"` // input[name=channel] 的值(sms/whatsapp)
	SMSSelected  bool        `json:"smsSelected"`
	WhatsappHint bool        `json:"whatsappHint"`
	Suspicious   []string    `json:"suspicious"`
}

// gptStateJS 一次性取回上面所有字段。合并了 signup 的 state() 与 login 的 state()。
const gptStateJS = `(() => {
  const attr = (el, name) => el.getAttribute(name) || '';
  const inputs = [...document.querySelectorAll('input,textarea,[contenteditable=true]')].map(i => ({
    type:i.type||'', name:i.name||'', id:i.id||'', placeholder:i.placeholder||'',
    autocomplete:i.autocomplete||'', disabled:!!i.disabled,
    value:(i.name==='code'||i.autocomplete==='one-time-code'||/code/i.test(i.id))?'[REDACTED]':(i.value||'')
  }));
  const buttons = [...document.querySelectorAll('button,a,[role=button]')].slice(0,150).map(b => ({
    text:(b.innerText||'').trim(), disabled:!!b.disabled, type:b.type||'', name:b.name||'', value:b.value||''
  }));
  const controls = [...document.querySelectorAll('input,button,label,[role=radio],[aria-label],[data-testid]')]
    .map(el => [el.tagName, attr(el,'type'), attr(el,'name'), attr(el,'value'), attr(el,'aria-label'), (el.innerText||'').slice(0,120)].join(' '));
  const hay = controls.join(' | ').toLowerCase();
  const smsRadio = [...document.querySelectorAll('input[type=radio]')].find(i => String(i.value).toLowerCase()==='sms');
  const channel = document.querySelector('input[name=channel]')?.value || '';
  const hasCodeInput = inputs.some(i => i.name==='code'||i.autocomplete==='one-time-code'||/(^|-)code$/i.test(i.id));
  const whatsappHint = /whatsapp/i.test(hay);
  return {
    url:location.href, title:document.title, ready:document.readyState, text:(document.body?document.body.innerText:'').slice(0,4800),
    inputs, buttons, hasCodeInput,
    country:[...document.querySelectorAll('button')].map(b=>b.innerText).find(t=>t&&t.includes('(+'))||'',
    phone:document.querySelector('input[name=phoneNumber]')?.value||'',
    channel,
    smsSelected:!!smsRadio?.checked || String(channel).toLowerCase()==='sms',
    whatsappHint,
    suspicious:[...document.querySelectorAll('iframe,[class],[id]')].map(e=>[e.tagName,e.id||'',e.className||'',e.src||''].join(' ')).filter(x=>/captcha|turnstile|hcaptcha|arkose|funcaptcha|challenge/i.test(x)).slice(0,20)
  };
})()`

// gptDriver 包一个端口模式的 gptChrome(直连页面 WS),提供 DOM 操作原语。
type gptDriver struct {
	chrome *gptChrome
}

// newGPTDriver 启动隔离 Chrome(端口模式 + 直连页面 WS,与 skill 一致,不触发 AutomationControlled)
// 打开 startURL。任务结束调 Close() 删临时 profile。
func newGPTDriver(ctx context.Context, startURL string) (*gptDriver, error) {
	chrome, err := launchGPTChrome(ctx, startURL)
	if err != nil {
		return nil, err
	}
	return &gptDriver{chrome: chrome}, nil
}

// Close 关闭 Chrome 并删除临时 profile。
func (d *gptDriver) Close() {
	if d.chrome != nil {
		d.chrome.Close()
	}
}

func (d *gptDriver) Alive() bool { return d.chrome != nil && d.chrome.Alive() }

// state 取一份页面快照。
func (d *gptDriver) state(ctx context.Context) (gptState, error) {
	var st gptState
	err := d.chrome.Evaluate(ctx, gptStateJS, &st)
	return st, err
}

// navigate 让页面跳转到 url。
func (d *gptDriver) navigate(ctx context.Context, url string) error {
	return d.chrome.Navigate(ctx, url)
}

// evalBool 执行一段返回布尔的 JS。DOM 只用于状态读取/坐标定位，正常输入点击走 CDP Input。
func (d *gptDriver) evalBool(ctx context.Context, expr string) bool {
	var ok bool
	_ = d.chrome.Evaluate(ctx, expr, &ok)
	return ok
}

type elementPoint struct {
	OK bool    `json:"ok"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

func (d *gptDriver) elementCenter(ctx context.Context, findExpr string) (elementPoint, bool) {
	expr := fmt.Sprintf(`(()=>{const e=%s;if(!e)return {ok:false};e.scrollIntoView({block:'center',inline:'center'});const r=e.getBoundingClientRect();return {ok:true,x:r.left+r.width/2,y:r.top+r.height/2};})()`, findExpr)
	var p elementPoint
	if err := d.chrome.Evaluate(ctx, expr, &p); err != nil || !p.OK {
		return p, false
	}
	return p, true
}

func (d *gptDriver) mouseClickFind(ctx context.Context, findExpr string) bool {
	p, ok := d.elementCenter(ctx, findExpr)
	if !ok {
		return false
	}
	if err := d.chrome.MouseClick(ctx, p.X, p.Y); err != nil {
		return false
	}
	sleepCtx(ctx, 250*time.Millisecond)
	return true
}

func (d *gptDriver) mouseClickSelector(ctx context.Context, selector string) bool {
	return d.mouseClickFind(ctx, fmt.Sprintf(`document.querySelector(%s)`, jsStr(selector)))
}

func selectAllModifier() int {
	if runtime.GOOS == "darwin" {
		return 4 // Meta(Command)
	}
	return 2 // Ctrl
}

func (d *gptDriver) clearFocusedInput(ctx context.Context) bool {
	// 严格清空：上层 mouseClick 已聚焦输入框。先把光标移到末尾，Backspace 删光；
	// 再移到开头，Delete 补删。每轮后 DOM 读取 activeElement.value，确认为空才允许输入新值。
	for round := 0; round < 4; round++ {
		d.keyCommand(ctx, "End", "End", 35, 119, "moveToEndOfLine")
		sleepCtx(ctx, 80*time.Millisecond)
		for i := 0; i < 80; i++ {
			d.deleteBackward(ctx)
			sleepCtx(ctx, 15*time.Millisecond)
		}
		sleepCtx(ctx, 180*time.Millisecond)
		if strings.TrimSpace(d.focusedValue(ctx)) == "" {
			return true
		}

		d.keyCommand(ctx, "Home", "Home", 36, 115, "moveToBeginningOfLine")
		sleepCtx(ctx, 80*time.Millisecond)
		for i := 0; i < 80; i++ {
			d.deleteForward(ctx)
			sleepCtx(ctx, 15*time.Millisecond)
		}
		sleepCtx(ctx, 220*time.Millisecond)
		if strings.TrimSpace(d.focusedValue(ctx)) == "" {
			return true
		}
	}
	return strings.TrimSpace(d.focusedValue(ctx)) == ""
}

func (d *gptDriver) keyCommand(ctx context.Context, key, code string, windowsVK, nativeVK int, command string) {
	params := map[string]any{
		"type":                  "keyDown",
		"key":                   key,
		"code":                  code,
		"windowsVirtualKeyCode": windowsVK,
		"nativeVirtualKeyCode":  nativeVK,
	}
	if command != "" {
		params["commands"] = []string{command}
	}
	_ = d.chrome.call(ctx, "Input.dispatchKeyEvent", params, nil)
	_ = d.chrome.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type":                  "keyUp",
		"key":                   key,
		"code":                  code,
		"windowsVirtualKeyCode": windowsVK,
		"nativeVirtualKeyCode":  nativeVK,
	}, nil)
}

func (d *gptDriver) deleteBackward(ctx context.Context) {
	// macOS Backspace/Delete backward: native key code 51; command 确保触发编辑动作。
	d.keyCommand(ctx, "Backspace", "Backspace", 8, 51, "deleteBackward")
}

func (d *gptDriver) deleteForward(ctx context.Context) {
	// macOS forward delete: native key code 117; command 确保触发编辑动作。
	d.keyCommand(ctx, "Delete", "Delete", 46, 117, "deleteForward")
}

func (d *gptDriver) tapKey(ctx context.Context, key, code string) {
	_ = d.chrome.Key(ctx, "keyDown", key, code, 0)
	_ = d.chrome.Key(ctx, "keyUp", key, code, 0)
}

func (d *gptDriver) deleteFocused(ctx context.Context) {
	d.deleteBackward(ctx)
	d.deleteForward(ctx)
}

func (d *gptDriver) focusedValue(ctx context.Context) string {
	var v string
	_ = d.chrome.Evaluate(ctx, `(()=>{const e=document.activeElement;if(!e)return '';if('value' in e)return String(e.value||'');return String(e.innerText||'');})()`, &v)
	return v
}

func (d *gptDriver) typeText(ctx context.Context, text string) error {
	for _, r := range text {
		if err := d.chrome.Char(ctx, string(r)); err != nil {
			return err
		}
		sleepCtx(ctx, 18*time.Millisecond)
	}
	sleepCtx(ctx, 250*time.Millisecond)
	return nil
}

const primarySubmitButtonExpr = `(()=>{
  const bad=/google|apple|microsoft|github|sso|saml|phone|telephone|手机号|电话号码|电话|苹果|谷歌|账户继续|账号继续/i;
  const good=/^(继续|Continue|下一步|Next|创建账号|Create account|Finish creating account|完成|Finish)$/i;
  const norm=s=>String(s||'').replace(/\s+/g,' ').trim();
  const visible=e=>{const r=e.getBoundingClientRect();const cs=getComputedStyle(e);return r.width>1&&r.height>1&&cs.visibility!=='hidden'&&cs.display!=='none'};
  const buttons=[...document.querySelectorAll('button,a,[role=button],input[type=submit]')].filter(b=>!b.disabled&&visible(b));
  const label=b=>norm(b.innerText||b.value||b.getAttribute('aria-label')||'');
  let exact=buttons.find(b=>{const t=label(b);return good.test(t)&&!bad.test(t)});
  if(exact)return exact;
  // 有些 OpenAI 构建按钮文案周围带 loading/span，允许包含式，但仍排除社交登录。
  let loose=buttons.find(b=>{const t=label(b);return /(继续|Continue|下一步|Next|Finish creating account|Create account)/i.test(t)&&!bad.test(t)});
  if(loose)return loose;
  // 最后只在当前聚焦输入框所属 form 内找 submit，并排除社交按钮；仍找不到就返回 null，让调用方按 Enter。
  const active=document.activeElement;
  const form=active&&(active.form||active.closest?.('form'));
  if(form){
    const local=[...form.querySelectorAll('button,input[type=submit]')].filter(b=>!b.disabled&&visible(b));
    return local.find(b=>!bad.test(label(b)))||null;
  }
  return null;
})()`

// hasElement 判断页面上是否已有指定元素；用于慢加载时避免把“DOM 还没出来”误判成失败。
func (d *gptDriver) hasElement(ctx context.Context, selector string) bool {
	var ok bool
	expr := fmt.Sprintf(`!!document.querySelector(%s)`, jsStr(selector))
	return d.chrome.Evaluate(ctx, expr, &ok) == nil && ok
}

// insertTrusted 用鼠标点击输入框后，通过 CDP 键盘事件逐字符输入。signup 用这条。
func (d *gptDriver) insertTrusted(ctx context.Context, selector, text string) error {
	if !d.mouseClickSelector(ctx, selector) {
		return fmt.Errorf("找不到输入框: %s", selector)
	}
	if !d.clearFocusedInput(ctx) {
		return fmt.Errorf("输入框未清空: %s", selector)
	}
	return d.typeText(ctx, text)
}

// reactFill 保留旧名字给 login 调用；实现已改为鼠标点击 + 键盘输入，不再 DOM set value。
// findExpr 是一段返回目标 input 元素的 JS 表达式(不含末尾分号)。
func (d *gptDriver) reactFill(ctx context.Context, findExpr, value string) bool {
	if !d.mouseClickFind(ctx, findExpr) {
		return false
	}
	if !d.clearFocusedInput(ctx) {
		return false
	}
	return d.typeText(ctx, value) == nil
}

// clickContinue 用鼠标点击第一个未禁用的「继续/Continue」按钮。
func (d *gptDriver) clickContinue(ctx context.Context) bool {
	return d.mouseClickFind(ctx, primarySubmitButtonExpr)
}

// clickContinueOrEnter 优先鼠标点「继续/Continue」按钮,点不到再键盘回车兜底。
func (d *gptDriver) clickContinueOrEnter(ctx context.Context) {
	if d.clickContinue(ctx) {
		sleepCtx(ctx, 1200*time.Millisecond)
		return
	}
	d.pressEnter(ctx)
}

// clickExact 用鼠标点击文案精确匹配的按钮/链接。
func (d *gptDriver) clickExact(ctx context.Context, texts ...string) bool {
	arr, _ := json.Marshal(texts)
	expr := fmt.Sprintf(`((texts)=>[...document.querySelectorAll('button,a,[role=button]')].find(x=>texts.includes((x.innerText||'').trim())))(%s)`, string(arr))
	return d.mouseClickFind(ctx, expr)
}

// submitForm 通过鼠标点击 submit/Continue 按钮提交，不再调用 DOM form.requestSubmit。
func (d *gptDriver) submitForm(ctx context.Context, activeSelector string) bool {
	// 先确保焦点还在目标输入框，再用严格的主按钮选择器。找不到按钮时按 Enter，
	// 不再退化点击任意 type=submit，避免误点 Google/Apple/phone 第三方登录。
	if activeSelector != "" {
		_ = d.mouseClickSelector(ctx, activeSelector)
	}
	if d.mouseClickFind(ctx, primarySubmitButtonExpr) {
		sleepCtx(ctx, 1200*time.Millisecond)
		return true
	}
	d.pressEnter(ctx)
	return true
}

// pressEnter 在当前焦点上敲回车。
func (d *gptDriver) pressEnter(ctx context.Context) {
	_ = d.chrome.Key(ctx, "keyDown", "Enter", "Enter", 0)
	_ = d.chrome.Key(ctx, "keyUp", "Enter", "Enter", 0)
	sleepCtx(ctx, 1200*time.Millisecond)
}

// gptErrorPageReason 检测 OpenAI 自己的 auth 报错页(如「糟糕，出错了」/ Route Error /
// Invalid content type: text/html)。命中返回一句可读原因,否则空串。
// 这类错误是后端返回了 HTML 而非 JSON(常见于 IP 信誉/反自动化/代理干扰),DOM 自动化无能为力,
// 命中即快速失败并报出原因,别一路 default 走到 max steps。
func gptErrorPageReason(st gptState) string {
	if strings.Contains(st.URL, "accounts.google.com") || strings.Contains(st.URL, "appleid.apple.com") || strings.Contains(st.URL, "login.microsoftonline.com") {
		return "误入第三方 OAuth 登录页:" + st.URL
	}
	text := st.Text
	if strings.Contains(text, "Invalid content type: text/html") || strings.Contains(text, "Route Error") {
		if i := strings.Index(text, "Route Error"); i >= 0 {
			return limitString(strings.TrimSpace(text[i:]), 200)
		}
		return "OpenAI 返回了 HTML 错误页(Invalid content type),多为 IP 信誉/反自动化/代理拦截,DOM 无法处理"
	}
	// 「糟糕，出错了！」/「Oops, an error occurred」独立出现且没有正常表单时才算(避免误伤)。
	if (strings.Contains(text, "糟糕，出错了") || strings.Contains(text, "an error occurred")) && !st.HasCodeInput && len(st.Inputs) == 0 {
		return "OpenAI 报错页:" + limitString(strings.TrimSpace(text), 160)
	}
	return ""
}

// checkHuman 判断当前快照是否命中人机验证。
func gptIsHuman(st gptState) bool {
	hay := st.URL + "\n" + st.Title + "\n" + limitString(st.Text, 5000) + "\n" + strings.Join(st.Suspicious, "\n")
	return gptHumanPat.MatchString(hay)
}

// waitUntil 每 interval 拍一次快照,直到 pred 为真或超时;返回最后一次快照。
func (d *gptDriver) waitUntil(ctx context.Context, pred func(gptState) bool, timeout, interval time.Duration) gptState {
	deadline := time.Now().Add(timeout)
	var last gptState
	for {
		st, err := d.state(ctx)
		if err == nil {
			last = st
			if pred(st) {
				return st
			}
		}
		if time.Now().After(deadline) || !d.Alive() {
			return last
		}
		if !sleepCtx(ctx, interval) {
			return last
		}
	}
}

// waitUntilStable 每 interval 拍一次快照，pred 连续满足 need 次才返回；用于前端慢加载/水合期间避免过早输入。
func (d *gptDriver) waitUntilStable(ctx context.Context, pred func(gptState) bool, timeout, interval time.Duration, need int) gptState {
	if need <= 0 {
		need = 2
	}
	deadline := time.Now().Add(timeout)
	var last gptState
	stable := 0
	for {
		st, err := d.state(ctx)
		if err == nil {
			last = st
			if pred(st) {
				stable++
				if stable >= need {
					return st
				}
			} else {
				stable = 0
			}
		}
		if time.Now().After(deadline) || !d.Alive() {
			return last
		}
		if !sleepCtx(ctx, interval) {
			return last
		}
	}
}

// jsStr 把字符串安全编码成 JS 字面量(用 JSON 编码即可)。
func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---- 邮箱验证码(新鲜 + OpenAI 来源过滤)----

// fetchFreshGPTCode 轮询该邮箱收件箱,取「发件人/主题含 openai|chatgpt 且收件时间晚于 notBefore」的验证码。
// 复用 Outlook 邮件接口那套(outlookMailFetch),避免命中旧的微软安全码或上一封 ChatGPT 码。
func (p *proxyServer) fetchFreshGPTCode(ctx context.Context, email string, notBefore time.Time, attempts int) (string, error) {
	id, err := p.store.GetOutlookIDByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("邮箱 %s 不在 outlook_login_tokens: %w", email, err)
	}
	listURL := fmt.Sprintf("%s/me/mailfolders/inbox/messages?$top=1&$select=Id,Subject,From,ReceivedDateTime,BodyPreview&$orderby=ReceivedDateTime%%20desc", outlookMailAPIBase)
	for i := 0; i < attempts; i++ {
		listData, ferr := p.outlookMailFetch(ctx, id, func() string { return listURL }, outlookMailListPrefer)
		if ferr == nil {
			if msgs := summarizeOutlookMail(listData); len(msgs) > 0 {
				latest := msgs[0]
				from := strings.ToLower(asString(latest["from"]))
				subject := strings.ToLower(asString(latest["subject"]))
				received := parseOutlookTime(asString(latest["received"]))
				isOpenAI := strings.Contains(from, "openai") || strings.Contains(from, "chatgpt") ||
					strings.Contains(subject, "openai") || strings.Contains(subject, "chatgpt")
				isFresh := notBefore.IsZero() || (!received.IsZero() && !received.Before(notBefore.Add(-2*time.Second)))
				if isOpenAI && isFresh {
					mid := asString(latest["id"])
					getURL := fmt.Sprintf("%s/me/messages/%s?$select=Id,Subject,From,ReceivedDateTime,Body", outlookMailAPIBase, url.QueryEscape(mid))
					msgData, merr := p.outlookMailFetch(ctx, id, func() string { return getURL }, `outlook.body-content-type="text"`)
					text := asString(latest["preview"]) + "\n" + asString(latest["subject"])
					if merr == nil {
						text = asString(reshapeOutlookMessage(msgData)["body"]) + "\n" + text
					}
					if code := extractVerificationCode(text); code != "" {
						return code, nil
					}
				}
			}
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return "", context.Canceled
		}
	}
	return "", fmt.Errorf("未取到 %s 的新鲜 OpenAI 验证码", email)
}

// parseOutlookTime 解析 Outlook 的 ISO-8601 时间(容错,失败返回零值)。
func parseOutlookTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
