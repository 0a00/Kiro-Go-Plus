package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMinCacheableTokensForOpus5(t *testing.T) {
	if got := minCacheableTokensForModel("claude-opus-5"); got != 512 {
		t.Fatalf("Opus 5 cache minimum = %d, want 512", got)
	}
	if got := minCacheableTokensForModel("claude-opus-5-thinking"); got != 512 {
		t.Fatalf("Opus 5 thinking cache minimum = %d, want 512", got)
	}
	if got := minCacheableTokensForModel("claude-opus-4.8"); got != 4096 {
		t.Fatalf("legacy Opus cache minimum = %d, want 4096", got)
	}
}

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

	profile := tracker.BuildClaudeProfile(req, 2048)
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

func buildKiroCacheTestPayload(model, system, current string) *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: current,
		ModelID: model,
		Origin:  "AI_EDITOR",
	}
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: system, ModelID: model, Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
	}
	return payload
}

func TestPromptCacheBuildKiroProfileSupportsUnmarkedFinalPayload(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	model := "claude-sonnet-4.6"
	stableSystem := strings.Repeat("stable project instruction ", 500)
	first := buildKiroCacheTestPayload(model, stableSystem, "first question")
	firstProfile := tracker.BuildKiroProfile(first, estimateKiroPayloadTokens(first))
	if firstProfile == nil {
		t.Fatal("expected a cache profile without explicit cache_control")
	}
	tracker.Update("acct-1", firstProfile)

	second := buildKiroCacheTestPayload(model, stableSystem, "follow-up question")
	second.ConversationState.History = append(second.ConversationState.History,
		KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "first question", ModelID: model, Origin: "AI_EDITOR"}},
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "first answer"}},
	)
	// Keep the historical turn before the current turn, matching the wire
	// layout produced by the translators on the next conversation request.
	second.ConversationState.CurrentMessage.UserInputMessage.Content = "follow-up question"
	secondProfile := tracker.BuildKiroProfile(second, estimateKiroPayloadTokens(second))
	if secondProfile == nil {
		t.Fatal("expected a second final-payload profile")
	}
	usage := tracker.Compute("acct-1", secondProfile)
	if usage.CacheReadInputTokens <= 0 {
		t.Fatalf("expected prior current message to become a reusable history prefix, got %+v", usage)
	}
}

func TestPromptCacheKiroProfileUsesConfiguredTTLForUnmarkedPayload(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := buildKiroCacheTestPayload("claude-sonnet-4.6", strings.Repeat("stable project instruction ", 500), "hello")
	profile := tracker.BuildKiroProfile(payload, estimateKiroPayloadTokens(payload))
	if profile == nil || len(profile.Breakpoints) == 0 {
		t.Fatal("expected an automatically tracked profile")
	}
	tracker.Update("acct-ttl", profile)
	entry, ok := tracker.entry("acct-ttl", profile.Breakpoints[len(profile.Breakpoints)-1].Fingerprint)
	if !ok {
		t.Fatal("expected automatic profile entry")
	}
	if entry.TTL != time.Hour {
		t.Fatalf("automatic profile TTL = %s, want configured one hour", entry.TTL)
	}
}

