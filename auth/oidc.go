package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oidcTokenURL 构造 idc/builderId 刷新 endpoint。测试可替换以拦截网络调用。
var oidcTokenURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
}

// socialTokenURL 构造 social 刷新 endpoint。测试可替换以拦截网络调用。
var socialTokenURL = func() string {
	return "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
}

// RefreshToken 刷新 access token
// Returns: accessToken, refreshToken, expiresAt, profileArn, error
func RefreshToken(account *config.Account) (string, string, int64, string, error) {
	return RefreshTokenContext(context.Background(), account)
}

// RefreshTokenContext refreshes an access token and cancels the underlying
// HTTP request when ctx is done.
func RefreshTokenContext(ctx context.Context, account *config.Account) (string, string, int64, string, error) {
	if account == nil {
		return "", "", 0, "", fmt.Errorf("account is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// An empty account proxy uses the already configured global client. Explicit
	// account values, including direct, get an isolated client.
	proxyURL := strings.TrimSpace(account.ProxyURL)
	client := httpClient()
	if proxyURL != "" {
		var err error
		client, err = GetAuthClientForProxy(proxyURL)
		if err != nil {
			return "", "", 0, "", fmt.Errorf("configure outbound proxy: %w", err)
		}
	}

	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		return refreshExternalIdpToken(ctx, account.RefreshToken, account.ClientID, account.TokenEndpoint, account.Scopes, client)
	}
	if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "social") {
		return refreshSocialToken(ctx, account.RefreshToken, client)
	}
	return refreshOIDCToken(ctx, account.RefreshToken, account.ClientID, account.ClientSecret, account.Region, client)
}

func refreshExternalIdpToken(ctx context.Context, refreshToken, clientID, tokenEndpoint, scopes string, client *http.Client) (string, string, int64, string, error) {
	if strings.TrimSpace(refreshToken) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(tokenEndpoint) == "" {
		return "", "", 0, "", fmt.Errorf("external IdP refresh requires refreshToken, clientId, and tokenEndpoint")
	}
	form := url.Values{}
	form.Set("client_id", strings.TrimSpace(clientID))
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", strings.TrimSpace(scopes))
	}
	accessToken, newRefreshToken, expiresIn, err := postExternalIdpTokenContext(ctx, client, tokenEndpoint, form)
	if err != nil {
		return "", "", 0, "", err
	}
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}
	return accessToken, newRefreshToken, time.Now().Unix() + int64(expiresIn), "", nil
}

func postExternalIdpTokenContext(ctx context.Context, client *http.Client, tokenEndpoint string, form url.Values) (string, string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return "", "", 0, fmt.Errorf("external IdP HTTP client is nil")
	}
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", 0, fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(tokenEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("build external IdP token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if readErr != nil {
		return "", "", 0, fmt.Errorf("read external IdP token response: %w", readErr)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || strings.TrimSpace(result.AccessToken) == "" {
		if result.Error != "" {
			return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d): %s: %s", resp.StatusCode, result.Error, result.Description)
		}
		return "", "", 0, fmt.Errorf("external IdP token exchange failed (status %d)", resp.StatusCode)
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

// refreshOIDCToken IdC/Builder ID token 刷新
func refreshOIDCToken(ctx context.Context, refreshToken, clientID, clientSecret, region string, client *http.Client) (string, string, int64, string, error) {
	if clientID == "" || clientSecret == "" {
		return "", "", 0, "", fmt.Errorf("OIDC refresh requires clientId and clientSecret")
	}
	if region == "" {
		region = "us-east-1"
	}

	url := oidcTokenURL(region)

	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody := httpbody.ReadAllTruncated(resp.Body, httpbody.DefaultLimit)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(httpbody.LimitReader(resp.Body, httpbody.DefaultLimit)).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}

// refreshSocialToken Social (GitHub/Google) token 刷新
func refreshSocialToken(ctx context.Context, refreshToken string, client *http.Client) (string, string, int64, string, error) {
	url := socialTokenURL()

	payload := map[string]string{
		"refreshToken": refreshToken,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody := httpbody.ReadAllTruncated(resp.Body, httpbody.DefaultLimit)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}

	if err := json.NewDecoder(httpbody.LimitReader(resp.Body, httpbody.DefaultLimit)).Decode(&result); err != nil {
		return "", "", 0, "", err
	}

	expiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	return result.AccessToken, result.RefreshToken, expiresAt, result.ProfileArn, nil
}
