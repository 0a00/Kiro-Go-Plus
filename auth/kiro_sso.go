package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	kiroSignInBaseURL    = "https://app.kiro.dev/signin"
	kiroRedirectURI      = "http://localhost:3128"
	kiroRedirectPort     = "3128"
	kiroRedirectFrom     = "KiroIDE"
	kiroOAuthCallback    = "/oauth/callback"
	kiroSocialTokenURL   = "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
	kiroSsoLoginTimeout  = 10 * time.Minute
	maxImportedJWTLength = 256 << 10
)

var allowedExternalIdpIssuerSuffixes = []string{
	".microsoftonline.com",
	".microsoftonline.us",
	".microsoftonline.cn",
}

var externalIdpTenantPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type KiroSsoSession struct {
	ID        string
	Verifier  string
	State     string
	Region    string
	ProxyURL  string
	ExpiresAt time.Time

	srv       *http.Server
	resultCh  chan kiroSsoCapture
	once      sync.Once
	closeOnce sync.Once
	timer     *time.Timer

	mu   sync.Mutex
	leg2 *kiroSsoLeg2
}

type kiroSsoLeg2 struct {
	state         string
	verifier      string
	tokenEndpoint string
	issuerURL     string
	clientID      string
	scopes        string
	redirectURI   string
}

type kiroSsoCapture struct {
	kind string
	code string
	err  error

	tokenEndpoint string
	issuerURL     string
	clientID      string
	scopes        string
	redirectURI   string
	codeVerifier  string
}

type KiroSsoResult struct {
	AccessToken   string
	RefreshToken  string
	AuthMethod    string
	Provider      string
	ClientID      string
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	ProfileArn    string
	Region        string
	ExpiresIn     int
	Email         string
}

var (
	kiroSsoSessions   = make(map[string]*KiroSsoSession)
	kiroSsoSessionsMu sync.RWMutex
)

func StartKiroSsoLogin(region string) (*KiroSsoSession, string, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = "us-east-1"
	}
	verifier, err := newKiroSsoSecret(96)
	if err != nil {
		return nil, "", fmt.Errorf("generate SSO verifier: %w", err)
	}
	state, err := newKiroSsoSecret(32)
	if err != nil {
		return nil, "", fmt.Errorf("generate SSO state: %w", err)
	}

	session := &KiroSsoSession{
		ID:        uuid.NewString(),
		Verifier:  verifier,
		State:     state,
		Region:    region,
		ProxyURL:  config.GetProxyURL(),
		ExpiresAt: time.Now().Add(kiroSsoLoginTimeout),
		resultCh:  make(chan kiroSsoCapture, 1),
	}
	if err := session.startListener(); err != nil {
		return nil, "", err
	}

	params := url.Values{}
	params.Set("state", state)
	params.Set("code_challenge", generateCodeChallenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("redirect_uri", kiroRedirectURI)
	params.Set("redirect_from", kiroRedirectFrom)

	kiroSsoSessionsMu.Lock()
	kiroSsoSessions[session.ID] = session
	kiroSsoSessionsMu.Unlock()
	session.timer = time.AfterFunc(kiroSsoLoginTimeout, func() {
		session.close()
		removeKiroSsoSession(session.ID)
	})
	return session, kiroSignInBaseURL + "?" + params.Encode(), nil
}

func PollKiroSsoAuth(sessionID string) (*KiroSsoResult, string, error) {
	return PollKiroSsoAuthContext(context.Background(), sessionID)
}

func PollKiroSsoAuthContext(ctx context.Context, sessionID string) (*KiroSsoResult, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	kiroSsoSessionsMu.RLock()
	session, ok := kiroSsoSessions[sessionID]
	kiroSsoSessionsMu.RUnlock()
	if !ok {
		return nil, "", fmt.Errorf("session not found or expired")
	}

	select {
	case capture := <-session.resultCh:
		session.close()
		removeKiroSsoSession(sessionID)
		if capture.err != nil {
			return nil, "", capture.err
		}
		return session.exchange(ctx, capture)
	default:
		if time.Now().After(session.ExpiresAt) {
			session.close()
			removeKiroSsoSession(sessionID)
			return nil, "", fmt.Errorf("SSO login timed out after %s", kiroSsoLoginTimeout)
		}
		return nil, "pending", nil
	}
}

