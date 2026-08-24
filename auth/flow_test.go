package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func authResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func installAuthTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	old := SetGlobalAuthClientForTest(&http.Client{Transport: transport, Timeout: 3 * time.Second})
	t.Cleanup(func() { SetGlobalAuthClientForTest(old) })
}

func resetBuilderIDSessionFixtures(t *testing.T) {
	t.Helper()
	builderIdMu.Lock()
	previous := builderIdSessions
	builderIdSessions = make(map[string]*BuilderIdSession)
	builderIdMu.Unlock()
	t.Cleanup(func() {
		builderIdMu.Lock()
		builderIdSessions = previous
		builderIdMu.Unlock()
	})
}

func TestBuilderIDDeviceFlowPendingSlowDownAndCompletion(t *testing.T) {
	resetBuilderIDSessionFixtures(t)
	var tokenRequests atomic.Int32
	installAuthTransport(t, authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/client/register":
			return authResponse(http.StatusOK, `{"clientId":"builder-client","clientSecret":"builder-secret"}`), nil
		case "/device_authorization":
			return authResponse(http.StatusOK, `{"deviceCode":"device-code","userCode":"ABCD-EFGH","verificationUri":"https://device.example","interval":5,"expiresIn":600}`), nil
		case "/token":
			switch tokenRequests.Add(1) {
			case 1:
				return authResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
			case 2:
				return authResponse(http.StatusBadRequest, `{"error":"slow_down"}`), nil
			default:
				return authResponse(http.StatusOK, `{"accessToken":"builder-access","refreshToken":"builder-refresh","expiresIn":3600}`), nil
			}
		default:
			return authResponse(http.StatusNotFound, `{}`), nil
		}
	}))

	session, err := StartBuilderIdLogin("US-EAST-1")
	if err != nil {
		t.Fatalf("start Builder ID: %v", err)
	}
	if session.Region != "us-east-1" || session.ClientID != "builder-client" || GetBuilderIdSession(session.ID) == nil {
		t.Fatalf("unexpected Builder ID session: %+v", session)
	}
	_, _, _, _, _, _, status, err := PollBuilderIdAuth(session.ID)
	if err != nil || status != "pending" {
		t.Fatalf("pending poll: status=%q err=%v", status, err)
	}
	_, _, _, _, _, _, status, err = PollBuilderIdAuth(session.ID)
	if err != nil || status != "slow_down" || GetBuilderIdSession(session.ID).Interval != 10 {
		t.Fatalf("slow poll: status=%q session=%+v err=%v", status, GetBuilderIdSession(session.ID), err)
	}
	access, refresh, clientID, secret, region, expiresIn, status, err := PollBuilderIdAuth(session.ID)
	if err != nil || status != "completed" || access != "builder-access" || refresh != "builder-refresh" || clientID != "builder-client" || secret != "builder-secret" || region != "us-east-1" || expiresIn != 3600 {
		t.Fatalf("completed poll: access=%q refresh=%q client=%q region=%q expires=%d status=%q err=%v", access, refresh, clientID, region, expiresIn, status, err)
	}
	if GetBuilderIdSession(session.ID) != nil {
		t.Fatal("completed Builder ID session was not removed")
	}
}

func TestBuilderIDSessionsRejectInvalidAndExpiredState(t *testing.T) {
	resetBuilderIDSessionFixtures(t)
	called := false
	installAuthTransport(t, authRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return authResponse(http.StatusInternalServerError, `{}`), nil
	}))
	if _, err := StartBuilderIdLogin("invalid.region.example"); err == nil || called {
		t.Fatalf("invalid region reached transport: called=%v err=%v", called, err)
	}
	if _, _, _, _, _, _, _, err := PollBuilderIdAuth("missing"); err == nil {
		t.Fatal("missing Builder ID session was accepted")
	}
	builderIdMu.Lock()
	builderIdSessions["expired"] = &BuilderIdSession{ID: "expired", ExpiresAt: time.Now().Add(-time.Second), Region: "us-east-1"}
	builderIdSessions["cleanup"] = &BuilderIdSession{ID: "cleanup", ExpiresAt: time.Now().Add(-time.Second)}
	builderIdMu.Unlock()
	if _, _, _, _, _, _, _, err := PollBuilderIdAuth("expired"); err == nil {
		t.Fatal("expired Builder ID session was accepted")
	}
	cleanupExpiredBuilderIdSessions()
	if GetBuilderIdSession("cleanup") != nil {
		t.Fatal("expired Builder ID session survived cleanup")
	}
}

