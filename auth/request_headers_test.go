package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type closingAuthTransport struct{ closed bool }

func (t *closingAuthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return authResponse(200, `{}`), nil
}
func (t *closingAuthTransport) CloseIdleConnections() { t.closed = true }
func TestAuthHeaderTransportPreservesConnectionCleanup(t *testing.T) {
	base := &closingAuthTransport{}
	(&http.Client{Transport: authHeaderTransport{base: base}}).CloseIdleConnections()
	if !base.closed {
		t.Fatal("wrapped connections leaked")
	}
}

func TestAuthHeadersDoNotMutateRequestOrOverrideExplicitIdentity(t *testing.T) {
	for _, ua := range []string{"", "endpoint-specific/2"} {
		var observed string
		transport := authHeaderTransport{base: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			observed = r.UserAgent()
			if r.Header.Get("Authorization") != "Bearer fixture" {
				t.Fatal("authorization changed")
			}
			return authResponse(200, `{}`), nil
		})}
		r, _ := http.NewRequest("GET", "https://example.invalid", nil)
		r.Header.Set("Authorization", "Bearer fixture")
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		resp, err := transport.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := ua
		if want == "" {
			want = "HTTPClient/1.0"
		}
		if observed != want || r.UserAgent() != ua {
			t.Fatal("header policy mutated caller or explicit value")
		}
	}
}

func TestAuthHeadersPreserveCancellationWithNilHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = nil
	transport := authHeaderTransport{base: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context() != ctx || r.UserAgent() != "HTTPClient/1.0" {
			t.Fatal("request context or fallback header changed")
		}
		return nil, r.Context().Err()
	})}
	if _, err := transport.RoundTrip(req); !errors.Is(err, context.Canceled) || req.Header != nil {
		t.Fatalf("cancellation or caller ownership changed: %v", err)
	}
}

func TestOIDCRefreshSendsExplicitNeutralUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "HTTPClient/1.0" {
			t.Errorf("unexpected UA %q", r.UserAgent())
		}
		w.Write([]byte(`{"accessToken":"fixture","expiresIn":3600}`))
	}))
	defer server.Close()
	old := oidcTokenURL
	oidcTokenURL = func(string) string { return server.URL }
	defer func() { oidcTokenURL = old }()
	// Keep local fixtures independent of net/http's process-wide proxy cache.
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	_, _, _, _, err := refreshOIDCToken(context.Background(), "refresh", "id", "secret", "us-east-1", &http.Client{Transport: authHeaderTransport{base: transport}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSSOImportAndUserInfoOmitLegacyProjectLabels(t *testing.T) {
	installAuthTransport(t, authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.UserAgent(), "KiroAPIProxy") || strings.Contains(r.Header.Get("x-amz-user-agent"), "KiroAPIProxy") {
			t.Fatal("legacy label")
		}
		if r.URL.Path == "/client/register" {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["clientName"] != "API Client" {
				t.Fatal("legacy registration name")
			}
			return authResponse(200, `{"clientId":"id","clientSecret":"secret"}`), nil
		}
		return authResponse(200, `{"userInfo":{"email":"fixture@example.invalid","userId":"id"}}`), nil
	}))
	if _, _, err := registerDeviceClient("https://example.invalid", "https://view.awsapps.com/start"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GetUserInfo("fixture"); err != nil {
		t.Fatal(err)
	}
}
