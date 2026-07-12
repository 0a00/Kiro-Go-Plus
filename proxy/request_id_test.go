package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithRequestIDPreservesValidAndReplacesInvalidValues(t *testing.T) {
	validRequest := httptest.NewRequest(http.MethodGet, "/health", nil)
	validRequest.Header.Set("X-Request-Id", "client-request-123")
	validRecorder := httptest.NewRecorder()
	validRequest = withRequestID(validRecorder, validRequest)
	if got := requestIDFromContext(validRequest.Context()); got != "client-request-123" {
		t.Fatalf("expected valid request ID to be preserved, got %q", got)
	}
	if got := validRecorder.Header().Get("X-Request-Id"); got != "client-request-123" {
		t.Fatalf("expected response request ID, got %q", got)
	}

	invalidRequest := httptest.NewRequest(http.MethodGet, "/health", nil)
	invalidRequest.Header.Set("X-Request-Id", "bad request id")
	invalidRecorder := httptest.NewRecorder()
	invalidRequest = withRequestID(invalidRecorder, invalidRequest)
	generated := requestIDFromContext(invalidRequest.Context())
	if !strings.HasPrefix(generated, "req_") || generated == "bad request id" {
		t.Fatalf("expected generated request ID, got %q", generated)
	}
}
