package proxy

import (
	"kiro-go/config"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestPromptCacheTrackerEnforcesPerAccountAndGlobalLimits(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	tracker.ConfigureLimits(2, 3)

	update := func(account string, marker byte) {
		var fingerprint [32]byte
		fingerprint[0] = marker
		tracker.Update(account, &promptCacheProfile{
			Model:            "claude-sonnet-4.5",
			TotalInputTokens: 2048,
			Breakpoints: []promptCacheBreakpoint{{
				Fingerprint:      fingerprint,
				CumulativeTokens: 2048,
				TTL:              time.Hour,
			}},
		})
	}

	update("acct-1", 1)
	update("acct-1", 2)
	update("acct-1", 3)
	if got := tracker.accountEntryCount("acct-1"); got != 2 {
		t.Fatalf("expected per-account cap of 2, got %d", got)
	}

	update("acct-2", 4)
	update("acct-2", 5)
	if got := tracker.entryCountValue(); got != 3 {
		t.Fatalf("expected global cap of 3, got %d", got)
	}
	for _, account := range []string{"acct-1", "acct-2"} {
		if got := tracker.accountEntryCount(account); got > 2 {
			t.Fatalf("account %s exceeded cap: %d", account, got)
		}
	}
}

func TestPromptCachePolicyAndNamespace(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	request := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{map[string]interface{}{
			"type": "text",
			"text": strings.Repeat("cacheable ", 600),
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		}},
	}

	tracker.ConfigurePolicy(false, config.PromptCacheNamespaceAccountAPIKey)
	if profile := tracker.BuildClaudeProfile(request, 2048); profile != nil {
		t.Fatal("expected disabled simulation to skip profile construction")
	}
	if got := tracker.ScopeKey("acct", "key-a"); got == "acct" {
		t.Fatalf("expected API key isolated scope, got %q", got)
	}

	tracker.ConfigurePolicy(true, config.PromptCacheNamespaceAccount)
	if profile := tracker.BuildClaudeProfile(request, 2048); profile == nil {
		t.Fatal("expected enabled simulation to build a profile")
	}
	if got := tracker.ScopeKey("acct", "key-a"); got != "acct" {
		t.Fatalf("expected account scope, got %q", got)
	}
}

func TestPromptCacheConcurrentShardsRespectLimits(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	tracker.ConfigureLimits(8, 128)
	var wg sync.WaitGroup
	for accountIndex := 0; accountIndex < 32; accountIndex++ {
		accountID := "acct-" + strconv.Itoa(accountIndex)
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for marker := 0; marker < 24; marker++ {
				var fingerprint [32]byte
				fingerprint[0] = byte(index)
				fingerprint[1] = byte(marker)
				profile := &promptCacheProfile{
					Model:            "claude-sonnet-4.5",
					TotalInputTokens: 2048,
					Breakpoints: []promptCacheBreakpoint{{
						Fingerprint:      fingerprint,
						CumulativeTokens: 2048,
						TTL:              time.Hour,
					}},
				}
				tracker.Update(accountID, profile)
				_ = tracker.Compute(accountID, profile)
			}
		}(accountIndex)
	}
	wg.Wait()

	if got := tracker.entryCountValue(); got > 128 {
		t.Fatalf("global cache limit exceeded: %d", got)
	}
	for accountIndex := 0; accountIndex < 32; accountIndex++ {
		if got := tracker.accountEntryCount("acct-" + strconv.Itoa(accountIndex)); got > 8 {
			t.Fatalf("per-account cache limit exceeded: %d", got)
		}
	}
}

func TestPromptCacheReadEfficiencyScalesCacheRead(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.5)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}
	tracker.Update("acct-1", profile)

	full := newPromptCacheTrackerWithSettings(time.Hour, 1)
	full.Update("acct-1", profile)
	fullHit := full.Compute("acct-1", profile)
	scaledHit := tracker.Compute("acct-1", profile)
	expectedRead := (fullHit.CacheReadInputTokens + 1) / 2
	if scaledHit.CacheReadInputTokens != expectedRead {
		t.Fatalf("expected 50%% cache read efficiency, full=%+v scaled=%+v", fullHit, scaledHit)
	}
	if scaledHit.CacheCreationInputTokens != 0 {
		t.Fatalf("expected exact hit to avoid repeated cache creation, got %+v", scaledHit)
	}
	if uncached := billedClaudeInputTokens(profile.TotalInputTokens, scaledHit); uncached <= 0 {
		t.Fatalf("expected the unread cached prefix to remain ordinary input, got %+v", scaledHit)
	}
}

