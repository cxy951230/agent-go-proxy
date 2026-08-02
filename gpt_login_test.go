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
