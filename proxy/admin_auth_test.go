package proxy

import (
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newAdminAuthTestHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	return &Handler{
		pool:          accountpool.GetPool(),
		adminSessions: make(map[[32]byte]adminSession),
		adminAttempts: make(map[string]adminAuthAttempt),
	}
}

func TestAdminLoginUsesHTTPOnlySessionCookie(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"changeme","remember":true}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}

	var session *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == adminSessionCookie {
			session = cookie
			break
		}
	}
	if session == nil || session.Value == "" || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %#v", session)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	statusReq.AddCookie(session)
	statusRec := httptest.NewRecorder()
	h.handleAdminAPI(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("session auth status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	for i := 0; i < adminMaxAttempts; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"wrong"}`))
		req.RemoteAddr = "192.0.2.10:12345"
		rec := httptest.NewRecorder()
		h.handleAdminAPI(rec, req)
		if i < adminMaxAttempts-1 && rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
		if i == adminMaxAttempts-1 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("final attempt status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestAdminSessionRejectsCrossOriginMutation(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"changeme"}`))
	loginRec := httptest.NewRecorder()
	h.handleAdminAPI(loginRec, loginReq)
	var session *http.Cookie
	for _, cookie := range loginRec.Result().Cookies() {
		if cookie.Name == adminSessionCookie {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("missing session cookie")
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{}`))
	req.Host = "admin.example.com"
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}
