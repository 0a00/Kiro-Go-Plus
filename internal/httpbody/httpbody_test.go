package httpbody

import (
	"errors"
	"strings"
	"testing"
)

func TestReadAllEnforcesLimit(t *testing.T) {
	body, err := ReadAll(strings.NewReader("12345"), 4)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if string(body) != "1234" {
		t.Fatalf("unexpected truncated body %q", body)
	}
}
