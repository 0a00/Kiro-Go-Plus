package proxy

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"kiro-go/config"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultPromptCacheTTL = 5 * time.Minute
const defaultPromptCacheMaxEntriesPerAccount = 2048
const defaultPromptCacheMaxEntriesTotal = 50000
const promptCacheShardCount = 64

// Anthropic requires cached prefixes to reach a minimum token count before
// caching takes effect. Breakpoints below this threshold are excluded from
// matching and storage to avoid reporting unrealistic 100% cache hits on
// short requests.
const defaultMinCacheableTokens = 1024
const opusMinCacheableTokens = 4096

type promptCacheUsage struct {
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type promptCacheDiagnostic struct {
	Status              string
	Reason              string
	Source              string
	MatchedInputTokens  int
	EligibleInputTokens int
	ReadEfficiency      float64
}

func (d promptCacheDiagnostic) Apply(entry *requestLogEntry) {
	if entry == nil {
		return
	}
	entry.CacheStatus = d.Status
	entry.CacheMissReason = d.Reason
	entry.CacheSource = d.Source
	entry.CacheMatchedInputTokens = d.MatchedInputTokens
	entry.CacheEligibleInputTokens = d.EligibleInputTokens
	entry.CacheReadEfficiency = d.ReadEfficiency
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
}

type promptCacheProfile struct {
	Breakpoints      []promptCacheBreakpoint
	TotalInputTokens int
	Model            string
}

func minCacheableTokensForModel(model string) int {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "opus") {
		return opusMinCacheableTokens
	}
	return defaultMinCacheableTokens
}

type promptCacheEntry struct {
	Scope       string
	Fingerprint [32]byte
	ExpiresAt   time.Time
	TTL         time.Duration
	LastAccess  time.Time
	accountElem *list.Element
	shardElem   *list.Element
}

type promptCacheShard struct {
	mu               sync.Mutex
	entriesByAccount map[string]map[[32]byte]*promptCacheEntry
	accountOrder     map[string]*list.List
	order            *list.List
}

type promptCacheTracker struct {
	settingsMu           sync.RWMutex
	shards               [promptCacheShardCount]promptCacheShard
	enabled              bool
	namespaceMode        string
	maxSupportedTTL      time.Duration
	readEfficiencyMin    float64
	readEfficiencyMax    float64
	maxEntriesPerAccount int
	maxEntriesTotal      int
	entryCount           atomic.Int64
	evictionMu           sync.Mutex
	trackedRequests      atomic.Uint64
	cacheHits            atomic.Uint64
	cacheMisses          atomic.Uint64
	cacheReadTokens      atomic.Uint64
	cacheCreationTokens  atomic.Uint64
	cacheSkipped         atomic.Uint64
	stateGeneration      atomic.Uint64
	persistedGeneration  atomic.Uint64
	persistMu            sync.Mutex
	diagnosticMu         sync.Mutex
	missReasons          map[string]uint64
}

type promptCacheStats struct {
	Entries             int               `json:"entries"`
	Accounts            int               `json:"accounts"`
	TrackedRequests     uint64            `json:"trackedRequests"`
	CacheHits           uint64            `json:"cacheHits"`
	CacheMisses         uint64            `json:"cacheMisses"`
	CacheSkipped        uint64            `json:"cacheSkipped"`
	HitRate             float64           `json:"hitRate"`
	CacheReadTokens     uint64            `json:"cacheReadTokens"`
	CacheCreationTokens uint64            `json:"cacheCreationTokens"`
	MissReasons         map[string]uint64 `json:"missReasons,omitempty"`
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
	return newPromptCacheTrackerWithEfficiencyRange(maxTTL, 1, 1)
}

func newPromptCacheTrackerWithSettings(maxTTL time.Duration, readEfficiency float64) *promptCacheTracker {
	return newPromptCacheTrackerWithEfficiencyRange(maxTTL, readEfficiency, readEfficiency)
}

