package main

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

// accountCooldown 是某个账号被上游拒绝(401 失效 / 封号 / 5xx 等)后被临时摘除的
// 冷却时长。冷却期内不再优先选它,过期后自动半开恢复。
//
// 注意传输层错误(EOF / 连接失败)**不**走冷却:所有账号打的是同一个上游地址,
// 换账号只换鉴权头、不换网络路径,那种失败不是账号的问题。
const accountCooldown = 90 * time.Second

// 429 限流单独给一档冷却:实测 ChatGPT 后端会带 Retry-After(绝大多数是 60 秒),
// 按它来比一刀切 90 秒准。上下限用来防住缺失或离谱的值。
const (
	accountRateLimitCooldownMin = 60 * time.Second
	accountRateLimitCooldownMax = 30 * time.Minute
)

// accountPool 负责在「API Key 直连 GPT 账号」路径上做账号调度:
//
//   - 会话粘性:同一 session_id 尽量固定落在同一账号,复用其上下文缓存(命中 cached_tokens);
//   - 负载均衡:新会话在候选账号里按「最少在途请求数」挑选(least-connections),
//     并发相同则随机打散,避免总是命中第一个;
//   - 熔断兜底:账号失败进入冷却期,粘性账号失败时解除绑定,下次请求重新选健康账号。
type accountPool struct {
	mu       sync.Mutex
	sticky   map[string]int64    // session_id|model -> account_db_id
	inflight map[int64]int       // account_db_id -> 在途请求数
	cooldown map[int64]time.Time // account_db_id -> 冷却截止
	// lastUsed 记账号上次被选中的时刻,用于「最久未使用优先」。只在进程内存里,
	// 重启后清零(所有账号回到同一起跑线,几个请求就重新拉开,不值得落库)。
	lastUsed map[int64]time.Time
	// quotaRefreshedAt 记上次因 429 触发额度刷新的时刻,防止并发 429 把同一个账号刷爆。
	quotaRefreshedAt map[int64]time.Time
}

// quotaRefreshMinInterval 是同一账号两次「429 触发的额度刷新」之间的最小间隔。
const quotaRefreshMinInterval = 60 * time.Second

func newAccountPool() *accountPool {
	return &accountPool{
		sticky:           make(map[string]int64),
		inflight:         make(map[int64]int),
		cooldown:         make(map[int64]time.Time),
		lastUsed:         make(map[int64]time.Time),
		quotaRefreshedAt: make(map[int64]time.Time),
	}
}

