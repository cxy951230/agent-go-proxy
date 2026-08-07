package main

import "testing"

func TestPhoneDeliveryClassifiers(t *testing.T) {
	sms := gptState{URL: "https://auth.openai.com/phone-verification", HasCodeInput: true, Channel: "sms", SMSSelected: true}
	if !isSMSCodeForm(sms) || isWhatsappOnly(sms) {
		t.Error("SMS 交付应判为 SMS、非 WhatsApp")
	}
	wa := gptState{URL: "https://auth.openai.com/phone-verification", HasCodeInput: true, Channel: "whatsapp", WhatsappHint: true}
	if !isWhatsappOnly(wa) || isSMSCodeForm(wa) {
		t.Error("WhatsApp 交付应判为 WhatsApp-only")
	}
	// 无 code 输入框(还在 add-phone)不是任何交付页。
	addPhone := gptState{URL: "https://auth.openai.com/add-phone", HasCodeInput: false}
	if isSMSCodeForm(addPhone) || isWhatsappOnly(addPhone) {
		t.Error("add-phone 页不应判为交付页")
	}
	// 无明确 channel、无 whatsapp 提示时,默认按 SMS 处理(与 skill 一致)。
	ambiguous := gptState{URL: "https://auth.openai.com/phone-verification", HasCodeInput: true}
	if !isSMSCodeForm(ambiguous) {
		t.Error("无 whatsapp 提示时应默认 SMS")
	}
}

func TestHasEnabledContinue(t *testing.T) {
	if !hasEnabledContinue(gptState{Buttons: []gptButton{{Text: "继续", Disabled: false}}}) {
		t.Error("启用的「继续」应命中")
	}
	if !hasEnabledContinue(gptState{Buttons: []gptButton{{Text: "Continue", Disabled: false}}}) {
		t.Error("启用的 Continue 应命中")
	}
	if hasEnabledContinue(gptState{Buttons: []gptButton{{Text: "继续", Disabled: true}}}) {
		t.Error("禁用的按钮不应命中")
	}
}

func TestEmailLocalPart(t *testing.T) {
	if emailLocalPart("cameronmoore9485820vk3u@outlook.com") != "cameronmoore9485820vk3u" {
		t.Error("邮箱前缀解析错误")
	}
}

func TestPhoneErrorClassifiers(t *testing.T) {
	if !phoneNumberRejected("无法向此电话号码发送验证码") {
		t.Error("中文无法发验证码应判为号码拒绝")
	}
	if !phoneNumberRejected("We couldn't send a code to this phone number") {
		t.Error("英文无法发验证码应判为号码拒绝")
	}
	if !phoneSMSUnsupported("无法向该电话号码发送短信", gptState{}) {
		t.Error("中文 SMS 失败应判为国家 SMS 不可用")
	}
	if !phoneSMSUnsupported("OpenAI cannot send SMS to this number", gptState{}) {
		t.Error("英文 SMS 失败应判为国家 SMS 不可用")
	}
	if !phoneTooManyRequests("Too many attempts, try again later") {
		t.Error("英文频率限制应命中")
	}
	if !phoneInvalidAuthStep("invalid_auth_step") {
		t.Error("invalid_auth_step 应命中")
	}
}

func TestOrderHeroSMSOffersByCountryLimit(t *testing.T) {
	// 入参按 herosmsOfferRows 的输出约定:全局价格升序。
	offers := []herosmsOfferRow{
		{CountryID: "73", Price: 0.0495, Count: 801},
		{CountryID: "66", Price: 0.0550, Count: 4898},
		{CountryID: "73", Price: 0.0618, Count: 915},
		{CountryID: "13", Price: 0.0825, Count: 643},
		{CountryID: "73", Price: 0.1690, Count: 56830},
	}

	// 单国家:该国家所有价格档都保留,便宜的在前。
	got := orderHeroSMSOffersByCountryLimit(offers, []string{"73"})
	if len(got) != 3 {
		t.Fatalf("限定巴西应留 3 行,得到 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Price > got[i].Price {
			t.Errorf("第 %d 行价格 %.4f 排在 %.4f 前面,没按便宜优先", i, got[i-1].Price, got[i].Price)
		}
	}

	// 多国家:必须跨国家按价格升序,不能按配置里的国家顺序分组。
	// 这里故意把 66 写在 73 前面,期望仍是 73(0.0495) 先于 66(0.0550)。
	got = orderHeroSMSOffersByCountryLimit(offers, []string{"66", "73"})
	want := []float64{0.0495, 0.0550, 0.0618, 0.1690}
	if len(got) != len(want) {
		t.Fatalf("限定巴西+巴基斯坦应留 %d 行,得到 %d", len(want), len(got))
	}
	for i, p := range want {
		if got[i].Price != p {
			t.Errorf("第 %d 行价格应为 %.4f,实际 %.4f(国家 %s)", i, p, got[i].Price, got[i].CountryID)
		}
	}

	// 未限定的国家一行都不能漏进来。
	for _, row := range got {
		if row.CountryID == "13" {
			t.Error("以色列没被限定,不该出现在结果里")
		}
	}

	// 限定了一个当前没有可买行的国家 -> 空结果(调用方据此报错结束)。
	if got := orderHeroSMSOffersByCountryLimit(offers, []string{"999"}); len(got) != 0 {
		t.Errorf("限定无报价国家应返回空,得到 %d 行", len(got))
	}
}
