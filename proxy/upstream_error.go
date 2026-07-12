package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UpstreamErrorKind string

const (
	UpstreamErrorUnknown             UpstreamErrorKind = "unknown"
	UpstreamErrorTokenExpired        UpstreamErrorKind = "token_expired"
	UpstreamErrorAuthRevoked         UpstreamErrorKind = "auth_revoked"
	UpstreamErrorForbidden           UpstreamErrorKind = "forbidden"
	UpstreamErrorSuspended           UpstreamErrorKind = "suspended"
	UpstreamErrorQuota               UpstreamErrorKind = "quota"
	UpstreamErrorRateLimit           UpstreamErrorKind = "rate_limit"
	UpstreamErrorModelUnavailable    UpstreamErrorKind = "model_unavailable"
	UpstreamErrorClientRequest       UpstreamErrorKind = "client_request"
	UpstreamErrorEndpointUnavailable UpstreamErrorKind = "endpoint_unavailable"
	UpstreamErrorTransient           UpstreamErrorKind = "transient"
	UpstreamErrorFirstTokenTimeout   UpstreamErrorKind = "first_token_timeout"
	UpstreamErrorCanceled            UpstreamErrorKind = "canceled"
	UpstreamErrorEmptyResponse       UpstreamErrorKind = "empty_response"
	UpstreamErrorRetryBudget         UpstreamErrorKind = "retry_budget_exhausted"
)

