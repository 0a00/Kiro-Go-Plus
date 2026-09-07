package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestApiKeyBatchCreateEndpoint(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/batch", strings.NewReader(`{"keys":"sk-batch-one\n\nsk-batch-two\nsk-batch-one"}`))
	(&Handler{}).apiCreateApiKeysBatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var response struct {
		CreatedCount          int `json:"createdCount"`
		SkippedDuplicateCount int `json:"skippedDuplicateCount"`
		IgnoredEmptyLineCount int `json:"ignoredEmptyLineCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.CreatedCount != 2 || response.SkippedDuplicateCount != 1 || response.IgnoredEmptyLineCount != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if got := config.ListApiKeys(); len(got) != 2 {
		t.Fatalf("expected two stored keys, got %d", len(got))
	}
}

func TestApiKeyBatchCreateRejectsEmptyInput(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/batch", strings.NewReader(`{"keys":"\n  \n"}`))
	(&Handler{}).apiCreateApiKeysBatch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiKeyModelFallbackOverrideAPI(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-fallback-api", Enabled: true})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/api-keys/"+created.ID, strings.NewReader(`{"modelFallbackEnabled":false}`))
	h.apiUpdateApiKey(rec, req, created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetApiKeyEntry(created.ID)
	if got == nil || got.ModelFallbackEnabled == nil || *got.ModelFallbackEnabled {
		t.Fatalf("override was not persisted: %+v", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/admin/api/api-keys/"+created.ID, strings.NewReader(`{"modelFallbackEnabled":null}`))
	h.apiUpdateApiKey(rec, req, created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := config.GetApiKeyEntry(created.ID); got == nil || got.ModelFallbackEnabled != nil {
		t.Fatalf("override was not cleared: %+v", got)
	}
}
