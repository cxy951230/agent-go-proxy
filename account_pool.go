package main

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

// accountCooldown 是某个账号请求失败(429 限流 / 401 失效 / 封号 / 5xx / 传输错误)后
// 被临时摘除的冷却时长。冷却期内不再优先选它,过期后自动半开恢复。
const accountCooldown = 90 * time.Second

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
}

func newAccountPool() *accountPool {
	return &accountPool{
		sticky:   make(map[string]int64),
		inflight: make(map[int64]int),
		cooldown: make(map[int64]time.Time),
	}
}

// stickyKey 把会话粘性按 (session_id, model) 绑定:同一会话换模型时命中不同的 key,
// 天然重新匹配账号;换回原模型还能复用原账号的上下文缓存(cached_tokens)。
// 候选集本身已按模型过滤,这里的 model 维度只影响「粘哪个账号」,不影响候选范围。
func stickyKey(sessionID, model string) string {
	return sessionID + "\x00" + model
}

// OrderedAccounts 返回该会话应依次尝试的账号顺序:
// 粘性账号(健康时)排最前,其余健康账号按在途数升序,冷却中的账号排在最后作为最终兜底
// (全部账号都在冷却时仍要给一次机会,而不是直接放弃)。
func (p *accountPool) OrderedAccounts(sessionID, model string, candidates []OpenAIAccount) []OpenAIAccount {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	stickyID := p.sticky[stickyKey(sessionID, model)]
	type scored struct {
		account  OpenAIAccount
		inflight int
		cooling  bool
		sticky   bool
		jitter   int
	}
	items := make([]scored, 0, len(candidates))
	for _, account := range candidates {
		until, cooling := p.cooldown[account.ID]
		if cooling && !until.After(now) {
			delete(p.cooldown, account.ID)
			cooling = false
		}
		items = append(items, scored{
			account:  account,
			inflight: p.inflight[account.ID],
			cooling:  cooling,
			sticky:   account.ID == stickyID,
			jitter:   rand.Intn(1 << 20),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		// 1) 冷却中的排最后
		if a.cooling != b.cooling {
			return !a.cooling
		}
		// 2) 健康集合内,粘性账号优先
		if a.sticky != b.sticky {
			return a.sticky
		}
		// 3) 最少在途请求优先
		if a.inflight != b.inflight {
			return a.inflight < b.inflight
		}
		// 4) 并发相同则随机打散
		return a.jitter < b.jitter
	})
	out := make([]OpenAIAccount, 0, len(items))
	for _, item := range items {
		out = append(out, item.account)
	}
	return out
}

func (p *accountPool) Acquire(accountID int64) {
	if accountID == 0 {
		return
	}
	p.mu.Lock()
	p.inflight[accountID]++
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

// MarkFailure 请求失败:账号进入冷却期;若它正是本会话该模型的粘性账号则解绑,促使下次重新选。
func (p *accountPool) MarkFailure(sessionID, model string, accountID int64) {
	if accountID == 0 {
		return
	}
	p.mu.Lock()
	p.cooldown[accountID] = time.Now().Add(accountCooldown)
	key := stickyKey(sessionID, model)
	if sessionID != "" && p.sticky[key] == accountID {
		delete(p.sticky, key)
	}
	p.mu.Unlock()
}
