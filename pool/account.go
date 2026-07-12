// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-go/config"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120
const accountStatsSaveDelay = 750 * time.Millisecond
const maxAccountWeight = 100

// AccountPool 账号池
type AccountPool struct {
	mu               sync.RWMutex
	accounts         []config.Account
	accountIndex     map[string]int
	totalAccounts    int
	currentIndex     atomic.Uint64
	cooldowns        map[string]time.Time       // 账号冷却时间
	errorCounts      map[string]int             // 连续错误计数
	modelLists       map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	accountUpstream  map[string]upstreamRuntimeState
	upstream         map[upstreamStateKey]upstreamRuntimeState
	profiles         map[profileStateKey]upstreamRuntimeState
	weightedCurrent  map[string]int64
	affinity         map[string]routeAffinityEntry
	modelNegative    map[modelAvailabilityKey]time.Time
	lastSuccess      map[string]time.Time
	refreshFailures  map[string]time.Time
	refreshCursor    int
	stateSaveMu      sync.Mutex
	stateSaveTimer   *time.Timer
	statsSaveMu      sync.Mutex
	statsSaveTimer   *time.Timer
	dirtyStats       map[string]struct{}
	configGeneration uint64
}

type modelAvailabilityKey struct {
	accountID string
	model     string
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:        make(map[string]time.Time),
			accountIndex:     make(map[string]int),
			errorCounts:      make(map[string]int),
			modelLists:       make(map[string]map[string]bool),
			accountUpstream:  make(map[string]upstreamRuntimeState),
			upstream:         make(map[upstreamStateKey]upstreamRuntimeState),
			profiles:         make(map[profileStateKey]upstreamRuntimeState),
			weightedCurrent:  make(map[string]int64),
			affinity:         make(map[string]routeAffinityEntry),
			modelNegative:    make(map[modelAvailabilityKey]time.Time),
			lastSuccess:      make(map[string]time.Time),
			refreshFailures:  make(map[string]time.Time),
			dirtyStats:       make(map[string]struct{}),
			configGeneration: config.GetGeneration(),
		}
		pool.Reload()
		pool.loadRuntimeState()
	})
	return pool
}

// Reload rebuilds the account list from config. Accounts are stored once;
// weighted routing state is maintained separately to avoid weight-sized memory
// growth and duplicate account state.
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	generation := config.GetGeneration()
	p.mu.RLock()
	sameConfig := p.configGeneration == generation
	p.mu.RUnlock()
	if sameConfig {
		p.flushAccountStats()
	}

	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	mode := config.GetRoutingConfig().LoadBalancingMode
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.configGeneration != generation {
		p.dirtyStats = make(map[string]struct{})
	}
	p.configGeneration = generation
	accounts := make([]config.Account, 0, len(enabled))
	accountIndex := make(map[string]int, len(enabled))
	activeIDs := make(map[string]struct{}, len(enabled))
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		a.Weight = effectiveWeight(a.Weight)
		accountIndex[a.ID] = len(accounts)
		accounts = append(accounts, a)
		activeIDs[a.ID] = struct{}{}
	}
	p.accounts = accounts
	p.accountIndex = accountIndex
	p.totalAccounts = len(enabled)
	if p.weightedCurrent == nil {
		p.weightedCurrent = make(map[string]int64)
	}
	for id := range p.weightedCurrent {
		if _, ok := activeIDs[id]; !ok {
			delete(p.weightedCurrent, id)
		}
	}
	if mode != "weighted" {
		for id := range p.weightedCurrent {
			delete(p.weightedCurrent, id)
		}
	}
}

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（加权轮询），并跳过指定账号。
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	indexes := p.selectionIndexesLocked("", excluded, now, allowOverUsage)
	if len(indexes) > 0 {
		idx := indexes[0]
		p.commitWeightedSelectionLocked(p.accounts[idx].ID, indexes)
		return cloneAccount(&p.accounts[idx])
	}

	// 无可用账号，返回冷却时间最短的（排除额度用尽的，除非允许超额）
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if !p.accountTokenRoutableLocked(acc, now) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return cloneAccount(acc)
		}
	}
	return cloneAccount(best)
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	p.mu.Lock()
	set := p.modelLists[accountID]
	if set == nil {
		set = make(map[string]bool, len(modelIDs))
	}
	for _, id := range modelIDs {
		if modelKey := strings.ToLower(strings.TrimSpace(id)); modelKey != "" {
			set[modelKey] = true
		}
	}
	p.modelLists[accountID] = set
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel reports whether an account may be probed for a model.
// ListAvailableModels is advisory: Kiro can expose hidden/new models that are
// absent from the advertised list, so only a learned negative entry is a hard
// exclusion. Advertised and learned-positive models are ordered first later.
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	modelKey := strings.ToLower(strings.TrimSpace(model))
	if until, ok := p.modelNegative[modelAvailabilityKey{accountID: accountID, model: modelKey}]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