func newPromptCacheTrackerWithEfficiencyRange(maxTTL time.Duration, readEfficiencyMin, readEfficiencyMax float64) *promptCacheTracker {
	if maxTTL <= 0 {
		maxTTL = defaultPromptCacheTTL
	}
	readEfficiencyMin, readEfficiencyMax = normalizeEfficiencyRange(readEfficiencyMin, readEfficiencyMax)
	tracker := &promptCacheTracker{
		enabled:              true,
		namespaceMode:        config.PromptCacheNamespaceAccount,
		maxSupportedTTL:      maxTTL,
		readEfficiencyMin:    readEfficiencyMin,
		readEfficiencyMax:    readEfficiencyMax,
		maxEntriesPerAccount: defaultPromptCacheMaxEntriesPerAccount,
		maxEntriesTotal:      defaultPromptCacheMaxEntriesTotal,
		missReasons:          make(map[string]uint64),
	}
	for i := range tracker.shards {
		tracker.shards[i].entriesByAccount = make(map[string]map[[32]byte]*promptCacheEntry)
		tracker.shards[i].accountOrder = make(map[string]*list.List)
		tracker.shards[i].order = list.New()
	}
	return tracker
}

func (t *promptCacheTracker) ConfigurePolicy(enabled bool, namespaceMode string) {
	if t == nil {
		return
	}
	if namespaceMode != config.PromptCacheNamespaceAccountAPIKey {
		namespaceMode = config.PromptCacheNamespaceAccount
	}
	t.settingsMu.Lock()
	clearState := (t.enabled && !enabled) || t.namespaceMode != namespaceMode
	t.enabled = enabled
	t.namespaceMode = namespaceMode
	t.settingsMu.Unlock()
	if clearState {
		t.Clear()
	}
}

func (t *promptCacheTracker) ScopeKey(accountID, apiKeyID string) string {
	if t == nil || accountID == "" {
		return ""
	}
	t.settingsMu.RLock()
	mode := t.namespaceMode
	t.settingsMu.RUnlock()
	if mode == config.PromptCacheNamespaceAccountAPIKey && apiKeyID != "" {
		return accountID + "\x00" + apiKeyID
	}
	return accountID
}

func (t *promptCacheTracker) ConfigureLimits(maxEntriesPerAccount, maxEntriesTotal int) {
	if t == nil {
		return
	}
	if maxEntriesPerAccount <= 0 {
		maxEntriesPerAccount = defaultPromptCacheMaxEntriesPerAccount
	}
	if maxEntriesTotal <= 0 {
		maxEntriesTotal = defaultPromptCacheMaxEntriesTotal
	}
	if maxEntriesTotal < maxEntriesPerAccount {
		maxEntriesTotal = maxEntriesPerAccount
	}
	t.settingsMu.Lock()
	t.maxEntriesPerAccount = maxEntriesPerAccount
	t.maxEntriesTotal = maxEntriesTotal
	t.settingsMu.Unlock()
	t.enforceAllAccountLimits(maxEntriesPerAccount)
	t.enforceGlobalLimit(maxEntriesTotal)
}

func (t *promptCacheTracker) Configure(maxTTL time.Duration, readEfficiency float64) {
	t.ConfigureEfficiencyRange(maxTTL, readEfficiency, readEfficiency)
}

func (t *promptCacheTracker) ConfigureEfficiencyRange(maxTTL time.Duration, readEfficiencyMin, readEfficiencyMax float64) {
	if t == nil {
		return
	}
	if maxTTL <= 0 {
		maxTTL = defaultPromptCacheTTL
	}
	readEfficiencyMin, readEfficiencyMax = normalizeEfficiencyRange(readEfficiencyMin, readEfficiencyMax)
	t.settingsMu.Lock()
	t.maxSupportedTTL = maxTTL
	t.readEfficiencyMin = readEfficiencyMin
	t.readEfficiencyMax = readEfficiencyMax
	t.settingsMu.Unlock()
	t.clampEntryTTLs(maxTTL, time.Now())
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int) *promptCacheProfile {
	if t == nil {
		return nil
	}
	t.settingsMu.RLock()
	enabled := t.enabled
	t.settingsMu.RUnlock()
	if !enabled {
		return nil
	}
	blocks := flattenClaudeCacheBlocks(req)
	if len(blocks) == 0 {
		return nil
	}

	hasher := sha256.New()
	breakpoints := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	var activeTTL time.Duration

	for _, block := range blocks {
		canonical := canonicalizeCacheValue(normalizeCacheFingerprintValue(block.Value))
		writeHashChunk(hasher, canonical)
		cumulativeTokens += block.Tokens

		// Determine whether this block acts as a cache breakpoint:
		//   1) Explicit cache_control on the block itself.
		//   2) Once any explicit breakpoint has been seen, every message-end
		//      boundary becomes an implicit breakpoint so that multi-turn
		//      conversations can hit earlier stored prefixes.
		breakpointTTL := time.Duration(0)
		if block.TTL > 0 {
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if block.IsMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		var fingerprint [32]byte
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
		})
	}

	if len(breakpoints) == 0 {
		return nil
	}

	if totalInputTokens < cumulativeTokens {
		totalInputTokens = cumulativeTokens
	}

	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            req.Model,
	}
}

