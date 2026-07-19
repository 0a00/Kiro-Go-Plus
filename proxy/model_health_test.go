package proxy

import (
	"context"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizeModelHealthStatus(t *testing.T) {
	healthy := modelHealthState{
		Text:     modelCapabilityHealth{Tested: true, Supported: true},
		Thinking: modelCapabilityHealth{Tested: true, Supported: true},
		Tools:    modelCapabilityHealth{Tested: true, Supported: true},
	}
	if got := summarizeModelHealthStatus(healthy); got != "healthy" {
		t.Fatalf("healthy status = %q", got)
	}
	healthy.Tools.Supported = false
	if got := summarizeModelHealthStatus(healthy); got != "degraded" {
		t.Fatalf("degraded status = %q", got)
	}
	healthy.Text.Supported = false
	if got := summarizeModelHealthStatus(healthy); got != "unavailable" {
		t.Fatalf("unavailable status = %q", got)
	}
}

func TestRunModelHealthTextProbe(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "model-health-account", Enabled: true, AccessToken: "token",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "OK"}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool}
	result := h.runModelHealthProbe(context.Background(), "claude-sonnet-4.5", modelHealthProbeText, 2*time.Second)
	if !result.Tested || !result.Supported || result.AccountID != "model-health-account" || result.Endpoint != "test" {
		t.Fatalf("unexpected probe result: %+v", result)
	}
}
