package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

const cacheCompatibilityInputTokens = 2000

func cacheCompatibilityPrompt() string {
	return strings.Repeat("stable cache compatibility prefix ", 500)
}

func setupCacheCompatibilityHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "cache compatibility response",
			"usage": map[string]interface{}{
				"input_tokens":                cacheCompatibilityInputTokens,
				"output_tokens":               8,
				"uncached_input_tokens":       cacheCompatibilityInputTokens,
				"cache_read_input_tokens":     0,
				"cache_creation_input_tokens": 0,
			}}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))

	h := setupStreamIntegrityPathTest(t, upstream)
	h.promptCache.ConfigureEfficiencyRange(time.Hour, 0.90, 0.95)
	h.promptCache.ConfigureAccountingMode(config.PromptCacheAccountingAggregatorTarget)
	return h, upstream
}

func cacheCompatibilityClaudeBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":%q}]}`,
		stream, cacheCompatibilityPrompt())
}

func cacheCompatibilityChatBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-4.5","stream":%t,"messages":[{"role":"user","content":%q}]}`,
		stream, cacheCompatibilityPrompt())
}

func cacheCompatibilityResponsesBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-4.5","input":%q,"stream":%t,"store":false}`,
		cacheCompatibilityPrompt(), stream)
}

func assertCacheRatio(t *testing.T, total, read, creation int, cold bool) {
	t.Helper()
	if total <= 0 {
		t.Fatalf("total input tokens = %d, want positive", total)
	}
	if read < 0 || creation < 0 || read+creation > total {
		t.Fatalf("invalid input buckets: total=%d read=%d creation=%d", total, read, creation)
	}
	if cold {
		if read != 0 {
			t.Fatalf("cold request reported synthetic cache read: total=%d read=%d creation=%d", total, read, creation)
		}
		return
	}
	ratio := float64(read) / float64(total)
	if ratio < 0.90 || ratio > 0.95 {
		t.Fatalf("warm cache read ratio = %.4f, want 0.90-0.95 (total=%d read=%d creation=%d)", ratio, total, read, creation)
	}
}

func claudeUsageFromStream(t *testing.T, body string) ClaudeUsage {
	t.Helper()
	data := sseEventDataForCacheTest(t, body, "message_delta")
	var event struct {
		Usage ClaudeUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode Claude terminal usage: %v data=%s", err, data)
	}
	return event.Usage
}

func openAIUsageFromStream(t *testing.T, body string) OpenAIUsage {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(strings.TrimPrefix(line, "data: ")) == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *OpenAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.PromptTokensDetails != nil {
			return *chunk.Usage
		}
	}
	t.Fatalf("OpenAI terminal usage not found in stream: %s", body)
	return OpenAIUsage{}
}