type UpstreamError struct {
	Kind                 UpstreamErrorKind
	StatusCode           int
	Endpoint             string
	Reason               string
	Message              string
	Body                 string
	RetryAcrossEndpoints bool
	RetryAcrossAccounts  bool
	RefreshToken         bool
	RetryAfter           time.Duration
	Cause                error
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if e.Endpoint != "" {
		parts = append(parts, "from "+e.Endpoint)
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	if e.Message != "" && !strings.EqualFold(e.Message, e.Reason) {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 && e.Cause != nil {
		return e.Cause.Error()
	}
	if len(parts) == 0 {
		return string(e.Kind)
	}
	return strings.Join(parts, ": ")
}

func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func asUpstreamError(err error) (*UpstreamError, bool) {
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr, true
	}
	return nil, false
}

type downstreamError struct {
	Status     int
	ClaudeType string
	OpenAIType string
	RetryAfter string
}

func mapDownstreamError(err error) downstreamError {
	mapped := downstreamError{
		Status:     http.StatusBadGateway,
		ClaudeType: "api_error",
		OpenAIType: "server_error",
	}
	if err == nil {
		mapped.Status = http.StatusServiceUnavailable
		return mapped
	}

	if errors.Is(err, context.DeadlineExceeded) {
		mapped.Status = http.StatusGatewayTimeout
		return mapped
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		mapped.Status = http.StatusGatewayTimeout
		return mapped
	}

	upstreamErr, ok := asUpstreamError(err)
	if !ok {
		return mapped
	}
	if upstreamErr.RetryAfter > 0 {
		seconds := int64((upstreamErr.RetryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		mapped.RetryAfter = strconv.FormatInt(seconds, 10)
	}

	switch upstreamErr.Kind {
	case UpstreamErrorClientRequest:
		mapped.Status = http.StatusBadRequest
		if upstreamErr.StatusCode == http.StatusRequestEntityTooLarge || upstreamErr.StatusCode == http.StatusUnprocessableEntity {
			mapped.Status = upstreamErr.StatusCode
		}
		mapped.ClaudeType = "invalid_request_error"
		mapped.OpenAIType = "invalid_request_error"
	case UpstreamErrorForbidden, UpstreamErrorSuspended:
		mapped.Status = http.StatusForbidden
		mapped.ClaudeType = "permission_error"
		mapped.OpenAIType = "permission_error"
	case UpstreamErrorQuota, UpstreamErrorRateLimit:
		mapped.Status = http.StatusTooManyRequests
		mapped.ClaudeType = "rate_limit_error"
		mapped.OpenAIType = "rate_limit_error"
	case UpstreamErrorTokenExpired, UpstreamErrorAuthRevoked, UpstreamErrorModelUnavailable, UpstreamErrorRetryBudget:
		// These credentials and models belong to the proxy, not the caller.
		// Returning 401 would incorrectly tell clients their own API key failed.
		mapped.Status = http.StatusServiceUnavailable
	case UpstreamErrorFirstTokenTimeout:
		mapped.Status = http.StatusGatewayTimeout
	case UpstreamErrorCanceled:
		if errors.Is(upstreamErr.Cause, context.DeadlineExceeded) {
			mapped.Status = http.StatusGatewayTimeout
		} else {
			mapped.Status = 499 // Widely used convention for a canceled client request.
		}
	case UpstreamErrorEndpointUnavailable, UpstreamErrorEmptyResponse:
		mapped.Status = http.StatusBadGateway
	case UpstreamErrorTransient, UpstreamErrorUnknown:
		if upstreamErr.StatusCode == http.StatusServiceUnavailable {
			mapped.Status = http.StatusServiceUnavailable
		} else {
			mapped.Status = http.StatusBadGateway
		}
	default:
		mapped.Status = http.StatusBadGateway
	}
	return mapped
}

func applyDownstreamErrorHeaders(w http.ResponseWriter, mapped downstreamError) {
	if mapped.RetryAfter != "" {
		w.Header().Set("Retry-After", mapped.RetryAfter)
	}
}

func shouldRetryAcrossAccounts(err error) bool {
	if err == nil {
		return false
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		return upstreamErr.RetryAcrossAccounts
	}
	return true
}

func shouldRetryAcrossEndpoints(err error) bool {
	if err == nil {
		return false
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		return upstreamErr.RetryAcrossEndpoints
	}
	return true
}

func classifyUpstreamHTTPError(statusCode int, endpoint string, body []byte) *UpstreamError {
	reason, message := upstreamErrorDetails(body)
	combined := strings.ToLower(strings.Join([]string{reason, message, string(body)}, " "))
	err := &UpstreamError{
		Kind:                 UpstreamErrorUnknown,
		StatusCode:           statusCode,
		Endpoint:             endpoint,
		Reason:               reason,
		Message:              message,
		Body:                 truncateUpstreamBody(body),
		RetryAcrossAccounts:  true,
		RetryAcrossEndpoints: false,
	}

	switch {
	case strings.Contains(combined, "temporarily_suspended") ||
		strings.Contains(combined, "temporarily is suspended") ||
		strings.Contains(combined, "account suspended") ||
		strings.Contains(combined, "unusual user activity"):
		err.Kind = UpstreamErrorSuspended
	case strings.Contains(combined, "invalid_grant") ||
		strings.Contains(combined, "refresh token revoked") ||
		strings.Contains(combined, "bad credentials"):
		err.Kind = UpstreamErrorAuthRevoked
	case statusCode == 401 || strings.Contains(combined, "token expired") ||
		strings.Contains(combined, "token has expired") ||
		strings.Contains(combined, "invalid_token") ||
		strings.Contains(combined, "access token expired"):
		err.Kind = UpstreamErrorTokenExpired
		err.RefreshToken = true
	case statusCode == 403:
		err.Kind = UpstreamErrorForbidden
		// During runtime migration an account may still be accepted by the legacy
		// data plane. Only runtime-originated unknown 403s fall back across endpoints;
		// explicit suspension/revocation markers were classified above.
		err.RetryAcrossEndpoints = strings.Contains(strings.ToLower(endpoint), "runtime")
	case statusCode == 402 || strings.Contains(combined, "monthly_request_count") ||
		strings.Contains(combined, "quota exhausted") || strings.Contains(combined, "overage"):
		err.Kind = UpstreamErrorQuota
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		err.Kind = UpstreamErrorFirstTokenTimeout
	case statusCode == 429 || strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "too many requests"):
		err.Kind = UpstreamErrorRateLimit
	case strings.Contains(combined, "invalid_model_id") || strings.Contains(combined, "invalid model") ||
		strings.Contains(combined, "model_not_found"):
		err.Kind = UpstreamErrorModelUnavailable
		err.RetryAcrossEndpoints = true
	case strings.Contains(combined, "content_length_exceeds_threshold") ||
		strings.Contains(combined, "input is too long") || statusCode == 422:
		err.Kind = UpstreamErrorClientRequest
		err.RetryAcrossAccounts = false
	case statusCode == 404 || statusCode == 405 || statusCode == 410 || statusCode == 501:
		err.Kind = UpstreamErrorEndpointUnavailable
		err.RetryAcrossEndpoints = true
	case statusCode >= 500:
		err.Kind = UpstreamErrorTransient
		err.RetryAcrossEndpoints = true
	case statusCode >= 400 && statusCode < 500:
		err.Kind = UpstreamErrorClientRequest
		err.RetryAcrossAccounts = false
	default:
		err.Kind = UpstreamErrorTransient
		err.RetryAcrossEndpoints = true
	}
	return err
}

func classifyTransportError(endpoint string, err error) *UpstreamError {
	kind := UpstreamErrorTransient
	message := "upstream transport failed"
	if errors.Is(err, context.DeadlineExceeded) {
		kind = UpstreamErrorFirstTokenTimeout
		message = "upstream did not produce a meaningful response before the timeout"
	} else if errors.Is(err, context.Canceled) {
		message = "upstream request canceled"
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			kind = UpstreamErrorFirstTokenTimeout
			message = "upstream request timed out"
		}
	}
	return &UpstreamError{
		Kind:                 kind,
		Endpoint:             endpoint,
		Message:              message,
		Cause:                err,
		RetryAcrossEndpoints: true,
		RetryAcrossAccounts:  true,
	}
}

func classifyRequestCancellation(endpoint string, err error) *UpstreamError {
	message := "client request canceled"
	if errors.Is(err, context.DeadlineExceeded) {
		message = "client request deadline exceeded"
	}
	return &UpstreamError{
		Kind:     UpstreamErrorCanceled,
		Endpoint: endpoint,
		Message:  message,
		Cause:    err,
	}
}

func newEmptyResponseError(endpoint string, retry bool) *UpstreamError {
	return &UpstreamError{
		Kind:                 UpstreamErrorEmptyResponse,
		Endpoint:             endpoint,
		Message:              "upstream returned HTTP 200 without actionable text or a complete tool call",
		RetryAcrossEndpoints: retry,
		RetryAcrossAccounts:  retry,
	}
}

func newRetryBudgetError() *UpstreamError {
	return &UpstreamError{
		Kind:    UpstreamErrorRetryBudget,
		Message: "upstream retry budget exhausted",
	}
}

func upstreamErrorDetails(body []byte) (string, string) {
	var value interface{}
	if json.Unmarshal(body, &value) != nil {
		return "", strings.TrimSpace(string(body))
	}
	var reason, message string
	var walk func(interface{})
	walk = func(current interface{}) {
		switch typed := current.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if text, ok := child.(string); ok {
					switch lower {
					case "reason", "code", "errorcode", "error_code", "__type":
						if reason == "" {
							reason = strings.TrimSpace(text)
						}
					case "message", "error", "detail", "description":
						if message == "" {
							message = strings.TrimSpace(text)
						}
					}
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return reason, message
}

func truncateUpstreamBody(body []byte) string {
	const maxBody = 2048
	text := strings.TrimSpace(string(body))
	if len(text) <= maxBody {
		return text
	}
	return text[:maxBody] + "..."
}

type upstreamAttemptBudget struct {
	mu             sync.Mutex
	attempts       int
	emptyResponses int
	maxAttempts    int
	maxEmpty       int
}

func newUpstreamAttemptBudget() *upstreamAttemptBudget {
	retry := config.GetRetryConfig()
	maxAttempts := retry.MaxUpstreamAttempts
	if retry.MaxAccountAttempts == 0 {
		maxAttempts = 0
	}
	return &upstreamAttemptBudget{
		maxAttempts: maxAttempts,
		maxEmpty:    retry.EmptyResponseRetries,
	}
}

func (b *upstreamAttemptBudget) take() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxAttempts > 0 && b.attempts >= b.maxAttempts {
		return false
	}
	b.attempts++
	return true
}

func (b *upstreamAttemptBudget) recordEmpty() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.emptyResponses >= b.maxEmpty {
		return false
	}
	b.emptyResponses++
	return true
}
