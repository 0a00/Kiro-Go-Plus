package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoEndpointOrderStartsWithRuntime(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	endpoints := getSortedEndpoints("auto")
	if len(endpoints) < 4 || endpoints[0].Key != "runtime" || endpoints[1].Key != "kiro" {
		t.Fatalf("unexpected auto endpoint order: %+v", endpoints)
	}
}

func TestGuardedCallKeepsLearnedAutoEndpointAffinity(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("auto"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(true); err != nil {
		t.Fatalf("set fallback: %v", err)
	}
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var runtimeRequests atomic.Int32
	runtimeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "runtime"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer runtimeServer.Close()

	var codeWhispererRequests atomic.Int32
	codeWhispererServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeWhispererRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "preferred"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer codeWhispererServer.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: runtimeServer.URL, Name: "Kiro Runtime"},
		{Key: "codewhisperer", URL: codeWhispererServer.URL, Name: "CodeWhisperer"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	account := &config.Account{ID: "affinity-account", AccessToken: "token"}
	model := "claude-sonnet-5"
	sharedAccountEndpointRoutes.recordSuccess(account.ID, model, kiroEndpoints[1])
	payload := &KiroPayload{requireActionableOutput: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
	var output strings.Builder
	if err := CallKiroAPI(account, payload, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	}); err != nil {
		t.Fatalf("guarded call failed: %v", err)
	}
	if codeWhispererRequests.Load() != 1 || runtimeRequests.Load() != 0 || output.String() != "preferred" {
		t.Fatalf("learned affinity was overridden: runtime=%d codewhisperer=%d output=%q",
			runtimeRequests.Load(), codeWhispererRequests.Load(), output.String())
	}
}

func TestRuntimeEndpointUsesRegionContentTypeTargetAndProfile(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("runtime"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("set fallback: %v", err)
	}

	var contentType, target, profileArn, modelID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		target = r.Header.Get("X-Amz-Target")
		body, _ := io.ReadAll(r.Body)
		var payload KiroPayload
		_ = json.Unmarshal(body, &payload)
		profileArn = payload.ProfileArn
		modelID = payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		Key: "runtime", URL: server.URL, Origin: "AI_EDITOR", Name: "Kiro Runtime",
		AmzTarget:   "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		ContentType: "application/x-amz-json-1.0", RequiresProfileArn: true,
	}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	account := &config.Account{
		ID: "runtime-account", AccessToken: "access", Region: "eu-central-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/test",
	}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	var output strings.Builder
	err := CallKiroAPI(account, payload, &KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }})
	if err != nil {
		t.Fatalf("runtime call failed: %v", err)
	}
	if output.String() != "ok" {
		t.Fatalf("unexpected output: %q", output.String())
	}
	if contentType != "application/x-amz-json-1.0" || target != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
		t.Fatalf("unexpected runtime headers: content-type=%q target=%q", contentType, target)
	}
	if profileArn != account.ProfileArn || modelID != "claude-sonnet-4.6" {
		t.Fatalf("unexpected runtime payload: profile=%q model=%q", profileArn, modelID)
	}

	actual := kiroEndpoint{URL: "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"}
	if got := actual.ResolveURL(account, account.ProfileArn); got != "https://runtime.eu-central-1.kiro.dev/generateAssistantResponse" {
		t.Fatalf("unexpected regional runtime URL: %q", got)
	}
}

func TestCallKiroAPIRejectsHTTP200EmptyResponse(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("runtime")
	_ = config.UpdateEndpointFallback(false)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{})
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorEmptyResponse || !upstreamErr.RetryAcrossAccounts {
		t.Fatalf("expected retryable empty response error, got %#v", err)
	}
}

func TestEmptyResponseRetryExhaustionStillAllowsAccountFailover(t *testing.T) {
	err := newEmptyResponseError("CodeWhisperer", false)
	if err.RetryAcrossEndpoints {
		t.Fatal("exhausted empty response should stop endpoint retries")
	}
	if !err.RetryAcrossAccounts {
		t.Fatal("exhausted empty response should still allow account failover")
	}
}

