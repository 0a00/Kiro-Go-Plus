package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestCachedModelsUsesCompatibilityCandidatesForOrdinaryAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "ordinary-cached-models", Enabled: true, AccessToken: "token",
		AuthMethod: "idc", Provider: "BuilderId", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	recorder := httptest.NewRecorder()
	h.apiGetAccountModelsCached(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/ordinary-cached-models/models/cached", nil), "ordinary-cached-models")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool     `json:"success"`
		Models  []string `json:"models"`
		Source  string   `json:"source"`
		Warning string   `json:"warning"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || response.Source != modelListSourceCompatibility || len(response.Models) == 0 || response.Warning == "" {
		t.Fatalf("unexpected compatibility response: %+v", response)
	}
}

func TestWebSearchCapabilityClassificationKeepsGenericForbiddenDistinct(t *testing.T) {
	unsupported := &UpstreamError{
		Kind: UpstreamErrorForbidden, StatusCode: http.StatusForbidden,
		Message: "AWS Builder ID is not supported for this operation",
	}
	if !isWebSearchCapabilityUnavailable(unsupported) {
		t.Fatal("explicit unsupported response was not classified as a capability failure")
	}
	generic := &UpstreamError{Kind: UpstreamErrorForbidden, StatusCode: http.StatusForbidden, Message: "Forbidden"}
	if isWebSearchCapabilityUnavailable(generic) {
		t.Fatal("generic forbidden response was incorrectly hidden as a capability failure")
	}
}
