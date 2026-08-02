package main

import (
	"strings"
	"testing"
)

func TestDeriveSignupName(t *testing.T) {
	cases := map[string]string{
		"nathanbradley19891118@outlook.com": "Nathanbradley",
		"jay.chou@outlook.com":              "Jay Chou",
		"cameron_henderson@outlook.com":     "Cameron Henderson",
		"ab@outlook.com":                    "ChatGPT User", // 单段且 <=5 字符
	}
	for email, want := range cases {
		if got := deriveSignupName(email); got != want {
			t.Errorf("deriveSignupName(%q)=%q want %q", email, got, want)
		}
	}
}

func TestSignupLoginEmailReady(t *testing.T) {
	st := gptState{
		URL:   "https://chatgpt.com/auth/login",
		Ready: "complete",
		Text:  "登录或注册",
		Inputs: []gptInput{{
			Type:        "text",
			Placeholder: "Email address",
		}},
	}
	if !signupLoginEmailReady(st) {
		t.Error("中文 ChatGPT 登录页上的 Email 输入框应算 ready")
	}
	st.Ready = ""
	if signupLoginEmailReady(st) {
		t.Error("readyState 缺失时不应算 ready，避免页面水合前过早输入")
	}
}

func TestGptStateJSIncludesReady(t *testing.T) {
	if !strings.Contains(gptStateJS, "ready:document.readyState") {
		t.Error("gptStateJS 必须返回 document.readyState，否则注册状态机会一直等待不输入邮箱")
	}
}

func TestSignupStateClassifiers(t *testing.T) {
	// email-verification 必须真的有验证码输入框才算 ready。
	if signupEmailVerifyReady(gptState{URL: "https://auth.openai.com/email-verification", HasCodeInput: false}) {
		t.Error("没有 code 输入框不应算 ready")
	}
	if !signupEmailVerifyReady(gptState{URL: "https://auth.openai.com/email-verification", HasCodeInput: true}) {
		t.Error("有 code 输入框应算 ready")
	}
	if !signupDoneText("Welcome, Nathan") || !signupDoneText("New chat") {
		t.Error("完成文案应命中")
	}
	if signupDoneText("Log in or sign up") {
		t.Error("登录页文案不应算完成")
	}
}

func TestGptIsHuman(t *testing.T) {
	if !gptIsHuman(gptState{Text: "Please complete the CAPTCHA to continue"}) {
		t.Error("captcha 文案应命中人机验证")
	}
	if !gptIsHuman(gptState{Suspicious: []string{"IFRAME arkose-frame challenge"}}) {
		t.Error("arkose iframe 应命中")
	}
	if gptIsHuman(gptState{Text: "Enter your email to continue"}) {
		t.Error("普通登录页不应误判为人机验证")
	}
}

func TestJsStr(t *testing.T) {
	if got := jsStr(`a"b`); got != `"a\"b"` {
		t.Errorf("jsStr 转义错误: %s", got)
	}
}
