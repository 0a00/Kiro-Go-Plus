package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestExternalCountTokensProviderUsesRemoteValue(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateProxySettings("direct"); err != nil {
		t.Fatalf("UpdateProxySettings: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "key-test" {
			t.Fatalf("expected x-api-key header, got %q", got)
		}
		var req ClaudeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "claude-sonnet-4" {
			t.Fatalf("expected model forwarded, got %q", req.Model)
		}
		w.Write([]byte(`{"input_tokens":1234}`))
	}))
	defer server.Close()

	if err := config.UpdateCountTokensProviderConfig(config.CountTokensProviderConfig{
		Enabled:  true,
		ApiURL:   server.URL,
		ApiKey:   "key-test",
		AuthType: "x-api-key",
	}); err != nil {
		t.Fatalf("UpdateCountTokensProviderConfig: %v", err)
	}

	tokens, err := callExternalCountTokens(&ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("callExternalCountTokens: %v", err)
	}
	if tokens != 1234 {
		t.Fatalf("expected 1234 tokens, got %d", tokens)
	}
}

func TestExtractTokenCountFromResponseAcceptsNestedUsage(t *testing.T) {
	tokens, ok := extractTokenCountFromResponse([]byte(`{"usage":{"inputTokens":42}}`))
	if !ok || tokens != 42 {
		t.Fatalf("expected nested inputTokens=42, got tokens=%d ok=%v", tokens, ok)
	}
}

func TestExternalCountTokensProviderHonorsCanceledContext(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateCountTokensProviderConfig(config.CountTokensProviderConfig{
		Enabled: true,
		ApiURL:  "http://127.0.0.1:1",
	}); err != nil {
		t.Fatalf("UpdateCountTokensProviderConfig: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := callExternalCountTokensContext(ctx, &ClaudeRequest{Model: "claude-sonnet-4"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