func (s *KiroSsoSession) exchange(ctx context.Context, capture kiroSsoCapture) (*KiroSsoResult, string, error) {
	client, err := GetAuthClientForProxy(s.ProxyURL)
	if err != nil {
		return nil, "", fmt.Errorf("configure SSO proxy: %w", err)
	}
	if capture.kind == "external_idp" {
		access, refresh, expiresIn, err := exchangeExternalIdpCodeContext(
			ctx, client, capture.tokenEndpoint, capture.clientID, capture.code,
			capture.codeVerifier, capture.redirectURI, capture.scopes,
		)
		if err != nil {
			return nil, "", fmt.Errorf("enterprise SSO token exchange failed: %w", err)
		}
		return &KiroSsoResult{
			AccessToken: access, RefreshToken: refresh, AuthMethod: "external_idp",
			Provider: "AzureAD", ClientID: capture.clientID,
			TokenEndpoint: capture.tokenEndpoint, IssuerURL: capture.issuerURL,
			Scopes: capture.scopes, Region: s.Region, ExpiresIn: expiresIn,
			Email: ExtractEmailFromJWT(access),
		}, "completed", nil
	}

	access, refresh, expiresIn, profileArn, err := exchangeSocialCodeContext(ctx, client, capture.code, s.Verifier)
	if err != nil {
		return nil, "", fmt.Errorf("SSO token exchange failed: %w", err)
	}
	return &KiroSsoResult{
		AccessToken: access, RefreshToken: refresh, AuthMethod: "social",
		Provider: "Kiro SSO", ProfileArn: profileArn, Region: s.Region,
		ExpiresIn: expiresIn, Email: ExtractEmailFromJWT(access),
	}, "completed", nil
}

func kiroCallbackBindAddrs() []string {
	if bind := strings.TrimSpace(os.Getenv("KIRO_SSO_CALLBACK_BIND")); bind != "" {
		return []string{net.JoinHostPort(bind, kiroRedirectPort)}
	}
	return []string{"127.0.0.1:" + kiroRedirectPort, "[::1]:" + kiroRedirectPort}
}

func (s *KiroSsoSession) startListener() error {
	addrs := kiroCallbackBindAddrs()
	ln, err := net.Listen("tcp", addrs[0])
	if err != nil {
		return fmt.Errorf("cannot bind %s for the SSO callback: %w", addrs[0], err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleCallback)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serve := func(listener net.Listener) {
		go func() {
			if serveErr := s.srv.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
				logger.Debugf("[KiroSSO] callback listener %s stopped: %v", listener.Addr(), serveErr)
			}
		}()
	}
	serve(ln)
	for _, addr := range addrs[1:] {
		extra, extraErr := net.Listen("tcp", addr)
		if extraErr != nil {
			logger.Debugf("[KiroSSO] secondary callback bind %s skipped: %v", addr, extraErr)
			continue
		}
		serve(extra)
	}
	return nil
}

func (s *KiroSsoSession) close() {
	s.closeOnce.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.srv.Shutdown(ctx)
		}
	})
}

func CancelKiroSsoLogin(sessionID string) {
	kiroSsoSessionsMu.RLock()
	session, ok := kiroSsoSessions[sessionID]
	kiroSsoSessionsMu.RUnlock()
	if !ok {
		return
	}
	session.close()
	removeKiroSsoSession(sessionID)
}

func (s *KiroSsoSession) deliver(capture kiroSsoCapture) {
	s.once.Do(func() { s.resultCh <- capture })
}

