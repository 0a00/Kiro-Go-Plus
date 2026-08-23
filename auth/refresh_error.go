package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// RefreshHTTPError preserves machine-readable refresh failure metadata without
// retaining or returning the upstream response body.
type RefreshHTTPError struct {
	Upstream               string
	StatusCode             int
	Code                   string
	AuthenticationMismatch bool
	CredentialRejected     bool
}

func (e *RefreshHTTPError) Error() string {
	if e == nil {
		return "token refresh failed"
	}
	upstream := strings.TrimSpace(e.Upstream)
	if upstream == "" {
		upstream = "upstream"
	}
	if e.Code != "" {
		return fmt.Sprintf("%s token refresh failed (status %d, code %s)", upstream, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("%s token refresh failed (status %d)", upstream, e.StatusCode)
}

func IsRefreshAuthMismatch(err error) bool {
	var refreshErr *RefreshHTTPError
	return errors.As(err, &refreshErr) && refreshErr.AuthenticationMismatch
}

func IsRefreshCredentialRejected(err error) bool {
	var refreshErr *RefreshHTTPError
	return errors.As(err, &refreshErr) && refreshErr.CredentialRejected
}

func newRefreshHTTPError(upstream string, statusCode int, body []byte) error {
	var payload struct {
		Error   string `json:"error"`
		Type    string `json:"__type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	code := sanitizeRefreshErrorCode(firstNonBlank(payload.Error, payload.Type, payload.Code))
	lower := strings.ToLower(string(body))
	authMismatch := statusCode == http.StatusUnauthorized
	credentialRejected := statusCode == http.StatusUnauthorized
	if statusCode != http.StatusRequestTimeout && statusCode != http.StatusTooEarly &&
		statusCode != http.StatusTooManyRequests && statusCode < http.StatusInternalServerError {
		for _, marker := range []string{
			"bad credentials", "invalid_client", "invalid client",
			"invalid_grant", "invalid grant", "invalid_request", "invalid request",
		} {
			if strings.Contains(lower, marker) {
				authMismatch = true
				break
			}
		}
		for _, marker := range []string{
			"bad credentials", "invalid_grant", "invalid grant", "invalid_token",
			"refresh token revoked", "refresh token is revoked", "token has been revoked",
			"invalid refresh token", "refresh token invalid", "refresh token has expired", "refresh token expired",
		} {
			if strings.Contains(lower, marker) {
				credentialRejected = true
				break
			}
		}
	}
	return &RefreshHTTPError{
		Upstream: upstream, StatusCode: statusCode, Code: code,
		AuthenticationMismatch: authMismatch, CredentialRejected: credentialRejected,
	}
}

func sanitizeRefreshErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "#"); index >= 0 {
		value = value[index+1:]
	}
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return ""
		}
	}
	return value
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