// ShouldRefreshQuota 判断这个账号现在能不能触发一次额度刷新(用于 429 之后异步补刷)。
// 返回 true 的同时就把时间戳占上,所以并发的多个 429 只有第一个会真的去刷。
func (p *accountPool) ShouldRefreshQuota(accountID int64) bool {
	if accountID == 0 {
		return false
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.quotaRefreshedAt[accountID]; ok && now.Sub(last) < quotaRefreshMinInterval {
		return false
	}
	p.quotaRefreshedAt[accountID] = now
	return true
}

// stickyKey 把会话粘性按 (session_id, model) 绑定:同一会话换模型时命中不同的 key,
// 天然重新匹配账号;换回原模型还能复用原账号的上下文缓存(cached_tokens)。
// 候选集本身已按模型过滤,这里的 model 维度只影响「粘哪个账号」,不影响候选范围。
func stickyKey(sessionID, model string) string {
	return sessionID + "\x00" + model
}

// OrderedAccounts 返回该会话应依次尝试的账号顺序:
// 粘性账号(健康时)排最前,其余健康账号按在途数升序、再按「最久未使用优先」,
// 冷却中的账号排在最后作为最终兜底(全部账号都在冷却时仍要给一次机会,而不是直接放弃)。
//
// 新会话没有粘性绑定、空闲时在途数又都是 0,所以真正起作用的是「最久未使用优先」:
// 它让新会话在账号间轮转,消耗自然摊平。这里刻意没有按额度排序——额度数据只在页面上
// 手动点「刷新全部额度」时才更新,拿可能是几天前的数据排序反而会把流量堆到某一个账号上。
func (p *accountPool) OrderedAccounts(sessionID, model string, candidates []OpenAIAccount) []OpenAIAccount {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	stickyID := p.sticky[stickyKey(sessionID, model)]
	type scored struct {
		account   OpenAIAccount
		inflight  int
		cooling   bool
		exhausted bool
		sticky    bool
		lastUsed  time.Time
		jitter    int
	}
	items := make([]scored, 0, len(candidates))
	for _, account := range candidates {
		until, cooling := p.cooldown[account.ID]
		if cooling && !until.After(now) {
			delete(p.cooldown, account.ID)
			cooling = false
		}
		items = append(items, scored{
			account:   account,
			inflight:  p.inflight[account.ID],
			cooling:   cooling,
			exhausted: account.QuotaExhausted,
			sticky:    account.ID == stickyID,
			lastUsed:  p.lastUsed[account.ID],
			jitter:    rand.Intn(1 << 20),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		// 1) 额度用满的沉到最底,比冷却还靠后:冷却可能只是暂时限流,额度用满则打过去
		//    必然 429。这里不直接剔除是留一条兜底——月度重置后额度数据要等下一次刷新
		//    才会更新,全员「已用满」时若一个都不试,请求就永远发不出去了。
		if a.exhausted != b.exhausted {
			return !a.exhausted
		}
		// 2) 冷却中的排最后
		if a.cooling != b.cooling {
			return !a.cooling
		}
		// 3) 健康集合内,粘性账号优先
		if a.sticky != b.sticky {
			return a.sticky
		}
		// 4) 最少在途请求优先
		if a.inflight != b.inflight {
			return a.inflight < b.inflight
		}
		// 5) 最久未使用优先。从没被选中过的账号 lastUsed 是零值,自然排在最前,
		//    所以新加的账号会先被用上。
		if !a.lastUsed.Equal(b.lastUsed) {
			return a.lastUsed.Before(b.lastUsed)
		}
		// 6) 完全并列(比如都还没用过)才随机打散
		return a.jitter < b.jitter
	})
	out := make([]OpenAIAccount, 0, len(items))
	for _, item := range items {
		out = append(out, item.account)
	}
	return out
}

// Acquire 记一次在途,同时把该账号标记为「刚用过」。
// 时间点取「开始尝试」而不是「成功」:失败的账号也算用过,否则它会一直是最久未使用的那个,
// 下一个请求又优先撞上去。
func (p *accountPool) Acquire(accountID int64) {
	if accountID == 0 {
		return
	}
	p.mu.Lock()
	p.inflight[accountID]++
	p.lastUsed[accountID] = time.Now()
	p.mu.Unlock()
}

func (p *accountPool) Release(accountID int64) {
	if accountID == 0 {
		return
	}
	p.mu.Lock()
	if p.inflight[accountID] > 0 {
		p.inflight[accountID]--
	}
	if p.inflight[accountID] == 0 {
		delete(p.inflight, accountID)
	}
	p.mu.Unlock()
}

// MarkSuccess 请求成功:按 (session, model) 绑定会话粘性到该账号,并清掉它的冷却标记。
func (p *accountPool) MarkSuccess(sessionID, model string, accountID int64) {
	if accountID == 0 {
		return
	}
	p.mu.Lock()
	if sessionID != "" {
		p.sticky[stickyKey(sessionID, model)] = accountID
	}
	delete(p.cooldown, accountID)
	p.mu.Unlock()
}

// MarkFailure 请求被上游拒绝:账号进入 cooldown 冷却期;若它正是本会话该模型的粘性账号
// 则解绑,促使下次重新选。cooldown<=0 时按默认时长。
func (p *accountPool) MarkFailure(sessionID, model string, accountID int64, cooldown time.Duration) {
	if accountID == 0 {
		return
	}
	if cooldown <= 0 {
		cooldown = accountCooldown
	}
	p.mu.Lock()
	p.cooldown[accountID] = time.Now().Add(cooldown)
	key := stickyKey(sessionID, model)
	if sessionID != "" && p.sticky[key] == accountID {
		delete(p.sticky, key)
	}
	p.mu.Unlock()
}
