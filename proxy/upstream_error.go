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
	UpstreamErrorActionableTimeout   UpstreamErrorKind = "actionable_output_timeout"
	UpstreamErrorToolAssemblyTimeout UpstreamErrorKind = "tool_assembly_timeout"
	UpstreamErrorToolOutputTruncated UpstreamErrorKind = "tool_output_truncated"
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
	ToolName             string
	ArgumentBytes        int
	FragmentCount        int
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

func upstreamErrorEndpoint(err error) string {
	if upstreamErr, ok := asUpstreamError(err); ok {
		return upstreamErr.Endpoint
	}
	return ""
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
	case UpstreamErrorFirstTokenTimeout, UpstreamErrorActionableTimeout, UpstreamErrorToolAssemblyTimeout:
		mapped.Status = http.StatusGatewayTimeout
	case UpstreamErrorToolOutputTruncated:
		mapped.Status = http.StatusBadGateway
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
		// Kiro Runtime and the legacy data planes do not always share quota
		// state. A quota response from one endpoint must not hide another
		// endpoint that is still usable for the same account.
		err.RetryAcrossEndpoints = true
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		err.Kind = UpstreamErrorFirstTokenTimeout
	case statusCode == 429 || strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "too many requests"):
		err.Kind = UpstreamErrorRateLimit
		// Rate limits can be isolated to one Kiro data plane. Try the remaining
		// endpoints for this account before cooling the whole account/model.
		err.RetryAcrossEndpoints = true
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

func newEmptyResponseError(endpoint string, retryEndpoints bool) *UpstreamError {
	return &UpstreamError{
		Kind:                 UpstreamErrorEmptyResponse,
		Endpoint:             endpoint,
		Message:              "upstream returned HTTP 200 without actionable text or a complete tool call",
		RetryAcrossEndpoints: retryEndpoints,
		// Exhausting endpoint-level empty retries must not prevent another
		// account from serving the request.
		RetryAcrossAccounts: true,
	}
}

func newToolAssemblyTimeoutError(endpoint, toolName string, argumentBytes int, timeout time.Duration) *UpstreamError {
	message := fmt.Sprintf("tool call did not complete within %s", timeout.Round(time.Second))
	if strings.TrimSpace(toolName) != "" {
		message = fmt.Sprintf("tool %q did not complete within %s", toolName, timeout.Round(time.Second))
	}
	if argumentBytes > 0 {
		message += fmt.Sprintf(" after %d argument bytes", argumentBytes)
	}
	return &UpstreamError{
		Kind:                 UpstreamErrorToolAssemblyTimeout,
		Endpoint:             endpoint,
		Message:              message,
		RetryAcrossEndpoints: true,
		RetryAcrossAccounts:  true,
	}
}

func newActionableOutputTimeoutError(endpoint string, timeout time.Duration) *UpstreamError {
	return &UpstreamError{
		Kind:                 UpstreamErrorActionableTimeout,
		Endpoint:             endpoint,
		Message:              fmt.Sprintf("upstream did not produce actionable text or a complete tool call within %s", timeout.Round(time.Second)),
		Cause:                context.DeadlineExceeded,
		RetryAcrossEndpoints: true,
		RetryAcrossAccounts:  true,
	}
}

func newToolOutputTruncatedError(endpoint string, streamErr *EventStreamError) *UpstreamError {
	toolName := ""
	argumentBytes := 0
	fragmentCount := 0
	var cause error
	if streamErr != nil {
		toolName = streamErr.ToolName
		argumentBytes = streamErr.ArgumentBytes
		fragmentCount = streamErr.FragmentCount
		cause = streamErr
	}
	message := "upstream truncated a tool call before its JSON arguments completed"
	if strings.TrimSpace(toolName) != "" {
		message = fmt.Sprintf("upstream truncated tool %q before its JSON arguments completed", toolName)
	}
	if argumentBytes > 0 {
		message += fmt.Sprintf(" after %d argument bytes", argumentBytes)
	}
	if fragmentCount > 0 {
		message += fmt.Sprintf(" across %d fragments", fragmentCount)
	}
	return &UpstreamError{
		Kind:                 UpstreamErrorToolOutputTruncated,
		Endpoint:             endpoint,
		Message:              message,
		RetryAcrossEndpoints: true,
		RetryAcrossAccounts:  true,
		Cause:                cause,
		ToolName:             toolName,
		ArgumentBytes:        argumentBytes,
		FragmentCount:        fragmentCount,
	}
}

