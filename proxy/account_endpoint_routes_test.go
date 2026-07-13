package proxy

import (
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func newAccountEndpointRouteTestRegistry(t *testing.T) (*accountEndpointRouteRegistry, *time.Time) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	registry := newAccountEndpointRouteRegistry()
	registry.now = func() time.Time { return now }
	return registry, &now
}

func testRouteEndpoints() []kiroEndpoint {
	return []kiroEndpoint{
		{Key: "runtime", Name: "Kiro Runtime"},
		{Key: "kiro", Name: "Kiro IDE"},
		{Key: "codewhisperer", Name: "CodeWhisperer"},
		{Key: "amazonq", Name: "AmazonQ"},
	}
}

func TestAccountEndpointRoutePrefersRecentAutoSuccess(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "codewhisperer", Name: "CodeWhisperer"})

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if err != nil {
		t.Fatalf("available endpoints: %v", err)
	}
	if len(endpoints) != 4 || endpoints[0].Key != "codewhisperer" {
		t.Fatalf("recent successful endpoint was not preferred: %+v", endpoints)
	}
}

func TestAccountEndpointRouteSkipsCoolingRateLimitedEndpoint(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "Kiro Runtime", []byte(`{"message":"too many requests"}`))
	if cooldown := registry.recordFailure("account-a", "claude-sonnet-5", kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"}, err); cooldown <= 0 {
		t.Fatal("expected endpoint cooldown")
	}

	endpoints, routeErr := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if routeErr != nil {
		t.Fatalf("available endpoints: %v", routeErr)
	}
	if len(endpoints) != 3 {
		t.Fatalf("expected one cooling endpoint to be skipped, got %+v", endpoints)
	}
	for _, endpoint := range endpoints {
		if endpoint.Key == "runtime" {
			t.Fatal("cooling runtime endpoint was returned")
		}
	}
}

func TestAccountEndpointRouteReturnsRateLimitWhenAllEndpointsCool(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "upstream", []byte(`{"message":"too many requests"}`))
	for _, endpoint := range testRouteEndpoints() {
		registry.recordFailure("account-a", "claude-sonnet-5", endpoint, err)
	}

	endpoints, routeErr := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if len(endpoints) != 0 {
		t.Fatalf("expected no available endpoints, got %+v", endpoints)
	}
	upstreamErr, ok := asUpstreamError(routeErr)
	if !ok || upstreamErr.Kind != UpstreamErrorRateLimit || !upstreamErr.RetryAcrossAccounts || upstreamErr.RetryAfter <= 0 {
		t.Fatalf("unexpected all-cooling error: %#v", routeErr)
	}
}

func TestExplicitPreferredEndpointStaysFirstUntilItCools(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "kiro", Name: "Kiro IDE"})

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "codewhisperer", getSortedEndpoints("codewhisperer"))
	if err != nil {
		t.Fatalf("available endpoints: %v", err)
	}
	if endpoints[0].Key != "codewhisperer" {
		t.Fatalf("explicit preferred endpoint was displaced: %+v", endpoints)
	}
}