func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) promptCacheUsage {
	usage, _ := t.ComputeDetailed(accountID, profile)
	return usage
}

func (t *promptCacheTracker) ComputeDetailed(accountID string, profile *promptCacheProfile) (promptCacheUsage, promptCacheDiagnostic) {
	diagnostic := promptCacheDiagnostic{Status: "skipped", Source: "local"}
	if t == nil {
		diagnostic.Reason = "tracker_unavailable"
		return promptCacheUsage{}, diagnostic
	}
	if profile == nil || len(profile.Breakpoints) == 0 {
		diagnostic.Reason = "no_cache_breakpoint"
		return promptCacheUsage{}, diagnostic
	}
	if accountID == "" {
		diagnostic.Reason = "missing_cache_scope"
		return promptCacheUsage{}, diagnostic
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	lastTokens := minInt(last.CumulativeTokens, profile.TotalInputTokens)
	diagnostic.EligibleInputTokens = lastTokens
	now := time.Now()

	readEfficiencyMin, readEfficiencyMax := t.efficiencyRange()
	shard := t.shardFor(accountID)
	shard.mu.Lock()
	expiredMatch := false
	for _, breakpoint := range profile.Breakpoints {
		if entry, ok := shard.entriesByAccount[accountID][breakpoint.Fingerprint]; ok && !entry.ExpiresAt.After(now) {
			expiredMatch = true
			break
		}
	}
	t.pruneExpiredAccountLocked(shard, accountID, now)
	entries := shard.entriesByAccount[accountID]
	if lastTokens < minTokens {
		shard.mu.Unlock()
		diagnostic.Reason = "below_minimum_tokens"
		return promptCacheUsage{}, diagnostic
	}

	rawMatchedTokens := 0
	var matchedFingerprint [32]byte
	for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
		breakpoint := profile.Breakpoints[i]
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[breakpoint.Fingerprint]
		if !ok || entry.ExpiresAt.Before(now) {
			continue
		}
		rawMatchedTokens = minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if rawMatchedTokens > lastTokens {
			rawMatchedTokens = lastTokens
		}
		matchedFingerprint = breakpoint.Fingerprint
		break
	}
	namespaceEntries := len(entries)
	shard.mu.Unlock()
	if rawMatchedTokens == 0 {
		diagnostic.Status = "miss"
		if expiredMatch {
			diagnostic.Reason = "expired"
		} else if namespaceEntries == 0 {
			diagnostic.Reason = "empty_namespace"
		} else {
			diagnostic.Reason = "prefix_not_found"
		}
	}

	readEfficiency := deterministicPromptCacheEfficiency(readEfficiencyMin, readEfficiencyMax, accountID, matchedFingerprint, now)
	matchedTokens := int(math.Round(float64(rawMatchedTokens) * readEfficiency))
	if matchedTokens > lastTokens {
		matchedTokens = lastTokens
	}

	// Read efficiency only controls how much of an existing prefix is reported
	// as a cache read. The unread part remains ordinary input; it must not be
	// reported as a new cache creation on every exact hit.
	creation := maxInt(lastTokens-rawMatchedTokens, 0)
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, rawMatchedTokens)
	usage := promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       matchedTokens,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}
	diagnostic.MatchedInputTokens = rawMatchedTokens
	if rawMatchedTokens > 0 {
		diagnostic.ReadEfficiency = float64(matchedTokens) / float64(rawMatchedTokens)
		if creation > 0 {
			diagnostic.Status = "partial_hit"
		} else {
			diagnostic.Status = "hit"
		}
		if matchedTokens == 0 {
			diagnostic.Reason = "read_efficiency_zero"
		}
	}
	return usage, diagnostic
}

