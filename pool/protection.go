package pool

import (
	"fmt"
	"kiro-go/config"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type upstreamStateKey struct {
	accountID string
	model     string
}

type profileStateKey struct {
	profileArn string
	model      string
}

type upstreamRuntimeState struct {
	inFlight       int
	rateLimitCount int
	cooldownUntil  time.Time
	lastCooldownMs int
	halfOpenProbe  bool
}

type routeAffinityEntry struct {
	accountID string
	lastSeen  time.Time
}

// UpstreamBusyError is returned when protection is enabled and every otherwise
// routable account is locally busy or cooling down for the requested model.
type UpstreamBusyError struct {
	Model       string
	RetryAfter  time.Duration
	Description string
}

func (e *UpstreamBusyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Description != "" {
		return e.Description
	}
	return fmt.Sprintf("upstream busy for model %s", e.Model)
}

// UpstreamRequestGuard releases an acquired local upstream slot.
type UpstreamRequestGuard struct {
	p           *AccountPool
	accountID   string
	profileArn  string
	model       string
	affinityHit bool
	released    atomic.Bool
}

func (g *UpstreamRequestGuard) AffinityHit() bool {
	return g != nil && g.affinityHit
}

func (g *UpstreamRequestGuard) Release() {
	if g == nil || g.p == nil || g.released.Swap(true) {
		return
	}
	g.p.releaseUpstreamSlot(g.accountID, g.profileArn, g.model)
}

// AcquireForModel picks an account and acquires its local upstream protection
// slot. routeKey enables session affinity when non-empty.
func (p *AccountPool) AcquireForModel(model, routeKey string, excluded map[string]bool) (*config.Account, *UpstreamRequestGuard, error) {
	up := config.GetUpstreamProtectionConfig()
	if !up.Enabled {
		account := p.GetNextForModelExcluding(model, excluded)
		if account != nil {
			p.mu.Lock()
			p.recordDispatchLocked(account.ID, false)
			p.mu.Unlock()
		}
		return account, nil, nil
	}
	allowOverUsage := config.GetAllowOverUsage()
	routingMode := config.GetRoutingConfig().LoadBalancingMode

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureProtectionMapsLocked()

	if len(p.accounts) == 0 {
		return nil, nil, nil
	}

	now := time.Now()
	modelKey := normalizeProtectionModel(model)
	routeKey = strings.TrimSpace(routeKey)
	p.pruneRouteAffinityLocked(now, up)

	var bestRetry time.Duration
	var bestReason string

	if routeKey != "" {
		if entry, ok := p.affinity[routeKey]; ok {
			if acc := p.findSelectableAccountLocked(entry.accountID, model, excluded, now, allowOverUsage); acc != nil {
				if guard, busy := p.tryAcquireSlotLocked(acc, modelKey, up, now); guard != nil {
					guard.affinityHit = true
					p.recordDispatchLocked(acc.ID, true)
					p.rememberRouteAffinityLocked(routeKey, acc.ID, now, up)
					return cloneAccount(acc), guard, nil
				} else if busy != nil {
					bestRetry = maxDuration(bestRetry, busy.RetryAfter)
					bestReason = busy.Description
				}
			}
		}
	}

	selectionIndexes := p.selectionIndexesLocked(model, excluded, now, allowOverUsage, routingMode)
	for _, idx := range selectionIndexes {
		acc := &p.accounts[idx]
		if guard, busy := p.tryAcquireSlotLocked(acc, modelKey, up, now); guard != nil {
			p.recordDispatchLocked(acc.ID, false)
			p.commitWeightedSelectionLocked(acc.ID, selectionIndexes, routingMode)
			p.rememberRouteAffinityLocked(routeKey, acc.ID, now, up)
			return cloneAccount(acc), guard, nil
		} else if busy != nil {
			bestRetry = maxDuration(bestRetry, busy.RetryAfter)
			bestReason = busy.Description
		}
	}

	if bestRetry > 0 {
		return nil, nil, &UpstreamBusyError{Model: modelKey, RetryAfter: bestRetry, Description: bestReason}
	}
	return nil, nil, nil
}

func (p *AccountPool) RecordUpstreamRateLimited(accountID, profileArn, model string) time.Duration {
	return p.RecordUpstreamRateLimitedWithRetryAfter(accountID, profileArn, model, 0)
}

func (p *AccountPool) RecordUpstreamRateLimitedWithRetryAfter(accountID, profileArn, model string, retryAfter time.Duration) time.Duration {
	up := config.GetUpstreamProtectionConfig()
	if !up.Enabled {
		return 0
	}

	p.mu.Lock()
	p.ensureProtectionMapsLocked()

	now := time.Now()
	modelKey := normalizeProtectionModel(model)
	var cooldown time.Duration

	key := upstreamStateKey{accountID: accountID, model: modelKey}
	state := p.upstream[key]
	cooldown = maxDuration(cooldown, applyProtectionCooldown(&state, up, now, retryAfter))
	p.upstream[key] = state

	if profileArn = strings.TrimSpace(profileArn); profileArn != "" && profileLimit(up, profileArn, modelKey) > 0 {
		pkey := profileStateKey{profileArn: profileArn, model: modelKey}
		pstate := p.profiles[pkey]
		cooldown = maxDuration(cooldown, applyProtectionCooldown(&pstate, up, now, retryAfter))
		p.profiles[pkey] = pstate
	}

	for key, entry := range p.affinity {
		if entry.accountID == accountID {
			delete(p.affinity, key)
		}
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
	return cooldown
}

// RecordUpstreamSuccess closes any half-open circuit for the successful
// account/model and profile/model pair while preserving current in-flight
// counters until the request guard is released.
func (p *AccountPool) RecordUpstreamSuccess(accountID, profileArn, model string) {
	up := config.GetUpstreamProtectionConfig()
	if !up.Enabled {
		return
	}

	p.mu.Lock()
	p.ensureProtectionMapsLocked()
	modelKey := normalizeProtectionModel(model)
	if modelKey != "" {
		key := upstreamStateKey{accountID: accountID, model: modelKey}
		state := p.upstream[key]
		closeProtectionCircuit(&state)
		if state.inFlight == 0 {
			delete(p.upstream, key)
		} else {
			p.upstream[key] = state
		}

		if profileArn = strings.TrimSpace(profileArn); profileArn != "" {
			pkey := profileStateKey{profileArn: profileArn, model: modelKey}
			pstate := p.profiles[pkey]
			closeProtectionCircuit(&pstate)
			if pstate.inFlight == 0 {
				delete(p.profiles, pkey)
			} else {
				p.profiles[pkey] = pstate
			}
		}
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

func (p *AccountPool) ProtectionSnapshot() map[string]interface{} {
	up := config.GetUpstreamProtectionConfig()
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	accounts := make([]map[string]interface{}, 0, len(p.accounts))
	seenAccounts := make(map[string]bool, len(p.accounts))
	for i := range p.accounts {
		accountID := p.accounts[i].ID
		if accountID == "" || seenAccounts[accountID] {
			continue
		}
		seenAccounts[accountID] = true
		state := p.accountUpstream[accountID]
		health := p.healthStats[accountID]
		affinityHitRate := 0.0
		if health.Dispatches > 0 {
			affinityHitRate = float64(health.AffinityHits) / float64(health.Dispatches)
		}
		lastOutcomeAt := int64(0)
		if !health.LastOutcomeAt.IsZero() {
			lastOutcomeAt = health.LastOutcomeAt.Unix()
		}
		accounts = append(accounts, map[string]interface{}{
			"accountId":       accountID,
			"inFlight":        state.inFlight,
			"latencyMsEwma":   health.LatencyMsEWMA,
			"errorRateEwma":   health.ErrorRateEWMA,
			"samples":         health.Samples,
			"dispatches":      health.Dispatches,
			"affinityHits":    health.AffinityHits,
			"affinityHitRate": affinityHitRate,
			"lastOutcomeAt":   lastOutcomeAt,
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		return fmt.Sprint(accounts[i]["accountId"]) < fmt.Sprint(accounts[j]["accountId"])
	})

	upstream := make([]map[string]interface{}, 0, len(p.upstream))
	for key, state := range p.upstream {
		upstream = append(upstream, map[string]interface{}{
			"accountId":       key.accountID,
			"model":           key.model,
			"inFlight":        state.inFlight,
			"cooldownUntil":   unixOrZero(state.cooldownUntil),
			"cooldownSeconds": secondsUntil(now, state.cooldownUntil),
			"rateLimitCount":  state.rateLimitCount,
			"lastCooldownMs":  state.lastCooldownMs,
			"circuitState":    protectionCircuitState(state, now),
		})
	}
	sort.Slice(upstream, func(i, j int) bool {
		if upstream[i]["model"] == upstream[j]["model"] {
			return fmt.Sprint(upstream[i]["accountId"]) < fmt.Sprint(upstream[j]["accountId"])
		}
		return fmt.Sprint(upstream[i]["model"]) < fmt.Sprint(upstream[j]["model"])
	})

	profiles := make([]map[string]interface{}, 0, len(p.profiles))
	for key, state := range p.profiles {
		profiles = append(profiles, map[string]interface{}{
			"profileArn":      key.profileArn,
			"model":           key.model,
			"inFlight":        state.inFlight,
			"cooldownUntil":   unixOrZero(state.cooldownUntil),
			"cooldownSeconds": secondsUntil(now, state.cooldownUntil),
			"rateLimitCount":  state.rateLimitCount,
			"lastCooldownMs":  state.lastCooldownMs,
			"circuitState":    protectionCircuitState(state, now),
		})
	}

	return map[string]interface{}{
		"config":             up,
		"accountStates":      accounts,
		"accountModelStates": upstream,
		"profileModelStates": profiles,
		"routeAffinityCount": len(p.affinity),
	}
}

func (p *AccountPool) ensureProtectionMapsLocked() {
	if p.cooldowns == nil {
		p.cooldowns = make(map[string]time.Time)
	}
	if p.cooldownKinds == nil {
		p.cooldownKinds = make(map[string]accountCooldownKind)
	}
	if p.errorCounts == nil {
		p.errorCounts = make(map[string]int)
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.accountUpstream == nil {
		p.accountUpstream = make(map[string]upstreamRuntimeState)
	}
	if p.upstream == nil {
		p.upstream = make(map[upstreamStateKey]upstreamRuntimeState)
	}
	if p.profiles == nil {
		p.profiles = make(map[profileStateKey]upstreamRuntimeState)
	}
	if p.weightedCurrent == nil {
		p.weightedCurrent = make(map[string]int64)
	}
	if p.affinity == nil {
		p.affinity = make(map[string]routeAffinityEntry)
	}
	if p.modelNegative == nil {
		p.modelNegative = make(map[modelAvailabilityKey]time.Time)
	}
	if p.lastSuccess == nil {
		p.lastSuccess = make(map[string]time.Time)
	}
	if p.refreshFailures == nil {
		p.refreshFailures = make(map[string]time.Time)
	}
}

func (p *AccountPool) findSelectableAccountLocked(accountID, model string, excluded map[string]bool, now time.Time, allowOverUsage bool) *config.Account {
	if i, ok := p.accountIndexLocked(accountID); ok {
		acc := &p.accounts[i]
		if p.accountSelectableLocked(acc, model, excluded, now, allowOverUsage) {
			return acc
		}
	}
	return nil
}

func (p *AccountPool) accountSelectableLocked(acc *config.Account, model string, excluded map[string]bool, now time.Time, allowOverUsage bool) bool {
	// Affinity must not make a stale account outrank another ready account.
	// selectionIndexesLocked performs a stale-but-refreshable fallback only
	// after it has established that no ready account is available.
	return p.accountSelectableBasicLocked(acc, model, excluded, now, allowOverUsage, false)
}

func (p *AccountPool) tryAcquireSlotLocked(acc *config.Account, model string, up config.UpstreamProtectionConfig, now time.Time) (*UpstreamRequestGuard, *UpstreamBusyError) {
	accountState := p.accountUpstream[acc.ID]
	if busy := stateBusy(model, accountState, accountConcurrencyLimit(up, acc), now, "account total concurrency limit reached"); busy != nil {
		return nil, busy
	}

	profileArn := strings.TrimSpace(acc.ProfileArn)
	profileLimitValue := profileLimit(up, profileArn, model)
	var profileState upstreamRuntimeState
	if profileLimitValue > 0 {
		key := profileStateKey{profileArn: profileArn, model: model}
		profileState = p.profiles[key]
		if busy := stateBusy(model, profileState, profileLimitValue, now, "profile local upstream limit reached"); busy != nil {
			return nil, busy
		}
	}

	var state upstreamRuntimeState
	if model != "" {
		key := upstreamStateKey{accountID: acc.ID, model: model}
		state = p.upstream[key]
		if busy := stateBusy(model, state, accountModelLimit(up, model), now, "account local upstream limit reached"); busy != nil {
			return nil, busy
		}
	}

	accountState.inFlight++
	p.accountUpstream[acc.ID] = accountState
	if profileLimitValue > 0 {
		pkey := profileStateKey{profileArn: profileArn, model: model}
		activateHalfOpenProbe(&profileState, now)
		profileState.inFlight++
		p.profiles[pkey] = profileState
	}
	if model != "" {
		key := upstreamStateKey{accountID: acc.ID, model: model}
		activateHalfOpenProbe(&state, now)
		state.inFlight++
		p.upstream[key] = state
	}

	return &UpstreamRequestGuard{p: p, accountID: acc.ID, profileArn: profileArn, model: model}, nil
}

func (p *AccountPool) releaseUpstreamSlot(accountID, profileArn, model string) {
	up := config.GetUpstreamProtectionConfig()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureProtectionMapsLocked()
	now := time.Now()

	accountState := p.accountUpstream[accountID]
	if accountState.inFlight > 0 {
		accountState.inFlight--
	}
	if accountState.inFlight == 0 {
		delete(p.accountUpstream, accountID)
	} else {
		p.accountUpstream[accountID] = accountState
	}

	if model != "" {
		key := upstreamStateKey{accountID: accountID, model: model}
		state := p.upstream[key]
		if state.inFlight > 0 {
			state.inFlight--
		}
		reopenAbandonedHalfOpenProbe(&state, up, now)
		if protectionStateIdle(state) {
			delete(p.upstream, key)
		} else {
			p.upstream[key] = state
		}
	}

	if profileArn != "" {
		pkey := profileStateKey{profileArn: profileArn, model: model}
		pstate := p.profiles[pkey]
		if pstate.inFlight > 0 {
			pstate.inFlight--
		}
		reopenAbandonedHalfOpenProbe(&pstate, up, now)
		if protectionStateIdle(pstate) {
			delete(p.profiles, pkey)
		} else {
			p.profiles[pkey] = pstate
		}
	}
}

func (p *AccountPool) rememberRouteAffinityLocked(routeKey, accountID string, now time.Time, up config.UpstreamProtectionConfig) {
	if routeKey == "" || up.RouteAffinityMaxEntries <= 0 {
		return
	}
	if len(p.affinity) >= up.RouteAffinityMaxEntries && p.affinity[routeKey].accountID == "" {
		var oldestKey string
		var oldest time.Time
		for key, entry := range p.affinity {
			if oldestKey == "" || entry.lastSeen.Before(oldest) {
				oldestKey = key
				oldest = entry.lastSeen
			}
		}
		if oldestKey != "" {
			delete(p.affinity, oldestKey)
		}
	}
	p.affinity[routeKey] = routeAffinityEntry{accountID: accountID, lastSeen: now}
}

func (p *AccountPool) pruneRouteAffinityLocked(now time.Time, up config.UpstreamProtectionConfig) {
	ttl := time.Duration(up.RouteAffinityTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	for key, entry := range p.affinity {
		if now.Sub(entry.lastSeen) > ttl {
			delete(p.affinity, key)
		}
	}
}

func accountConcurrencyLimit(up config.UpstreamProtectionConfig, account *config.Account) int {
	if account != nil && account.MaxConcurrency > 0 {
		return account.MaxConcurrency
	}
	if up.MaxPerAccountConcurrency > 0 {
		return up.MaxPerAccountConcurrency
	}
	return 10
}

func accountModelLimit(up config.UpstreamProtectionConfig, model string) int {
	model = normalizeProtectionModel(model)
	if limit, ok := up.PerModelConcurrency[model]; ok && limit > 0 {
		return limit
	}
	if up.MaxPerAccountModelConcurrency > 0 {
		return up.MaxPerAccountModelConcurrency
	}
	return 5
}

func profileLimit(up config.UpstreamProtectionConfig, profileArn, model string) int {
	if profileArn == "" {
		return 0
	}
	model = normalizeProtectionModel(model)
	if byModel, ok := up.PerProfileModelConcurrency[profileArn]; ok {
		if limit, ok := byModel[model]; ok && limit > 0 {
			return limit
		}
	}
	return 0
}

func stateBusy(model string, state upstreamRuntimeState, limit int, now time.Time, reason string) *UpstreamBusyError {
	if state.halfOpenProbe {
		return &UpstreamBusyError{
			Model:       model,
			RetryAfter:  time.Second,
			Description: fmt.Sprintf("%s: half-open probe already in flight for %s", reason, model),
		}
	}
	if !state.cooldownUntil.IsZero() {
		if now.Before(state.cooldownUntil) {
			return &UpstreamBusyError{
				Model:       model,
				RetryAfter:  state.cooldownUntil.Sub(now),
				Description: fmt.Sprintf("%s: 429 cooldown for %s", reason, model),
			}
		}
	}
	if limit <= 0 {
		limit = 1
	}
	if state.inFlight >= limit {
		return &UpstreamBusyError{
			Model:       model,
			RetryAfter:  time.Second,
			Description: fmt.Sprintf("%s: %d/%d", reason, state.inFlight, limit),
		}
	}
	return nil
}

func applyProtectionCooldown(state *upstreamRuntimeState, up config.UpstreamProtectionConfig, now time.Time, retryAfter time.Duration) time.Duration {
	state.rateLimitCount++
	state.halfOpenProbe = false
	base := up.RateLimitCooldownMs
	if base <= 0 {
		base = 2000
	}
	maxMs := up.MaxRateLimitCooldownMs
	if maxMs <= 0 {
		maxMs = 60000
	}
	cooldownMs := base
	for i := 1; i < state.rateLimitCount; i++ {
		cooldownMs *= 2
		if cooldownMs >= maxMs {
			cooldownMs = maxMs
			break
		}
	}
	retryAfterMs := int(retryAfter.Round(time.Millisecond) / time.Millisecond)
	if retryAfterMs > cooldownMs {
		const maxRetryAfterMs = int((24 * time.Hour) / time.Millisecond)
		if retryAfterMs > maxRetryAfterMs {
			retryAfterMs = maxRetryAfterMs
		}
		cooldownMs = retryAfterMs
	}
	if retryAfterMs == 0 && cooldownMs < maxMs {
		jitterMax := cooldownMs / 10
		if jitterMax < 1 {
			jitterMax = 1
		}
		cooldownMs += rand.Intn(jitterMax + 1)
		if cooldownMs > maxMs {
			cooldownMs = maxMs
		}
	}
	state.lastCooldownMs = cooldownMs
	state.cooldownUntil = now.Add(time.Duration(cooldownMs) * time.Millisecond)
	return time.Duration(cooldownMs) * time.Millisecond
}

func activateHalfOpenProbe(state *upstreamRuntimeState, now time.Time) {
	if state == nil || state.rateLimitCount == 0 || state.cooldownUntil.IsZero() || now.Before(state.cooldownUntil) {
		return
	}
	state.cooldownUntil = time.Time{}
	state.halfOpenProbe = true
}

func closeProtectionCircuit(state *upstreamRuntimeState) {
	if state == nil {
		return
	}
	state.rateLimitCount = 0
	state.cooldownUntil = time.Time{}
	state.lastCooldownMs = 0
	state.halfOpenProbe = false
}

func reopenAbandonedHalfOpenProbe(state *upstreamRuntimeState, up config.UpstreamProtectionConfig, now time.Time) {
	if state == nil || !state.halfOpenProbe || state.rateLimitCount == 0 {
		return
	}
	state.halfOpenProbe = false
	delayMs := state.lastCooldownMs
	if delayMs <= 0 {
		delayMs = up.RateLimitCooldownMs
	}
	if delayMs <= 0 {
		delayMs = 2000
	}
	state.cooldownUntil = now.Add(time.Duration(delayMs) * time.Millisecond)
}

func protectionStateIdle(state upstreamRuntimeState) bool {
	return state.inFlight == 0 && state.cooldownUntil.IsZero() && state.rateLimitCount == 0 && !state.halfOpenProbe
}

func protectionCircuitState(state upstreamRuntimeState, now time.Time) string {
	if state.halfOpenProbe || (state.rateLimitCount > 0 && !state.cooldownUntil.IsZero() && !now.Before(state.cooldownUntil)) {
		return "half_open"
	}
	if !state.cooldownUntil.IsZero() && now.Before(state.cooldownUntil) {
		return "open"
	}
	return "closed"
}

func normalizeProtectionModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func cloneAccount(acc *config.Account) *config.Account {
	if acc == nil {
		return nil
	}
	cloned := *acc
	return &cloned
}

func maxDuration(a, b time.Duration) time.Duration {
	if b > a {
		return b
	}
	return a
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func secondsUntil(now, t time.Time) int {
	if t.IsZero() || !t.After(now) {
		return 0
	}
	return int(t.Sub(now).Round(time.Second).Seconds())
}
