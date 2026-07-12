package proxy

import (
	"net/url"
	"strings"
)

func sanitizedProxyURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "direct") {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, false
	}
	username := u.User.Username()
	_, hasPassword := u.User.Password()
	if !hasPassword {
		return raw, false
	}
	if username == "" {
		u.User = nil
	} else {
		u.User = url.User(username)
	}
	return u.String(), true
}

func preserveProxyPassword(current, replacement string) string {
	currentURL, currentErr := url.Parse(strings.TrimSpace(current))
	replacementURL, replacementErr := url.Parse(strings.TrimSpace(replacement))
	if currentErr != nil || replacementErr != nil || currentURL.User == nil {
		return replacement
	}
	currentPassword, hasCurrentPassword := currentURL.User.Password()
	if !hasCurrentPassword || replacementURL.User == nil {
		return replacement
	}
	if _, hasReplacementPassword := replacementURL.User.Password(); hasReplacementPassword {
		return replacement
	}
	if !strings.EqualFold(currentURL.Scheme, replacementURL.Scheme) ||
		!strings.EqualFold(currentURL.Host, replacementURL.Host) ||
		currentURL.User.Username() != replacementURL.User.Username() {
		return replacement
	}
	replacementURL.User = url.UserPassword(replacementURL.User.Username(), currentPassword)
	return replacementURL.String()
}

func maskedCredentialValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + strings.Repeat("*", 8) + value[len(value)-4:]
}
