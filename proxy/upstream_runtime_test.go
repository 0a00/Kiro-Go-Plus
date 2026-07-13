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
	}))
	defer runtimeServer.Close()

	var codeWhispererRequests atomic.Int32
	codeWhispererServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeWhispererRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "preferred"}))
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
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{"input": "x"}))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
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
	if !strings.Contains(upstreamErr.Error(), `tool "Write" did not complete`) {
		t.Fatalf("unexpected tool timeout error: %v", upstreamErr)
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

func TestRuntimeUnknown403FallsBackWithoutMarkingRevoked(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusForbidden, "Kiro Runtime", []byte(`{"message":"Forbidden"}`))
	if err.Kind != UpstreamErrorForbidden || !err.RetryAcrossEndpoints || !err.RetryAcrossAccounts {
		t.Fatalf("unexpected runtime 403 classification: %+v", err)
	}
	legacy := classifyUpstreamHTTPError(http.StatusForbidden, "Kiro IDE", []byte(`{"message":"Forbidden"}`))
	if legacy.Kind != UpstreamErrorForbidden || legacy.RetryAcrossEndpoints {
		t.Fatalf("unexpected legacy 403 classification: %+v", legacy)
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
		&config.Account{ID: "rate-limit-account", AccessToken: "token"},
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