func TestPromptCacheKiroProfilePreservesClaudeTTL(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{map[string]interface{}{
			"type": "text",
			"text": strings.Repeat("stable system instruction ", 500),
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
				"ttl":  "1h",
			},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	payload := ClaudeToKiro(req, false)
	tracker := newPromptCacheTracker(time.Hour)
	profile := tracker.BuildKiroProfile(payload, estimateKiroPayloadTokens(payload))
	if profile == nil {
		t.Fatal("expected translated Claude profile")
	}
	tracker.Update("acct-ttl", profile)
	entry, ok := tracker.entry("acct-ttl", profile.Breakpoints[len(profile.Breakpoints)-1].Fingerprint)
	if !ok {
		t.Fatal("expected translated profile entry")
	}
	if entry.TTL != time.Hour {
		t.Fatalf("explicit Claude TTL = %s, want one hour", entry.TTL)
	}
}

func TestPromptCacheKiroProfileIgnoresTransportIdentity(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	first := buildKiroCacheTestPayload("claude-sonnet-4.6", strings.Repeat("stable project instruction ", 500), "hello")
	second := buildKiroCacheTestPayload("claude-sonnet-4.6", strings.Repeat("stable project instruction ", 500), "hello")
	first.ConversationState.ConversationID = "conversation-a"
	first.ConversationState.AgentContinuationId = "continuation-a"
	first.ProfileArn = "arn:aws:codewhisperer:profile/a"
	second.ConversationState.ConversationID = "conversation-b"
	second.ConversationState.AgentContinuationId = "continuation-b"
	second.ProfileArn = "arn:aws:codewhisperer:profile/b"
	left := tracker.BuildKiroProfile(first, estimateKiroPayloadTokens(first))
	right := tracker.BuildKiroProfile(second, estimateKiroPayloadTokens(second))
	if left == nil || right == nil || len(left.Breakpoints) != len(right.Breakpoints) {
		t.Fatal("expected equivalent profiles")
	}
	for i := range left.Breakpoints {
		if left.Breakpoints[i].Fingerprint != right.Breakpoints[i].Fingerprint {
			t.Fatalf("transport identity changed cache fingerprint at breakpoint %d", i)
		}
	}
}

func TestPromptCacheKiroProfileKeepsFinalInputCeiling(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := buildKiroCacheTestPayload(
		"claude-sonnet-4.6",
		strings.Repeat("stable project instruction ", 500),
		"current request",
	)
	profile := tracker.BuildKiroProfile(payload, 2048)
	if profile == nil {
		t.Fatal("expected a bounded profile")
	}
	if profile.TotalInputTokens != 2048 {
		t.Fatalf("profile total input was inflated to %d", profile.TotalInputTokens)
	}
	for _, breakpoint := range profile.Breakpoints {
		if breakpoint.CumulativeTokens > profile.TotalInputTokens {
			t.Fatalf("breakpoint exceeds final input ceiling: %+v", breakpoint)
		}
	}
	first := tracker.Compute("acct-ceiling", profile)
	if first.CacheCreationInputTokens <= 0 || first.CacheCreationInputTokens > 2048 {
		t.Fatalf("unexpected bounded creation usage: %+v", first)
	}
}

func TestPromptCacheKiroProfileTokenEstimateMatchesPayload(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	payload := buildKiroCacheTestPayload("claude-sonnet-4.6", strings.Repeat("stable context ", 500), "describe")
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "readFile"
	tool.ToolSpecification.Description = "Read a file from the workspace."
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	current.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
		ToolResults: []KiroToolResult{{
			ToolUseID: "call-1",
			Status:    "success",
			Content:   []KiroResultContent{{Text: strings.Repeat("tool output ", 80)}},
		}},
	}
	payload.ConversationState.History = append(payload.ConversationState.History,
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: strings.Repeat("assistant context ", 80),
			ToolUses: []KiroToolUse{{
				ToolUseID: "call-0",
				Name:      "readFile",
				Input:     map[string]interface{}{"path": "README.md"},
			}},
		}},
	)
	profile := tracker.BuildKiroProfile(payload, estimateKiroPayloadTokens(payload))
	if profile == nil || len(profile.Breakpoints) == 0 {
		t.Fatal("expected profile with semantic payload blocks")
	}
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	want := estimateKiroPayloadTokens(payload)
	if last.CumulativeTokens != want {
		t.Fatalf("profile token estimate=%d, payload estimate=%d", last.CumulativeTokens, want)
	}
}

