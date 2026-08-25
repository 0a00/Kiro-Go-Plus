package proxy

import (
	"errors"
	"fmt"
	"kiro-go/config"
	"strings"
)

// accountInfoUnavailableError indicates that credentials remain usable but
// the optional usage/subscription endpoint is not available for this account.
// It must not enter the token-refresh failure cooldown or disable the account.
type accountInfoUnavailableError struct {
	cause error
}

func (e *accountInfoUnavailableError) Error() string {
	if e == nil || e.cause == nil {
		return "account usage metadata unavailable"
	}
	return fmt.Sprintf("account usage metadata unavailable: %v", e.cause)
}

func (e *accountInfoUnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func isAccountInfoUnavailableError(err error) bool {
	var target *accountInfoUnavailableError
	return errors.As(err, &target)
}

// isUsageLimitsUnavailable reports a control-plane failure that does not prove
// the generation credential is unusable. Usage limits are optional for some
// Builder ID/IDC identities. A generic transient failure remains a retryable
// refresh failure and is deliberately not classified by this helper.
func isUsageLimitsUnavailable(account *config.Account, err error) bool {
	if account == nil || err == nil || isKiroAPIKeyAccount(account) {
		return false
	}
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorForbidden ||
		!strings.EqualFold(strings.TrimSpace(upstreamErr.Endpoint), "GetUsageLimits") {
		return false
	}
	message := strings.ToLower(strings.Join([]string{
		upstreamErr.Reason,
		upstreamErr.Message,
		upstreamErr.Body,
	}, " "))
	if strings.Contains(message, "bad credentials") ||
		strings.Contains(message, "invalid token") ||
		strings.Contains(message, "token expired") ||
		strings.Contains(message, "access token expired") ||
		strings.Contains(message, "invalid_grant") {
		return false
	}

	provider := compactIdentityValue(account.Provider)
	authMethod := compactIdentityValue(account.AuthMethod)
	// Imports from older helpers sometimes store Builder ID in authMethod
	// (builder_id) and leave provider empty. Treat those records like the
	// canonical {authMethod:idc, provider:BuilderId} shape, while retaining the
	// explicit auth-marker exclusions above for revoked credentials.
	isBuilderID := strings.Contains(provider, "builderid") || strings.Contains(authMethod, "builderid")
	isIDC := authMethod == "idc" || authMethod == "awssso" || strings.Contains(authMethod, "identitycenter")
	return (isBuilderID || isIDC) &&
		(strings.Contains(message, "not authorized") ||
			(strings.Contains(message, "access denied") && strings.Contains(message, "usage")) ||
			strings.Contains(message, "builder id is not supported for this operation"))
}

func compactIdentityValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(value)
	return value
}
