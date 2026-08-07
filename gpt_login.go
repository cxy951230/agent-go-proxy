package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// GPT 账号登录 Codex 自动化(移植自 gpt-login-automation skill)。
// openaiLogins.Start 拿 auth_url(Bridge open_browser:false,只起 1455 回调)→ 开隔离 Chrome →
// 填邮箱+验证码 → 手机验证(HeroSMS)→ Codex consent 点 Continue → 轮询登录状态 completed。
//
// 与 skill 的关键差异:
//   - 手机号买了不再同步等 2 分钟取消,而是登记进 herosmsTracker,后台超 125s 自动取消(见 herosms.go)。
//   - 不 resume、不切 Clash、不做模型兜底;任务结束删临时 profile。

const (
	// gptLoginMaxAttempts 是一次登录最多买几个号。每个国家一批买最多 3 个,所以这个数决定
	// 能换几个国家试(15 ≈ 5 个国家)。skill 默认 3 但要求操作者传 --max-attempts 9/15 才能
	// 多国轮换;这里直接给足,否则第一个国家(常是 WhatsApp-only)一批 3 个就到顶、看起来像
	// "发送失败就结束"。买多的号超 125s 未用会被后台自动取消。
	gptLoginMaxAttempts           = 15
	gptLoginBatch                 = 3    // 每个国家一批买几个
	gptLoginMaxAttemptsPerCountry = 3    // 同一个国家最多实际尝试几个号码，防止同国连续撞号/频控
	gptLoginDefaultMinCount       = 2000 // 默认优惠库存下限，实际值可在 HeroSMS 页面配置
	gptLoginDefaultMaxPrice       = 0.2  // 默认单号价格上限，实际值可在 HeroSMS 页面配置
)