func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	now := time.Now()
	maxTTL, maxPerAccount, maxTotal := t.settingsSnapshot()
	shard := t.shardFor(accountID)
	shard.mu.Lock()
	t.pruneExpiredAccountLocked(shard, accountID, now)
	updated := false
	for _, breakpoint := range profile.Breakpoints {
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		ttl := effectivePromptCacheTTL(maxTTL, breakpoint.TTL)
		t.putEntryLocked(shard, accountID, breakpoint.Fingerprint, now.Add(ttl), ttl, now)
		updated = true
	}
	t.enforceAccountLimitLocked(shard, accountID, maxPerAccount)
	shard.mu.Unlock()
	if updated {
		t.markStateChanged()
	}
	t.enforceGlobalLimit(maxTotal)
}

func effectivePromptCacheTTL(maxTTL, requestedTTL time.Duration) time.Duration {
	if requestedTTL <= 0 {
		requestedTTL = defaultPromptCacheTTL
	}
	if maxTTL > 0 && maxTTL < requestedTTL {
		return maxTTL
	}
	return requestedTTL
}

func (t *promptCacheTracker) efficiencyRange() (float64, float64) {
	t.settingsMu.RLock()
	defer t.settingsMu.RUnlock()
	return t.readEfficiencyMin, t.readEfficiencyMax
}

func (t *promptCacheTracker) settingsSnapshot() (time.Duration, int, int) {
	t.settingsMu.RLock()
	defer t.settingsMu.RUnlock()
	return t.maxSupportedTTL, t.maxEntriesPerAccount, t.maxEntriesTotal
}

func (t *promptCacheTracker) shardFor(accountID string) *promptCacheShard {
	hash := uint32(2166136261)
	for i := 0; i < len(accountID); i++ {
		hash ^= uint32(accountID[i])
		hash *= 16777619
	}
	return &t.shards[hash%promptCacheShardCount]
}

func (t *promptCacheTracker) putEntryLocked(shard *promptCacheShard, accountID string, fingerprint [32]byte, expiresAt time.Time, ttl time.Duration, lastAccess time.Time) {
	if shard.entriesByAccount == nil {
		shard.entriesByAccount = make(map[string]map[[32]byte]*promptCacheEntry)
	}
	if shard.accountOrder == nil {
		shard.accountOrder = make(map[string]*list.List)
	}
	if shard.order == nil {
		shard.order = list.New()
	}
	entries := shard.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[[32]byte]*promptCacheEntry)
		shard.entriesByAccount[accountID] = entries
	}
	accountOrder := shard.accountOrder[accountID]
	if accountOrder == nil {
		accountOrder = list.New()
		shard.accountOrder[accountID] = accountOrder
	}
	if entry := entries[fingerprint]; entry != nil {
		entry.ExpiresAt = expiresAt
		entry.TTL = ttl
		entry.LastAccess = lastAccess
		if entry.accountElem != nil {
			accountOrder.MoveToFront(entry.accountElem)
		}
		if entry.shardElem != nil {
			shard.order.MoveToFront(entry.shardElem)
		}
		return
	}
	entry := &promptCacheEntry{
		Scope:       accountID,
		Fingerprint: fingerprint,
		ExpiresAt:   expiresAt,
		TTL:         ttl,
		LastAccess:  lastAccess,
	}
	entry.accountElem = accountOrder.PushFront(entry)
	entry.shardElem = shard.order.PushFront(entry)
	entries[fingerprint] = entry
	t.entryCount.Add(1)
}

func (t *promptCacheTracker) removeEntryLocked(shard *promptCacheShard, accountID string, fingerprint [32]byte, expected *promptCacheEntry) bool {
	entries := shard.entriesByAccount[accountID]
	entry := entries[fingerprint]
	if entry == nil || (expected != nil && entry != expected) {
		return false
	}
	if order := shard.accountOrder[accountID]; order != nil && entry.accountElem != nil {
		order.Remove(entry.accountElem)
	}
	if shard.order != nil && entry.shardElem != nil {
		shard.order.Remove(entry.shardElem)
	}
	delete(entries, fingerprint)
	entry.accountElem = nil
	entry.shardElem = nil
	t.entryCount.Add(-1)
	t.markStateChanged()
	if len(entries) == 0 {
		delete(shard.entriesByAccount, accountID)
		delete(shard.accountOrder, accountID)
	}
	return true
}

func (t *promptCacheTracker) pruneExpiredAccountLocked(shard *promptCacheShard, accountID string, now time.Time) {
	entries := shard.entriesByAccount[accountID]
	for fingerprint, entry := range entries {
		if !entry.ExpiresAt.After(now) {
			t.removeEntryLocked(shard, accountID, fingerprint, entry)
		}
	}
}