func TestRetryBudgetErrorDoesNotDuplicateEndpointPrefix(t *testing.T) {
	now := time.Now()
	budget := &upstreamAttemptBudget{
		maxAttempts: 1,
		startedAt:   now,
		now:         func() time.Time { return now },
	}
	if !budget.take() {
		t.Fatal("initial upstream attempt was rejected")
	}
	diagnostics := &eventStreamDiagnostics{}
	diagnostics.record(64, decodedStreamEvent{kind: streamEventUnknown})
	failure := newEmptyResponseErrorWithDiagnostics("AmazonQ", false, diagnostics)
	budget.recordFailure("AmazonQ", failure)

	message := newRetryBudgetError(budget).Error()
	if strings.Contains(message, "from AmazonQ: from AmazonQ:") {
		t.Fatalf("endpoint prefix was duplicated: %s", message)
	}
	if strings.Count(message, "from AmazonQ") != 1 {
		t.Fatalf("unexpected endpoint context: %s", message)
	}
	if !strings.Contains(message, "stream diagnostics: frames=1 payload_bytes=64") {
		t.Fatalf("safe stream diagnostics were lost: %s", message)
	}
}

func TestUpstreamAttemptBudgetBoundsRepeatedEmptyResponses(t *testing.T) {
	budget := &upstreamAttemptBudget{maxEmpty: 2, maxEmptyTotal: 3}

	if retry, exhausted := budget.recordEmpty(); !retry || exhausted {
		t.Fatalf("first empty response = retry=%v exhausted=%v", retry, exhausted)
	}
	if retry, exhausted := budget.recordEmpty(); !retry || exhausted {
		t.Fatalf("second empty response = retry=%v exhausted=%v", retry, exhausted)
	}
	if retry, exhausted := budget.recordEmpty(); retry || !exhausted {
		t.Fatalf("third empty response = retry=%v exhausted=%v", retry, exhausted)
	}
	if got := budget.snapshot().EmptyResponses; got != 3 {
		t.Fatalf("empty response count = %d, want 3", got)
	}
}

func TestEmptyResponseLimitErrorIsRetryBudgetFailure(t *testing.T) {
	budget := &upstreamAttemptBudget{emptyResponses: 6}
	err := newEmptyResponseLimitError(budget, newEmptyResponseError("Kiro Runtime", true))
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorRetryBudget {
		t.Fatalf("unexpected error: %#v", err)
	}
	if shouldRetryAcrossAccounts(err) || shouldRetryAcrossEndpoints(err) {
		t.Fatalf("empty response limit should stop failover: %#v", upstreamErr)
	}
	if !strings.Contains(err.Error(), "after 6 empty responses") {
		t.Fatalf("missing empty response count: %v", err)
	}
}

func TestStripEndpointPrefixTerminatesForNestedPrefix(t *testing.T) {
	message := "HTTP 200: from AmazonQ: from AmazonQ: empty response"
	got := stripEndpointPrefix(message, "AmazonQ")
	if got != "HTTP 200: empty response" {
		t.Fatalf("stripped message = %q, want %q", got, "HTTP 200: empty response")
	}
}

func TestCallKiroAPIRetriesSameEndpointBeforeVisibleOutput(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	preOutputRetries := 1
	retry.PreOutputStreamRetries = &preOutputRetries
	retry.PreOutputRetryBackoffMs = 100
	retry.MaxUpstreamAttempts = 3
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	var requests atomic.Int32
	var invocationIDs []string
	var sdkAttempts []string
	var headerMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		headerMu.Lock()
		invocationIDs = append(invocationIDs, r.Header.Get("Amz-Sdk-Invocation-Id"))
		sdkAttempts = append(sdkAttempts, r.Header.Get("Amz-Sdk-Request"))
		headerMu.Unlock()
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "discarded"})
			_, _ = w.Write(frame[:5])
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "recovered"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Name: "Kiro IDE"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	var responseStarts atomic.Int32
	err := CallKiroAPI(&config.Account{ID: "same-endpoint-retry", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{
		OnResponseStart: func() { responseStarts.Add(1) },
		OnText:          func(text string, _ bool) { output.WriteString(text) },
	})
	if err != nil {
		t.Fatalf("same-endpoint retry failed: %v", err)
	}
	if requests.Load() != 2 || output.String() != "recovered" {
		t.Fatalf("unexpected retry result: requests=%d output=%q", requests.Load(), output.String())
	}
	if responseStarts.Load() != 1 {
		t.Fatalf("response start callback count = %d, want 1", responseStarts.Load())
	}
	if len(invocationIDs) != 2 || invocationIDs[0] == "" || invocationIDs[0] != invocationIDs[1] {
		t.Fatalf("SDK invocation ID was not reused: %v", invocationIDs)
	}
	if !reflect.DeepEqual(sdkAttempts, []string{"attempt=1; max=2", "attempt=2; max=2"}) {
		t.Fatalf("unexpected SDK attempt headers: %v", sdkAttempts)
	}
}