func TestImportFromSSOTokenRunsCompleteDeviceApprovalFlow(t *testing.T) {
	var tokenCalls atomic.Int32
	installAuthTransport(t, authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/client/register":
			return authResponse(http.StatusOK, `{"clientId":"sso-client","clientSecret":"sso-secret"}`), nil
		case "/device_authorization":
			return authResponse(http.StatusOK, `{"deviceCode":"sso-device","userCode":"SSO-CODE","interval":0}`), nil
		case "/token/whoAmI":
			if req.Header.Get("Authorization") != "Bearer imported-bearer" {
				return authResponse(http.StatusUnauthorized, `{}`), nil
			}
			return authResponse(http.StatusOK, `{}`), nil
		case "/session/device":
			return authResponse(http.StatusOK, `{"token":"device-session"}`), nil
		case "/device_authorization/accept_user_code":
			return authResponse(http.StatusOK, `{"deviceContext":{"deviceContextId":"ctx","clientId":"sso-client","clientType":"public"}}`), nil
		case "/device_authorization/associate_token":
			return authResponse(http.StatusOK, `{}`), nil
		case "/token":
			tokenCalls.Add(1)
			return authResponse(http.StatusOK, `{"accessToken":"sso-access","refreshToken":"sso-refresh","expiresIn":7200}`), nil
		case "/getUsageLimits":
			return authResponse(http.StatusOK, `{"userInfo":{"email":"user@example.test","userId":"user-123"}}`), nil
		default:
			return authResponse(http.StatusNotFound, `{}`), nil
		}
	}))

	access, refresh, clientID, secret, expiresIn, err := ImportFromSsoToken("imported-bearer", "eu-north-1")
	if err != nil {
		t.Fatalf("import SSO token: %v", err)
	}
	if access != "sso-access" || refresh != "sso-refresh" || clientID != "sso-client" || secret != "sso-secret" || expiresIn != 7200 || tokenCalls.Load() != 1 {
		t.Fatalf("unexpected imported credentials: access=%q refresh=%q client=%q secret=%q expires=%d calls=%d", access, refresh, clientID, secret, expiresIn, tokenCalls.Load())
	}
	email, userID, err := GetUserInfo(access)
	if err != nil || email != "user@example.test" || userID != "user-123" {
		t.Fatalf("get imported user info: email=%q userID=%q err=%v", email, userID, err)
	}
}

func TestKiroSocialCallbackPollAndSessionLifecycle(t *testing.T) {
	installAuthTransport(t, authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != kiroSocialTokenURL {
			return authResponse(http.StatusNotFound, `{}`), nil
		}
		return authResponse(http.StatusOK, `{"accessToken":"social-access","refreshToken":"social-refresh","profileArn":"profile","expiresIn":3600}`), nil
	}))
	session := &KiroSsoSession{
		ID: "social-session", Verifier: "verifier", State: "expected-state", Region: "us-east-1",
		ExpiresAt: time.Now().Add(time.Minute), resultCh: make(chan kiroSsoCapture, 1),
	}
	kiroSsoSessionsMu.Lock()
	kiroSsoSessions[session.ID] = session
	kiroSsoSessionsMu.Unlock()
	t.Cleanup(func() { removeKiroSsoSession(session.ID) })

	bad := httptest.NewRecorder()
	session.handleCallback(bad, httptest.NewRequest(http.MethodGet, "/?code=ignored&state=wrong", nil))
	if bad.Code != http.StatusNoContent || len(session.resultCh) != 0 {
		t.Fatalf("invalid callback state was accepted: status=%d pending=%d", bad.Code, len(session.resultCh))
	}
	good := httptest.NewRecorder()
	session.handleCallback(good, httptest.NewRequest(http.MethodGet, "/?code=auth-code&state=expected-state", nil))
	if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), "sign-in complete") {
		t.Fatalf("callback page = status %d body %q", good.Code, good.Body.String())
	}
	result, status, err := PollKiroSsoAuthContext(context.Background(), session.ID)
	if err != nil || status != "completed" || result.AccessToken != "social-access" || result.RefreshToken != "social-refresh" || result.ProfileArn != "profile" {
		t.Fatalf("poll social callback: result=%+v status=%q err=%v", result, status, err)
	}
	if _, _, err := PollKiroSsoAuth(session.ID); err == nil {
		t.Fatal("completed SSO session remained pollable")
	}
}