func (t *promptCacheTracker) enforceAccountLimitLocked(shard *promptCacheShard, accountID string, maxEntries int) {
	entries := shard.entriesByAccount[accountID]
	for len(entries) > maxEntries {
		order := shard.accountOrder[accountID]
		if order == nil || order.Back() == nil {
			break
		}
		entry, _ := order.Back().Value.(*promptCacheEntry)
		if entry == nil || !t.removeEntryLocked(shard, accountID, entry.Fingerprint, entry) {
			break
		}
		entries = shard.entriesByAccount[accountID]
	}
}

func (t *promptCacheTracker) enforceAllAccountLimits(maxEntries int) {
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		for accountID := range shard.entriesByAccount {
			t.enforceAccountLimitLocked(shard, accountID, maxEntries)
		}
		shard.mu.Unlock()
	}
}

func (t *promptCacheTracker) enforceGlobalLimit(maxEntries int) {
	if maxEntries <= 0 || t.entryCount.Load() <= int64(maxEntries) {
		return
	}
	t.evictionMu.Lock()
	defer t.evictionMu.Unlock()

	for t.entryCount.Load() > int64(maxEntries) {
		oldestShard := -1
		var oldestEntry *promptCacheEntry
		var oldestTime time.Time
		found := false
		for i := range t.shards {
			shard := &t.shards[i]
			shard.mu.Lock()
			var entry *promptCacheEntry
			if shard.order != nil && shard.order.Back() != nil {
				entry, _ = shard.order.Back().Value.(*promptCacheEntry)
			}
			if entry != nil && (!found || entry.LastAccess.Before(oldestTime)) {
				oldestShard = i
				oldestEntry = entry
				oldestTime = entry.LastAccess
				found = true
			}
			shard.mu.Unlock()
		}
		if !found {
			return
		}

		shard := &t.shards[oldestShard]
		shard.mu.Lock()
		entry := shard.entriesByAccount[oldestEntry.Scope][oldestEntry.Fingerprint]
		if entry == oldestEntry && entry.LastAccess.Equal(oldestTime) {
			t.removeEntryLocked(shard, entry.Scope, entry.Fingerprint, entry)
		}
		shard.mu.Unlock()
	}
}

func (t *promptCacheTracker) PruneExpired(now time.Time) {
	if t == nil {
		return
	}
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		for accountID := range shard.entriesByAccount {
			t.pruneExpiredAccountLocked(shard, accountID, now)
		}
		shard.mu.Unlock()
	}
}

func (t *promptCacheTracker) clampEntryTTLs(maxTTL time.Duration, now time.Time) {
	if maxTTL <= 0 {
		return
	}
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		changed := false
		for _, entries := range shard.entriesByAccount {
			for _, entry := range entries {
				if entry.TTL <= maxTTL {
					continue
				}
				entry.TTL = maxTTL
				if deadline := now.Add(maxTTL); entry.ExpiresAt.After(deadline) {
					entry.ExpiresAt = deadline
				}
				changed = true
			}
		}
		shard.mu.Unlock()
		if changed {
			t.markStateChanged()
		}
	}
}

func (t *promptCacheTracker) entryCountValue() int {
	if t == nil {
		return 0
	}
	return int(t.entryCount.Load())
}

