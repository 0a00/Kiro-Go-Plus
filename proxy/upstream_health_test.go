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
	if err := CallKiroAPI(account, secondPayload, &KiroStreamCallback{}); err == nil {
		t.Fatal("expected open-circuit failure")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected open circuit to suppress second request, got %d requests", got)
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
