package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminVersionIncludesBuildCommit(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	old := config.BuildCommit
	config.BuildCommit = "abc123def456"
	t.Cleanup(func() { config.BuildCommit = old })

	req := httptest.NewRequest(http.MethodGet, "/admin/api/version", nil)
	req.Header.Set("X-Admin-Password", "changeme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"buildCommit":"abc123def456"`) {
		t.Fatalf("build commit missing from version response: %s", rec.Body.String())
	}
}
