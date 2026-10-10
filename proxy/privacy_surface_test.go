package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnonymousProbesExposeOnlyStatus(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	ready, _, _, _, _ := h.readinessSnapshot()
	for _, p := range []string{"/", "/health", "/ready"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		want := http.StatusOK
		if p == "/ready" && !ready {
			want = http.StatusServiceUnavailable
		}
		if rec.Code != want {
			t.Fatalf("probe %s status=%d, want %d", p, rec.Code, want)
		}
		var data map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		for k := range data {
			if k != "status" && k != "ready" {
				t.Fatalf("public field %s", k)
			}
		}
	}
	r := httptest.NewRequest("GET", "/admin/api/ready", nil)
	r.Header.Set("X-Admin-Password", "changeme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), `"accounts"`) || !strings.Contains(rec.Body.String(), `"version"`) {
		t.Fatal("admin diagnostics missing")
	}
	addCustomerTestKey(t, config.ApiKeyEntry{Key: "sk-privacy-customer", Name: "privacy", Enabled: true})
	for _, path := range []string{"/admin/api/health", "/admin/api/ready", "/admin/api/version"} {
		for _, key := range []string{"", "sk-privacy-customer"} {
			r := httptest.NewRequest("GET", path, nil)
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), `"version"`) {
				t.Fatalf("admin probe %s exposed without admin session: %d", path, rec.Code)
			}
		}
	}
}

func TestAdminAssetsRequireSessionAndConfinePaths(t *testing.T) {
	h := newAdminAuthTestHandler(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "login.html", "login.css", "login.js", "appearance.css", "appearance.js", "admin-boot.js", "app.js", "index-legacy.html"} {
		if err := os.WriteFile(filepath.Join(dir, "web", name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "private"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../private", filepath.Join(dir, "web", "escape.js")); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	for _, p := range []string{"/admin", "/admin/", "/admin/login.js", "/admin/login.css", "/admin/appearance.js", "/admin/appearance.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Fatalf("public login %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/admin/app.js", "/admin/index.html", "/admin/admin-boot.js", "/admin/../private", "/admin/escape.js", "/admin/vendor/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 {
			t.Fatalf("anonymous asset %s: %d", p, rec.Code)
		}
	}
	legacy := httptest.NewRecorder()
	h.ServeHTTP(legacy, httptest.NewRequest("GET", "/admin/index-legacy.html", nil))
	if legacy.Code != http.StatusFound || legacy.Header().Get("Location") != "/admin/" {
		t.Fatal("legacy page does not use the canonical login flow")
	}
	login := httptest.NewRecorder()
	if err := h.issueAdminSession(login, httptest.NewRequest("POST", "/admin/api/login", nil), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == adminSessionCookie {
			cookie = c
		}
	}
	for _, p := range []string{"/admin", "/admin/app.js", "/admin/escape.js", "/admin/../private", "/admin/vendor/"} {
		r := httptest.NewRequest("GET", p, nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		want := 404
		if p == "/admin" || p == "/admin/app.js" {
			want = 200
		}
		if rec.Code != want {
			t.Fatalf("session asset %s: %d", p, rec.Code)
		}
	}
	h.clearAdminSessions()
	r := httptest.NewRequest("GET", "/admin/app.js", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 404 {
		t.Fatal("expired session served bundle")
	}
}

func TestPublicAppearanceAssetsAreExplicitlyAllowlisted(t *testing.T) {
	for _, name := range []string{"appearance.js", "appearance.css", "login.js", "login.css", "login.html", "vendor/fontawesome/css/all.min.css", "vendor/fontawesome/webfonts/fa-solid-900.woff2"} {
		if !publicAdminAsset(name) {
			t.Fatalf("missing public asset %s", name)
		}
	}
	for _, name := range []string{"app.js", "admin-boot.js", "styles.css", "locales/zh.json", "index.html", "index-legacy.html", "vendor/fontawesome/webfonts/../../app.js", "vendor/fontawesome/webfonts/private.key", "vendor/tailwindcss-browser/index.global.js"} {
		if publicAdminAsset(name) {
			t.Fatalf("private asset public: %s", name)
		}
	}
}

func TestClientErrorsNeverIncludeDiagnosticContents(t *testing.T) {
	for _, kind := range []UpstreamErrorKind{UpstreamErrorClientRequest, UpstreamErrorForbidden, UpstreamErrorQuota, UpstreamErrorTransient, UpstreamErrorRetryBudget, UpstreamErrorToolOutputTruncated, UpstreamErrorToolAssemblyTimeout} {
		err := &UpstreamError{Kind: kind, Endpoint: "PrivateEndpoint", Message: "private-user@example.invalid http://private.example/path?token=fixture", Cause: errors.New("private-path")}
		msg := clientErrorMessage(err)
		for _, secret := range []string{"PrivateEndpoint", "private-user", "private.example", "private-path", "token="} {
			if strings.Contains(msg, secret) {
				t.Fatalf("public error leaked %s", secret)
			}
		}
		if !strings.Contains(diagnosticErrorMessage(err), "private-user") {
			t.Fatal("diagnostic cause lost")
		}
	}
}

func TestRequestIDIsNotForwardedToUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Id") != "" {
			t.Error("client trace ID forwarded")
		}
		writeIntegrityText(t, w, "response ok", true)
	}))
	defer server.Close()
	h := setupStreamIntegrityPathTest(t, server)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":128,"messages":[{"role":"user","content":"test"}]}`))
	r.Header.Set("X-Request-Id", "customer-private-correlation")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || rec.Header().Get("X-Request-Id") != "customer-private-correlation" {
		t.Fatal("local correlation or response changed")
	}
	if config.Version == "" {
		t.Fatal("missing internal version")
	}
}

func TestPublicUpstreamFailureIsGenericAcrossProtocolsAndKeepsAdminDetail(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, protocol := range []string{"messages", "chat/completions", "responses"} {
			t.Run(protocol+fmt.Sprint(stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(503)
					w.Write([]byte(`{"message":"private-endpoint https://internal.example/token?id=private-fixture"}`))
				}))
				defer upstream.Close()
				h := setupStreamIntegrityPathTest(t, upstream)
				input := `"messages":[{"role":"user","content":"test"}]`
				if protocol == "responses" {
					input = `"input":"test","store":false`
				}
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":128,"stream":%t,%s}`, stream, input)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(body)))
				for _, secret := range []string{"internal.example", "private-endpoint", "private-fixture", "integrity-test"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Fatalf("public diagnostic leak: %s", secret)
					}
				}
				if !strings.Contains(rec.Body.String(), "temporarily unavailable") {
					t.Fatalf("missing public category: %s", rec.Body.String())
				}
				logs := h.requestLog.list(1)
				if len(logs) != 1 || !strings.Contains(logs[0].Error, "private-endpoint") {
					t.Fatal("admin cause lost")
				}
			})
		}
	}
}