type gptLoginStatus struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	State       string    `json:"state"` // running / completed / failed / cancelled
	Message     string    `json:"message,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func (s gptLoginStatus) done() bool {
	switch s.State {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

type gptLoginProcess struct {
	status   gptLoginStatus
	cancel   context.CancelFunc
	driver   *gptDriver
	bridgeID string
}

type gptLoginManager struct {
	srv *proxyServer
	mu  sync.Mutex
	wg  sync.WaitGroup
	all map[string]*gptLoginProcess
}

func newGPTLoginManager(srv *proxyServer) *gptLoginManager {
	return &gptLoginManager{srv: srv, all: make(map[string]*gptLoginProcess)}
}

func (m *gptLoginManager) running() bool {
	for _, p := range m.all {
		if !p.status.done() {
			if p.driver != nil && !p.driver.Alive() {
				p.status.State = "cancelled"
				p.status.Message = "Chrome 窗口已关闭"
				p.status.CompletedAt = time.Now()
				p.cancel()
				continue
			}
			return true
		}
	}
	return false
}

func (m *gptLoginManager) Start(email string) (gptLoginStatus, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return gptLoginStatus{}, errors.New("缺少邮箱")
	}
	// 与 signup 也互斥:两者都要开浏览器。先查对方(锁各自的锁,顺序固定避免交叉死锁)。
	if m.srv.gptSignups != nil && m.srv.gptSignups.hasRunning() {
		return gptLoginStatus{}, errStartConflict
	}
	m.mu.Lock()
	if m.running() {
		m.mu.Unlock()
		return gptLoginStatus{}, errStartConflict
	}
	id, err := randomLoginID()
	if err != nil {
		m.mu.Unlock()
		return gptLoginStatus{}, err
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), scopedProxyContextKey{}, true))
	proc := &gptLoginProcess{
		status: gptLoginStatus{ID: id, Email: email, State: "running", Message: "启动中…", StartedAt: time.Now()},
		cancel: cancel,
	}
	m.all[id] = proc
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.run(ctx, id, email)
	}()
	return proc.status, nil
}

func (m *gptLoginManager) hasRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running()
}

func (m *gptLoginManager) attachDriver(id string, driver *gptDriver) {
	m.mu.Lock()
	proc := m.all[id]
	if proc != nil {
		proc.driver = driver
	}
	m.mu.Unlock()
}

func (m *gptLoginManager) attachBridge(id, bridgeID string) {
	m.mu.Lock()
	proc := m.all[id]
	if proc != nil {
		proc.bridgeID = bridgeID
	}
	m.mu.Unlock()
}

func (m *gptLoginManager) set(id, state, message, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc := m.all[id]
	if proc == nil {
		return
	}
	if proc.status.done() && proc.status.State != state {
		return
	}
	proc.status.State = state
	if message != "" {
		proc.status.Message = message
	}
	if errMsg != "" {
		proc.status.Error = errMsg
	}
	if proc.status.done() && proc.status.CompletedAt.IsZero() {
		proc.status.CompletedAt = time.Now()
	}
}

func (m *gptLoginManager) note(id, message string) { m.set(id, "running", message, "") }

func (m *gptLoginManager) Status(id string) (gptLoginStatus, bool) {
	m.mu.Lock()
	proc := m.all[id]
	if proc == nil {
		m.mu.Unlock()
		return gptLoginStatus{}, false
	}
	if proc.status.State == "running" && proc.driver != nil && !proc.driver.Alive() {
		proc.status.State = "cancelled"
		proc.status.Message = "Chrome 窗口已关闭"
		proc.status.CompletedAt = time.Now()
		proc.cancel()
	}
	st := proc.status
	m.mu.Unlock()
	return st, true
}

func (m *gptLoginManager) Cancel(id string) {
	var driver *gptDriver
	var bridgeID string
	m.mu.Lock()
	proc := m.all[id]
	if proc != nil && !proc.status.done() {
		proc.status.State = "cancelled"
		proc.status.Message = "已取消"
		proc.status.CompletedAt = time.Now()
		proc.cancel()
		driver = proc.driver
		bridgeID = proc.bridgeID
	}
	m.mu.Unlock()
	if driver != nil {
		driver.Close()
	}
	if bridgeID != "" && m.srv != nil && m.srv.openaiLogins != nil {
		_ = m.srv.openaiLogins.Cancel(bridgeID)
	}
}

func (m *gptLoginManager) Close() {
	m.mu.Lock()
	for _, p := range m.all {
		p.cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
}

// run 是登录状态机主体。
func (m *gptLoginManager) run(ctx context.Context, id, email string) {
	// 1) 生成 Codex 登录 URL(Bridge 只起 1455 回调,不自己开浏览器)。
	m.note(id, "生成 Codex 登录链接…")
	loginStart, err := m.srv.openaiLogins.Start(emailLocalPart(email))
	if err != nil {
		m.set(id, "failed", "", "生成登录链接失败: "+err.Error())
		return
	}
	bridgeID := loginStart.ID
	m.attachBridge(id, bridgeID)
	authURL := loginStart.AuthURL
	if ctx.Err() != nil {
		m.set(id, "cancelled", "已取消", "")
		_ = m.srv.openaiLogins.Cancel(bridgeID)
		return
	}
	if authURL == "" {
		m.set(id, "failed", "", "Bridge 没返回 auth_url")
		return
	}

	// 2) 开隔离 Chrome 打开 auth_url。
	m.note(id, "启动浏览器登录…")
	driver, err := newGPTDriver(ctx, authURL)
	if err != nil {
		m.set(id, "failed", "", "启动浏览器失败: "+err.Error())
		return
	}
	m.attachDriver(id, driver)
	defer driver.Close() // 任务结束删临时 profile

	// 3) 邮箱 + 验证码。
	st, err := m.enterEmailAndCode(ctx, id, driver, email)
	if err != nil {
		m.finishLoginErr(ctx, id, err)
		return
	}
	if reason := gptErrorPageReason(st); reason != "" {
		m.set(id, "failed", "", reason)
		return
	}

	// 4) 若还没到 consent,走手机验证。
	if !strings.Contains(st.URL, "consent") {
		if err := m.phoneFlow(ctx, id, driver); err != nil {
			m.finishLoginErr(ctx, id, err)
			return
		}
	}

	// 5) consent 点 Continue,等回调 success + 轮询登录状态 completed。
	m.note(id, "确认 Codex 授权…")
	m.clickConsent(ctx, driver)
	completed := m.waitLoginCompleted(ctx, bridgeID, 40*time.Second)
	if completed {
		m.set(id, "completed", "登录成功", "")
	} else {
		m.set(id, "failed", "", "未确认到登录完成(Bridge 状态非 completed)")
	}
}

func (m *gptLoginManager) finishLoginErr(ctx context.Context, id string, err error) {
	if ctx.Err() != nil {
		m.set(id, "cancelled", "已取消", "")
		return
	}
	if he, ok := err.(*gptHumanErr); ok {
		m.set(id, "failed", "遇到人机验证,请在窗口里完成或稍后重试", he.Error())
		return
	}
	m.set(id, "failed", "", err.Error())
}

// gptHumanErr 标记撞到人机验证。
type gptHumanErr struct{ msg string }

func (e *gptHumanErr) Error() string { return e.msg }

// enterEmailAndCode 填邮箱、取新鲜码填入,直到到手机页或 consent。
func (m *gptLoginManager) enterEmailAndCode(ctx context.Context, id string, d *gptDriver, email string) (gptState, error) {
	st := d.waitUntil(ctx, func(s gptState) bool {
		return s.URL != "about:blank" && (strings.Contains(s.Title, "OpenAI") || strings.Contains(s.URL, "auth.openai.com"))
	}, 30*time.Second, time.Second)
	if gptIsHuman(st) {
		return st, &gptHumanErr{"邮箱页遇到人机验证"}
	}
	submitAt := time.Now().UTC()
	if !strings.Contains(st.URL, "email-verification") {
		m.note(id, "填写邮箱…")
		d.reactFill(ctx, `[...document.querySelectorAll('input')].find(i=>i.type==='email')||document.querySelector('input')`, email)
		d.clickContinue(ctx)
		submitAt = time.Now().UTC()
		st = d.waitUntil(ctx, func(s gptState) bool {
			return strings.Contains(s.URL, "email-verification") || strings.Contains(s.Text, "检查你的收件箱") || strings.Contains(s.Text, "Check your inbox")
		}, 30*time.Second, time.Second)
	}
	if gptIsHuman(st) {
		return st, &gptHumanErr{"提交邮箱后遇到人机验证"}
	}
	if !strings.Contains(st.URL, "email-verification") && !strings.Contains(st.Text, "检查你的收件箱") && !strings.Contains(st.Text, "Check your inbox") {
		return st, fmt.Errorf("没跳到邮箱验证页: %s", st.URL)
	}
	m.note(id, "等待邮箱验证码…")
	code, err := m.srv.fetchFreshGPTCode(ctx, email, submitAt, 20)
	if err != nil {
		return st, err
	}
	m.note(id, "填入验证码…")
	d.reactFill(ctx, `[...document.querySelectorAll('input')].find(i=>i.name==='code'||/验证码|代码|code/i.test([i.id,i.placeholder,i.getAttribute('aria-label')].join(' ')))`, code)
	d.clickContinue(ctx)
	st = d.waitUntil(ctx, func(s gptState) bool {
		return strings.Contains(s.URL, "add-phone") || strings.Contains(s.URL, "consent") || strings.Contains(s.Text, "电话号码是必填项")
	}, 30*time.Second, time.Second)
	return st, nil
}

// phoneFlow 手机验证:逐国买号试,SMS 成功即填码;买过的号登记后台自动取消。
func (m *gptLoginManager) phoneFlow(ctx context.Context, id string, d *gptDriver) error {
	key := m.srv.herosmsKey(ctx)
	maxPrice := m.srv.herosmsGPTLoginMaxPrice(ctx)
	minCount := m.srv.herosmsGPTLoginMinCount(ctx)
	offers, err := m.srv.herosmsOfferRows(ctx, key, herosmsService, maxPrice, minCount)
	if err != nil {
		return fmt.Errorf("拉 HeroSMS 优惠失败: %w", err)
	}
	names := m.srv.herosmsCountryNames(ctx, key)
	limitedCountries := parseHeroSMSCountryLimit(m.srv.herosmsGPTLoginCountries(ctx), names)
	limitedMode := len(limitedCountries) > 0
	if limitedMode {
		offers = orderHeroSMSOffersByCountryLimit(offers, limitedCountries)
		labels := make([]string, 0, len(limitedCountries))
		for _, cid := range limitedCountries {
			labels = append(labels, fallback(names[cid], cid))
		}
		m.note(id, "按配置限定国家买号: "+strings.Join(labels, ", "))
		if len(offers) == 0 {
			return fmt.Errorf("配置的国家没有可买号码(最高 $%.4f, 库存>%d): %s", maxPrice, minCount, strings.Join(labels, ", "))
		}
	}
	blocked := map[string]bool{}
	// countryAttempts 只统计本次登录任务里“实际拿去填 OpenAI 手机框”的号码数。HeroSMS
	// offers 可能同一国家按多个价格返回多行；不加这层会一行买 3 个、下一价格再买 3 个。
	countryAttempts := map[string]int{}
	bought := 0

	for _, row := range offers {
		if ctx.Err() != nil {
			return context.Canceled
		}
		if !limitedMode && bought >= gptLoginMaxAttempts {
			return fmt.Errorf("试了 %d 个号仍未通过手机验证", gptLoginMaxAttempts)
		}
		cid := row.CountryID
		if blocked[cid] || countryAttempts[cid] >= gptLoginMaxAttemptsPerCountry {
			continue
		}
		chn := names[cid]
		if chn == "" {
			chn = cid
		}
		remain := gptLoginMaxAttempts - bought
		if limitedMode {
			remain = gptLoginBatch
		}
		countryRemain := gptLoginMaxAttemptsPerCountry - countryAttempts[cid]
		batch := gptLoginBatch
		if remain < batch {
			batch = remain
		}
		if countryRemain < batch {
			batch = countryRemain
		}
		if batch <= 0 {
			continue
		}
		m.note(id, fmt.Sprintf("试国家 %s($%.4f，上限 $%.4f，库存>%d),买 %d 个号，本国已试 %d/%d…", chn, row.Price, maxPrice, minCount, batch, countryAttempts[cid], gptLoginMaxAttemptsPerCountry))

		var nums []struct{ actID, phone string }
		for i := 0; i < batch; i++ {
			a, _, berr := m.srv.herosmsBuyNumber(ctx, key, herosmsService, cid, row.Price, "gpt_login")
			if berr != nil {
				continue
			}
			nums = append(nums, struct{ actID, phone string }{a.ID, a.Phone})
			bought++
		}
		if len(nums) == 0 {
			continue
		}

		countryBroken := false
		for idx, n := range nums {
			if ctx.Err() != nil {
				return context.Canceled
			}
			if !m.ensureAddPhone(ctx, d) {
				return errors.New("无法回到 OpenAI 手机号填写页,请重试整个登录")
			}
			m.note(id, fmt.Sprintf("试国家 %s 的第 %d/%d 个号…", chn, idx+1, len(nums)))
			attemptStarted := false
			startAttempt := func() {
				if attemptStarted {
					return
				}
				attemptStarted = true
				m.logHeroSMSAttempt(ctx, n.actID, n.phone, herosmsService, cid, chn, "gpt_login", "used", "已输入手机号框", "")
			}
			logAttempt := func(result, reason, raw string) {
				if attemptStarted {
					m.updateHeroSMSAttempt(ctx, n.actID, n.phone, herosmsService, cid, chn, "gpt_login", result, reason, raw)
					return
				}
				m.logHeroSMSAttempt(ctx, n.actID, n.phone, herosmsService, cid, chn, "gpt_login", result, reason, raw)
			}
			startAttempt()
			countryAttempts[cid]++
			if !m.setCountryPhone(ctx, d, n.phone) {
				m.note(id, "手机号控件未接受号码,换下一个号…")
				logAttempt("input_failed", "手机号控件未接受号码", "")
				continue // 号码填不进去,试下一个
			}
			pre, _ := d.state(ctx)
			if phoneWillSendWhatsapp(pre) {
				m.note(id, "页面提示该国家/号码将通过 WhatsApp 发送验证码,不点继续,直接换国家…")
				logAttempt("whatsapp", "提交前检测到 WhatsApp 发送提示,换国家", truncate(pre.Text, 1000))
				blocked[cid] = true
				countryBroken = true
				break
			}
			st := m.submitPhone(ctx, d)
			body := st.Text
			switch {
			case phoneInvalidAuthStep(body):
				logAttempt("invalid_auth", "OpenAI 授权步骤失效", truncate(body, 1000))
				return errors.New("OpenAI 授权步骤失效,请重试整个登录")
			case phoneTooManyRequests(body):
				// 这个提示经常是当前手机号/号码段触发频控，不一定是账号级封锁；不要直接终止任务。
				m.note(id, "该号码触发手机验证频控,换下一个号…")
				logAttempt("too_many", "该号码触发手机验证频控", truncate(body, 1000))
				m.returnToAddPhone(ctx, d)
			case isWhatsappOnly(st):
				m.note(id, "该国家切到 WhatsApp,换国家…")
				logAttempt("whatsapp", "该国家切到 WhatsApp", truncate(body, 1000))
				blocked[cid] = true
				countryBroken = true
			case isSMSCodeForm(st):
				m.note(id, "已发短信,等验证码…")
				if code := m.pollSMS(ctx, key, n.actID); code != "" {
					_ = m.srv.herosmsFinish(ctx, key, n.actID)
					m.srv.herosmsMarkUsed(n.actID)
					logAttempt("success", "短信验证码已收到并填写", "STATUS_OK:"+code)
					m.note(id, "填手机验证码…")
					m.fillPhoneCode(ctx, d, code)
					return nil
				}
				// 没收到码:回到 add-phone 后试下一个号,避免停在 phone-verification 页导致后续号码全都填不进去。
				m.note(id, "30 秒未收到短信,换下一个号…")
				logAttempt("sms_sent_timeout", "已发短信但 30 秒未收到验证码", truncate(body, 1000))
				m.returnToAddPhone(ctx, d)
			case phoneSMSUnsupported(body, st):
				m.note(id, "该国家无法发 SMS,换国家…")
				logAttempt("sms_unsupported", "该国家无法发 SMS", truncate(body, 1000))
				blocked[cid] = true
				countryBroken = true
			case phoneNumberRejected(body):
				m.note(id, "号码被拒绝/已使用,换下一个号…")
				logAttempt("number_rejected", "号码被拒绝/已使用", truncate(body, 1000))
				m.returnToAddPhone(ctx, d)
			default:
				// 未知结果也尽量回到 add-phone,否则后续 retry 会在错误页面/验证码页上空转。
				m.note(id, "手机号提交结果未知,换下一个号…")
				logAttempt("unknown", "手机号提交结果未知", truncate(body, 1000))
				m.returnToAddPhone(ctx, d)
			}
			if countryBroken {
				m.returnToAddPhone(ctx, d)
				break
			}
			if countryAttempts[cid] >= gptLoginMaxAttemptsPerCountry {
				m.note(id, fmt.Sprintf("国家 %s 已试满 %d 个号，换国家…", chn, gptLoginMaxAttemptsPerCountry))
				break
			}
		}
	}
	if limitedMode {
		return errors.New("配置的 HeroSMS 国家都已试完,仍未通过手机验证")
	}
	return errors.New("HeroSMS 没有可用国家/号码了")
}

func parseHeroSMSCountryLimit(raw string, names map[string]string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cid := resolveHeroSMSCountryID(part, names)
		key := strings.ToLower(cid)
		if cid == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cid)
	}
	return out
}

func resolveHeroSMSCountryID(v string, names map[string]string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, ok := names[v]; ok {
		return v
	}
	lower := strings.ToLower(v)
	for cid, name := range names {
		if strings.ToLower(strings.TrimSpace(cid)) == lower || strings.ToLower(strings.TrimSpace(name)) == lower {
			return cid
		}
	}
	// HeroSMS 国家 ID 常是数字;names 拉取失败时也允许用户直接填 ID。
	return v
}

// orderHeroSMSOffersByCountryLimit 只保留限定国家的行，并保持 herosmsOfferRows 给的
// 全局价格升序(价格同则库存多的在前)。
//
// 早期实现是按配置里的国家顺序分组拼接的，限定多个国家时会变成「第一个国家的贵档排在
// 第二个国家的便宜档前面」，跟“先买便宜的”相悖；这里改成原地过滤，天然沿用入参顺序。
// 价格/库存是否达标已经在 herosmsOfferRows 里按 maxPrice/minCount 过滤过，这里不再重复判。
func orderHeroSMSOffersByCountryLimit(offers []herosmsOfferRow, countries []string) []herosmsOfferRow {
	want := make(map[string]bool, len(countries))
	for _, cid := range countries {
		want[cid] = true
	}
	out := make([]herosmsOfferRow, 0, len(offers))
	for _, row := range offers {
		if want[row.CountryID] {
			out = append(out, row)
		}
	}
	return out
}

func (m *gptLoginManager) logHeroSMSAttempt(ctx context.Context, actID, phone, service, countryID, countryName, source, result, reason, raw string) {
	if m == nil || m.srv == nil || m.srv.store == nil {
		return
	}
	_ = m.srv.store.InsertHeroSMSAttemptLog(ctx, HeroSMSAttemptLog{
		ActivationID: actID,
		Phone:        phone,
		Service:      service,
		CountryID:    countryID,
		CountryName:  countryName,
		Source:       source,
		Result:       result,
		Reason:       reason,
		Raw:          raw,
	})
}

func (m *gptLoginManager) updateHeroSMSAttempt(ctx context.Context, actID, phone, service, countryID, countryName, source, result, reason, raw string) {
	if m == nil || m.srv == nil || m.srv.store == nil {
		return
	}
	ok, err := m.srv.store.UpdateLatestHeroSMSAttemptLog(ctx, actID, result, reason, raw)
	if err == nil && ok {
		return
	}
	m.logHeroSMSAttempt(ctx, actID, phone, service, countryID, countryName, source, result, reason, raw)
}

// setCountryPhone 输完整 E.164 让 OpenAI 手机控件自己解析国家,并强制 SMS。移植 set_country_phone。
func (m *gptLoginManager) setCountryPhone(ctx context.Context, d *gptDriver, phone string) bool {
	full := phone
	if !strings.HasPrefix(full, "+") {
		full = "+" + full
	}
	// 鼠标点击手机号输入框 + 键盘输入完整 E.164，不再 DOM set value。
	// CDP 逐字符输入在 OpenAI 手机控件自动格式化时偶尔会漏/错末位，所以每次输入后必须 DOM
	// 校验真实 digits；不一致就清空重输，绝不带错号去点「继续」。
	if m.tryPhoneFill(ctx, d, full, full, 3) {
		return true
	}
	// 有些国家控件识别 country code 后会吞掉末位；此时改为输入去掉国家码后的 national number。
	if national := nationalNumberForVisibleCountry(ctx, d, full); national != "" {
		return m.tryPhoneFill(ctx, d, national, full, 2)
	}
	return false
}

func (m *gptLoginManager) tryPhoneFill(ctx context.Context, d *gptDriver, input, full string, attempts int) bool {
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if !d.reactFill(ctx, `document.querySelector('input[type=tel], #tel')`, input) {
			return false
		}
		sleepCtx(ctx, 1700*time.Millisecond)
		// 交付方式如果有 SMS radio，用鼠标点；没有就让后续 submit 后按页面状态分类。
		_ = d.mouseClickFind(ctx, `[...document.querySelectorAll('input[type=radio]')].find(i=>String(i.value).toLowerCase()==='sms'&&!i.checked)`)
		sleepCtx(ctx, 650*time.Millisecond)
		if m.phoneInputComplete(ctx, d, full) {
			return true
		}
		// 让 React/phone widget 稳定一下再重输，避免刚格式化时继续追加/漏字。
		sleepCtx(ctx, 450*time.Millisecond)
	}
	return false
}

type phoneInputView struct {
	Country string `json:"country"`
	Phone   string `json:"phone"`
	Tel     string `json:"tel"`
}

func (m *gptLoginManager) phoneInputComplete(ctx context.Context, d *gptDriver, full string) bool {
	var v phoneInputView
	_ = d.chrome.Evaluate(ctx, `(()=>({country:[...document.querySelectorAll('button')].map(b=>b.innerText||'').find(t=>t.includes('(+'))||'',phone:document.querySelector('input[name=phoneNumber]')?.value||'',tel:document.querySelector('input[type=tel], #tel')?.value||''}))()`, &v)
	want := digitsOnly(full)
	national := digitsOnly(nationalNumberFromCountry(v.Country, full))
	// hidden phoneNumber 与可见 tel 分开判断。hidden 有时滞后保留旧值，不能因为它非空
	// 就忽略可见输入框；但任一字段出现 old+new 追加也不会等于 want/national，因此不会误过。
	for _, got := range []string{digitsOnly(v.Phone), digitsOnly(v.Tel)} {
		if want != "" && got == want {
			return true
		}
		if national != "" && got == national {
			return true
		}
	}
	return false
}

func nationalNumberForVisibleCountry(ctx context.Context, d *gptDriver, full string) string {
	var v phoneInputView
	_ = d.chrome.Evaluate(ctx, `(()=>({country:[...document.querySelectorAll('button')].map(b=>b.innerText||'').find(t=>t.includes('(+'))||''}))()`, &v)
	return nationalNumberFromCountry(v.Country, full)
}

func nationalNumberFromCountry(country, full string) string {
	want := digitsOnly(full)
	start := strings.Index(country, "(+")
	if start < 0 {
		return ""
	}
	start += 2
	end := start
	for end < len(country) && country[end] >= '0' && country[end] <= '9' {
		end++
	}
	dial := country[start:end]
	if dial == "" || !strings.HasPrefix(want, dial) {
		return ""
	}
	return want[len(dial):]
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// submitPhone 点 Continue 提交手机号,必要时补一次(部分构建首次只做校验)。移植 submit_phone。
func (m *gptLoginManager) submitPhone(ctx context.Context, d *gptDriver) gptState {
	d.clickContinue(ctx)
	sleepCtx(ctx, 8*time.Second)
	st, _ := d.state(ctx)
	if strings.Contains(st.URL, "add-phone") && !st.HasCodeInput {
		bad := phoneTooManyRequests(st.Text) || phoneNumberRejected(st.Text) || phoneSMSUnsupported(st.Text, st)
		if !bad && (st.Channel == "" || st.Channel == "sms" || st.SMSSelected || !st.WhatsappHint) {
			d.clickContinue(ctx)
			sleepCtx(ctx, 8*time.Second)
			st, _ = d.state(ctx)
		}
	}
	return st
}

// pollSMS 每 2s 查一次状态,最多 30s;拿到 STATUS_OK:<code> 即返回。移植 poll_sms。
func (m *gptLoginManager) pollSMS(ctx context.Context, key, actID string) string {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, code, _ := m.srv.herosmsCheckStatus(ctx, key, actID); code != "" {
			return code
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return ""
		}
	}
	return ""
}

// fillPhoneCode 填手机验证码并点 Continue,等到 consent 或 success。移植 fill_phone_code。
func (m *gptLoginManager) fillPhoneCode(ctx context.Context, d *gptDriver, code string) {
	d.reactFill(ctx, `[...document.querySelectorAll('input')].find(i=>i.name==='code'||/验证码|代码|code/i.test([i.id,i.placeholder,i.getAttribute('aria-label')].join(' ')))`, code)
	d.clickContinue(ctx)
	d.waitUntil(ctx, func(s gptState) bool {
		return strings.Contains(s.URL, "localhost:1455/success") ||
			((strings.Contains(s.URL, "consent") || strings.Contains(s.Text, "使用 ChatGPT 登录到 Codex")) && hasEnabledContinue(s))
	}, 30*time.Second, 400*time.Millisecond)
}

// returnToAddPhone 回到 add-phone 页(换国家前)。移植 return_to_add_phone。
func (m *gptLoginManager) returnToAddPhone(ctx context.Context, d *gptDriver) {
	st, _ := d.state(ctx)
	if strings.Contains(st.URL, "add-phone") {
		return
	}
	d.evalBool(ctx, `(()=>{history.back();return true;})()`)
	d.waitUntil(ctx, func(s gptState) bool { return strings.Contains(s.URL, "add-phone") }, 15*time.Second, 500*time.Millisecond)
}

// ensureAddPhone 保证下一次号码尝试从 add-phone 表单页开始。短信超时/WhatsApp 页如果不先退回，
// 后续 setCountryPhone 会找不到 tel input，然后整批预买号码和后续国家都会空转。
func (m *gptLoginManager) ensureAddPhone(ctx context.Context, d *gptDriver) bool {
	st, _ := d.state(ctx)
	if strings.Contains(st.URL, "add-phone") {
		return true
	}
	m.returnToAddPhone(ctx, d)
	st, _ = d.state(ctx)
	return strings.Contains(st.URL, "add-phone")
}

func phoneInvalidAuthStep(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "invalid_auth_step") || strings.Contains(text, "授权步骤无效")
}

func phoneTooManyRequests(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "请求手机验证的次数过多") ||
		strings.Contains(text, "too many phone verification") ||
		strings.Contains(text, "too many attempts") ||
		strings.Contains(text, "try again later")
}

// phoneWillSendWhatsapp 在点击继续前判断当前号码是否已经被页面判定为 WhatsApp 发送。
// 这种页面还在 add-phone，但说明点继续会发 WhatsApp 而不是 SMS，所以直接换号。
func phoneWillSendWhatsapp(st gptState) bool {
	text := strings.ToLower(st.Text)
	if strings.EqualFold(st.Channel, "whatsapp") {
		return true
	}
	if st.SMSSelected || strings.EqualFold(st.Channel, "sms") {
		return false
	}
	if !strings.Contains(text, "whatsapp") {
		return false
	}
	patterns := []string{
		"通过 whatsapp",
		"via whatsapp",
		"using whatsapp",
		"send a one-time code",
		"send you a code",
		"send an otp",
		"向该号码发送一次性验证码",
		"向此号码发送一次性验证码",
		"发送一次性验证码",
	}
	for _, pat := range patterns {
		if strings.Contains(text, pat) {
			return true
		}
	}
	return false
}

func phoneSMSUnsupported(text string, st gptState) bool {
	text = strings.ToLower(text)
	return strings.EqualFold(st.Channel, "whatsapp") ||
		strings.Contains(text, "无法向该电话号码发送短信") ||
		strings.Contains(text, "无法发送短信") ||
		strings.Contains(text, "can't send sms") ||
		strings.Contains(text, "cannot send sms") ||
		strings.Contains(text, "couldn't send sms") ||
		strings.Contains(text, "unable to send sms")
}

func phoneNumberRejected(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "无法向此电话号码发送验证码") ||
		strings.Contains(text, "该电话号码已被使用") ||
		strings.Contains(text, "电话号码无效") ||
		strings.Contains(text, "不是手机号码") ||
		strings.Contains(text, "can't send a code to this phone number") ||
		strings.Contains(text, "cannot send a code to this phone number") ||
		strings.Contains(text, "couldn't send a code") ||
		strings.Contains(text, "unable to send a code") ||
		strings.Contains(text, "phone number has already") ||
		strings.Contains(text, "already been used") ||
		strings.Contains(text, "already associated") ||
		strings.Contains(text, "invalid phone") ||
		strings.Contains(text, "not a mobile")
}

// clickConsent 若在 consent 页则点 Continue。
func (m *gptLoginManager) clickConsent(ctx context.Context, d *gptDriver) {
	st := d.waitUntil(ctx, func(s gptState) bool {
		return strings.Contains(s.URL, "localhost:1455/success") ||
			((strings.Contains(s.URL, "consent") || strings.Contains(s.Text, "使用 ChatGPT 登录到 Codex")) && hasEnabledContinue(s))
	}, 30*time.Second, 400*time.Millisecond)
	if hasEnabledContinue(st) && !strings.Contains(st.URL, "localhost:1455/success") {
		d.clickContinue(ctx)
		d.waitUntil(ctx, func(s gptState) bool { return strings.Contains(s.URL, "localhost:1455/success") }, 30*time.Second, 400*time.Millisecond)
	}
}

// waitLoginCompleted 轮询 Bridge 登录状态直到 completed 或超时。
func (m *gptLoginManager) waitLoginCompleted(ctx context.Context, bridgeID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, ok := m.srv.openaiLogins.Status(bridgeID); ok && st.State == "completed" {
			return true
		}
		if !sleepCtx(ctx, time.Second) {
			return false
		}
	}
	return false
}

// ---- 手机页交付方式判定(移植 is_whatsapp_only_delivery / is_sms_code_form) ----

func isWhatsappOnly(st gptState) bool {
	return strings.Contains(st.URL, "phone-verification") && st.HasCodeInput &&
		(strings.EqualFold(st.Channel, "whatsapp") || st.WhatsappHint)
}

func isSMSCodeForm(st gptState) bool {
	return strings.Contains(st.URL, "phone-verification") && st.HasCodeInput && !isWhatsappOnly(st) &&
		(strings.EqualFold(st.Channel, "sms") || st.SMSSelected || !st.WhatsappHint)
}

func hasEnabledContinue(st gptState) bool {
	for _, b := range st.Buttons {
		if !b.Disabled && (strings.Contains(b.Text, "继续") || strings.Contains(strings.ToLower(b.Text), "continue")) {
			return true
		}
	}
	return false
}

func emailLocalPart(email string) string { return strings.SplitN(email, "@", 2)[0] }

// ---- HTTP handlers ----

func (m *gptLoginManager) handleStart(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	acc, err := m.srv.store.GetOutlookAccountByID(r.Context(), id)
	if err != nil {
		http.Error(w, "账号不存在", http.StatusNotFound)
		return
	}
	st, err := m.Start(acc.Email)
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

func (m *gptLoginManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := m.Status(chi.URLParam(r, "id"))
	if !ok {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, st, nil)
}

func (m *gptLoginManager) handleCancel(w http.ResponseWriter, r *http.Request) {
	m.Cancel(chi.URLParam(r, "id"))
	writeJSON(w, map[string]any{"ok": true}, nil)
}