func TestPromptCacheKiroProfileFingerprintsModelAndOrigin(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	build := func(model, origin string) *promptCacheProfile {
		payload := buildKiroCacheTestPayload(model, strings.Repeat("stable context ", 500), "hello")
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = origin
		return tracker.BuildKiroProfile(payload, estimateKiroPayloadTokens(payload))
	}
	base := build("claude-sonnet-4.6", "AI_EDITOR")
	modelVariant := build("claude-opus-4.8", "AI_EDITOR")
	originVariant := build("claude-sonnet-4.6", "AMAZON_Q")
	if base == nil || modelVariant == nil || originVariant == nil {
		t.Fatal("expected all profiles")
	}
	if base.Breakpoints[0].Fingerprint == modelVariant.Breakpoints[0].Fingerprint {
		t.Fatal("model change must split cache fingerprint")
	}
	baseLast := base.Breakpoints[len(base.Breakpoints)-1]
	originLast := originVariant.Breakpoints[len(originVariant.Breakpoints)-1]
	if baseLast.Fingerprint == originLast.Fingerprint {
		t.Fatal("origin change must split cache fingerprint")
	}
}

func TestPromptCacheBuildKiroProfileCoversOpenAIAndResponsesPayloads(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	long := strings.Repeat("shared coding context ", 500)
	openAI := &OpenAIRequest{
		Model: "claude-sonnet-4.6",
		Messages: []OpenAIMessage{
			{Role: "system", Content: long},
			{Role: "user", Content: "hello"},
		},
	}
	chatPayload := OpenAIToKiro(openAI, false)
	chatProfile := tracker.BuildKiroProfile(chatPayload, estimateKiroPayloadTokens(chatPayload))
	if chatProfile == nil {
		t.Fatal("expected OpenAI Chat payload to produce a cache profile")
	}

	responsesMessages, err := parseResponsesInput(json.RawMessage(`[{
		"type":"message","role":"system","content":"shared coding context shared coding context"
	},{"type":"message","role":"user","content":"hello"}]`))
	if err != nil {
		t.Fatalf("parse Responses input: %v", err)
	}
	responsesMessages[0].Content = long
	responsesPayload := OpenAIToKiro(&OpenAIRequest{Model: "claude-sonnet-4.6", Messages: responsesMessages}, false)
	responsesProfile := tracker.BuildKiroProfile(responsesPayload, estimateKiroPayloadTokens(responsesPayload))
	if responsesProfile == nil {
		t.Fatal("expected Responses-converted payload to produce a cache profile")
	}
}

