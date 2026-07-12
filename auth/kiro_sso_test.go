package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestKiroCallbackBindAddrs(t *testing.T) {
	t.Setenv("KIRO_SSO_CALLBACK_BIND", "")
	if got, want := kiroCallbackBindAddrs(), []string{"127.0.0.1:3128", "[::1]:3128"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default callback bind addresses = %v, want %v", got, want)
	}

	t.Setenv("KIRO_SSO_CALLBACK_BIND", "0.0.0.0")
	if got, want := kiroCallbackBindAddrs(), []string{"0.0.0.0:3128"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configured callback bind addresses = %v, want %v", got, want)
	}

	t.Setenv("KIRO_SSO_CALLBACK_BIND", "::")
	if got, want := kiroCallbackBindAddrs(), []string{"[::]:3128"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv6 callback bind addresses = %v, want %v", got, want)
	}
}

func TestValidateExternalIdpEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://login.microsoftonline.com/tenant/v2.0",
		"https://login.microsoftonline.us/tenant/oauth2/v2.0/token",
		"https://login.partner.microsoftonline.cn/tenant/v2.0",
	} {
		if err := validateExternalIdpEndpoint(endpoint); err != nil {
			t.Fatalf("expected %q to be accepted: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://login.microsoftonline.com/tenant/v2.0",
		"https://login.microsoftonline.com.evil.example/tenant/v2.0",
		"https://127.0.0.1/tenant/v2.0",
		"https://login.microsoftonline.com:8443/tenant/v2.0",
		"https://user@login.microsoftonline.com/tenant/v2.0",
	} {
		if err := validateExternalIdpEndpoint(endpoint); err == nil {
			t.Fatalf("expected %q to be rejected", endpoint)
		}
	}
}

func TestDeriveExternalIdpEndpointsFromUserID(t *testing.T) {
	tokenEndpoint, issuerURL, scopes := DeriveExternalIdpEndpoints(
		"https://login.microsoftonline.com/tenant-123/v2.0.object-id",
		"client-456",
		"",
	)
	if tokenEndpoint != "https://login.microsoftonline.com/tenant-123/oauth2/v2.0/token" {
		t.Fatalf("unexpected token endpoint: %q", tokenEndpoint)
	}
	if issuerURL != "https://login.microsoftonline.com/tenant-123/v2.0" {
		t.Fatalf("unexpected issuer: %q", issuerURL)
	}
	if !strings.Contains(scopes, "api://client-456/codewhisperer:conversations") || !strings.Contains(scopes, "offline_access") {
		t.Fatalf("unexpected scopes: %q", scopes)
	}
}

func TestExternalIdpJWTClaims(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	token := testJWT(fmt.Sprintf(`{"iss":"https://login.microsoftonline.com/tenant/v2.0","preferred_username":"user@example.com","exp":%d}`, exp))
	if got := ExtractEmailFromJWT(token); got != "user@example.com" {
		t.Fatalf("unexpected email: %q", got)
	}
	if got := ExpFromAccessTokenJWT(token); got != exp {
		t.Fatalf("unexpected exp: %d", got)
	}
	tokenEndpoint, _, _ := DeriveExternalIdpEndpoints("", "client", token)
	if tokenEndpoint != "https://login.microsoftonline.com/tenant/oauth2/v2.0/token" {
		t.Fatalf("unexpected derived endpoint: %q", tokenEndpoint)
	}
}

func TestRefreshExternalIdpTokenUsesFormAndRetainsRefreshToken(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-new","expires_in":3600}`))
	}))
	defer server.Close()

	oldValidator := SetExternalIdpValidatorForTest(func(endpoint string) error {
		if endpoint != server.URL {
			return fmt.Errorf("unexpected endpoint: %s", endpoint)
		}
		return nil
	})
	defer SetExternalIdpValidatorForTest(oldValidator)
	oldClient := SetGlobalAuthClientForTest(server.Client())
	defer SetGlobalAuthClientForTest(oldClient)

	before := time.Now().Unix()
	access, refresh, expiresAt, profileArn, err := RefreshTokenContext(context.Background(), &config.Account{
		AuthMethod: "external_idp", RefreshToken: "refresh-old",
		ClientID: "client-id", TokenEndpoint: server.URL,
		Scopes: "scope-a offline_access",
	})
	if err != nil {
		t.Fatalf("refresh external IdP: %v", err)
	}
	if access != "access-new" || refresh != "refresh-old" || profileArn != "" {
		t.Fatalf("unexpected refresh result: access=%q refresh=%q profile=%q", access, refresh, profileArn)
	}
	if expiresAt < before+3599 || expiresAt > time.Now().Unix()+3601 {
		t.Fatalf("unexpected expiry: %d", expiresAt)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "refresh-old" || gotForm.Get("client_id") != "client-id" {
		t.Fatalf("unexpected form: %#v", gotForm)
	}
	if gotForm.Get("scope") != "scope-a offline_access" {
		t.Fatalf("unexpected scope: %q", gotForm.Get("scope"))
	}
}

func TestExternalIdpAuthorizeURL(t *testing.T) {
	raw := externalIdpAuthorizeURL(
		"https://login.microsoftonline.com/tenant/oauth2/v2.0/authorize",
		"client", "http://localhost:3128/oauth/callback", "scope offline_access",
		"challenge", "state", "user@example.com",
	)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("state") != "state" {
		t.Fatalf("unexpected authorize query: %s", parsed.RawQuery)
	}
}

func TestExternalIdpAuthorizeURLOmitsEmptyLoginHint(t *testing.T) {
	raw := externalIdpAuthorizeURL(
		"https://login.microsoftonline.com/tenant/oauth2/v2.0/authorize",
		"client", "http://localhost:3128/oauth/callback", "scope",
		"challenge", "state", "",
	)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if _, ok := parsed.Query()["login_hint"]; ok {
		t.Fatal("empty login hint should be omitted")
	}
}

func testJWT(payload string) string {
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return encode(`{"alg":"none"}`) + "." + encode(payload) + ".signature"
}
