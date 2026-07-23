package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
)

// NormalizeStartURL returns the stable AWS IAM Identity Center start URL form.
func NormalizeStartURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// IsBuilderIDStartURL distinguishes the shared Builder ID portal from an
// enterprise IAM Identity Center portal.
func IsBuilderIDStartURL(raw string) bool {
	parsed, err := url.Parse(NormalizeStartURL(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Hostname(), "view.awsapps.com") &&
		strings.TrimRight(parsed.EscapedPath(), "/") == "/start"
}

// ExtractStartURLFromClientSecret decodes the AWS OIDC registration JWT and
// reads serialized.initiateLoginUri. Invalid or non-AWS values are ignored.
func ExtractStartURLFromClientSecret(clientSecret string) string {
	parts := strings.Split(strings.TrimSpace(clientSecret), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var outer struct {
		Serialized string `json:"serialized"`
	}
	if err := json.Unmarshal(payload, &outer); err != nil || strings.TrimSpace(outer.Serialized) == "" {
		return ""
	}
	var inner struct {
		InitiateLoginURI string `json:"initiateLoginUri"`
	}
	if err := json.Unmarshal([]byte(outer.Serialized), &inner); err != nil {
		return ""
	}
	startURL := NormalizeStartURL(inner.InitiateLoginURI)
	parsed, err := url.Parse(startURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".awsapps.com") ||
		strings.TrimRight(parsed.EscapedPath(), "/") != "/start" {
		return ""
	}
	return startURL
}