func newRetryBudgetError(budget *upstreamAttemptBudget) *UpstreamError {
	message := "upstream retry budget exhausted"
	if budget != nil {
		snapshot := budget.snapshot()
		limitType := "budget"
		if snapshot.MaxDuration > 0 && snapshot.Elapsed >= snapshot.MaxDuration && (snapshot.MaxAttempts == 0 || snapshot.Attempts < snapshot.MaxAttempts) {
			limitType = "window"
		}
		message = fmt.Sprintf("upstream retry %s exhausted after %d attempts in %s", limitType, snapshot.Attempts, snapshot.Elapsed.Round(time.Second))
		if snapshot.LastError != "" {
			if snapshot.LastEndpoint != "" {
				message += fmt.Sprintf("; last failure from %s: %s", snapshot.LastEndpoint, snapshot.LastError)
			} else {
				message += "; last failure: " + snapshot.LastError
			}
		}
	}
	return &UpstreamError{
		Kind:    UpstreamErrorRetryBudget,
		Message: message,
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
	startedAt      time.Time
	maxDuration    time.Duration
	lastEndpoint   string
	lastError      string
	now            func() time.Time
}

type upstreamAttemptBudgetSnapshot struct {
	Attempts     int
	MaxAttempts  int
	Elapsed      time.Duration
	MaxDuration  time.Duration
	LastEndpoint string
	LastError    string
}

func newUpstreamAttemptBudget() *upstreamAttemptBudget {
	retry := config.GetRetryConfig()
	now := time.Now
	return &upstreamAttemptBudget{
		maxAttempts: retry.MaxUpstreamAttempts,
		maxEmpty:    retry.EmptyResponseRetries,
		startedAt:   now(),
		maxDuration: time.Duration(retry.MaxRetryDurationSeconds) * time.Second,
		now:         now,
	}
}

func (b *upstreamAttemptBudget) take() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.currentTimeLocked()
	if b.maxDuration > 0 && now.Sub(b.startedAt) >= b.maxDuration {
		return false
	}
	if b.maxAttempts > 0 && b.attempts >= b.maxAttempts {
		return false
	}
	b.attempts++
	return true
}

func (b *upstreamAttemptBudget) deadline() (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxDuration <= 0 {
		return time.Time{}, false
	}
	return b.startedAt.Add(b.maxDuration), true
}

func (b *upstreamAttemptBudget) expired() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxDuration > 0 && b.currentTimeLocked().Sub(b.startedAt) >= b.maxDuration
}

func (b *upstreamAttemptBudget) recordFailure(endpoint string, err error) {
	if b == nil || err == nil {
		return
	}
	if endpoint == "" {
		endpoint = upstreamErrorEndpoint(err)
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	b.mu.Lock()
	b.lastEndpoint = strings.TrimSpace(endpoint)
	b.lastError = message
	b.mu.Unlock()
}

func (b *upstreamAttemptBudget) snapshot() upstreamAttemptBudgetSnapshot {
	if b == nil {
		return upstreamAttemptBudgetSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := b.currentTimeLocked().Sub(b.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return upstreamAttemptBudgetSnapshot{
		Attempts:     b.attempts,
		MaxAttempts:  b.maxAttempts,
		Elapsed:      elapsed,
		MaxDuration:  b.maxDuration,
		LastEndpoint: b.lastEndpoint,
		LastError:    b.lastError,
	}
}

func (b *upstreamAttemptBudget) currentTimeLocked() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
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
