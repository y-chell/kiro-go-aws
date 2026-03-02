// Package pool 账号池管理
// 实现随机负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-api-proxy/config"
	"math/rand"
	"sort"
	"sync"
	"time"
)

const (
	primaryUsageThreshold = 10
	topCandidateLimit     = 3
)

// AccountPool 账号池
type AccountPool struct {
	mu          sync.RWMutex
	accounts    []config.Account
	cooldowns   map[string]time.Time // 账号冷却时间
	errorCounts map[string]int       // 连续错误计数
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:   make(map[string]time.Time),
			errorCounts: make(map[string]int),
		}
		pool.Reload()
	})
	return pool
}

// Reload 从配置重新加载账号
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = config.GetEnabledAccounts()
}

// GetNext 获取一个可用账号：主池/兜底池 +（排序优先后的）组内加权随机
func (p *AccountPool) GetNext() *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	now := time.Now()
	primary := make([]*config.Account, 0, len(p.accounts))
	fallback := make([]*config.Account, 0, len(p.accounts))

	for i := range p.accounts {
		acc := &p.accounts[i]

		// 跳过冷却中的账号
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}

		// 跳过即将过期的 Token
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-300 {
			continue
		}

		if acc.UsageCurrent >= primaryUsageThreshold {
			primary = append(primary, acc)
		} else {
			fallback = append(fallback, acc)
		}
	}

	picked := pickWeightedWithRanking(primary)
	if picked == nil {
		picked = pickWeightedWithRanking(fallback)
	}
	if picked != nil {
		return picked
	}

	// 无可用账号，返回冷却时间最短的
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

func pickWeightedWithRanking(candidates []*config.Account) *config.Account {
	if len(candidates) == 0 {
		return nil
	}

	// 先按积分(usageCurrent) / 更新时间(lastRefresh)排序
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].UsageCurrent == candidates[j].UsageCurrent {
			return candidates[i].LastRefresh > candidates[j].LastRefresh
		}
		return candidates[i].UsageCurrent > candidates[j].UsageCurrent
	})

	// 只在前N个候选里按权重随机，兼顾“优先”与“分流”
	limit := topCandidateLimit
	if len(candidates) < limit {
		limit = len(candidates)
	}
	top := candidates[:limit]

	totalWeight := 0
	for _, a := range top {
		w := a.Weight
		if w <= 0 {
			w = 100
		}
		totalWeight += w
	}

	if totalWeight <= 0 {
		return top[rand.Intn(len(top))]
	}

	r := rand.Intn(totalWeight)
	for _, a := range top {
		w := a.Weight
		if w <= 0 {
			w = 100
		}
		r -= w
		if r < 0 {
			return a
		}
	}
	return top[len(top)-1]
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.errorCounts[id]++

	if isQuotaError {
		// 配额错误，冷却 1 小时
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
			break
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	for _, acc := range p.accounts {
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].RequestCount++
			p.accounts[i].TotalTokens += tokens
			p.accounts[i].TotalCredits += credits
			p.accounts[i].LastUsed = time.Now().Unix()
			go config.UpdateAccountStats(id, p.accounts[i].RequestCount, p.accounts[i].ErrorCount, p.accounts[i].TotalTokens, p.accounts[i].TotalCredits, p.accounts[i].LastUsed)
			break
		}
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}