func TestCallKiroAPIRetriesEmptyStreamOnSameEndpoint(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	preOutputRetries := 1
	retry.PreOutputStreamRetries = &preOutputRetries
	retry.PreOutputRetryBackoffMs = 100
	retry.EmptyResponseRetries = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "recovered-empty"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Name: "Kiro IDE"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	err := CallKiroAPI(&config.Account{ID: "same-endpoint-empty", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	})
	if err != nil {
		t.Fatalf("empty-stream retry failed: %v", err)
	}
	if requests.Load() != 2 || output.String() != "recovered-empty" {
		t.Fatalf("unexpected empty retry result: requests=%d output=%q", requests.Load(), output.String())
	}
}

func TestCallKiroAPISwitchesEndpointAfterTelemetryOnlyResponse(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.EmptyResponseRetries = 0
	retry.PreOutputStreamRetries = func() *int { value := 0; return &value }()
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("telemetry-only")
	_ = config.UpdateEndpointFallback(true)

	var firstRequests, secondRequests atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "endpoint-fallback"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer second.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "telemetry-only", URL: first.URL, Name: "Telemetry Only"},
		{Key: "fallback", URL: second.URL, Name: "Fallback"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	err := CallKiroAPI(&config.Account{ID: "telemetry-fallback", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	})
	if err != nil || output.String() != "endpoint-fallback" {
		t.Fatalf("endpoint fallback failed: err=%v output=%q", err, output.String())
	}
	if firstRequests.Load() != 1 || secondRequests.Load() != 1 {
		t.Fatalf("unexpected endpoint requests: telemetry=%d fallback=%d", firstRequests.Load(), secondRequests.Load())
	}
}

func TestCallKiroAPIDoesNotRetryAfterVisibleOutput(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	preOutputRetries := 1
	retry.PreOutputStreamRetries = &preOutputRetries
	retry.PreOutputRetryBackoffMs = 100
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "visible"}))
		frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "tail"})
		_, _ = w.Write(frame[:5])
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Name: "Kiro IDE"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	err := CallKiroAPI(&config.Account{ID: "visible-no-retry", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	})
	if err == nil {
		t.Fatal("expected truncated stream error")
	}
	if requests.Load() != 1 || output.String() != "visible" {
		t.Fatalf("visible output was retried or lost: requests=%d output=%q", requests.Load(), output.String())
	}
}

func TestWaitForPreOutputStreamRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startedAt := time.Now()
	if err := waitForPreOutputStreamRetry(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled retry wait took too long: %s", elapsed)
	}
}

func TestCallKiroAPIStopsToolStreamThatNeverCompletes(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 2
	retry.MaxRetryDurationSeconds = 5
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 15
	retry.ToolAssemblyTimeoutSeconds = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_never_finishes",
			"name":      "Write",
			"input":     `{"content":"`,
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// The watchdog is an idle timeout. Keep this fixture genuinely idle
		// after the partial argument so a stalled tool is still rejected.
		<-r.Context().Done()
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Name: "Kiro IDE"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true}
	startedAt := time.Now()
	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, payload, &KiroStreamCallback{})
	if time.Since(startedAt) > 3*time.Second {
		t.Fatalf("tool assembly timeout took too long: %s", time.Since(startedAt))
	}
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorToolAssemblyTimeout || !upstreamErr.RetryAcrossAccounts {
		t.Fatalf("expected retryable tool assembly timeout, got %#v", err)
	}
	if !strings.Contains(upstreamErr.Error(), `tool "Write" had no argument activity`) {
		t.Fatalf("unexpected tool timeout error: %v", upstreamErr)
	}
}