func TestPromptCacheKiroImageFingerprintDoesNotChargeBase64Length(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	build := func(data string) *KiroPayload {
		payload := buildKiroCacheTestPayload("claude-sonnet-4.6", strings.Repeat("image context ", 500), "describe")
		image := KiroImage{Format: "png"}
		image.Source.Bytes = data
		payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{image}
		return payload
	}
	short := tracker.BuildKiroProfile(build(onePixelPNGBase64), 0)
	long := tracker.BuildKiroProfile(build(onePixelPNGBase64+strings.Repeat("A", 4096)), 0)
	if short == nil || long == nil {
		t.Fatal("expected image payload profiles")
	}
	shortLast := short.Breakpoints[len(short.Breakpoints)-1]
	longLast := long.Breakpoints[len(long.Breakpoints)-1]
	if shortLast.CumulativeTokens != longLast.CumulativeTokens {
		t.Fatalf("image Base64 length changed cache token estimate: short=%d long=%d", shortLast.CumulativeTokens, longLast.CumulativeTokens)
	}
	if shortLast.Fingerprint == longLast.Fingerprint {
		t.Fatal("different image bytes must produce different cache fingerprints")
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

func TestPromptCacheHitProtectsEntryFromLRUEviction(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	tracker.ConfigureLimits(2, 10)
	profile := func(marker byte) *promptCacheProfile {
		var fingerprint [32]byte
		fingerprint[0] = marker
		return &promptCacheProfile{
			Model: "claude-sonnet-4.5", TotalInputTokens: 2048,
			Breakpoints: []promptCacheBreakpoint{{Fingerprint: fingerprint, CumulativeTokens: 2048, TTL: time.Hour}},
		}
	}

	first := profile(1)
	second := profile(2)
	third := profile(3)
	tracker.Update("acct-1", first)
	tracker.Update("acct-1", second)
	if usage := tracker.Compute("acct-1", first); usage.CacheReadInputTokens == 0 {
		t.Fatalf("expected first entry to hit: %+v", usage)
	}
	tracker.Update("acct-1", third)

	if _, ok := tracker.entry("acct-1", first.Breakpoints[0].Fingerprint); !ok {
		t.Fatal("recently hit entry was evicted")
	}
	if _, ok := tracker.entry("acct-1", second.Breakpoints[0].Fingerprint); ok {
		t.Fatal("least recently used entry was retained")
	}
	if _, ok := tracker.entry("acct-1", third.Breakpoints[0].Fingerprint); !ok {
		t.Fatal("new cache entry was not retained")
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

func TestPromptCacheConcurrentColdPopulationConverges(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	tracker.ConfigureLimits(4, 16)
	var fingerprint [32]byte
	fingerprint[0] = 0x42
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	}
	const workers = 64
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer group.Done()
			<-start
			_ = tracker.Compute("account-stampede", profile)
			tracker.Update("account-stampede", profile)
		}()
	}
	close(start)
	group.Wait()

	if got := tracker.accountEntryCount("account-stampede"); got != 1 {
		t.Fatalf("cold concurrent population retained %d entries, want one", got)
	}
	warm := tracker.Compute("account-stampede", profile)
	if warm.CacheReadInputTokens <= 0 || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("warm request did not converge to a read-only hit: %+v", warm)
	}
}

func TestPromptCacheConcurrentColdCreationIsReservedOnce(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	fingerprint[0] = 0x73
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.6",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	}
	const workers = 64
	start := make(chan struct{})
	usages := make(chan promptCacheUsage, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer group.Done()
			<-start
			usage, _ := tracker.ComputeDetailed("account-reservation", profile)
			usages <- usage
		}()
	}
	close(start)
	group.Wait()
	close(usages)

	creationCount := 0
	reservationCount := 0
	for usage := range usages {
		if usage.CacheCreationInputTokens > 0 {
			creationCount++
			if usage.CacheCreationInputTokens != 2048 {
				t.Fatalf("unexpected first creation amount: %+v", usage)
			}
		}
		if usage.reservation != nil {
			reservationCount++
		}
	}
	if creationCount != 1 || reservationCount != 1 {
		t.Fatalf("cold concurrent requests reserved creation %d times, reservations=%d", creationCount, reservationCount)
	}

	tracker.Update("account-reservation", profile)
	warm := tracker.Compute("account-reservation", profile)
	if warm.CacheReadInputTokens <= 0 || warm.CacheCreationInputTokens != 0 {
		t.Fatalf("warm request did not converge: %+v", warm)
	}
}

