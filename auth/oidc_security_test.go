package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRefreshOIDCTokenRejectsRegionHostInjection(t *testing.T) {
	called := false
	client := &http.Client{Transport: authRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}
	_, _, _, _, err := refreshOIDCToken(context.Background(), "refresh", "client", "secret", "attacker.example#", client)
	if err == nil {
		t.Fatal("expected malicious region to be rejected")
	}
	if called {
		t.Fatal("invalid region reached the HTTP transport")
	}
}

func TestRefreshTokenRejectsMalformedSuccessResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "missing access token", body: `{"expiresIn":3600}`},
		{name: "missing expiry", body: `{"accessToken":"token"}`},
		{name: "nonpositive expiry", body: `{"accessToken":"token","expiresIn":0}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: authRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(test.body)),
					Header:     make(http.Header),
				}, nil
			})}
			if _, _, _, _, err := refreshOIDCToken(context.Background(), "refresh", "client", "secret", "us-east-1", client); err == nil {
				t.Fatal("OIDC malformed success was accepted")
			}
			if _, _, _, _, err := refreshSocialToken(context.Background(), "refresh", client); err == nil {
				t.Fatal("Social malformed success was accepted")
			}
		})
	}
}

func TestRefreshHTTPErrorClassificationAndSanitization(t *testing.T) {
	tests := []struct {
		status       int
		body         string
		wantMismatch bool
		wantRejected bool
	}{
		{status: 401, body: `{"message":"unauthorized"}`, wantMismatch: true, wantRejected: true},
		{status: 400, body: `{"error":"invalid_grant","error_description":"revoked secret value"}`, wantMismatch: true, wantRejected: true},
		{status: 400, body: `{"error":"invalid_request"}`, wantMismatch: true, wantRejected: false},
		{status: 429, body: `{"error":"invalid_request"}`, wantMismatch: false, wantRejected: false},
		{status: 503, body: `{"error":"invalid_client"}`, wantMismatch: false, wantRejected: false},
		{status: 400, body: `{"error":"malformed"}`, wantMismatch: false, wantRejected: false},
	}
	for _, test := range tests {
		err := newRefreshHTTPError("test", test.status, []byte(test.body))
		if got := IsRefreshAuthMismatch(err); got != test.wantMismatch {
			t.Fatalf("status %d mismatch classification = %v, want %v", test.status, got, test.wantMismatch)
		}
		if got := IsRefreshCredentialRejected(err); got != test.wantRejected {
			t.Fatalf("status %d rejected classification = %v, want %v", test.status, got, test.wantRejected)
		}
		if strings.Contains(err.Error(), "secret value") {
			t.Fatalf("refresh error exposed upstream description: %v", err)
		}
	}
}