func TestCallKiroAPIAllowsToolAssemblyThatKeepsReceivingFragments(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 5
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 5
	retry.ToolAssemblyTimeoutSeconds = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("runtime")
	_ = config.UpdateEndpointFallback(false)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeFrame := func(eventType string, payload map[string]interface{}) {
			_, _ = w.Write(awsEventStreamFrame(t, eventType, payload))
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame("toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_growing",
			"name":      "Write",
			"input":     `{"content":"`,
		})
		for i := 0; i < 12; i++ {
			time.Sleep(100 * time.Millisecond)
			writeFrame("toolUseInputEvent", map[string]interface{}{"input": "x"})
		}
		writeFrame("toolUseInputEvent", map[string]interface{}{"input": `"}`})
		writeFrame("toolUseStopEvent", map[string]interface{}{})
		writeFrame("contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0})
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}

	var toolUses []KiroToolUse
	startedAt := time.Now()
	err := CallKiroAPI(&config.Account{ID: "growing-tool-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("active tool stream failed: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("active tool stream was retried: %d requests", requests.Load())
	}
	if elapsed := time.Since(startedAt); elapsed < 1*time.Second {
		t.Fatalf("fixture did not exercise a long tool stream: %s", elapsed)
	}
	if len(toolUses) != 1 || toolUses[0].Name != "Write" || toolUses[0].Input["content"] != "xxxxxxxxxxxx" {
		t.Fatalf("unexpected long tool result: %+v", toolUses)
	}
}

func TestCallKiroAPIRecoversSchemaDeclaredZeroArgumentTool(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 5
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 5
	retry.ToolAssemblyTimeoutSeconds = 2
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("runtime")
	_ = config.UpdateEndpointFallback(false)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_zero",
			"name":      "mcpMemoryReadGraphH123",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := payloadWithTestTool("mcpMemoryReadGraphH123", map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{},
		"additionalProperties": false,
	})
	payload.requireActionableOutput = true
	payload.requireToolUse = true
	payload.deferTextUntilComplete = true
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"

	var toolUses []KiroToolUse
	err := CallKiroAPI(&config.Account{ID: "zero-tool-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("zero-argument tool call failed: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("zero-argument recovery retried upstream %d times", requests.Load())
	}
	if len(toolUses) != 1 || toolUses[0].Name != "mcpMemoryReadGraphH123" || len(toolUses[0].Input) != 0 {
		t.Fatalf("unexpected recovered tool use: %#v", toolUses)
	}
}

func TestCallKiroAPIRotatesEndpointAfterActionableOutputTimeout(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 4
	retry.MaxRetryDurationSeconds = 5
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 15
	retry.ToolAssemblyTimeoutSeconds = 0
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()

	oldTimeoutResolver := resolveLongToolActionableOutputTimeout
	resolveLongToolActionableOutputTimeout = func(*KiroPayload) time.Duration { return 150 * time.Millisecond }
	t.Cleanup(func() { resolveLongToolActionableOutputTimeout = oldTimeoutResolver })

	var stalledCalls atomic.Int32
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_stalled",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"`,
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer stalled.Close()

	var recoveredCalls atomic.Int32
	recovered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recoveredCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_recovered",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"complete"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0}))
	}))
	defer recovered.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "kiro", URL: stalled.URL, Name: "Kiro IDE"},
		{Key: "runtime", URL: recovered.URL, Name: "Kiro Runtime"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true, deferTextUntilComplete: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}

	var toolUses []KiroToolUse
	startedAt := time.Now()
	err := CallKiroAPI(&config.Account{ID: "actionable-timeout-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("expected endpoint recovery, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("actionable timeout recovery took too long: %s", elapsed)
	}
	if stalledCalls.Load() != 1 || recoveredCalls.Load() != 1 {
		t.Fatalf("unexpected endpoint attempts: stalled=%d recovered=%d", stalledCalls.Load(), recoveredCalls.Load())
	}
	if len(toolUses) != 1 || toolUses[0].ToolUseID != "toolu_recovered" || toolUses[0].Input["content"] != "complete" {
		t.Fatalf("partial tool leaked or recovered tool missing: %+v", toolUses)
	}
	status := sharedAccountEndpointRoutes.snapshot()
	cooldowns, _ := status["cooldowns"].([]accountEndpointRouteSnapshot)
	if len(cooldowns) != 1 || cooldowns[0].Workload != "long-tool" || cooldowns[0].Endpoint != "Kiro IDE" {
		t.Fatalf("actionable timeout did not cool only the long-tool route: %+v", status)
	}
}

func TestCallKiroAPIKeepsActionableWatchdogAliveDuringThinking(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 5
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 5
	retry.ToolAssemblyTimeoutSeconds = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("runtime")
	_ = config.UpdateEndpointFallback(false)

	const actionableTimeout = 250 * time.Millisecond
	oldTimeoutResolver := resolveLongToolActionableOutputTimeout
	resolveLongToolActionableOutputTimeout = func(*KiroPayload) time.Duration { return actionableTimeout }
	t.Cleanup(func() { resolveLongToolActionableOutputTimeout = oldTimeoutResolver })

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeFrame := func(eventType string, payload map[string]interface{}) {
			_, _ = w.Write(awsEventStreamFrame(t, eventType, payload))
			if flusher != nil {
				flusher.Flush()
			}
		}
		for i := 0; i < 8; i++ {
			writeFrame("reasoningContentEvent", map[string]interface{}{"text": "planning"})
			time.Sleep(40 * time.Millisecond)
		}
		writeFrame("toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_after_thinking",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"ok"}`,
			"stop":      true,
		})
		writeFrame("contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0})
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true, deferTextUntilComplete: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}

	var toolUses []KiroToolUse
	err := CallKiroAPI(&config.Account{ID: "thinking-tool-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("thinking activity was treated as an actionable timeout: %v", err)
	}
	if requests.Load() != 1 || len(toolUses) != 1 || toolUses[0].Input["content"] != "ok" {
		t.Fatalf("unexpected thinking/tool result: requests=%d tools=%+v", requests.Load(), toolUses)
	}
}

