package proxy

import (
	"errors"
	"strings"
	"testing"
)

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
