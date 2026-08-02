package main

import "testing"

// 这些用例钉住 skill 里那几条「踩过坑」的判定规则,防止后续改动把顺序或判据搞反。
func TestClassifyOutlookLoginState(t *testing.T) {
	emailInput := domMeta{Tag: "input", Type: "email", Name: "loginfmt"}
	passwordInput := domMeta{Tag: "input", Type: "password", Name: "passwd"}
	nextButton := domMeta{Tag: "button", Text: "Next"}

	cases := []struct {
		name string
		snap domSnapshot
		want string
	}{
		{
			name: "邮箱页",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs: []domMeta{emailInput}, Buttons: []domMeta{nextButton}, BodyText: "Sign in"},
			want: "email",
		},
		{
			name: "密码页",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs: []domMeta{passwordInput}, Buttons: []domMeta{nextButton}, BodyText: "Enter password"},
			want: "password",
		},
		{
			name: "保持登录状态",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Buttons:  []domMeta{{Tag: "button", Text: "Yes"}, {Tag: "button", Text: "No"}},
				BodyText: "Stay signed in? Reduce the number of times you are asked to sign in."},
			want: "stay_signed_in",
		},
		{
			// cookie 横幅那句 "improve your experience" 里含 "prove you",
			// 用宽泛子串判定会把正常页面误判成人机验证。
			name: "cookie 横幅不能误判成验证码",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs:   []domMeta{emailInput},
				Buttons:  []domMeta{nextButton},
				BodyText: "We use cookies to improve your experience on our site."},
			want: "email",
		},
		{
			name: "真正的人机验证",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Buttons: []domMeta{{Tag: "button", Text: "Verify"}}, BodyText: "Press and hold to confirm you are human"},
			want: "captcha",
		},
		{
			// 「Verify your email」页同时含验证类词和 Use your password,
			// password_choice 必须排在 captcha / mfa 前面才不会被抢判。
			name: "选择验证方式页优先于验证类判定",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Buttons:  []domMeta{{Tag: "button", Text: "Use your password"}},
				BodyText: "Verify your identity. We'll send a security code. Use your password instead."},
			want: "password_choice",
		},
		{
			name: "两步验证",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Buttons: []domMeta{nextButton}, BodyText: "Help us protect your account. Enter code from the authenticator app."},
			want: "mfa_or_protection",
		},
		{
			name: "密码错误",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs: []domMeta{passwordInput}, Buttons: []domMeta{nextButton},
				BodyText: "Your account or password is incorrect."},
			want: "problem",
		},
		{
			// 账号不存在时邮箱输入框还在,不认这句报错就会被判回 email、
			// 状态机原地重试到任务超时。
			name: "账号不存在",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs: []domMeta{emailInput}, Buttons: []domMeta{nextButton},
				BodyText: "Sign in\nWe couldn't find a Microsoft account. Try entering your details again, or create an account."},
			want: "problem",
		},
		{
			// bodyText 被截断时,报错块单独参与判定。
			name: "报错只出现在 alert 块里",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete",
				Inputs:   []domMeta{emailInput},
				Buttons:  []domMeta{nextButton},
				Errors:   []domMeta{{Tag: "div", ID: "usernameError", Text: "We couldn't find a Microsoft account."}},
				BodyText: "Sign in"},
			want: "problem",
		},
		{
			name: "进入信箱即成功",
			snap: domSnapshot{Href: "https://outlook.live.com/mail/0/", Ready: "complete",
				Buttons: []domMeta{{Tag: "button", Text: "New mail"}}, BodyText: "Focused Other"},
			want: "success",
		},
		{
			// 已登录的账户主页算成功;但同一个域上如果还摆着登录表单,就不能算。
			name: "账户主页无登录表单算成功",
			snap: domSnapshot{Href: "https://account.microsoft.com/", Ready: "complete",
				Buttons: []domMeta{{Tag: "button", Text: "Your info"}}, BodyText: "Welcome"},
			want: "success",
		},
		{
			name: "账户主页仍有登录表单不算成功",
			snap: domSnapshot{Href: "https://account.microsoft.com/", Ready: "complete",
				Inputs: []domMeta{emailInput}, Buttons: []domMeta{nextButton}, BodyText: "Sign in"},
			want: "email",
		},
		{
			name: "还没渲染完",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "loading"},
			want: "loading",
		},
		{
			// 跳转过程中的中间文档:readyState 已经 complete,但 DOM 里什么都没有。
			// 实测微软登录页在表单渲染前就是这个样子,不能因此判成 unknown。
			name: "readyState 已完成但文档是空的",
			snap: domSnapshot{Href: "https://login.live.com/", Ready: "complete", Title: ""},
			want: "loading",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOutlookLoginState(tc.snap); got != tc.want {
				t.Fatalf("classifyOutlookLoginState() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOutlookProblemReason(t *testing.T) {
	cases := map[string]string{
		"Your account or password is incorrect.":            "账号或密码不正确",
		"That Microsoft account doesn't exist.":             "账号不存在",
		"We couldn't find a Microsoft account.":             "账号不存在（微软找不到这个账号）",
		"Your account has been locked.":                     "账号已被锁定",
		"You've tried to sign in too many times.":           "尝试次数过多",
		"We ran into a problem. Please try again later.":    "微软登录页报错",
		"Some unrelated page text without a known keyword.": "未知原因",
	}
	for body, want := range cases {
		if got := outlookProblemReason(domSnapshot{BodyText: body}); got != want {
			t.Fatalf("outlookProblemReason(%q) = %q, want %q", body, got, want)
		}
	}

	// 关键词都不认识时,把页面原话带回去,比一句「未知原因」有用。
	snap := domSnapshot{
		BodyText: "Sign in",
		Errors:   []domMeta{{Tag: "div", Text: "Sign-in is blocked by your organization policy."}},
	}
	if got := outlookProblemReason(snap); got != "Sign-in is blocked by your organization policy." {
		t.Fatalf("outlookProblemReason() = %q, want 页面原话", got)
	}
}