func TestPromptCacheFailedCreationReservationCanBeReleased(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	fingerprint[0] = 0x74
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.6",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	}
	first, firstDiagnostic := tracker.ComputeDetailed("account-release", profile)
	if first.CacheCreationInputTokens == 0 || first.reservation == nil || firstDiagnostic.Reason != "empty_namespace" {
		t.Fatalf("expected initial creation reservation: usage=%+v diagnostic=%+v", first, firstDiagnostic)
	}
	second, secondDiagnostic := tracker.ComputeDetailed("account-release", profile)
	if second.CacheCreationInputTokens != 0 || secondDiagnostic.Reason != "cache_creation_in_flight" {
		t.Fatalf("expected in-flight miss: usage=%+v diagnostic=%+v", second, secondDiagnostic)
	}

	tracker.ReleaseReservation(first)
	retry, retryDiagnostic := tracker.ComputeDetailed("account-release", profile)
	if retry.CacheCreationInputTokens == 0 || retry.reservation == nil || retryDiagnostic.Reason != "empty_namespace" {
		t.Fatalf("released reservation did not allow retry: usage=%+v diagnostic=%+v", retry, retryDiagnostic)
	}
	tracker.ReleaseReservation(retry)
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

func TestPromptCacheHitRefreshesTTLAndMarksStateChanged(t *testing.T) {
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
	oldAccess := time.Now().Add(-time.Hour)
	oldExpiry := time.Now().Add(time.Minute)
	shard := tracker.shardFor("acct-1")
	shard.mu.Lock()
	entry := shard.entriesByAccount["acct-1"][fingerprint]
	entry.LastAccess = oldAccess
	entry.ExpiresAt = oldExpiry
	shard.mu.Unlock()
	beforeGeneration := tracker.stateGeneration.Load()
	if got := tracker.Compute("acct-1", profile); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache hit, got %+v", got)
	}
	after, ok := tracker.entry("acct-1", fingerprint)
	if !ok {
		t.Fatal("expected cache entry after compute")
	}
	if !after.LastAccess.After(oldAccess) || !after.ExpiresAt.After(oldExpiry) {
		t.Fatalf("cache hit did not refresh TTL/LRU: access=%s expiry=%s", after.LastAccess, after.ExpiresAt)
	}
	if tracker.stateGeneration.Load() <= beforeGeneration {
		t.Fatal("cache hit did not mark persisted state as changed")
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

func TestResolvePromptCacheUsageOfficialActualRequiresUpstream(t *testing.T) {
	synthetic := promptCacheUsage{
		CacheCreationInputTokens: 200,
		CacheReadInputTokens:     700,
		localMatchedInputTokens:  800,
		targetReadRate:           0.9,
		hasTargetReadRate:        true,
	}
	usage, inputTokens := resolvePromptCacheUsageForMode(
		synthetic,
		KiroTokenUsage{},
		1000,
		nil,
		config.PromptCacheAccountingActual,
		0.9,
		0.95,
	)
	if inputTokens != 1000 || usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 {
		t.Fatalf("official actual must not synthesize missing upstream usage, input=%d usage=%+v", inputTokens, usage)
	}
	if got := billedClaudeInputTokens(inputTokens, usage); got != 1000 {
		t.Fatalf("official actual uncached input = %d, want 1000", got)
	}
	diagnostic := finalizePromptCacheDiagnostic(promptCacheDiagnostic{Status: "hit", Source: "local"}, KiroTokenUsage{}, usage, inputTokens)
	if diagnostic.Status != "skipped" || diagnostic.Reason != "upstream_cache_usage_unavailable" || diagnostic.AccountingMode != config.PromptCacheAccountingActual {
		t.Fatalf("unexpected official actual diagnostic: %+v", diagnostic)
	}

	upstream := KiroTokenUsage{InputTokens: 1000, CacheReadInputTokens: 250, CacheCreationInputTokens: 50, HasCacheBreakdown: true}
	usage, _ = resolvePromptCacheUsageForMode(synthetic, upstream, 1000, nil, config.PromptCacheAccountingActual, 0.9, 0.95)
	if usage.CacheReadInputTokens != 250 || usage.CacheCreationInputTokens != 50 || usage.targetApplied {
		t.Fatalf("official actual must preserve upstream buckets: %+v", usage)
	}
}

func TestResolvePromptCacheUsageAggregatorTargetsTotalInput(t *testing.T) {
	synthetic := promptCacheUsage{
		CacheReadInputTokens:    100,
		localMatchedInputTokens: 200,
		targetReadRate:          0.93,
		hasTargetReadRate:       true,
	}
	upstream := KiroTokenUsage{
		InputTokens:          1000,
		CacheReadInputTokens: 150,
		HasCacheBreakdown:    true,
	}
	usage, inputTokens := resolvePromptCacheUsageForMode(
		synthetic,
		upstream,
		1000,
		nil,
		config.PromptCacheAccountingAggregatorTarget,
		0.9,
		0.95,
	)
	if inputTokens != 1000 || usage.CacheReadInputTokens != 930 || usage.CacheCreationInputTokens != 0 {
		t.Fatalf("aggregator target usage = %+v input=%d, want read=930 creation=0 total=1000", usage, inputTokens)
	}
	if got := billedClaudeInputTokens(inputTokens, usage) + usage.CacheCreationInputTokens + usage.CacheReadInputTokens; got != inputTokens {
		t.Fatalf("Anthropic input buckets total %d, want %d", got, inputTokens)
	}
	if !usage.targetApplied || usage.upstreamCacheRead != 150 || !usage.hasUpstreamBreakdown {
		t.Fatalf("expected target and upstream metadata to remain separate: %+v", usage)
	}

	diagnostic := finalizePromptCacheDiagnostic(promptCacheDiagnostic{Status: "hit", MatchedInputTokens: 200}, upstream, usage, inputTokens)
	entry := requestLogEntry{}
	diagnostic.Apply(&entry)
	if entry.CacheAccountingMode != config.PromptCacheAccountingAggregatorTarget || !entry.CacheTargetApplied || entry.CacheUpstreamRead != 150 {
		t.Fatalf("unexpected persisted cache diagnostic: %+v", entry)
	}

	claudeUsage := buildClaudeUsageMap(inputTokens, 10, 0, usage, true)
	if claudeUsage["input_tokens"] != 70 || claudeUsage["cache_read_input_tokens"] != 930 {
		t.Fatalf("unexpected Anthropic usage: %#v", claudeUsage)
	}
	openAIUsage := buildOpenAIUsage(inputTokens, 10, 0, usage)
	if openAIUsage.PromptTokens != 1000 || openAIUsage.TotalTokens != 1010 || openAIUsage.PromptTokensDetails == nil || openAIUsage.PromptTokensDetails.CachedTokens != 930 {
		t.Fatalf("unexpected OpenAI usage: %+v", openAIUsage)
	}
	responsesUsage := buildResponsesUsage(inputTokens, 10, 0, usage)
	if responsesUsage.InputTokens != 1000 || responsesUsage.TotalTokens != 1010 || responsesUsage.InputTokensDetails == nil || responsesUsage.InputTokensDetails.CachedTokens != 930 {
		t.Fatalf("unexpected Responses usage: %+v", responsesUsage)
	}
}

func TestResolvePromptCacheUsageAggregatorPreservesWarmupAndCreation(t *testing.T) {
	miss := promptCacheUsage{
		CacheCreationInputTokens: 800,
		targetReadRate:           0.95,
		hasTargetReadRate:        true,
	}
	usage, _ := resolvePromptCacheUsageForMode(miss, KiroTokenUsage{}, 1000, nil, config.PromptCacheAccountingAggregatorTarget, 0.9, 0.95)
	if usage.targetApplied || usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 800 {
		t.Fatalf("first request must remain a cache creation miss: %+v", usage)
	}

	partial := promptCacheUsage{
		localMatchedInputTokens:    500,
		targetReadRate:             0.95,
		hasTargetReadRate:          true,
		CacheCreationInputTokens:   200,
		CacheCreation5mInputTokens: 200,
		CacheReadInputTokens:       300,
	}
	usage, _ = resolvePromptCacheUsageForMode(partial, KiroTokenUsage{}, 1000, nil, config.PromptCacheAccountingAggregatorTarget, 0.9, 0.95)
	if !usage.targetApplied || usage.CacheCreationInputTokens != 200 || usage.CacheReadInputTokens != 800 {
		t.Fatalf("cache creation must cap the target read bucket: %+v", usage)
	}
	if got := billedClaudeInputTokens(1000, usage) + usage.CacheCreationInputTokens + usage.CacheReadInputTokens; got != 1000 {
		t.Fatalf("partial-hit input buckets total %d, want 1000", got)
	}
}

func TestResolvePromptCacheUsageAggregatorUsesRangeMidpointWithoutLocalProfile(t *testing.T) {
	upstream := KiroTokenUsage{InputTokens: 1000, CacheReadInputTokens: 100, HasCacheBreakdown: true}
	usage, _ := resolvePromptCacheUsageForMode(
		promptCacheUsage{},
		upstream,
		1000,
		nil,
		config.PromptCacheAccountingAggregatorTarget,
		0.9,
		0.94,
	)
	if usage.CacheReadInputTokens != 920 || !usage.targetApplied {
		t.Fatalf("upstream-only warm hit should use range midpoint, got %+v", usage)
	}
}

func TestPromptCacheAggregatorRatioIsConsistentAcrossUsageUpdates(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.91)
	tracker.ConfigureAccountingMode(config.PromptCacheAccountingAggregatorTarget)
	synthetic := promptCacheUsage{
		CacheReadInputTokens:    200,
		localMatchedInputTokens: 400,
		targetReadRate:          0.91,
		hasTargetReadRate:       true,
	}
	startUsage, startInput := tracker.ResolveUsage(synthetic, KiroTokenUsage{}, 1000, nil)
	finalUsage, finalInput := tracker.ResolveUsage(synthetic, KiroTokenUsage{InputTokens: 1000, CacheReadInputTokens: 100, HasCacheBreakdown: true}, 1000, nil)
	if startInput != finalInput || startUsage.CacheReadInputTokens != 910 || finalUsage.CacheReadInputTokens != 910 {
		t.Fatalf("stream usage ratio changed between start and final: start=%+v/%d final=%+v/%d", startUsage, startInput, finalUsage, finalInput)
	}
}

func TestPromptCacheAggregatorAppliesOnlyAfterTrackerWarmup(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.9)
	tracker.ConfigureAccountingMode(config.PromptCacheAccountingAggregatorTarget)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4.6",
		TotalInputTokens: 2000,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      [32]byte{1, 2, 3},
			CumulativeTokens: 2000,
			TTL:              time.Hour,
		}},
	}

	miss, _ := tracker.ComputeDetailed("acct", profile)
	miss, _ = tracker.ResolveUsage(miss, KiroTokenUsage{}, 2000, profile)
	if miss.targetApplied || miss.CacheReadInputTokens != 0 || miss.CacheCreationInputTokens != 2000 {
		t.Fatalf("cold request must remain a creation miss: %+v", miss)
	}

	tracker.Update("acct", profile)
	hit, _ := tracker.ComputeDetailed("acct", profile)
	hit, _ = tracker.ResolveUsage(hit, KiroTokenUsage{}, 2000, profile)
	if !hit.targetApplied || hit.CacheReadInputTokens != 1800 || hit.CacheCreationInputTokens != 0 {
		t.Fatalf("warm request must target 90%% of total input: %+v", hit)
	}
}

func TestPromptCacheDisabledBypassesAggregatorTarget(t *testing.T) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.95)
	tracker.ConfigureAccountingMode(config.PromptCacheAccountingAggregatorTarget)
	tracker.ConfigurePolicy(false, config.PromptCacheNamespaceAccount)
	usage, _ := tracker.ResolveUsage(
		promptCacheUsage{},
		KiroTokenUsage{InputTokens: 1000, CacheReadInputTokens: 100, HasCacheBreakdown: true},
		1000,
		nil,
	)
	if usage.CacheReadInputTokens != 100 || usage.targetApplied || usage.accountingMode != config.PromptCacheAccountingActual {
		t.Fatalf("disabled local cache must preserve actual upstream accounting, got %+v", usage)
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