func (p *AccountPool) accountAdvertisesModel(accountID, model string) bool {
	modelKey := strings.ToLower(strings.TrimSpace(model))
	if modelKey == "" {
		return false
	}
	return p.modelLists[accountID][modelKey]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding 获取下一个支持指定模型的可用账号，并跳过指定账号。
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	indexes := p.selectionIndexesLocked(model, excluded, now, allowOverUsage)
	if len(indexes) > 0 {
		idx := indexes[0]
		p.commitWeightedSelectionLocked(p.accounts[idx].ID, indexes)
		return cloneAccount(&p.accounts[idx])
	}

	// fallback：找冷却时间最短且支持该模型的账号
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if !p.accountTokenRoutableLocked(acc, now) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return cloneAccount(acc)
		}
	}
	return cloneAccount(best)
}

func (p *AccountPool) selectionIndexesLocked(model string, excluded map[string]bool, now time.Time, allowOverUsage bool) []int {
	indexes := p.selectionIndexesForTokenStateLocked(model, excluded, now, allowOverUsage, false)
	if len(indexes) == 0 {
		// Background refresh is deliberately not the only recovery path. When no
		// ready account exists, allow an OAuth account with a refresh token to be
		// selected so the request path can refresh it before upstream use.
		indexes = p.selectionIndexesForTokenStateLocked(model, excluded, now, allowOverUsage, true)
	}
	return p.orderSelectionIndexesLocked(indexes, model)
}

func (p *AccountPool) selectionIndexesForTokenStateLocked(model string, excluded map[string]bool, now time.Time, allowOverUsage, allowRefreshFallback bool) []int {
	mode := config.GetRoutingConfig().LoadBalancingMode
	if mode == "priority" || mode == "balanced" {
		indexes := make([]int, 0, len(p.accounts))
		seen := make(map[string]bool)
		for i := range p.accounts {
			acc := &p.accounts[i]
			if seen[acc.ID] {
				continue
			}
			seen[acc.ID] = true
			if !p.accountSelectableBasicLocked(acc, model, excluded, now, allowOverUsage, allowRefreshFallback) {
				continue
			}
			indexes = append(indexes, i)
		}
		return indexes
	}

	indexes := make([]int, 0, len(p.accounts))
	for idx := range p.accounts {
		acc := &p.accounts[idx]
		if !p.accountSelectableBasicLocked(acc, model, excluded, now, allowOverUsage, allowRefreshFallback) {
			continue
		}
		indexes = append(indexes, idx)
	}
	return indexes
}

