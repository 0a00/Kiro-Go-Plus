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
