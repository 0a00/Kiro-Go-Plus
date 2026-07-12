package proxy

import (
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