func (p *AccountPool) orderSelectionIndexesLocked(indexes []int, model string) []int {
	mode := config.GetRoutingConfig().LoadBalancingMode
	if mode == "priority" {
		sort.SliceStable(indexes, func(i, j int) bool {
			a := p.accounts[indexes[i]]
			b := p.accounts[indexes[j]]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			aAdvertised := p.accountAdvertisesModel(a.ID, model)
			bAdvertised := p.accountAdvertisesModel(b.ID, model)
			if aAdvertised != bAdvertised {
				return aAdvertised
			}
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
			return a.ID < b.ID
		})
		return indexes
	}

	if mode == "balanced" {
		// Historical request totals are unsuitable for balancing: a newly added
		// account starts at zero and would receive all traffic until it catches up.
		// Rotate equally within each priority tier instead.
		sort.SliceStable(indexes, func(i, j int) bool {
			a := p.accounts[indexes[i]]
			b := p.accounts[indexes[j]]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			aAdvertised := p.accountAdvertisesModel(a.ID, model)
			bAdvertised := p.accountAdvertisesModel(b.ID, model)
			if aAdvertised != bAdvertised {
				return aAdvertised
			}
			return a.ID < b.ID
		})
		if len(indexes) < 2 {
			return indexes
		}

		// Rotate only the leading priority/capability tier. Lower-priority or
		// unverified accounts remain available as fallbacks when the preferred
		// accounts are locally busy.
		first := p.accounts[indexes[0]]
		firstAdvertised := p.accountAdvertisesModel(first.ID, model)
		tierEnd := 1
		for tierEnd < len(indexes) {
			candidate := p.accounts[indexes[tierEnd]]
			if candidate.Priority != first.Priority || p.accountAdvertisesModel(candidate.ID, model) != firstAdvertised {
				break
			}
			tierEnd++
		}
		if tierEnd > 1 {
			offset := int(p.currentIndex.Load() % uint64(tierEnd))
			rotated := append([]int(nil), indexes[offset:tierEnd]...)
			rotated = append(rotated, indexes[:offset]...)
			rotated = append(rotated, indexes[tierEnd:]...)
			indexes = rotated
		}
		return indexes
	}

	sort.SliceStable(indexes, func(i, j int) bool {
		a := p.accounts[indexes[i]]
		b := p.accounts[indexes[j]]
		aScore := p.weightedCurrent[a.ID] + int64(effectiveWeight(a.Weight))
		bScore := p.weightedCurrent[b.ID] + int64(effectiveWeight(b.Weight))
		if aScore != bScore {
			return aScore > bScore
		}
		return a.ID < b.ID
	})
	return p.preferAdvertisedModelsLocked(indexes, model)
}

func (p *AccountPool) preferAdvertisedModelsLocked(indexes []int, model string) []int {
	if strings.TrimSpace(model) == "" || len(indexes) < 2 {
		return indexes
	}
	preferred := make([]int, 0, len(indexes))
	unknown := make([]int, 0, len(indexes))
	for _, idx := range indexes {
		if p.accountAdvertisesModel(p.accounts[idx].ID, model) {
			preferred = append(preferred, idx)
		} else {
			unknown = append(unknown, idx)
		}
	}
	return append(preferred, unknown...)
}

func (p *AccountPool) commitWeightedSelectionLocked(selectedID string, eligibleIndexes []int) {
	if selectedID == "" || len(eligibleIndexes) == 0 {
		return
	}
	mode := config.GetRoutingConfig().LoadBalancingMode
	if mode == "balanced" {
		p.currentIndex.Add(1)
		return
	}
	if mode != "weighted" {
		return
	}
	if p.weightedCurrent == nil {
		p.weightedCurrent = make(map[string]int64)
	}
	var totalWeight int64
	for _, idx := range eligibleIndexes {
		if idx < 0 || idx >= len(p.accounts) {
			continue
		}
		acc := p.accounts[idx]
		weight := int64(effectiveWeight(acc.Weight))
		p.weightedCurrent[acc.ID] += weight
		totalWeight += weight
	}
	if totalWeight > 0 {
		p.weightedCurrent[selectedID] -= totalWeight
		p.currentIndex.Add(1)
	}
}

func (p *AccountPool) accountSelectableBasicLocked(acc *config.Account, model string, excluded map[string]bool, now time.Time, allowOverUsage, allowRefreshFallback bool) bool {
	if acc == nil {
		return false
	}
	if excluded != nil && excluded[acc.ID] {
		return false
	}
	if model != "" && !p.accountHasModel(acc.ID, model) {
		return false
	}
	if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
		return false
	}
	if accountTokenNeedsRefresh(acc, now) {
		if !allowRefreshFallback || strings.TrimSpace(acc.RefreshToken) == "" {
			return false
		}
		if until := p.refreshFailures[acc.ID]; until.After(now) {
			return false
		}
	}
	return !isQuotaBlocked(*acc, allowOverUsage)
}

func accountTokenNeedsRefresh(acc *config.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if strings.TrimSpace(acc.RefreshToken) != "" && strings.TrimSpace(acc.AccessToken) == "" {
		return true
	}
	return acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds
}

func (p *AccountPool) accountTokenRoutableLocked(acc *config.Account, now time.Time) bool {
	if !accountTokenNeedsRefresh(acc, now) {
		return true
	}
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return false
	}
	return !p.refreshFailures[acc.ID].After(now)
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx, ok := p.accountIndexLocked(id); ok {
		return cloneAccount(&p.accounts[idx])
	}
	return nil
}

