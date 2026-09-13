package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeProxyPoolLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "url", in: "http://proxy.example:8080", want: "http://proxy.example:8080"},
		{name: "bare", in: "proxy.example:8080", want: "http://proxy.example:8080"},
		{name: "credentials", in: "proxy.example:8080:user:pass:with:colon", want: "socks5h://user:pass%3Awith%3Acolon@proxy.example:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProxyPoolLine(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("normalizeProxyPoolLine(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
	if _, err := normalizeProxyPoolLine("not-a-proxy"); err == nil {
		t.Fatal("invalid proxy was accepted")
	}
}

func TestProbeProxyPoolEntryClassifiesTransport(t *testing.T) {
	originalClient := proxyPoolProbeHTTPClient
	originalDo := proxyPoolProbeDo
	t.Cleanup(func() {
		proxyPoolProbeHTTPClient = originalClient
		proxyPoolProbeDo = originalDo
	})
	proxyPoolProbeHTTPClient = func(string) (*http.Client, error) { return &http.Client{}, nil }
	proxyPoolProbeDo = func(_ context.Context, _ *http.Client, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	result := probeProxyPoolEntry(context.Background(), config.ProxyPoolEntry{ID: "p", ProxyURL: "http://proxy.example:8080"})
	if result.Health != "healthy" || result.LatencyMs < 0 {
		t.Fatalf("proxy probe result = %+v", result)
	}
	proxyPoolProbeDo = func(_ context.Context, _ *http.Client, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusProxyAuthRequired, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	result = probeProxyPoolEntry(context.Background(), config.ProxyPoolEntry{ID: "p", ProxyURL: "http://proxy.example:8080"})
	if result.Health != "unhealthy" || result.Error != "proxy authentication failed" {
		t.Fatalf("proxy auth probe result = %+v", result)
	}
}

func TestProxyPoolAPIAddsDeduplicatesAndAssignsInOneBatch(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{}
	add := httptest.NewRecorder()
	h.apiUpdateProxyPool(add, httptest.NewRequest(http.MethodPost, "/admin/api/proxy-pool", strings.NewReader(`{"action":"add","proxies":["http://p1.example:8080","http://p1.example:8080","http://p2.example:8080"]}`)))
	if add.Code != http.StatusOK {
		t.Fatalf("add status=%d body=%s", add.Code, add.Body.String())
	}
	if got := len(config.GetProxyPoolEntries()); got != 2 {
		t.Fatalf("proxy pool entries=%d, want 2", got)
	}
	entries := config.GetProxyPoolEntries()
	assign := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]interface{}{"action": "assign", "accountIds": []string{"a", "b", "c"}, "roundRobin": true})
	h.apiUpdateProxyPool(assign, httptest.NewRequest(http.MethodPost, "/admin/api/proxy-pool", strings.NewReader(string(body))))
	if assign.Code != http.StatusOK {
		t.Fatalf("assign status=%d body=%s", assign.Code, assign.Body.String())
	}
	accounts := config.GetAccounts()
	if accounts[0].ProxyURL != entries[0].ProxyURL || accounts[1].ProxyURL != entries[1].ProxyURL || accounts[2].ProxyURL != entries[0].ProxyURL {
		t.Fatalf("unexpected fixed assignments: %+v", accounts)
	}
}

func TestProxyPoolCredentialsUseConfigEncryption(t *testing.T) {
	key := make([]byte, 32)
	t.Setenv("KIRO_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	secret := "socks5://user:secret-password@proxy.example:1080"
	if err := config.ReplaceProxyPoolEntries([]config.ProxyPoolEntry{{ID: "p", ProxyURL: secret, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-password") {
		t.Fatalf("proxy password was persisted in plaintext: %s", raw)
	}
}