func TestPromptCacheReadEfficiencyRangeScalesWithinBounds(t *testing.T) {
	tracker := newPromptCacheTrackerWithEfficiencyRange(time.Hour, 0.5, 0.6)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}
	tracker.Update("acct-1", profile)

	full := newPromptCacheTrackerWithSettings(time.Hour, 1)
	full.Update("acct-1", profile)
	fullHit := full.Compute("acct-1", profile)
	rangeHit := tracker.Compute("acct-1", profile)
	minRead := int(float64(fullHit.CacheReadInputTokens)*0.5 + 0.5)
	maxRead := int(float64(fullHit.CacheReadInputTokens)*0.6 + 0.5)
	if rangeHit.CacheReadInputTokens < minRead || rangeHit.CacheReadInputTokens > maxRead {
		t.Fatalf("expected cache read within 50-60%% [%d..%d], full=%+v range=%+v", minRead, maxRead, fullHit, rangeHit)
	}
}

func TestPromptCacheDetailedMissReasons(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	fingerprint[0] = 1
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint: fingerprint, CumulativeTokens: 2048, TTL: time.Hour,
		}},
	}

	usage, diagnostic := tracker.ComputeDetailed("acct", profile)
	if usage.CacheReadInputTokens != 0 || diagnostic.Status != "miss" || diagnostic.Reason != "empty_namespace" {
		t.Fatalf("unexpected empty-namespace diagnostic: usage=%+v diagnostic=%+v", usage, diagnostic)
	}
	tracker.RecordDecision(usage, diagnostic)
	stats := tracker.Stats()
	if stats.CacheMisses != 1 || stats.MissReasons["empty_namespace"] != 1 {
		t.Fatalf("unexpected diagnostic stats: %+v", stats)
	}

	short := *profile
	short.TotalInputTokens = 100
	short.Breakpoints = []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 100, TTL: time.Hour}}
	_, diagnostic = tracker.ComputeDetailed("acct", &short)
	if diagnostic.Status != "skipped" || diagnostic.Reason != "below_minimum_tokens" {
		t.Fatalf("unexpected below-threshold diagnostic: %+v", diagnostic)
	}
}

func TestPromptCacheEfficiencyIsStableWithinWindow(t *testing.T) {
	var fingerprint [32]byte
	fingerprint[0] = 42
	now := time.Unix(1_700_000_000, 0)
	first := deterministicPromptCacheEfficiency(0.5, 0.6, "acct-1", fingerprint, now)
	second := deterministicPromptCacheEfficiency(0.5, 0.6, "acct-1", fingerprint, now.Add(time.Minute))
	if first != second {
		t.Fatalf("expected stable efficiency in one window, got %v and %v", first, second)
	}
	if first < 0.5 || first > 0.6 {
		t.Fatalf("efficiency outside configured range: %v", first)
	}
}

func TestPromptCacheConfiguredTTLExpiresEntries(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Nanosecond, 1)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}
	tracker.Update("acct-1", profile)
	time.Sleep(time.Millisecond)

	got := tracker.Compute("acct-1", profile)
	if got.CacheReadInputTokens != 0 {
		t.Fatalf("expected configured TTL to expire cache entry, got %+v", got)
	}
}

func TestPromptCacheGlobalTTLDoesNotExtendRequestedTTL(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 1)
	var fingerprint [32]byte
	fingerprint[0] = 1
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              5 * time.Minute,
		}},
	}

	tracker.Update("acct-1", profile)
	entry, ok := tracker.entry("acct-1", fingerprint)
	if !ok {
		t.Fatal("expected cache entry")
	}
	if entry.TTL != 5*time.Minute {
		t.Fatalf("expected requested 5m TTL to be preserved, got %s", entry.TTL)
	}
}

func TestPromptCacheComputeDoesNotRefreshBeforeSuccess(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 1)
	var fingerprint [32]byte
	fingerprint[0] = 2
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              5 * time.Minute,
		}},
	}

	tracker.Update("acct-1", profile)
	before, ok := tracker.entry("acct-1", fingerprint)
	if !ok {
		t.Fatal("expected cache entry")
	}
	if got := tracker.Compute("acct-1", profile); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache hit, got %+v", got)
	}
	after, ok := tracker.entry("acct-1", fingerprint)
	if !ok {
		t.Fatal("expected cache entry after compute")
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) || !after.LastAccess.Equal(before.LastAccess) {
		t.Fatalf("compute refreshed cache before upstream success: before=%+v after=%+v", before, after)
	}
}