func TestRetryWindowErrorIncludesCurrentTransportFailure(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 0
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 1
	retry.FirstTokenTimeoutSeconds = 5
	retry.StreamIdleTimeoutSeconds = 15
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Name: "Kiro IDE"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	startedAt := time.Now()
	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{})
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("retry window took too long: %s", elapsed)
	}
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorRetryBudget {
		t.Fatalf("expected retry-window error, got %#v", err)
	}
	if !strings.Contains(upstreamErr.Error(), "last failure from Kiro IDE") ||
		!strings.Contains(upstreamErr.Error(), "meaningful response before the timeout") {
		t.Fatalf("retry-window error lost current failure: %v", upstreamErr)
	}
}

func TestCallKiroAPIRetriesToolStreamWithOnlyThinkingAndStructuralTail(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>hidden first attempt\n<html>unfinished"}))
			_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "second attempt"}))
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "done"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{}
	payload.requireActionableOutput = true
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}
	var visible strings.Builder
	var thinking strings.Builder
	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinking.WriteString(text)
				return
			}
			visible.WriteString(text)
		},
	})
	if err != nil {
		t.Fatalf("expected fallback response, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected two endpoint attempts, got %d", got)
	}
	if visible.String() != "done" || thinking.String() != "second attempt" {
		t.Fatalf("invalid first attempt leaked: visible=%q thinking=%q", visible.String(), thinking.String())
	}
}

func TestCallKiroAPIRetriesCodeOnlyResponseWhenToolIsRequired(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "```html\n<html>code only</html>\n```"}))
			_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_write", "name": "Write", "input": `{"file_path":"index.html","content":"<html></html>"}`, "stop": true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true}
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}
	var visible strings.Builder
	var toolUses []KiroToolUse
	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnText:    func(text string, _ bool) { visible.WriteString(text) },
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("expected tool fallback response, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected two endpoint attempts, got %d", got)
	}
	if visible.Len() != 0 || len(toolUses) != 1 || toolUses[0].Name != "Write" {
		t.Fatalf("unexpected required-tool result: visible=%q tools=%+v", visible.String(), toolUses)
	}
}