func (t *promptCacheTracker) accountEntryCount(accountID string) int {
	if t == nil || accountID == "" {
		return 0
	}
	shard := t.shardFor(accountID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return len(shard.entriesByAccount[accountID])
}

func (t *promptCacheTracker) entry(accountID string, fingerprint [32]byte) (promptCacheEntry, bool) {
	if t == nil || accountID == "" {
		return promptCacheEntry{}, false
	}
	shard := t.shardFor(accountID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	entry, ok := shard.entriesByAccount[accountID][fingerprint]
	if !ok || entry == nil {
		return promptCacheEntry{}, false
	}
	return *entry, true
}

func (t *promptCacheTracker) RecordUsage(usage promptCacheUsage, tracked bool) {
	if t == nil || !tracked {
		return
	}
	diagnostic := promptCacheDiagnostic{Status: "miss", Source: "local"}
	if usage.CacheReadInputTokens > 0 {
		diagnostic.Status = "hit"
	}
	t.RecordDecision(usage, diagnostic)
}

func (t *promptCacheTracker) RecordDecision(usage promptCacheUsage, diagnostic promptCacheDiagnostic) {
	if t == nil {
		return
	}
	if diagnostic.Status == "skipped" || diagnostic.Status == "" {
		t.cacheSkipped.Add(1)
		t.recordMissReason(diagnostic.Reason)
		return
	}
	t.trackedRequests.Add(1)
	if usage.CacheReadInputTokens > 0 {
		t.cacheHits.Add(1)
		t.cacheReadTokens.Add(uint64(usage.CacheReadInputTokens))
	} else {
		t.cacheMisses.Add(1)
		t.recordMissReason(diagnostic.Reason)
	}
	if usage.CacheCreationInputTokens > 0 {
		t.cacheCreationTokens.Add(uint64(usage.CacheCreationInputTokens))
	}
}

func (t *promptCacheTracker) recordMissReason(reason string) {
	reason = strings.TrimSpace(reason)
	if t == nil || reason == "" {
		return
	}
	t.diagnosticMu.Lock()
	if t.missReasons == nil {
		t.missReasons = make(map[string]uint64)
	}
	t.missReasons[reason]++
	t.diagnosticMu.Unlock()
}

func (t *promptCacheTracker) Stats() promptCacheStats {
	if t == nil {
		return promptCacheStats{}
	}
	stats := promptCacheStats{
		Entries:             t.entryCountValue(),
		TrackedRequests:     t.trackedRequests.Load(),
		CacheHits:           t.cacheHits.Load(),
		CacheMisses:         t.cacheMisses.Load(),
		CacheSkipped:        t.cacheSkipped.Load(),
		CacheReadTokens:     t.cacheReadTokens.Load(),
		CacheCreationTokens: t.cacheCreationTokens.Load(),
	}
	t.diagnosticMu.Lock()
	stats.MissReasons = make(map[string]uint64, len(t.missReasons))
	for reason, count := range t.missReasons {
		stats.MissReasons[reason] = count
	}
	t.diagnosticMu.Unlock()
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		stats.Accounts += len(shard.entriesByAccount)
		shard.mu.Unlock()
	}
	if stats.TrackedRequests > 0 {
		stats.HitRate = float64(stats.CacheHits) / float64(stats.TrackedRequests)
	}
	return stats
}

func (t *promptCacheTracker) Clear() {
	if t == nil {
		return
	}
	for i := range t.shards {
		t.shards[i].mu.Lock()
	}
	for i := range t.shards {
		t.shards[i].entriesByAccount = make(map[string]map[[32]byte]*promptCacheEntry)
		t.shards[i].accountOrder = make(map[string]*list.List)
		t.shards[i].order = list.New()
	}
	t.entryCount.Store(0)
	for i := len(t.shards) - 1; i >= 0; i-- {
		t.shards[i].mu.Unlock()
	}
	t.trackedRequests.Store(0)
	t.cacheHits.Store(0)
	t.cacheMisses.Store(0)
	t.cacheReadTokens.Store(0)
	t.cacheCreationTokens.Store(0)
	t.cacheSkipped.Store(0)
	t.diagnosticMu.Lock()
	t.missReasons = make(map[string]uint64)
	t.diagnosticMu.Unlock()
	t.markStateChanged()
}

type cacheablePromptBlock struct {
	Value        interface{}
	Tokens       int
	TTL          time.Duration
	IsMessageEnd bool
}

func flattenClaudeCacheBlocks(req *ClaudeRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		blocks = append(blocks, cacheablePromptBlock{
			Value:  fingerprintValue,
			Tokens: estimateCacheValueTokens(fingerprintValue),
			TTL:    normalizePromptCacheTTL(extractPromptCacheTTL(tool)),
		})
	}

	appendSystemCacheBlocks(&blocks, req.System)

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg)
	}

	return blocks
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
	}
	return cacheablePromptBlock{
		Value:  prelude,
		Tokens: estimateCacheValueTokens(prelude),
	}
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, false)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, false)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, false)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage) {
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true)
		}
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isMessageEnd bool) {
	blockValue := wrapper["block"]
	ttl := normalizePromptCacheTTL(extractPromptCacheTTL(blockValue))

	// Drop volatile billing metadata from the cache fingerprint. Claude Code's
	// x-anthropic-billing-header can drift, appear, or disappear across
	// otherwise identical requests, and it does not change model semantics.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	fingerprintValue := stripCachePositionKeys(wrapper)
	*blocks = append(*blocks, cacheablePromptBlock{
		Value:        fingerprintValue,
		Tokens:       estimateCacheValueTokens(fingerprintValue),
		TTL:          ttl,
		IsMessageEnd: isMessageEnd,
	})
}

