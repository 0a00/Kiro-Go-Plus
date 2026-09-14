package proxy

import (
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamHealthCircuitOpensAndUsesSingleHalfOpenProbe(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.EndpointFailureThreshold = 2
	retry.EndpointCircuitCooldownSeconds = 5
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("UpdateRetryConfig: %v", err)
	}

	now := time.Unix(1000, 0)
	registry := newUpstreamHealthRegistry()
	registry.now = func() time.Time { return now }
	if !registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected closed circuit")
	}
	registry.endpointFailure("runtime|us-east-1", errors.New("first"), time.Second)
	if !registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected circuit below threshold to remain closed")
	}
	registry.endpointFailure("runtime|us-east-1", errors.New("second"), time.Second)
	if registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected open circuit")
	}

	now = now.Add(6 * time.Second)
	if !registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected one half-open probe")
	}
	if registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected concurrent half-open probe to be rejected")
	}
	registry.endpointSuccess("runtime|us-east-1", 100*time.Millisecond)
	if !registry.beginEndpoint("runtime|us-east-1", "runtime") {
		t.Fatal("expected successful probe to close circuit")
	}
}

func TestCallKiroAPISkipsOpenEndpointCircuit(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.EndpointFailureThreshold = 1
	retry.EndpointCircuitCooldownSeconds = 60
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("UpdateRetryConfig: %v", err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "temporary", http.StatusInternalServerError)
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "circuit-test", URL: server.URL, Origin: "AI_EDITOR", Name: "circuit-test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })
	oldHealth := sharedUpstreamHealth
	sharedUpstreamHealth = newUpstreamHealthRegistry()
	t.Cleanup(func() { sharedUpstreamHealth = oldHealth })

	account := &config.Account{ID: "a", AccessToken: "token"}
	firstPayload := &KiroPayload{}
	if err := CallKiroAPI(account, firstPayload, &KiroStreamCallback{}); err == nil {
		t.Fatal("expected first upstream failure")
	}
	secondPayload := &KiroPayload{}
	secondErr := CallKiroAPI(account, secondPayload, &KiroStreamCallback{})
	if secondErr == nil {
		t.Fatal("expected open-circuit failure")
	}
	upstreamErr, ok := asUpstreamError(secondErr)
	if !ok || upstreamErr.Kind != UpstreamErrorEndpointUnavailable || upstreamErr.RetryAcrossAccounts || upstreamErr.RetryAfter <= 0 {
		t.Fatalf("open circuit did not return a bounded retry error: %#v", secondErr)
	}
	if shouldRetryAcrossAccounts(secondErr) {
		t.Fatal("shared open circuit would still scan another account")
	}
	if mapped := mapDownstreamError(secondErr); mapped.Status != http.StatusServiceUnavailable || mapped.RetryAfter == "" {
		t.Fatalf("open circuit mapped to %+v, want retryable 503", mapped)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected open circuit to suppress second request, got %d requests", got)
	}
}

func TestCallKiroAPIEmptyResponseDoesNotOpenSharedEndpointCircuit(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.EndpointFailureThreshold = 1
	retry.EmptyResponseRetries = 0
	retry.MaxUpstreamAttempts = 1
	preOutputRetries := 0
	retry.PreOutputStreamRetries = &preOutputRetries
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("test"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "test", URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })
	oldHealth := sharedUpstreamHealth
	sharedUpstreamHealth = newUpstreamHealthRegistry()
	t.Cleanup(func() { sharedUpstreamHealth = oldHealth })

	account := &config.Account{ID: "empty-account", AccessToken: "token"}
	for attempt := 0; attempt < 2; attempt++ {
		err := CallKiroAPI(account, &KiroPayload{}, &KiroStreamCallback{})
		upstreamErr, ok := asUpstreamError(err)
		if !ok || upstreamErr.Kind != UpstreamErrorEmptyResponse {
			t.Fatalf("attempt %d error = %#v, want empty response", attempt+1, err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("content-level empty response opened shared circuit; requests=%d", got)
	}
}

func TestProxyCircuitSnapshotDoesNotExposePassword(t *testing.T) {
	registry := newUpstreamHealthRegistry()
	raw := "socks5://user:super-secret@127.0.0.1:1080"
	if !registry.beginProxy(raw, raw) {
		t.Fatal("expected proxy circuit to allow request")
	}
	registry.proxyFailure(raw, errors.New("dial failed"), time.Second)
	snapshot := registry.Snapshot()
	if containsJSONValue(snapshot, "super-secret") {
		t.Fatalf("proxy password leaked in snapshot: %+v", snapshot)
	}
}

func containsJSONValue(value interface{}, needle string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, needle)
	case map[string]interface{}:
		for _, item := range typed {
			if containsJSONValue(item, needle) {
				return true
			}
		}
	case []map[string]interface{}:
		for _, item := range typed {
			if containsJSONValue(item, needle) {
				return true
			}
		}
	}
	return false
}
