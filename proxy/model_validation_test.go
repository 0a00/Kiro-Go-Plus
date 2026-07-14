package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestedModelAvailabilityUsesCurrentCache(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{cachedModels: []ModelInfo{{ModelId: "claude-sonnet-5"}}}
	if !h.requestedModelAvailable("claude-sonnet-5-thinking", "claude-sonnet-5") {
		t.Fatal("cached thinking variant was rejected")
	}
	if !h.requestedModelAvailable("claude-opus-4.8", "claude-opus-4.8") {
		t.Fatal("known hidden model was rejected because it was absent from the live cache")
	}
	if h.requestedModelAvailable("claude-sonnet-4.8", "claude-sonnet-4.8") {
		t.Fatal("model absent from a populated cache was accepted")
	}
}

func TestClaudeMessagesRejectsUnavailableModelBeforeAccountSelection(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{cachedModels: []ModelInfo{{ModelId: "claude-sonnet-5"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"definitely-invalid-model-xyz",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "requested model is not available") {
		t.Fatalf("unexpected response: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDedupeModelResponseKeepsFirstModel(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "auto", "owned_by": "first"},
		{"id": "AUTO", "owned_by": "second"},
		{"id": "claude-sonnet-5"},
	}
	got := dedupeModelResponse(models)
	if len(got) != 2 || got[0]["owned_by"] != "first" {
		t.Fatalf("unexpected deduplicated models: %+v", got)
	}
}