func TestPromptCacheOnlyCreatesNewPrefix(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.5)
	var firstFingerprint, secondFingerprint [32]byte
	firstFingerprint[0] = 1
	secondFingerprint[0] = 2
	initial := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 1200,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      firstFingerprint,
			CumulativeTokens: 1200,
			TTL:              5 * time.Minute,
		}},
	}
	extended := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2000,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: firstFingerprint, CumulativeTokens: 1200, TTL: 5 * time.Minute},
			{Fingerprint: secondFingerprint, CumulativeTokens: 2000, TTL: 5 * time.Minute},
		},
	}

	tracker.Update("acct-1", initial)
	usage := tracker.Compute("acct-1", extended)
	if usage.CacheReadInputTokens != 600 {
		t.Fatalf("expected 50%% read of existing 1200-token prefix, got %+v", usage)
	}
	if usage.CacheCreationInputTokens != 800 {
		t.Fatalf("expected only the new 800-token prefix to be created, got %+v", usage)
	}
}

func TestReconcilePromptCacheUsageClampsToActualInput(t *testing.T) {
	usage := reconcilePromptCacheUsage(promptCacheUsage{
		CacheCreationInputTokens:   900,
		CacheReadInputTokens:       600,
		CacheCreation5mInputTokens: 300,
		CacheCreation1hInputTokens: 600,
	}, 1000)

	if got := usage.CacheCreationInputTokens + usage.CacheReadInputTokens; got != 1000 {
		t.Fatalf("expected cache usage to clamp to actual input, got %d: %+v", got, usage)
	}
	if got := usage.CacheCreation5mInputTokens + usage.CacheCreation1hInputTokens; got != usage.CacheCreationInputTokens {
		t.Fatalf("expected TTL breakdown to equal cache creation, got %d: %+v", got, usage)
	}
	if got := billedClaudeInputTokens(1000, usage) + usage.CacheCreationInputTokens + usage.CacheReadInputTokens; got != 1000 {
		t.Fatalf("expected reconciled input invariant, got %d: %+v", got, usage)
	}
}

func TestResolvePromptCacheUsagePrefersUpstreamBreakdown(t *testing.T) {
	usage, inputTokens := resolvePromptCacheUsage(
		promptCacheUsage{CacheCreationInputTokens: 900, CacheReadInputTokens: 100},
		KiroTokenUsage{
			InputTokens:              1000,
			UncachedInputTokens:      300,
			CacheReadInputTokens:     500,
			CacheCreationInputTokens: 200,
			CacheCreation5mTokens:    150,
			CacheCreation1hTokens:    50,
			HasCacheBreakdown:        true,
			hasUncachedBreakdown:     true,
		},
		1000,
		nil,
	)

	if inputTokens != 1000 || usage.CacheReadInputTokens != 500 || usage.CacheCreationInputTokens != 200 {
		t.Fatalf("expected upstream cache usage to win, input=%d usage=%+v", inputTokens, usage)
	}
	if billedClaudeInputTokens(inputTokens, usage) != 300 {
		t.Fatalf("expected upstream uncached input to be preserved, usage=%+v", usage)
	}
}

func TestPromptCacheStatsAndClear(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	}
	tracker.Update("acct-1", profile)
	tracker.RecordUsage(promptCacheUsage{CacheReadInputTokens: 1200, CacheCreationInputTokens: 100}, true)

	stats := tracker.Stats()
	if stats.Entries != 1 || stats.Accounts != 1 || stats.CacheHits != 1 || stats.CacheReadTokens != 1200 || stats.CacheCreationTokens != 100 {
		t.Fatalf("unexpected cache stats: %+v", stats)
	}
	tracker.Clear()
	if stats = tracker.Stats(); stats.Entries != 0 || stats.TrackedRequests != 0 {
		t.Fatalf("expected cache state and stats to clear, got %+v", stats)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, 12, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	if got := m["thinking_tokens"]; got != 12 {
		t.Fatalf("expected thinking tokens 12, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}