func (s *KiroSsoSession) handleCallback(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := req.URL.Query()
	if req.URL.Path != kiroOAuthCallback &&
		(strings.EqualFold(strings.TrimSpace(q.Get("login_option")), "external_idp") || strings.TrimSpace(q.Get("issuer_url")) != "") {
		s.mu.Lock()
		alreadyStarted := s.leg2 != nil
		s.mu.Unlock()
		if alreadyStarted {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		issuerURL := strings.TrimSpace(q.Get("issuer_url"))
		clientID := strings.TrimSpace(q.Get("client_id"))
		scopes := strings.TrimSpace(q.Get("scopes"))
		loginHint := strings.TrimSpace(q.Get("login_hint"))
		if issuerURL == "" || clientID == "" || scopes == "" {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: fmt.Errorf("invalid external IdP descriptor")})
			return
		}
		client, err := GetAuthClientForProxy(s.ProxyURL)
		if err != nil {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: fmt.Errorf("configure SSO proxy: %w", err)})
			return
		}
		authEndpoint, tokenEndpoint, err := oidcDiscoverContext(req.Context(), client, issuerURL)
		if err != nil {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: err})
			return
		}
		verifier, err := newKiroSsoSecret(96)
		if err != nil {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: fmt.Errorf("generate IdP verifier: %w", err)})
			return
		}
		state, err := newKiroSsoSecret(32)
		if err != nil {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: fmt.Errorf("generate IdP state: %w", err)})
			return
		}
		redirectURI := kiroRedirectURI + kiroOAuthCallback
		s.mu.Lock()
		if s.leg2 != nil {
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.leg2 = &kiroSsoLeg2{
			state: state, verifier: verifier, tokenEndpoint: tokenEndpoint,
			issuerURL: issuerURL, clientID: clientID, scopes: scopes,
			redirectURI: redirectURI,
		}
		s.mu.Unlock()
		authURL := externalIdpAuthorizeURL(authEndpoint, clientID, redirectURI, scopes, generateCodeChallenge(verifier), state, loginHint)
		http.Redirect(w, req, authURL, http.StatusFound)
		return
	}

	if req.URL.Path == kiroOAuthCallback {
		s.mu.Lock()
		ctx2 := s.leg2
		s.mu.Unlock()
		state := strings.TrimSpace(q.Get("state"))
		if ctx2 == nil || state == "" || state != ctx2.state {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if errParam := strings.TrimSpace(q.Get("error")); errParam != "" {
			writeKiroCallbackPage(w, false)
			s.deliver(kiroSsoCapture{err: fmt.Errorf("external IdP authorization error: %s %s", errParam, strings.TrimSpace(q.Get("error_description")))})
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeKiroCallbackPage(w, true)
		s.deliver(kiroSsoCapture{
			kind: "external_idp", code: code, tokenEndpoint: ctx2.tokenEndpoint,
			issuerURL: ctx2.issuerURL, clientID: ctx2.clientID, scopes: ctx2.scopes,
			redirectURI: ctx2.redirectURI, codeVerifier: ctx2.verifier,
		})
		return
	}

	code := strings.TrimSpace(q.Get("code"))
	errParam := strings.TrimSpace(q.Get("error"))
	if code == "" && errParam == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if state := strings.TrimSpace(q.Get("state")); s.State == "" || state != s.State {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errParam != "" {
		writeKiroCallbackPage(w, false)
		s.deliver(kiroSsoCapture{err: fmt.Errorf("SSO authorization error: %s %s", errParam, strings.TrimSpace(q.Get("error_description")))})
		return
	}
	writeKiroCallbackPage(w, true)
	s.deliver(kiroSsoCapture{kind: "social", code: code})
}

func validateExternalIdpEndpoint(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid external IdP URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("external IdP URL must use https")
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("external IdP URL contains unsupported components")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("external IdP URL must use the default HTTPS port")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("external IdP URL must use a named host")
	}
	for _, suffix := range allowedExternalIdpIssuerSuffixes {
		if strings.HasSuffix(host, suffix) {
			return nil
		}
	}
	return fmt.Errorf("external IdP host %q is not allow-listed", host)
}

var externalIdpEndpointValidator = validateExternalIdpEndpoint

func ValidateExternalIdpEndpoint(rawURL string) error {
	return externalIdpEndpointValidator(rawURL)
}

func issuerFromAccessTokenJWT(accessToken string) string {
	var claims struct {
		Issuer string `json:"iss"`
	}
	if !decodeAccessTokenClaims(accessToken, &claims) {
		return ""
	}
	return strings.TrimSpace(claims.Issuer)
}

func ExpFromAccessTokenJWT(accessToken string) int64 {
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if !decodeAccessTokenClaims(accessToken, &claims) {
		return 0
	}
	return claims.ExpiresAt
}

func ExtractEmailFromJWT(accessToken string) string {
	var claims map[string]interface{}
	if !decodeAccessTokenClaims(accessToken, &claims) {
		return ""
	}
	for _, key := range []string{"email", "preferred_username", "upn", "unique_name", "name"} {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func decodeAccessTokenClaims(accessToken string, out interface{}) bool {
	raw := strings.TrimSpace(accessToken)
	if raw == "" || len(raw) > maxImportedJWTLength {
		return false
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > maxImportedJWTLength {
		return false
	}
	return json.Unmarshal(payload, out) == nil
}

func DeriveExternalIdpEndpoints(userID, clientID, accessToken string) (tokenEndpoint, issuerURL, scopes string) {
	source := strings.TrimSpace(userID)
	if source == "" {
		source = issuerFromAccessTokenJWT(accessToken)
	}
	if source == "" || ValidateExternalIdpEndpoint(source) != nil {
		return "", "", ""
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", "", ""
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) == 0 {
		return "", "", ""
	}
	tenant, err := url.PathUnescape(segments[0])
	if err != nil || tenant == "" || !externalIdpTenantPattern.MatchString(tenant) {
		return "", "", ""
	}
	host := strings.ToLower(u.Hostname())
	tokenEndpoint = fmt.Sprintf("https://%s/%s/oauth2/v2.0/token", host, tenant)
	issuerURL = fmt.Sprintf("https://%s/%s/v2.0", host, tenant)
	if strings.TrimSpace(clientID) != "" {
		clientID = strings.TrimSpace(clientID)
		scopes = fmt.Sprintf("api://%s/codewhisperer:conversations api://%s/codewhisperer:completions offline_access", clientID, clientID)
	}
	return tokenEndpoint, issuerURL, scopes
}

func oidcDiscoverContext(ctx context.Context, client *http.Client, issuerURL string) (string, string, error) {
	if err := ValidateExternalIdpEndpoint(issuerURL); err != nil {
		return "", "", err
	}
	if client == nil {
		return "", "", fmt.Errorf("OIDC discovery client is nil")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(strings.TrimSpace(issuerURL), "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", "", fmt.Errorf("build OIDC discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	discoveryClient := *client
	discoveryClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if discoveryClient.Timeout <= 0 || discoveryClient.Timeout > 30*time.Second {
		discoveryClient.Timeout = 30 * time.Second
	}
	resp, err := discoveryClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("OIDC discovery request failed: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := httpbody.ReadAll(resp.Body, 1<<20)
	if readErr != nil {
		return "", "", fmt.Errorf("read OIDC discovery response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("OIDC discovery failed (status %d)", resp.StatusCode)
	}
	var document struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", "", fmt.Errorf("parse OIDC discovery document: %w", err)
	}
	if document.AuthorizationEndpoint == "" || document.TokenEndpoint == "" {
		return "", "", fmt.Errorf("OIDC discovery document is incomplete")
	}
	if err := ValidateExternalIdpEndpoint(document.AuthorizationEndpoint); err != nil {
		return "", "", fmt.Errorf("authorization endpoint rejected: %w", err)
	}
	if err := ValidateExternalIdpEndpoint(document.TokenEndpoint); err != nil {
		return "", "", fmt.Errorf("token endpoint rejected: %w", err)
	}
	return document.AuthorizationEndpoint, document.TokenEndpoint, nil
}

func externalIdpAuthorizeURL(authEndpoint, clientID, redirectURI, scopes, challenge, state, loginHint string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("response_mode", "query")
	q.Set("state", state)
	if strings.TrimSpace(loginHint) != "" {
		q.Set("login_hint", strings.TrimSpace(loginHint))
	}
	return authEndpoint + "?" + q.Encode()
}

func exchangeExternalIdpCodeContext(ctx context.Context, client *http.Client, tokenEndpoint, clientID, code, verifier, redirectURI, scopes string) (string, string, int, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", scopes)
	}
	return postExternalIdpTokenContext(ctx, client, tokenEndpoint, form)
}

func exchangeSocialCodeContext(ctx context.Context, client *http.Client, code, verifier string) (string, string, int, string, error) {
	payload := map[string]string{
		"code": strings.TrimSpace(code), "code_verifier": verifier, "redirect_uri": kiroRedirectURI,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroSocialTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, "", fmt.Errorf("build social token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()
	respBody, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if readErr != nil {
		return "", "", 0, "", readErr
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	_ = json.Unmarshal(respBody, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.AccessToken == "" {
		return "", "", 0, "", fmt.Errorf("social token exchange failed (status %d)", resp.StatusCode)
	}
	return out.AccessToken, out.RefreshToken, out.ExpiresIn, out.ProfileArn, nil
}

func writeKiroCallbackPage(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	message := "Kiro sign-in complete. You can close this tab and return to the admin panel."
	if !ok {
		message = "Kiro sign-in failed. Return to the admin panel and try again."
	}
	_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>Kiro Sign-In</title></head><body style=\"font-family:sans-serif;padding:2rem\"><p>%s</p></body></html>", message)
}

func newKiroSsoSecret(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func removeKiroSsoSession(sessionID string) {
	kiroSsoSessionsMu.Lock()
	delete(kiroSsoSessions, sessionID)
	kiroSsoSessionsMu.Unlock()
}