func TestKiroSSOPendingExpiryCancelAndOIDCExchange(t *testing.T) {
	pending := &KiroSsoSession{ID: "pending", ExpiresAt: time.Now().Add(time.Minute), resultCh: make(chan kiroSsoCapture, 1)}
	expired := &KiroSsoSession{ID: "expired", ExpiresAt: time.Now().Add(-time.Second), resultCh: make(chan kiroSsoCapture, 1)}
	kiroSsoSessionsMu.Lock()
	kiroSsoSessions[pending.ID] = pending
	kiroSsoSessions[expired.ID] = expired
	kiroSsoSessionsMu.Unlock()
	t.Cleanup(func() {
		CancelKiroSsoLogin(pending.ID)
		CancelKiroSsoLogin(expired.ID)
	})
	if result, status, err := PollKiroSsoAuth(pending.ID); err != nil || result != nil || status != "pending" {
		t.Fatalf("pending SSO poll: result=%+v status=%q err=%v", result, status, err)
	}
	if _, _, err := PollKiroSsoAuth(expired.ID); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expired SSO poll error = %v", err)
	}
	CancelKiroSsoLogin(pending.ID)
	if _, _, err := PollKiroSsoAuth(pending.ID); err == nil {
		t.Fatal("canceled SSO session remained pollable")
	}

	var posted url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/issuer/.well-known/openid-configuration":
			_, _ = fmt.Fprintf(w, `{"authorization_endpoint":%q,"token_endpoint":%q}`, serverURL(req, "/authorize"), serverURL(req, "/token"))
		case "/token":
			if err := req.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			posted = req.PostForm
			_, _ = w.Write([]byte(`{"access_token":"external-access","refresh_token":"external-refresh","expires_in":1800}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	oldValidator := SetExternalIdpValidatorForTest(func(raw string) error {
		if strings.HasPrefix(raw, server.URL) {
			return nil
		}
		return fmt.Errorf("unexpected endpoint %s", raw)
	})
	defer SetExternalIdpValidatorForTest(oldValidator)
	authorize, tokenEndpoint, err := oidcDiscoverContext(context.Background(), server.Client(), server.URL+"/issuer")
	if err != nil || authorize != server.URL+"/authorize" || tokenEndpoint != server.URL+"/token" {
		t.Fatalf("OIDC discovery: authorize=%q token=%q err=%v", authorize, tokenEndpoint, err)
	}
	access, refresh, expiresIn, err := exchangeExternalIdpCodeContext(context.Background(), server.Client(), tokenEndpoint, "client", "code", "verifier", "http://localhost/callback", "scope offline_access")
	if err != nil || access != "external-access" || refresh != "external-refresh" || expiresIn != 1800 {
		t.Fatalf("external exchange: access=%q refresh=%q expires=%d err=%v", access, refresh, expiresIn, err)
	}
	if posted.Get("grant_type") != "authorization_code" || posted.Get("code_verifier") != "verifier" || posted.Get("scope") != "scope offline_access" {
		t.Fatalf("external exchange form = %v", posted)
	}
}

func serverURL(req *http.Request, path string) string {
	return "http://" + req.Host + path
}