func TestCallKiroAPIRetriesTruncatedToolWithRecoveryHint(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	longTool := config.GetLongToolConfig()
	longTool.TruncationRetries = 1
	if err := config.UpdateLongToolConfig(longTool); err != nil {
		t.Fatalf("update long-tool config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)

	var requests atomic.Int32
	var sawRecoveryHint atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		var requestPayload KiroPayload
		_ = json.Unmarshal(body, &requestPayload)
		if attempt > 1 && strings.Contains(requestPayload.ConversationState.CurrentMessage.UserInputMessage.Content, toolRecoveryHintMarker) {
			sawRecoveryHint.Store(true)
		}

		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "toolu_truncated",
				"name":      "Write",
				"input":     `{"file_path":"index.html","content":"unfinished`,
			}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_recovered",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"complete"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{requireActionableOutput: true, requireToolUse: true, deferTextUntilComplete: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "Create index.html."
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}
	payload.beginStreamMetrics(time.Now())

	var toolUses []KiroToolUse
	err := CallKiroAPI(&config.Account{ID: "long-tool-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("expected recovered tool call, got %v", err)
	}
	if requests.Load() != 2 || !sawRecoveryHint.Load() {
		t.Fatalf("expected one hinted retry, requests=%d hint=%v", requests.Load(), sawRecoveryHint.Load())
	}
	if len(toolUses) != 1 || toolUses[0].ToolUseID != "toolu_recovered" || toolUses[0].Input["content"] != "complete" {
		t.Fatalf("partial tool leaked or recovered tool missing: %+v", toolUses)
	}
	_, _, argumentBytes, fragments, truncations, recoveries := payload.streamMetrics()
	if argumentBytes == 0 || fragments == 0 || truncations != 1 || recoveries != 1 {
		t.Fatalf("unexpected recovery metrics: bytes=%d fragments=%d truncations=%d recoveries=%d", argumentBytes, fragments, truncations, recoveries)
	}
}

func TestCallKiroAPIStopsWhenClientRequestIsCanceled(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := &KiroPayload{requestContext: ctx}
	err := CallKiroAPI(&config.Account{ID: "a", AccessToken: "token"}, payload, &KiroStreamCallback{})
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorCanceled || upstreamErr.RetryAcrossAccounts || upstreamErr.RetryAcrossEndpoints {
		t.Fatalf("expected non-retryable cancellation, got %#v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("expected no upstream requests after cancellation, got %d", got)
	}
}

func TestEventStreamExceptionIsClassified(t *testing.T) {
	stream := strings.NewReader(string(awsEventStreamFrame(t, "validationException", map[string]interface{}{
		"reason": "INVALID_MODEL_ID", "message": "model is unavailable",
	})))
	err := parseEventStream(stream, &KiroStreamCallback{})
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorModelUnavailable {
		t.Fatalf("expected model-unavailable error, got %#v", err)
	}
}

func TestCallKiroAPIMapsContentLengthExceededToMaxTokens(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("runtime"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "bounded output"}))
		_, _ = w.Write(awsEventStreamExceptionFrame(t, "ContentLengthExceededException", map[string]interface{}{"message": "limit"}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	var stopReason string
	err := CallKiroAPI(&config.Account{ID: "max-token-account", AccessToken: "token"}, &KiroPayload{}, &KiroStreamCallback{
		OnText:       func(text string, _ bool) { output.WriteString(text) },
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil {
		t.Fatalf("max-token endpoint response failed: %v", err)
	}
	if output.String() != "bounded output" || stopReason != "max_tokens" || mapClaudeStopReason(stopReason, 0) != "max_tokens" {
		t.Fatalf("unexpected endpoint completion: output=%q reason=%q", output.String(), stopReason)
	}
}

func TestRuntimeUnknown403FallsBackWithoutMarkingRevoked(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusForbidden, "Kiro Runtime", []byte(`{"message":"Forbidden"}`))
	if err.Kind != UpstreamErrorForbidden || !err.RetryAcrossEndpoints || !err.RetryAcrossAccounts {
		t.Fatalf("unexpected runtime 403 classification: %+v", err)
	}
	legacy := classifyUpstreamHTTPError(http.StatusForbidden, "Kiro IDE", []byte(`{"message":"Forbidden"}`))
	if legacy.Kind != UpstreamErrorForbidden || !legacy.RetryAcrossEndpoints || !legacy.RetryAcrossAccounts {
		t.Fatalf("unexpected legacy 403 classification: %+v", legacy)
	}
	suspended := classifyUpstreamHTTPError(http.StatusForbidden, "Kiro IDE", []byte(`{"message":"Your User ID temporarily is suspended due to unusual user activity"}`))
	if suspended.Kind != UpstreamErrorSuspended || suspended.RetryAcrossEndpoints {
		t.Fatalf("suspension must not fall through to another endpoint: %+v", suspended)
	}
}

func TestCallKiroAPIFallsBackFromLegacyGeneric403ToRuntime(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "runtime-ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-5"
	var output strings.Builder
	err := CallKiroAPI(
		&config.Account{ID: "legacy-403-account", AccessToken: "token"},
		payload,
		&KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }},
	)
	if err != nil {
		t.Fatalf("expected runtime fallback, got %v", err)
	}
	if requests.Load() != 2 || output.String() != "runtime-ok" {
		t.Fatalf("unexpected 403 fallback result: requests=%d output=%q", requests.Load(), output.String())
	}
	endpoints, routeErr := sharedAccountEndpointRoutes.availableEndpoints("legacy-403-account", "claude-sonnet-5", "auto", kiroEndpoints)
	if routeErr != nil || len(endpoints) != 1 || endpoints[0].Key != "runtime" {
		t.Fatalf("expected learned runtime route with legacy cooling, endpoints=%+v err=%v", endpoints, routeErr)
	}
}

func TestCallKiroAPISuspensionDoesNotFallThroughEndpoints(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Your User ID temporarily is suspended due to unusual user activity"}`))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-5"
	err := CallKiroAPI(&config.Account{ID: "suspended-account", AccessToken: "token"}, payload, &KiroStreamCallback{})
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorSuspended {
		t.Fatalf("expected suspension error, got %#v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("suspension fell through to another endpoint: requests=%d", got)
	}
}

func TestQuotaErrorFallsBackAcrossEndpoints(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "Kiro Runtime", []byte(`{"message":"quota exhausted"}`))
	if err.Kind != UpstreamErrorQuota || !err.RetryAcrossEndpoints || !err.RetryAcrossAccounts {
		t.Fatalf("unexpected quota classification: %+v", err)
	}
}

func TestRateLimitErrorFallsBackAcrossEndpoints(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "CodeWhisperer", []byte(`{"message":"too many requests"}`))
	if err.Kind != UpstreamErrorRateLimit || !err.RetryAcrossEndpoints || !err.RetryAcrossAccounts {
		t.Fatalf("unexpected rate-limit classification: %+v", err)
	}
}

