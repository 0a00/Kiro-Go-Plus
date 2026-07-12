package proxy

import (
	"kiro-go/config"
	"net/http"
	"strings"
	"testing"
)

func TestBuildStreamingHeaderValuesAlignsWithKiroIDEFormat(t *testing.T) {
	account := &config.Account{MachineId: "machine-123"}
	values := buildStreamingHeaderValues(account, "q.us-east-1.amazonaws.com")

	if values.Host != "q.us-east-1.amazonaws.com" {
		t.Fatalf("expected host to be preserved, got %q", values.Host)
	}
	if !strings.Contains(values.UserAgent, "aws-sdk-js/1.0.34") {
		t.Fatalf("expected streaming sdk version in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "api/codewhispererstreaming#1.0.34") {
		t.Fatalf("expected streaming API marker in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "KiroIDE-0.11.107-machine-123") {
		t.Fatalf("expected kiro version and machine id in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.AmzUserAgent, "aws-sdk-js/1.0.34 KiroIDE-0.11.107-machine-123") {
		t.Fatalf("expected x-amz-user-agent to include version and machine id, got %q", values.AmzUserAgent)
	}
}

func TestBuildRuntimeHeaderValuesUsesRuntimeAPIFormat(t *testing.T) {
	account := &config.Account{MachineId: "machine-456"}
	values := buildRuntimeHeaderValues(account, "codewhisperer.us-east-1.amazonaws.com")

	if !strings.Contains(values.UserAgent, "aws-sdk-js/1.0.0") {
		t.Fatalf("expected runtime sdk version in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "api/codewhispererruntime#1.0.0") {
		t.Fatalf("expected runtime API marker in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "m/N,E") {
		t.Fatalf("expected runtime mode marker in user agent, got %q", values.UserAgent)
	}
}

func TestApplyKiroBaseHeadersMarksAPIKeyCredential(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://q.us-east-1.amazonaws.com/generateAssistantResponse", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	applyKiroBaseHeaders(req, &config.Account{
		AccessToken: "kiro-key",
		KiroApiKey:  "kiro-key",
		AuthMethod:  "api_key",
	}, kiroHeaderValues{
		UserAgent:    "ua",
		AmzUserAgent: "amz-ua",
		Host:         "q.us-east-1.amazonaws.com",
	})

	if got := req.Header.Get("tokentype"); got != "API_KEY" {
		t.Fatalf("expected API_KEY tokentype, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer kiro-key" {
		t.Fatalf("expected bearer API key authorization, got %q", got)
	}
}

func TestApplyKiroBaseHeadersMarksExternalIdpCredential(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	applyKiroBaseHeaders(req, &config.Account{
		AccessToken: "entra-access-token",
		AuthMethod:  "external_idp",
	}, kiroHeaderValues{})

	if got := req.Header.Get("TokenType"); got != "EXTERNAL_IDP" {
		t.Fatalf("expected EXTERNAL_IDP token type, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer entra-access-token" {
		t.Fatalf("expected bearer authorization, got %q", got)
	}
}

func TestApplyKiroBaseHeadersOmitsExternalIdpTokenTypeForOtherOAuthMethods(t *testing.T) {
	for _, method := range []string{"", "idc", "social"} {
		req, err := http.NewRequest(http.MethodPost, "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		applyKiroBaseHeaders(req, &config.Account{AccessToken: "token", AuthMethod: method}, kiroHeaderValues{})
		if got := req.Header.Get("TokenType"); got != "" {
			t.Fatalf("auth method %q should not set TokenType, got %q", method, got)
		}
	}
}