func estimateCacheValueTokens(value interface{}) int {
	switch v := value.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(v)
	case []interface{}:
		total := 0
		for _, item := range v {
			total += estimateCacheValueTokens(item)
		}
		return total
	case map[string]interface{}:
		if isImageContentBlock(v) {
			return estimateImageContentTokens(v)
		}
		total := 0
		for key, item := range v {
			if key == "cache_control" || isCachePositionKey(key) {
				continue
			}
			total += estimateApproxTokens(key)
			total += estimateCacheValueTokens(item)
		}
		return total
	default:
		return estimateJSONTokens(v)
	}
}

func normalizeCacheFingerprintValue(value interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = normalizeCacheFingerprintValue(item)
		}
		return out
	case map[string]interface{}:
		if isImageContentBlock(v) {
			return cacheImageFingerprint(v)
		}
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			out[key] = normalizeCacheFingerprintValue(item)
		}
		return out
	default:
		return value
	}
}

func cacheImageFingerprint(block map[string]interface{}) map[string]interface{} {
	descriptor := map[string]interface{}{"type": "image"}
	kiroImage := extractImageFromClaudeBlock(block)
	if kiroImage != nil {
		descriptor["format"] = kiroImage.Format
		data := []byte(kiroImage.Source.Bytes)
		if decoded, err := decodeBase64Payload(kiroImage.Source.Bytes); err == nil {
			data = decoded
		}
		digest := sha256.Sum256(data)
		descriptor["sha256"] = hex.EncodeToString(digest[:])
		return descriptor
	}

	raw, _ := json.Marshal(block)
	digest := sha256.Sum256(raw)
	descriptor["sha256"] = hex.EncodeToString(digest[:])
	return descriptor
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	// Only normalize text blocks (or blocks without an explicit type but containing text).
	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}) time.Duration {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}

	if ttl, ok := parsePromptCacheTTLValue(cacheControl["ttl"]); ok {
		return ttl
	}
	return defaultPromptCacheTTL
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func normalizeEfficiencyRange(minValue, maxValue float64) (float64, float64) {
	minValue = clampFloat(minValue, 0, 1)
	maxValue = clampFloat(maxValue, 0, 1)
	if minValue > maxValue {
		return maxValue, minValue
	}
	return minValue, maxValue
}