func TestCallKiroAPIFallsBackAfterRuntimeQuota(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"quota exhausted"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	var output strings.Builder
	err := CallKiroAPI(
		&config.Account{ID: "a", AccessToken: "token"},
		&KiroPayload{},
		&KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }},
	)
	if err != nil {
		t.Fatalf("expected legacy endpoint fallback, got %v", err)
	}
	if requests.Load() != 2 || output.String() != "ok" {
		t.Fatalf("unexpected fallback result: requests=%d output=%q", requests.Load(), output.String())
	}
}

func TestCallKiroAPIFallsBackAfterEndpointRateLimit(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"too many requests"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{
		{Key: "runtime", URL: server.URL, Name: "Kiro Runtime"},
		{Key: "kiro", URL: server.URL, Name: "Kiro IDE"},
	}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-5"
	var output strings.Builder
	err := CallKiroAPI(
		&config.Account{
			ID:          "rate-limit-account",
			AccessToken: "token",
			ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
		},
		payload,
		&KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }},
	)
	if err != nil {
		t.Fatalf("expected endpoint fallback, got %v", err)
	}
	if requests.Load() != 2 || output.String() != "ok" {
		t.Fatalf("unexpected fallback result: requests=%d output=%q", requests.Load(), output.String())
	}

	endpoints, routeErr := sharedAccountEndpointRoutes.availableEndpoints("rate-limit-account", "claude-sonnet-5", "auto", kiroEndpoints)
	if routeErr != nil || len(endpoints) != 1 || endpoints[0].Key != "kiro" {
		t.Fatalf("expected successful endpoint affinity with runtime cooling, endpoints=%+v err=%v", endpoints, routeErr)
	}
}

func TestParseRetryAfterSupportsSecondsAndHTTPDate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if got := parseRetryAfter("7", now); got != 7*time.Second {
		t.Fatalf("expected 7s Retry-After, got %s", got)
	}
	when := now.Add(11 * time.Second)
	if got := parseRetryAfter(when.Format(http.TimeFormat), now); got != 11*time.Second {
		t.Fatalf("expected HTTP-date Retry-After of 11s, got %s", got)
	}
	if got := parseRetryAfter("invalid", now); got != 0 {
		t.Fatalf("expected invalid Retry-After to be ignored, got %s", got)
	}
}

func TestMapDownstreamErrorPreservesActionableStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{
			name:       "client request",
			err:        &UpstreamError{Kind: UpstreamErrorClientRequest, StatusCode: http.StatusUnprocessableEntity},
			wantStatus: http.StatusUnprocessableEntity,
			wantType:   "invalid_request_error",
		},
		{
			name:       "rate limit",
			err:        &UpstreamError{Kind: UpstreamErrorRateLimit, RetryAfter: 1500 * time.Millisecond},
			wantStatus: http.StatusTooManyRequests,
			wantType:   "rate_limit_error",
		},
		{
			name:       "timeout",
			err:        &UpstreamError{Kind: UpstreamErrorFirstTokenTimeout},
			wantStatus: http.StatusGatewayTimeout,
			wantType:   "server_error",
		},
		{
			name:       "endpoint",
			err:        &UpstreamError{Kind: UpstreamErrorEndpointUnavailable},
			wantStatus: http.StatusBadGateway,
			wantType:   "server_error",
		},
		{
			name:       "upstream credentials",
			err:        &UpstreamError{Kind: UpstreamErrorAuthRevoked},
			wantStatus: http.StatusServiceUnavailable,
			wantType:   "server_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapDownstreamError(tt.err)
			if got.Status != tt.wantStatus || got.OpenAIType != tt.wantType {
				t.Fatalf("got status/type %d/%q, want %d/%q", got.Status, got.OpenAIType, tt.wantStatus, tt.wantType)
			}
			if tt.name == "rate limit" && got.RetryAfter != "2" {
				t.Fatalf("expected rounded Retry-After=2, got %q", got.RetryAfter)
			}
		})
	}
}