func responsesUsageFromStream(t *testing.T, body string) ResponsesUsage {
	t.Helper()
	data := sseEventDataForCacheTest(t, body, "response.completed")
	var event struct {
		Response ResponsesObject `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode Responses terminal usage: %v data=%s", err, data)
	}
	return event.Response.Usage
}

func sseEventDataForCacheTest(t *testing.T, body, eventName string) []byte {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "event: "+eventName {
			continue
		}
		for _, next := range lines[i+1:] {
			next = strings.TrimSpace(next)
			if strings.HasPrefix(next, "data: ") {
				return []byte(strings.TrimPrefix(next, "data: "))
			}
			if next == "" {
				break
			}
		}
	}
	t.Fatalf("SSE event %q not found: %s", eventName, body)
	return nil
}

func TestCacheCompatibilityClaudeColdWarmAndStream(t *testing.T) {
	h, upstream := setupCacheCompatibilityHandler(t)
	defer upstream.Close()

	cold := httptest.NewRecorder()
	h.handleClaudeMessages(cold, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheCompatibilityClaudeBody(false))))
	if cold.Code != http.StatusOK {
		t.Fatalf("Claude cold request status=%d body=%s", cold.Code, cold.Body.String())
	}
	var coldResponse ClaudeResponse
	if err := json.Unmarshal(cold.Body.Bytes(), &coldResponse); err != nil {
		t.Fatalf("decode Claude cold response: %v body=%s", err, cold.Body.String())
	}
	assertCacheRatio(t, coldResponse.Usage.InputTokens+coldResponse.Usage.CacheReadInputTokens+coldResponse.Usage.CacheCreationInputTokens,
		coldResponse.Usage.CacheReadInputTokens, coldResponse.Usage.CacheCreationInputTokens, true)

	warm := httptest.NewRecorder()
	h.handleClaudeMessages(warm, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheCompatibilityClaudeBody(false))))
	if warm.Code != http.StatusOK {
		t.Fatalf("Claude warm request status=%d body=%s", warm.Code, warm.Body.String())
	}
	var warmResponse ClaudeResponse
	if err := json.Unmarshal(warm.Body.Bytes(), &warmResponse); err != nil {
		t.Fatalf("decode Claude warm response: %v body=%s", err, warm.Body.String())
	}
	assertCacheRatio(t, warmResponse.Usage.InputTokens+warmResponse.Usage.CacheReadInputTokens+warmResponse.Usage.CacheCreationInputTokens,
		warmResponse.Usage.CacheReadInputTokens, warmResponse.Usage.CacheCreationInputTokens, false)

	stream := httptest.NewRecorder()
	h.handleClaudeMessages(stream, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheCompatibilityClaudeBody(true))))
	if stream.Code != http.StatusOK {
		t.Fatalf("Claude stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	usage := claudeUsageFromStream(t, stream.Body.String())
	assertCacheRatio(t, usage.InputTokens+usage.CacheReadInputTokens+usage.CacheCreationInputTokens,
		usage.CacheReadInputTokens, usage.CacheCreationInputTokens, false)
}

func TestCacheCompatibilityOpenAIChatColdWarmAndStream(t *testing.T) {
	h, upstream := setupCacheCompatibilityHandler(t)
	defer upstream.Close()

	request := func(stream bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleOpenAIChat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(cacheCompatibilityChatBody(stream))))
		return rec
	}

	cold := request(false)
	if cold.Code != http.StatusOK {
		t.Fatalf("Chat cold request status=%d body=%s", cold.Code, cold.Body.String())
	}
	var coldResponse OpenAIResponse
	if err := json.Unmarshal(cold.Body.Bytes(), &coldResponse); err != nil {
		t.Fatalf("decode Chat cold response: %v", err)
	}
	assertCacheRatio(t, coldResponse.Usage.PromptTokens, coldResponse.Usage.PromptTokensDetails.CachedTokens,
		coldResponse.Usage.PromptTokensDetails.CacheCreationTokens, true)
	if coldResponse.Usage.PromptTokensDetails.CacheWriteTokens != coldResponse.Usage.PromptTokensDetails.CacheCreationTokens {
		t.Fatalf("OpenAI cold cache write alias mismatch: %+v", coldResponse.Usage.PromptTokensDetails)
	}

	warm := request(false)
	if warm.Code != http.StatusOK {
		t.Fatalf("Chat warm request status=%d body=%s", warm.Code, warm.Body.String())
	}
	var warmResponse OpenAIResponse
	if err := json.Unmarshal(warm.Body.Bytes(), &warmResponse); err != nil {
		t.Fatalf("decode Chat warm response: %v", err)
	}
	assertCacheRatio(t, warmResponse.Usage.PromptTokens, warmResponse.Usage.PromptTokensDetails.CachedTokens,
		warmResponse.Usage.PromptTokensDetails.CacheCreationTokens, false)

	stream := request(true)
	if stream.Code != http.StatusOK {
		t.Fatalf("Chat stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	usage := openAIUsageFromStream(t, stream.Body.String())
	assertCacheRatio(t, usage.PromptTokens, usage.PromptTokensDetails.CachedTokens,
		usage.PromptTokensDetails.CacheCreationTokens, false)
}

func TestCacheCompatibilityResponsesColdWarmAndStream(t *testing.T) {
	h, upstream := setupCacheCompatibilityHandler(t)
	defer upstream.Close()

	request := func(stream bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleOpenAIResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(cacheCompatibilityResponsesBody(stream))))
		return rec
	}

	cold := request(false)
	if cold.Code != http.StatusOK {
		t.Fatalf("Responses cold request status=%d body=%s", cold.Code, cold.Body.String())
	}
	var coldResponse ResponsesObject
	if err := json.Unmarshal(cold.Body.Bytes(), &coldResponse); err != nil {
		t.Fatalf("decode Responses cold response: %v", err)
	}
	assertCacheRatio(t, coldResponse.Usage.InputTokens, coldResponse.Usage.InputTokensDetails.CachedTokens,
		coldResponse.Usage.InputTokensDetails.CacheCreationTokens, true)
	if coldResponse.Usage.InputTokensDetails.CacheWriteTokens != coldResponse.Usage.InputTokensDetails.CacheCreationTokens {
		t.Fatalf("Responses cold cache write alias mismatch: %+v", coldResponse.Usage.InputTokensDetails)
	}

	warm := request(false)
	if warm.Code != http.StatusOK {
		t.Fatalf("Responses warm request status=%d body=%s", warm.Code, warm.Body.String())
	}
	var warmResponse ResponsesObject
	if err := json.Unmarshal(warm.Body.Bytes(), &warmResponse); err != nil {
		t.Fatalf("decode Responses warm response: %v", err)
	}
	assertCacheRatio(t, warmResponse.Usage.InputTokens, warmResponse.Usage.InputTokensDetails.CachedTokens,
		warmResponse.Usage.InputTokensDetails.CacheCreationTokens, false)

	stream := request(true)
	if stream.Code != http.StatusOK {
		t.Fatalf("Responses stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	usage := responsesUsageFromStream(t, stream.Body.String())
	assertCacheRatio(t, usage.InputTokens, usage.InputTokensDetails.CachedTokens,
		usage.InputTokensDetails.CacheCreationTokens, false)
}

func TestNormalizeKiroTokenUsageRepairsNestedCreationAndOverlappingInput(t *testing.T) {
	usage := normalizeKiroTokenUsage(KiroTokenUsage{
		InputTokens:           800,
		UncachedInputTokens:   300,
		CacheReadInputTokens:  500,
		CacheCreation5mTokens: 150,
		CacheCreation1hTokens: 50,
		HasCacheBreakdown:     true,
		hasUncachedBreakdown:  true,
	})
	if usage.InputTokens != 1000 || usage.CacheCreationInputTokens != 200 || usage.UncachedInputTokens != 300 {
		t.Fatalf("inconsistent usage was not repaired: %+v", usage)
	}
	if usage.CacheCreation5mTokens+usage.CacheCreation1hTokens != usage.CacheCreationInputTokens {
		t.Fatalf("nested creation buckets do not reconcile: %+v", usage)
	}
}

func TestResolveInputTokenCountPrefersUsageOverContextPercentage(t *testing.T) {
	got := resolveInputTokenCount(1200, KiroTokenUsage{InputTokens: 1500}, 90000, 1000)
	if got != 1500 {
		t.Fatalf("input token resolver chose context estimate %d, want upstream value 1500", got)
	}
}

func TestNormalizeKiroTokenUsagePreservesExplicitTotalWhenCacheBucketsAreOmitted(t *testing.T) {
	usage := updateTokenUsageFromEvent(map[string]interface{}{
		"usage": map[string]interface{}{
			"input_tokens":          1200,
			"uncached_input_tokens": 0,
		},
	}, KiroTokenUsage{})
	if usage.InputTokens != 1200 || usage.UncachedInputTokens != 0 {
		t.Fatalf("omitted cache buckets erased explicit input total: %+v", usage)
	}
	if usage.hasCacheReadTokens || usage.hasCacheCreationTokens {
		t.Fatalf("omitted cache buckets were marked present: %+v", usage)
	}
}

func TestNormalizeKiroTokenUsageHonorsExplicitZeroCacheBuckets(t *testing.T) {
	usage := updateTokenUsageFromEvent(map[string]interface{}{
		"usage": map[string]interface{}{
			"input_tokens":                1200,
			"uncached_input_tokens":       0,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	}, KiroTokenUsage{})
	if !usage.hasUncachedBreakdown || !usage.hasCacheReadTokens || !usage.hasCacheCreationTokens {
		t.Fatalf("explicit zero cache fields lost their presence: %+v", usage)
	}
	if usage.InputTokens != 1200 {
		t.Fatalf("explicit zero cache fields must not erase the positive input total, got %+v", usage)
	}
}

func TestTokenUsageCandidateMergeIsDeterministic(t *testing.T) {
	event := map[string]interface{}{
		"outputTokens": 3,
		"usage": map[string]interface{}{
			"input_tokens":            900,
			"output_tokens":           4,
			"cache_read_input_tokens": 100,
		},
		"metrics": map[string]interface{}{
			"inputTokens":         700,
			"outputTokens":        1,
			"cacheReadTokens":     50,
			"cacheCreationTokens": 25,
		},
		"metadataEvent": map[string]interface{}{
			"tokenUsage": map[string]interface{}{
				"inputTokens":              1200,
				"outputTokens":             8,
				"uncachedInputTokens":      300,
				"cacheReadInputTokens":     900,
				"cacheCreationInputTokens": 0,
			},
		},
	}

	first := updateTokenUsageFromEvent(event, KiroTokenUsage{})
	for i := 0; i < 100; i++ {
		got := updateTokenUsageFromEvent(event, KiroTokenUsage{})
		if got != first {
			t.Fatalf("usage merge changed between runs: first=%+v run=%d=%+v", first, i, got)
		}
	}
	if first.InputTokens != 1200 || first.OutputTokens != 8 || first.CacheReadInputTokens != 900 ||
		first.CacheCreationInputTokens != 0 || first.UncachedInputTokens != 300 {
		t.Fatalf("canonical tokenUsage was not selected: %+v", first)
	}
}

func TestTokenUsageCandidateDoesNotClearPositiveInputWithCanonicalZero(t *testing.T) {
	got := updateTokenUsageFromEvent(map[string]interface{}{
		"inputTokens": 1200,
		"usage": map[string]interface{}{
			"input_tokens": 0,
		},
	}, KiroTokenUsage{})
	if got.InputTokens != 1200 {
		t.Fatalf("canonical zero input erased a valid positive value: %+v", got)
	}
}

func TestTokenUsageProgressIgnoresTrailingZeroPlaceholders(t *testing.T) {
	current := updateTokenUsageFromEvent(map[string]interface{}{
		"usage": map[string]interface{}{
			"input_tokens":            1200,
			"output_tokens":           4,
			"uncached_input_tokens":   300,
			"cache_read_input_tokens": 900,
		},
	}, KiroTokenUsage{})
	got := updateTokenUsageFromEvent(map[string]interface{}{
		"metrics": map[string]interface{}{
			"inputTokens":         0,
			"outputTokens":        0,
			"uncachedInputTokens": 0,
			"cacheReadTokens":     0,
			"cacheCreationTokens": 0,
		},
	}, current)
	if got.InputTokens != 1200 || got.OutputTokens != 4 || got.UncachedInputTokens != 300 || got.CacheReadInputTokens != 900 {
		t.Fatalf("trailing zero snapshot regressed usage: before=%+v after=%+v", current, got)
	}
}

func TestTokenUsageNormalizationSupportsBreakdownWithoutTotal(t *testing.T) {
	usage := updateTokenUsageFromEvent(map[string]interface{}{
		"tokenUsage": map[string]interface{}{
			"uncached_input_tokens":       300,
			"cache_read_input_tokens":     650,
			"cache_creation_input_tokens": 50,
		},
	}, KiroTokenUsage{})
	if usage.InputTokens != 1000 || usage.UncachedInputTokens != 300 || usage.CacheReadInputTokens != 650 || usage.CacheCreationInputTokens != 50 {
		t.Fatalf("cache breakdown did not supply a stable input total: %+v", usage)
	}
	if !usage.hasInputTokens {
		t.Fatalf("recovered input total lost its presence state: %+v", usage)
	}
}

func TestTokenUsageProgressPreservesLegacyPositiveCacheValue(t *testing.T) {
	current := KiroTokenUsage{InputTokens: 1200, OutputTokens: 40, ThinkingTokens: 12, CacheReadInputTokens: 900, CacheCreationInputTokens: 100}
	got := updateTokenUsageFromEvent(map[string]interface{}{
		"metrics": map[string]interface{}{
			"inputTokens":         0,
			"outputTokens":        0,
			"thinkingTokens":      0,
			"cacheReadTokens":     0,
			"cacheCreationTokens": 0,
		},
	}, current)
	if got.InputTokens != 1200 || got.OutputTokens != 40 || got.ThinkingTokens != 12 || got.CacheReadInputTokens != 900 || got.CacheCreationInputTokens != 100 {
		t.Fatalf("legacy positive cache snapshot was erased by zero telemetry: before=%+v after=%+v", current, got)
	}
}

func TestWebSearchLoopCacheUsageCompatibilityNonStream(t *testing.T) {
	result := &webSearchLoopResult{
		content:           []ClaudeContentBlock{{Type: "text", Text: "search result"}},
		inputTokens:       2000,
		outputTokens:      20,
		cacheUsage:        promptCacheUsage{CacheReadInputTokens: 1800},
		stopReason:        "end_turn",
		webSearchRequests: 1,
	}
	response := buildWebSearchLoopResponse("claude-sonnet-5", result)
	if response == nil {
		t.Fatal("expected WebSearch response")
	}
	assertCacheRatio(t, response.Usage.InputTokens+response.Usage.CacheReadInputTokens+response.Usage.CacheCreationInputTokens,
		response.Usage.CacheReadInputTokens, response.Usage.CacheCreationInputTokens, false)
}

func TestWebSearchLoopCacheUsageCompatibilityStream(t *testing.T) {
	recorder := httptest.NewRecorder()
	session, err := newWebSearchSSESession(context.Background(), &Handler{}, recorder, "claude-sonnet-5", 2000, newRequestFirstContentTimer(time.Now()))
	if err != nil {
		t.Fatalf("create WebSearch stream session: %v", err)
	}
	session.finish(nil, "end_turn", 2000, 20, 0, promptCacheUsage{CacheReadInputTokens: 1800}, 1)
	session.close()

	data := sseEventDataForCacheTest(t, recorder.Body.String(), "message_delta")
	var event struct {
		Usage ClaudeUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode WebSearch stream usage: %v data=%s", err, data)
	}
	assertCacheRatio(t, event.Usage.InputTokens+event.Usage.CacheReadInputTokens+event.Usage.CacheCreationInputTokens,
		event.Usage.CacheReadInputTokens, event.Usage.CacheCreationInputTokens, false)
}
