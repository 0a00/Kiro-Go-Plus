package proxy

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedCircuitOnlyCountsServiceAndTransportFailures(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 408, 429, 500, 502, 503, 504} {
		err := classifyUpstreamHTTPError(status, "test", []byte(`{"message":"failure"}`))
		want := status == 502 || status == 503 || status == 504
		if got := circuitEligibleFailure(err); got != want {
			t.Fatalf("status %d eligible=%v want=%v", status, got, want)
		}
	}
	for _, err := range []error{
		context.Canceled, context.DeadlineExceeded,
		classifyTransportError("test", context.DeadlineExceeded),
		newStreamIdleTimeoutError("test", time.Minute),
		newToolAssemblyTimeoutError("test", "Edit", 90, time.Minute),
		newEmptyResponseError("test", true),
		newEndpointCircuitOpenError("test", time.Second),
		&EventStreamError{Kind: EventStreamInvalidPayload},
	} {
		if circuitEligibleFailure(err) {
			t.Fatalf("request-local failure poisons shared circuit: %v", err)
		}
	}
	if !circuitEligibleFailure(classifyTransportError("test", &net.OpError{Op: "dial", Err: errors.New("refused")})) {
		t.Fatal("transport outage ignored")
	}
}

func TestStreamIdleTimeoutPreservesRequestLocalRecoveryPolicy(t *testing.T) {
	err := newStreamIdleTimeoutError("fixture", time.Minute)
	if !shouldRetryAcrossEndpoints(err) || !shouldRetryAcrossAccounts(err) {
		t.Fatal("idle timeout lost retry eligibility before client output")
	}
	if _, ok := endpointRouteFailure(err); !ok {
		t.Fatal("idle timeout lost account-route cooldown")
	}
	if trigger, ok := fallbackTriggerForError(err); !ok || trigger != config.ModelFallbackTriggerUpstreamError {
		t.Fatalf("idle timeout changed fallback classification: %s %v", trigger, ok)
	}
	if requestDetailRetryReason(err) != "stream_idle_timeout" || circuitEligibleFailure(err) {
		t.Fatal("idle timeout diagnosis or shared circuit policy is incorrect")
	}
}

func TestCallKiroAPIGeneric500DoesNotPoisonOtherAccountsOrModels(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			http.Error(w, "request-specific error", 500)
			return
		}
		writeIntegrityText(t, w, "healthy", true)
	}))
	defer server.Close()
	setupStreamIntegrityPathTest(t, server)
	retry := config.GetRetryConfig()
	retry.EndpointFailureThreshold = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatal(err)
	}
	payload := func() *KiroPayload {
		p := &KiroPayload{}
		p.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
		return p
	}
	err := CallKiroAPI(&config.Account{ID: "bad", AccessToken: "bad"}, payload(), &KiroStreamCallback{})
	if e, ok := asUpstreamError(err); !ok || e.StatusCode != 500 || !e.RetryAcrossAccounts {
		t.Fatalf("bad response %v", err)
	}
	var output string
	if err := CallKiroAPI(&config.Account{ID: "good", AccessToken: "good"}, payload(), &KiroStreamCallback{OnText: func(s string, _ bool) { output += s }}); err != nil {
		t.Fatal(err)
	}
	if output != "healthy" || hits.Load() != 2 {
		t.Fatalf("healthy account suppressed: hits=%d output=%q", hits.Load(), output)
	}
}

func TestCallKiroAPIKeepsReal500WhenOtherEndpointsAreOpen(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, "original failure", 500) }))
	defer server.Close()
	setupStreamIntegrityPathTest(t, server)
	if err := config.UpdateEndpointFallback(true); err != nil {
		t.Fatal(err)
	}
	kiroEndpoints = append(kiroEndpoints, kiroEndpoint{Key: "backup", Name: "backup", URL: server.URL})
	p := &KiroPayload{}
	p.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	u, _ := url.Parse(server.URL)
	key := endpointCircuitScope("backup", u.Host, endpointRouteModel(p), "")
	sharedUpstreamHealth.endpoints[key] = circuitRuntimeState{cooldownUntil: time.Now().Add(time.Minute)}
	err := CallKiroAPI(&config.Account{ID: "origin", AccessToken: "token"}, p, &KiroStreamCallback{})
	if e, ok := asUpstreamError(err); !ok || e.StatusCode != 500 || !e.RetryAcrossAccounts || !strings.Contains(e.Message, "original failure") {
		t.Fatalf("real failure masked: %#v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("open backup contacted: %d", hits.Load())
	}
}