func deterministicPromptCacheEfficiency(minValue, maxValue float64, accountID string, fingerprint [32]byte, now time.Time) float64 {
	if minValue >= maxValue {
		return minValue
	}

	// Keep retries and identical requests stable within a five-minute window
	// while still distributing values across the configured range.
	hash := uint64(1469598103934665603)
	for i := 0; i < len(accountID); i++ {
		hash ^= uint64(accountID[i])
		hash *= 1099511628211
	}
	for _, value := range fingerprint {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	bucket := uint64(now.Unix() / int64(defaultPromptCacheTTL/time.Second))
	for i := 0; i < 8; i++ {
		hash ^= uint64(byte(bucket >> (8 * i)))
		hash *= 1099511628211
	}
	fraction := float64(hash>>11) / float64(uint64(1)<<53)
	return minValue + fraction*(maxValue-minValue)
}

func computePromptCacheTTLBreakdown(profile *promptCacheProfile, matchedTokens int) (int, int) {
	if profile == nil || len(profile.Breakpoints) == 0 {
		return 0, 0
	}

	cache5m := 0
	cache1h := 0
	previous := matchedTokens
	for _, breakpoint := range profile.Breakpoints {
		current := minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if current <= previous {
			continue
		}
		delta := current - previous
		if breakpoint.TTL >= time.Hour {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

func billedClaudeInputTokens(inputTokens int, usage promptCacheUsage) int {
	return maxInt(inputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens, 0)
}

// reconcilePromptCacheUsage clamps estimated cache usage to the final input
// token count while preserving the creation/read ratio and TTL breakdown.
func reconcilePromptCacheUsage(usage promptCacheUsage, inputTokens int) promptCacheUsage {
	if inputTokens <= 0 {
		return promptCacheUsage{}
	}

	usage.CacheCreationInputTokens = maxInt(usage.CacheCreationInputTokens, 0)
	usage.CacheReadInputTokens = maxInt(usage.CacheReadInputTokens, 0)
	cacheTokens := usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	if cacheTokens > inputTokens {
		creation := int(math.Round(float64(usage.CacheCreationInputTokens) * float64(inputTokens) / float64(cacheTokens)))
		creation = minInt(maxInt(creation, 0), inputTokens)
		usage.CacheCreationInputTokens = creation
		usage.CacheReadInputTokens = inputTokens - creation
	}

	creation := usage.CacheCreationInputTokens
	if creation == 0 {
		usage.CacheCreation5mInputTokens = 0
		usage.CacheCreation1hInputTokens = 0
		return usage
	}
	breakdownTotal := maxInt(usage.CacheCreation5mInputTokens, 0) + maxInt(usage.CacheCreation1hInputTokens, 0)
	if breakdownTotal == 0 {
		usage.CacheCreation5mInputTokens = creation
		usage.CacheCreation1hInputTokens = 0
		return usage
	}
	cache5m := int(math.Round(float64(maxInt(usage.CacheCreation5mInputTokens, 0)) * float64(creation) / float64(breakdownTotal)))
	cache5m = minInt(maxInt(cache5m, 0), creation)
	usage.CacheCreation5mInputTokens = cache5m
	usage.CacheCreation1hInputTokens = creation - cache5m
	return usage
}

func resolvePromptCacheUsage(synthetic promptCacheUsage, upstream KiroTokenUsage, inputTokens int, profile *promptCacheProfile) (promptCacheUsage, int) {
	if !upstream.HasCacheBreakdown {
		return reconcilePromptCacheUsage(synthetic, inputTokens), inputTokens
	}

	usage := promptCacheUsage{
		CacheCreationInputTokens:   upstream.CacheCreationInputTokens,
		CacheReadInputTokens:       upstream.CacheReadInputTokens,
		CacheCreation5mInputTokens: upstream.CacheCreation5mTokens,
		CacheCreation1hInputTokens: upstream.CacheCreation1hTokens,
	}
	if usage.CacheCreationInputTokens > 0 && usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens == 0 {
		usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens = computePromptCacheTTLBreakdown(profile, 0)
	}

	if upstream.hasUncachedBreakdown {
		inputTokens = maxInt(upstream.UncachedInputTokens, 0) + maxInt(upstream.CacheReadInputTokens, 0) + maxInt(upstream.CacheCreationInputTokens, 0)
	} else if upstream.InputTokens > 0 {
		inputTokens = upstream.InputTokens
	}
	return reconcilePromptCacheUsage(usage, inputTokens), inputTokens
}

func finalizePromptCacheDiagnostic(diagnostic promptCacheDiagnostic, upstream KiroTokenUsage, usage promptCacheUsage, inputTokens int) promptCacheDiagnostic {
	if upstream.HasCacheBreakdown {
		diagnostic = promptCacheDiagnostic{
			Status:              "miss",
			Reason:              "upstream_no_cache_read",
			Source:              "upstream",
			MatchedInputTokens:  usage.CacheReadInputTokens,
			EligibleInputTokens: inputTokens,
		}
		if usage.CacheReadInputTokens > 0 {
			diagnostic.Status = "hit"
			diagnostic.Reason = ""
			if usage.CacheCreationInputTokens > 0 {
				diagnostic.Status = "partial_hit"
			}
		}
		if inputTokens > 0 {
			diagnostic.ReadEfficiency = float64(usage.CacheReadInputTokens) / float64(inputTokens)
		}
		return diagnostic
	}

	diagnostic.Source = "local"
	if diagnostic.MatchedInputTokens > 0 {
		diagnostic.ReadEfficiency = float64(usage.CacheReadInputTokens) / float64(diagnostic.MatchedInputTokens)
	}
	return diagnostic
}

func buildClaudeUsageMap(inputTokens, outputTokens, thinkingTokens int, usage promptCacheUsage, includeCache bool) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  billedClaudeInputTokens(inputTokens, usage),
		"output_tokens": outputTokens,
	}
	if thinkingTokens > 0 {
		result["thinking_tokens"] = thinkingTokens
	}
	if !includeCache {
		return result
	}
	result["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	result["cache_read_input_tokens"] = usage.CacheReadInputTokens
	result["cache_creation"] = map[string]int{
		"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
		"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
	}
	return result
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
