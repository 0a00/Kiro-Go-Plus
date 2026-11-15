package proxy

import (
	"encoding/json"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCapabilityAdminQueryFiltersAndNormalizesEndpoint(t *testing.T) {
	p := &pool.AccountPool{}
	p.RecordCapability("account-1", "Kiro IDE", "claude-sonnet-4.5", pool.CapabilityText, pool.CapabilitySupported, "", 10*time.Millisecond)
	p.RecordCapability("account-2", "runtime", "claude-sonnet-4.5", pool.CapabilityTools, pool.CapabilityProfileRequired, "profileArn required", 20*time.Millisecond)
	h := &Handler{pool: p}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/capabilities?endpoint=kiro&feature=text", nil)
	h.apiGetCapabilities(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Count   int                      `json:"count"`
		Records []pool.AccountCapability `json:"records"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Count != 1 || len(response.Records) != 1 || response.Records[0].Endpoint != "kiro" {
		t.Fatalf("unexpected filtered response: %+v", response)
	}

	recorder = httptest.NewRecorder()
	h.apiGetAccountCapabilities(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/account-2/capabilities", nil), "account-2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("account status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("account capability response is not JSON: %s", recorder.Body.String())
	}
}

func TestCapabilityErrorClassificationKeepsRateLimitShortLived(t *testing.T) {
	rateLimit := &UpstreamError{Kind: UpstreamErrorRateLimit, Endpoint: "Kiro IDE", Message: "rate limited"}
	if got := capabilityStatusForError(rateLimit); got != pool.CapabilityRateLimited {
		t.Fatalf("status=%q, want %q", got, pool.CapabilityRateLimited)
	}
	if got := capabilityStatusForError(&UpstreamError{Kind: UpstreamErrorProfileUnavailable}); got != pool.CapabilityProfileRequired {
		t.Fatalf("profile status=%q, want %q", got, pool.CapabilityProfileRequired)
	}
}
