package proxy

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticPayloadUsesRequestedFallbackModel(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateDiagnosticConfig(config.DiagnosticConfig{Enabled: true, MaxEntries: 10}); err != nil {
		t.Fatalf("enable diagnostics: %v", err)
	}
	payload := &KiroPayload{}
	payload.recordModelFallback("claude-opus-5", "claude-sonnet-4.5", "always")
	h := &Handler{diagnosticLog: newDiagnosticLog(10)}
	h.recordDiagnosticFailureForPayload("claude.messages", "claude-sonnet-4.5", nil, 502, errors.New("failed"), payload)
	entries := h.diagnosticLog.list(1)
	if len(entries) != 1 || entries[0].Model != "claude-opus-5" {
		t.Fatalf("diagnostic model leaked fallback target: %+v", entries)
	}
}

func TestRedactDiagnosticTextRemovesSecretsAndEmail(t *testing.T) {
	input := `Authorization: Bearer abcdefghijklmnop
{"refreshToken":"rt-secret","clientSecret":"cs-secret","kiroApiKey":"not-a-real-key","email":"user@example.com"}`

	got := redactDiagnosticText(input)
	for _, leaked := range []string{"abcdefghijklmnop", "rt-secret", "cs-secret", "not-a-real-key", "user@example.com"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("expected %q to be redacted from %q", leaked, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "[EMAIL_REDACTED]") {
		t.Fatalf("expected redaction markers, got %q", got)
	}
}

func TestDiagnosticErrorMessageIncludesWrappedTransportCause(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorTransient,
		Endpoint: "Kiro Runtime",
		Message:  "upstream transport failed",
		Cause:    errors.New("unexpected EOF"),
	}
	got := diagnosticErrorMessage(err)
	if !strings.Contains(got, "upstream transport failed") || !strings.Contains(got, "unexpected EOF") {
		t.Fatalf("expected high-level error and root cause, got %q", got)
	}
}
