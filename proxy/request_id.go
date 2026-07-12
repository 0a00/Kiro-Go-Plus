package proxy

import (
	"context"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

type requestIDContextKey struct{}

func withRequestID(w http.ResponseWriter, r *http.Request) *http.Request {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if !validRequestID(requestID) {
		requestID = "req_" + uuid.NewString()
	}
	w.Header().Set("X-Request-Id", requestID)
	return r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
}

func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