func (p *AccountPool) accountIndexLocked(id string) (int, bool) {
	if idx, ok := p.accountIndex[id]; ok && idx >= 0 && idx < len(p.accounts) && p.accounts[idx].ID == id {
		return idx, true
	}
	p.accountIndex = make(map[string]int, len(p.accounts))
	for i := range p.accounts {
		if _, exists := p.accountIndex[p.accounts[i].ID]; !exists {
			p.accountIndex[p.accounts[i].ID] = i
		}
	}
	idx, ok := p.accountIndex[id]
	return idx, ok
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	p.ensureProtectionMapsLocked()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	p.lastSuccess[id] = time.Now()
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	generation := config.GetGeneration()
	p.mu.Lock()
	p.ensureProtectionMapsLocked()

	p.errorCounts[id]++
	var stats config.AccountStatsSnapshot
	updated := false
	for i := range p.accounts {
		if p.accounts[i].ID != id {
			continue
		}
		if !updated {
			p.accounts[i].ErrorCount++
			stats = p.accountStatsLocked(id)
			updated = true
			continue
		}
		p.accounts[i].ErrorCount = stats.ErrorCount
	}
	if updated {
		if p.dirtyStats == nil {
			p.dirtyStats = make(map[string]struct{})
		}
		if p.configGeneration == 0 {
			p.configGeneration = generation
		}
		p.dirtyStats[id] = struct{}{}
	}

	if isQuotaError {
		// 配额错误，冷却 1 小时
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
	if updated {
		p.scheduleAccountStatsSave()
	}
}

// RecordModelUnavailable temporarily excludes one account only for the rejected model.
func (p *AccountPool) RecordModelUnavailable(accountID, model string) time.Time {
	ttl := time.Duration(config.GetModelRegistryConfig().NegativeCacheTTLSeconds) * time.Second
	if ttl < time.Minute {
		ttl = time.Hour
	}
	until := time.Now().Add(ttl)
	p.mu.Lock()
	if p.modelNegative == nil {
		p.modelNegative = make(map[modelAvailabilityKey]time.Time)
	}
	modelKey := strings.ToLower(strings.TrimSpace(model))
	p.modelNegative[modelAvailabilityKey{accountID: accountID, model: modelKey}] = until
	if models := p.modelLists[accountID]; models != nil {
		delete(models, modelKey)
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
	return until
}

// ClearModelUnavailable removes a learned negative entry after a successful request.
func (p *AccountPool) ClearModelUnavailable(accountID, model string) {
	p.mu.Lock()
	modelKey := strings.ToLower(strings.TrimSpace(model))
	delete(p.modelNegative, modelAvailabilityKey{accountID: accountID, model: modelKey})
	if modelKey != "" {
		if p.modelLists == nil {
			p.modelLists = make(map[string]map[string]bool)
		}
		if p.modelLists[accountID] == nil {
			p.modelLists[accountID] = make(map[string]bool)
		}
		p.modelLists[accountID][modelKey] = true
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

func (p *AccountPool) RefreshCursor() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.refreshCursor
}

func (p *AccountPool) SetRefreshCursor(cursor int) {
	if cursor < 0 {
		cursor = 0
	}
	p.mu.Lock()
	p.refreshCursor = cursor
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

func (p *AccountPool) RefreshFailureCooldowns() map[string]int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make(map[string]int64)
	for accountID, until := range p.refreshFailures {
		if until.After(now) {
			out[accountID] = until.Unix()
		}
	}
	return out
}

func (p *AccountPool) SetRefreshFailureCooldown(accountID string, until time.Time) {
	p.mu.Lock()
	if p.refreshFailures == nil {
		p.refreshFailures = make(map[string]time.Time)
	}
	p.refreshFailures[accountID] = until
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

func (p *AccountPool) ClearRefreshFailureCooldown(accountID string) {
	p.mu.Lock()
	delete(p.refreshFailures, accountID)
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.UpdateCredentials(id, accessToken, refreshToken, expiresAt, "")
}

// UpdateCredentials updates runtime credential fields on an account.
func (p *AccountPool) UpdateCredentials(id, accessToken, refreshToken string, expiresAt int64, profileArn string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i, ok := p.accountIndexLocked(id); ok {
		p.accounts[i].AccessToken = accessToken
		if refreshToken != "" {
			p.accounts[i].RefreshToken = refreshToken
		}
		p.accounts[i].ExpiresAt = expiresAt
		if profileArn != "" {
			p.accounts[i].ProfileArn = profileArn
		}
		if len(p.accountIndex) != len(p.accounts) {
			for j := range p.accounts {
				if j != i && p.accounts[j].ID == id {
					p.accounts[j].AccessToken = p.accounts[i].AccessToken
					p.accounts[j].RefreshToken = p.accounts[i].RefreshToken
					p.accounts[j].ExpiresAt = p.accounts[i].ExpiresAt
					p.accounts[j].ProfileArn = p.accounts[i].ProfileArn
				}
			}
		}
	}
}

// UpdateProfileArn updates the cached profile on an account.
func (p *AccountPool) UpdateProfileArn(id, profileArn string) {
	if strings.TrimSpace(profileArn) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if i, ok := p.accountIndexLocked(id); ok {
		p.accounts[i].ProfileArn = profileArn
		if len(p.accountIndex) != len(p.accounts) {
			for j := range p.accounts {
				if j != i && p.accounts[j].ID == id {
					p.accounts[j].ProfileArn = profileArn
				}
			}
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	allowOverUsage := config.GetAllowOverUsage()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isQuotaBlocked(acc, allowOverUsage) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	generation := config.GetGeneration()
	p.mu.Lock()
	updated := false
	if i, ok := p.accountIndexLocked(id); ok {
		p.accounts[i].RequestCount++
		p.accounts[i].TotalTokens += tokens
		p.accounts[i].TotalCredits += credits
		p.accounts[i].LastUsed = time.Now().Unix()
		if len(p.accountIndex) != len(p.accounts) {
			for j := range p.accounts {
				if j != i && p.accounts[j].ID == id {
					p.accounts[j].RequestCount = p.accounts[i].RequestCount
					p.accounts[j].ErrorCount = p.accounts[i].ErrorCount
					p.accounts[j].TotalTokens = p.accounts[i].TotalTokens
					p.accounts[j].TotalCredits = p.accounts[i].TotalCredits
					p.accounts[j].LastUsed = p.accounts[i].LastUsed
				}
			}
		}
		updated = true
	}
	if updated {
		if p.dirtyStats == nil {
			p.dirtyStats = make(map[string]struct{})
		}
		if p.configGeneration == 0 {
			p.configGeneration = generation
		}
		p.dirtyStats[id] = struct{}{}
	}
	p.mu.Unlock()
	if updated {
		p.scheduleAccountStatsSave()
	}
}

func (p *AccountPool) accountStatsLocked(id string) config.AccountStatsSnapshot {
	if i, ok := p.accountIndexLocked(id); ok {
		return config.AccountStatsSnapshot{
			RequestCount: p.accounts[i].RequestCount,
			ErrorCount:   p.accounts[i].ErrorCount,
			TotalTokens:  p.accounts[i].TotalTokens,
			TotalCredits: p.accounts[i].TotalCredits,
			LastUsed:     p.accounts[i].LastUsed,
		}
	}
	return config.AccountStatsSnapshot{}
}

func (p *AccountPool) scheduleAccountStatsSave() {
	if p == nil {
		return
	}
	p.statsSaveMu.Lock()
	defer p.statsSaveMu.Unlock()
	if p.statsSaveTimer != nil {
		return
	}
	p.statsSaveTimer = time.AfterFunc(accountStatsSaveDelay, p.flushAccountStats)
}

func (p *AccountPool) flushAccountStats() {
	if p == nil {
		return
	}
	p.statsSaveMu.Lock()
	defer p.statsSaveMu.Unlock()
	if p.statsSaveTimer != nil {
		p.statsSaveTimer.Stop()
	}

	updates := make(map[string]config.AccountStatsSnapshot)
	p.mu.Lock()
	generation := p.configGeneration
	for id := range p.dirtyStats {
		updates[id] = p.accountStatsLocked(id)
	}
	p.dirtyStats = make(map[string]struct{})
	p.mu.Unlock()

	if err := config.UpdateAccountStatsBatch(generation, updates); err != nil {
		p.mu.Lock()
		for id := range updates {
			p.dirtyStats[id] = struct{}{}
		}
		p.mu.Unlock()
		p.statsSaveTimer = time.AfterFunc(2*accountStatsSaveDelay, p.flushAccountStats)
		return
	}
	p.statsSaveTimer = nil
}

// FlushStats persists pending account counters immediately.
func (p *AccountPool) FlushStats() {
	p.flushAccountStats()
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	if weight > maxAccountWeight {
		return maxAccountWeight
	}
	return weight
}