func TestTokenRefreshCoordinatorDeduplicatesConcurrentRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID: "refresh-account", Email: "refresh@example.com", Enabled: true,
		AuthMethod: "idc", Region: "us-east-1", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret", AccessToken: "old", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accountpool.GetPool().Reload()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"accessToken":"new","refreshToken":"rotated","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/test"}`))
	}))
	defer server.Close()
	oldURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return server.URL })
	oldClient := auth.SetGlobalAuthClientForTest(&http.Client{Timeout: 5 * time.Second})
	t.Cleanup(func() {
		auth.SetOIDCTokenURLForTest(oldURL)
		auth.SetGlobalAuthClientForTest(oldClient)
	})
	oldCoordinator := sharedTokenRefreshCoordinator
	sharedTokenRefreshCoordinator = &tokenRefreshCoordinator{inFlight: make(map[string]*coordinatedRefreshCall)}
	t.Cleanup(func() { sharedTokenRefreshCoordinator = oldCoordinator })

	start := make(chan struct{})
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := account
			<-start
			errs <- sharedTokenRefreshCoordinator.Refresh(&copy, true)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh failed: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected one upstream refresh request, got %d", got)
	}
	refreshed := accountpool.GetPool().GetByID(account.ID)
	if refreshed == nil || refreshed.AccessToken != "new" || refreshed.ProfileArn != "arn:aws:codewhisperer:us-east-1:123:profile/test" {
		t.Fatalf("expected refreshed credentials in account pool, got %+v", refreshed)
	}
}

func TestTokenRefreshCoordinatorCallerCancellationDoesNotCancelSharedRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID: "refresh-cancel", Email: "cancel@example.com", Enabled: true,
		AuthMethod: "idc", Region: "us-east-1", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret", AccessToken: "old", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accountpool.GetPool().Reload()

	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	var canceled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(started)
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"accessToken":"new","refreshToken":"rotated","expiresIn":3600}`))
		case <-r.Context().Done():
			canceled.Store(true)
		}
	}))
	defer server.Close()
	oldURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return server.URL })
	oldClient := auth.SetGlobalAuthClientForTest(&http.Client{Timeout: 5 * time.Second})
	t.Cleanup(func() {
		auth.SetOIDCTokenURLForTest(oldURL)
		auth.SetGlobalAuthClientForTest(oldClient)
	})
	oldCoordinator := sharedTokenRefreshCoordinator
	sharedTokenRefreshCoordinator = &tokenRefreshCoordinator{inFlight: make(map[string]*coordinatedRefreshCall)}
	t.Cleanup(func() { sharedTokenRefreshCoordinator = oldCoordinator })

	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	firstCopy := account
	go func() {
		firstResult <- sharedTokenRefreshCoordinator.RefreshContext(ctx, &firstCopy, true)
	}()
	<-started
	cancel()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled caller, got %v", err)
	}

	secondResult := make(chan error, 1)
	secondCopy := account
	go func() {
		secondResult <- sharedTokenRefreshCoordinator.RefreshContext(context.Background(), &secondCopy, true)
	}()
	close(release)
	if err := <-secondResult; err != nil {
		t.Fatalf("shared refresh should complete for second waiter: %v", err)
	}
	if canceled.Load() {
		t.Fatal("caller cancellation canceled the shared upstream refresh")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected one shared request, got %d", got)
	}
}

func TestKiroPayloadTracksTokenRefreshAttemptsPerAccount(t *testing.T) {
	payload := &KiroPayload{}
	accountA := &config.Account{ID: "a"}
	accountB := &config.Account{ID: "b"}
	if !payload.takeTokenRefreshAttempt(accountA) {
		t.Fatal("expected first refresh attempt for account a")
	}
	if payload.takeTokenRefreshAttempt(accountA) {
		t.Fatal("expected second refresh attempt for account a to be rejected")
	}
	if !payload.takeTokenRefreshAttempt(accountB) {
		t.Fatal("expected account b to retain its own refresh attempt")
	}
}
