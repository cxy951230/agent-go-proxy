package main

import (
	"net/http"
	"testing"
	"time"
)

func TestAccountFailureCooldown(t *testing.T) {
	resp := func(status string, retryAfter string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		switch status {
		case "429":
			r.StatusCode = http.StatusTooManyRequests
		case "500":
			r.StatusCode = http.StatusInternalServerError
		}
		if retryAfter != "" {
			r.Header.Set("Retry-After", retryAfter)
		}
		return r
	}

	cases := []struct {
		name string
		resp *http.Response
		want time.Duration
	}{
		{"429 带 Retry-After(实测最常见的 60 秒)", resp("429", "60"), 60 * time.Second},
		{"429 的 Retry-After 太小则抬到下限", resp("429", "5"), accountRateLimitCooldownMin},
		{"429 的 Retry-After 离谱则压到上限", resp("429", "999999"), accountRateLimitCooldownMax},
		{"429 没给 Retry-After 用下限兜底", resp("429", ""), accountRateLimitCooldownMin},
		{"429 的 Retry-After 是垃圾值也不崩", resp("429", "soon"), accountRateLimitCooldownMin},
		{"其它失败沿用默认冷却", resp("500", ""), accountCooldown},
		{"没有响应(传输错误)沿用默认", nil, accountCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountFailureCooldown(tc.resp); got != tc.want {
				t.Fatalf("accountFailureCooldown() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 限流的账号要排到候选末尾,但不能被彻底剔除——全部账号都在冷却时仍要给一次机会。
func TestMarkFailureCooldownOrdersAccountLast(t *testing.T) {
	pool := newAccountPool()
	candidates := []OpenAIAccount{{ID: 1}, {ID: 2}, {ID: 3}}

	pool.MarkFailure("sess", "gpt-5.6-terra", 1, 10*time.Minute)
	ordered := pool.OrderedAccounts("sess", "gpt-5.6-terra", candidates)
	if len(ordered) != 3 {
		t.Fatalf("候选数 = %d, want 3(冷却账号殿后但不剔除)", len(ordered))
	}
	if ordered[len(ordered)-1].ID != 1 {
		t.Fatalf("冷却中的账号应排最后, 实际顺序 = %v", accountIDs(ordered))
	}

	// 成功一次要清掉冷却并绑定粘性,下次该账号排最前。
	pool.MarkSuccess("sess", "gpt-5.6-terra", 1)
	ordered = pool.OrderedAccounts("sess", "gpt-5.6-terra", candidates)
	if ordered[0].ID != 1 {
		t.Fatalf("成功后应恢复并粘住该账号, 实际顺序 = %v", accountIDs(ordered))
	}
}

// 新会话没有粘性、在途数又都是 0,靠「最久未使用优先」轮转,而不是随机。
func TestOrderedAccountsPrefersLeastRecentlyUsed(t *testing.T) {
	pool := newAccountPool()
	candidates := []OpenAIAccount{{ID: 1}, {ID: 2}, {ID: 3}}

	// 谁都没用过时,顺序随机;连续取三次,每次都把选中的那个「用掉」并立刻释放,
	// 模拟三个先后发起、互不重叠的新会话。三次应该正好覆盖三个账号。
	picked := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		first := pool.OrderedAccounts("sess-"+string(rune('a'+i)), "m", candidates)[0]
		pool.Acquire(first.ID)
		pool.Release(first.ID)
		picked = append(picked, first.ID)
	}
	seen := map[int64]bool{}
	for _, id := range picked {
		if seen[id] {
			t.Fatalf("三个新会话应轮到三个不同账号，实际 = %v", picked)
		}
		seen[id] = true
	}

	// 再来一次,应该轮回最早用过的那个。
	if next := pool.OrderedAccounts("sess-d", "m", candidates)[0].ID; next != picked[0] {
		t.Fatalf("第四个会话应轮回最久未使用的 %d，实际 = %d", picked[0], next)
	}
}

// 粘性优先级高于最久未使用:同一会话要粘住,否则上下文缓存就白费了。
func TestStickyBeatsLeastRecentlyUsed(t *testing.T) {
	pool := newAccountPool()
	candidates := []OpenAIAccount{{ID: 1}, {ID: 2}, {ID: 3}}

	pool.Acquire(1) // 1 刚用过,按 LRU 应该排最后
	pool.Release(1)
	pool.MarkSuccess("sess", "m", 1)

	if got := pool.OrderedAccounts("sess", "m", candidates)[0].ID; got != 1 {
		t.Fatalf("粘性账号应排最前，实际 = %d", got)
	}
	// 换个会话就不该粘 1 了,轮到没用过的。
	if got := pool.OrderedAccounts("other", "m", candidates)[0].ID; got == 1 {
		t.Fatalf("其它会话不应命中 1 的粘性绑定")
	}
}

func TestQuotaExhausted(t *testing.T) {
	window := func(primary, secondary string) string {
		body := `{"usage":{"rate_limit":{`
		if primary != "" {
			body += `"primary_window":{"used_percent":` + primary + `}`
		}
		if secondary != "" {
			if primary != "" {
				body += ","
			}
			body += `"secondary_window":{"used_percent":` + secondary + `}`
		}
		return body + `}}}`
	}
	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{"月窗口用满", window("100", ""), true},
		{"月窗口 99% 还能用", window("99.5", ""), false},
		{"次窗口用满也算(plus 的周窗口)", window("12", "100"), true},
		{"两个窗口都没满", window("12", "34"), false},
		{"超过 100 也算满", window("103", ""), true},
		{"没有额度数据不当成用满", "null", false},
		{"空字符串不当成用满", "", false},
		{"结构不对也不当成用满", `{"usage":{}}`, false},
		{"坏 JSON 不崩", `{oops`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaExhausted(tc.status); got != tc.want {
				t.Fatalf("quotaExhausted(%s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}

	// quotaMaxUsedPercent 取各窗口最大值,并区分「有数据」与「未知」(quotaExhausted 基于它)。
	pctCases := []struct {
		name    string
		status  string
		wantPct float64
		wantOK  bool
	}{
		{"取两窗口最大值", window("12", "93"), 93, true},
		{"100% 边界", window("100", ""), 100, true},
		{"99% 未满", window("99", ""), 99, true},
		{"无数据 ok=false", "null", 0, false},
		{"结构不对 ok=false", `{"usage":{}}`, 0, false},
	}
	for _, tc := range pctCases {
		t.Run(tc.name, func(t *testing.T) {
			pct, ok := quotaMaxUsedPercent(tc.status)
			if ok != tc.wantOK || pct != tc.wantPct {
				t.Fatalf("quotaMaxUsedPercent(%s) = %v,%v, want %v,%v", tc.status, pct, ok, tc.wantPct, tc.wantOK)
			}
		})
	}
}

// 额度用满的账号要沉到最底——比冷却中的还靠后,但仍保留兜底机会。
func TestOrderedAccountsSinksQuotaExhausted(t *testing.T) {
	pool := newAccountPool()
	candidates := []OpenAIAccount{
		{ID: 1, QuotaExhausted: true},
		{ID: 2},
		{ID: 3},
	}
	pool.MarkFailure("sess", "m", 2, time.Minute) // 2 在冷却中

	ordered := accountIDs(pool.OrderedAccounts("sess", "m", candidates))
	if len(ordered) != 3 {
		t.Fatalf("候选数 = %d, want 3(用满的沉底但不剔除)", len(ordered))
	}
	if ordered[0] != 3 {
		t.Fatalf("健康账号应排最前, 实际 = %v", ordered)
	}
	if ordered[1] != 2 || ordered[2] != 1 {
		t.Fatalf("应为 健康 → 冷却 → 额度用满, 实际 = %v", ordered)
	}
}

// 额度用满 + 粘性:粘性不能把用满的账号拉回最前,否则会一直撞 429。
func TestQuotaExhaustedBeatsSticky(t *testing.T) {
	pool := newAccountPool()
	candidates := []OpenAIAccount{{ID: 1, QuotaExhausted: true}, {ID: 2}}
	pool.MarkSuccess("sess", "m", 1) // 1 是本会话的粘性账号,但额度已用满

	if got := pool.OrderedAccounts("sess", "m", candidates)[0].ID; got != 2 {
		t.Fatalf("额度用满的粘性账号不应排最前, 实际首选 = %d", got)
	}
}

func accountIDs(accounts []OpenAIAccount) []int64 {
	out := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, account.ID)
	}
	return out
}