func TestEndpointCircuitScopeIsolatesModelWorkloadAndProxy(t *testing.T) {
	r := newUpstreamHealthRegistry()
	base := endpointCircuitScope("kiro", "example.invalid", "claude-sonnet-4.5", "")
	r.endpoints[base] = circuitRuntimeState{cooldownUntil: time.Now().Add(time.Minute)}
	if r.beginEndpoint(base, "test") {
		t.Fatal("open route admitted")
	}
	for _, key := range []string{
		endpointCircuitScope("kiro", "example.invalid", "claude-sonnet-4.5|long-tool", ""),
		endpointCircuitScope("kiro", "example.invalid", "claude-haiku-4.5", ""),
		endpointCircuitScope("kiro", "example.invalid", "claude-sonnet-4.5", "http://user:fixture@proxy.invalid"),
	} {
		if !r.beginEndpoint(key, "test") {
			t.Fatal("unrelated scope poisoned")
		}
		if strings.Contains(key, "fixture") {
			t.Fatal("proxy credentials in key")
		}
	}
	if endpointCircuitScope("kiro", "example.invalid", "claude-sonnet-4.5", "direct") != base {
		t.Fatal("direct route not canonical")
	}
}

func TestCircuitRegistryBoundPreservesOpenAndHalfOpen(t *testing.T) {
	r := newUpstreamHealthRegistry()
	for i := 0; i < maxCircuitRuntimeEntries; i++ {
		r.endpoints[fmt.Sprint(i)] = circuitRuntimeState{cooldownUntil: time.Now().Add(time.Minute)}
	}
	if r.beginEndpoint("new", "new") {
		t.Fatal("registry bypassed saturated protections")
	}
	r.endpoints["0"] = circuitRuntimeState{lastAccess: time.Unix(1, 0)}
	if !r.beginEndpoint("new", "new") || len(r.endpoints) != maxCircuitRuntimeEntries {
		t.Fatal("closed eviction failed")
	}
	r.endpointSuccess("0", time.Second)
	r.endpointFailure("0", errors.New("stale"), time.Second)
	r.releaseEndpoint("0")
	if len(r.endpoints) != maxCircuitRuntimeEntries {
		t.Fatal("late callback resurrected evicted scope")
	}
}

func TestCircuitConcurrentHalfOpenAllowsOneProbe(t *testing.T) {
	r := newUpstreamHealthRegistry()
	r.endpoints["test"] = circuitRuntimeState{cooldownUntil: time.Now().Add(-time.Second)}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.beginEndpoint("test", "test") {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("half-open probes=%d", admitted.Load())
	}
	r.releaseEndpoint("test")
	if !r.beginEndpoint("test", "test") {
		t.Fatal("canceled probe not released")
	}
}

func TestCircuitBoundReclaimsOnlyIdleExpiredScopes(t *testing.T) {
	r := newUpstreamHealthRegistry()
	now := time.Now()
	for i := 0; i < maxCircuitRuntimeEntries; i++ {
		r.endpoints[fmt.Sprint(i)] = circuitRuntimeState{cooldownUntil: now.Add(-2 * time.Hour), lastAccess: now.Add(-3 * time.Hour)}
	}
	r.endpoints["0"] = circuitRuntimeState{inFlight: 1, lastAccess: now.Add(-4 * time.Hour)}
	if !r.beginEndpoint("new", "new") || len(r.endpoints) != maxCircuitRuntimeEntries {
		t.Fatal("expired scopes permanently blocked new routes")
	}
	if _, ok := r.endpoints["0"]; !ok {
		t.Fatal("active route was evicted")
	}
}

func TestUpstreamHeaderAndBodyTimesSeparateInitialWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	h := setupStreamIntegrityPathTest(t, server)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"ok"}]} `))
	h.handleClaudeMessages(rec, req)
	rows := h.requestLog.list(1)
	if len(rows) != 1 || rows[0].FirstUpstreamHeadersMs == nil || rows[0].FirstUpstreamBodyByteMs == nil {
		t.Fatalf("missing timings %+v", rows)
	}
	if *rows[0].FirstUpstreamHeadersMs < 25 || *rows[0].FirstUpstreamBodyByteMs-*rows[0].FirstUpstreamHeadersMs < 30 {
		t.Fatalf("initial wait lost %+v", rows[0])
	}
}
