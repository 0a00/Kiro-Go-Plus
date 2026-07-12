package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestLogKeepsNewestFirstWithLimit(t *testing.T) {
	log := newRequestLog(2)
	log.add(requestLogEntry{Protocol: "one", Timestamp: 1})
	log.add(requestLogEntry{Protocol: "two", Timestamp: 2})
	log.add(requestLogEntry{Protocol: "three", Timestamp: 3})

	got := log.list(10)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Protocol != "three" || got[1].Protocol != "two" {
		t.Fatalf("expected newest retained entries first, got %+v", got)
	}
}

func TestAdminRequestsEndpointReturnsRequestLog(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{
		pool:       accountpool.GetPool(),
		requestLog: newRequestLog(defaultRequestLogLimit),
	}
	h.recordRequestLog(requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   "openai.chat",
		Model:      "claude-sonnet-4.5",
		Status:     "success",
		StatusCode: 200,
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/requests?limit=1", nil)
	req.Header.Set("X-Admin-Password", "changeme")
	rec := httptest.NewRecorder()

	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Requests []requestLogEntry `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Requests) != 1 || body.Requests[0].Protocol != "openai.chat" {
		t.Fatalf("unexpected response: %+v", body)
	}
}
