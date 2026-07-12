package proxy

import (
	"context"
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveProfileArnReturnsCachedValueWithoutRequest(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request for cached profile ARN")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/test "}
	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/test" {
		t.Fatalf("expected trimmed cached ARN, got %q", got)
	}
}

func TestRegionalizeURLPrefersProfileArnRegion(t *testing.T) {
	account := &config.Account{
		Region:     "ap-southeast-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}

	rawURL := "https://q.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR"
	if got := regionalizeURL(rawURL, account); got != rawURL {
		t.Fatalf("expected profile ARN region to keep us-east-1 URL, got %q", got)
	}
}

func TestRegionalizeURLForProfileUsesPayloadProfileArnRegion(t *testing.T) {
	account := &config.Account{Region: "ap-southeast-1"}

	got := regionalizeURLForProfile(
		"https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		account,
		"arn:aws:codewhisperer:eu-central-1:123456789012:profile/test",
	)
	want := "https://q.eu-central-1.amazonaws.com/generateAssistantResponse"
	if got != want {
		t.Fatalf("expected payload profile ARN region URL %q, got %q", want, got)
	}
}

func TestKiroProfileRegionCandidates(t *testing.T) {
	tests := []struct {
		name    string
		account *config.Account
		want    []string
	}{
		{
			name:    "external idp probes built-in fallbacks",
			account: &config.Account{AuthMethod: "external_idp", Region: "eu-central-1"},
			want:    []string{"eu-central-1", "us-east-1"},
		},
		{
			name:    "account without region probes defaults",
			account: &config.Account{},
			want:    []string{"us-east-1", "eu-central-1"},
		},
		{
			name:    "established OAuth account stays in its region",
			account: &config.Account{AuthMethod: "idc", Region: "ap-southeast-2"},
			want:    []string{"ap-southeast-2"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := kiroProfileRegionCandidates(test.account); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("candidate regions = %v, want %v", got, test.want)
			}
		})
	}
}

func TestKiroProfileRegionCandidatesHonorsEnvironmentOverride(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "eu-west-1, ap-south-1,eu-west-1")
	account := &config.Account{AuthMethod: "external_idp", Region: "us-east-1"}
	want := []string{"us-east-1", "eu-west-1", "ap-south-1"}
	if got := kiroProfileRegionCandidates(account); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate regions = %v, want %v", got, want)
	}
}

func TestDiscoverKiroProfilesAcrossRegions(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1")
	var calls int32
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		if req.URL.Path != "/ListAvailableProfiles" {
			t.Fatalf("unexpected profile request path: %s", req.URL.Path)
		}
		if got := req.Header.Get("TokenType"); got != "EXTERNAL_IDP" {
			t.Fatalf("expected EXTERNAL_IDP token type, got %q", got)
		}
		body := `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:1:profile/east"}]}`
		if req.URL.Host == "q.eu-central-1.amazonaws.com" {
			body = `{"profiles":[{"arn":"arn:aws:codewhisperer:eu-central-1:1:profile/eu"}]}`
		} else if req.URL.Host != "codewhisperer.us-east-1.amazonaws.com" {
			t.Fatalf("unexpected profile request host: %s", req.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})})
	t.Cleanup(func() { InitKiroHttpClient("") })

	profiles, err := DiscoverKiroProfiles(&config.Account{
		Email:       "entra@example.com",
		AccessToken: "access-token",
		AuthMethod:  "external_idp",
		Region:      "us-east-1",
	})
	if err != nil {
		t.Fatalf("discover profiles: %v", err)
	}
	want := []KiroProfile{
		{Arn: "arn:aws:codewhisperer:us-east-1:1:profile/east", Region: "us-east-1"},
		{Arn: "arn:aws:codewhisperer:eu-central-1:1:profile/eu", Region: "eu-central-1"},
	}
	if !reflect.DeepEqual(profiles, want) {
		t.Fatalf("profiles = %+v, want %+v", profiles, want)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("profile request count = %d, want 2", got)
	}
}

func TestListAvailableModelsFollowsPaginationAndCachesTokenLimits(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	var calls int32
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := atomic.AddInt32(&calls, 1)
		if req.URL.Path != "/ListAvailableModels" || req.URL.Query().Get("maxResults") != "50" {
			t.Fatalf("unexpected models request: %s", req.URL.String())
		}
		body := `{"models":[{"modelId":"model-page-one","tokenLimits":{"maxInputTokens":111,"maxOutputTokens":11}}],"nextToken":"page-two"}`
		if call == 2 {
			if got := req.URL.Query().Get("nextToken"); got != "page-two" {
				t.Fatalf("expected pagination token, got %q", got)
			}
			body = `{"models":[{"modelId":"model-page-two","tokenLimits":{"maxInputTokens":222,"maxOutputTokens":22}}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})})
	t.Cleanup(func() { InitKiroHttpClient("") })

	models, err := ListAvailableModels(&config.Account{
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	})
	if err != nil {
		t.Fatalf("ListAvailableModels: %v", err)
	}
	if len(models) != 2 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected two pages, models=%d calls=%d", len(models), calls)
	}
	if limits, ok := getDiscoveredModelTokenLimits("model-page-two"); !ok || limits.MaxInputTokens != 222 || limits.MaxOutputTokens != 22 {
		t.Fatalf("expected discovered limits to be cached, got %+v ok=%v", limits, ok)
	}
}

func TestGetUsageLimitsContextCancelsTransport(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})
	t.Cleanup(func() { InitKiroHttpClient("") })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := GetUsageLimitsContext(ctx, &config.Account{
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	})
	if err == nil {
		t.Fatal("expected canceled usage request")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context cancellation took too long: %s", elapsed)
	}
}

func TestEnsureRestProfileArnSuppressesUnsupportedBuilderIDLookup(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	var calls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{
		ID:          "builder-1",
		Email:       "builder@example.com",
		AccessToken: "access-token",
		Provider:    "BuilderId",
		Region:      "us-east-1",
	}

	if err := ensureRestProfileArn(account); err != nil {
		t.Fatalf("expected unsupported lookup to be soft, got %v", err)
	}
	if err := ensureRestProfileArn(account); err != nil {
		t.Fatalf("expected suppressed lookup to remain soft, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one profile lookup during cooldown, got %d", got)
	}
}

func TestResolveProfileArnFetchesAndCachesProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "acct-1",
		Email:        "user@example.com",
		AccessToken:  "access-token",
		Region:       "us-east-1",
		UsageCurrent: 7,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", req.Method)
			}
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected JSON content type, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[{"arn":" arn:aws:codewhisperer:profile/fetched "}]} `)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	requestAccount.UsageCurrent = 0
	got, err := ResolveProfileArn(&requestAccount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/fetched" {
		t.Fatalf("expected fetched ARN, got %q", got)
	}
	if requestAccount.ProfileArn != got {
		t.Fatalf("expected account to be updated with fetched ARN, got %q", requestAccount.ProfileArn)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	if accounts[0].ProfileArn != got {
		t.Fatalf("expected persisted account profile ARN %q, got %q", got, accounts[0].ProfileArn)
	}
	if accounts[0].UsageCurrent != 7 {
		t.Fatalf("expected profile cache update to preserve usage fields, got usageCurrent=%v", accounts[0].UsageCurrent)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func clearProfileArnResolutionCooldowns() {
	profileArnResolutionCooldowns.Range(func(key, _ interface{}) bool {
		profileArnResolutionCooldowns.Delete(key)
		return true
	})
}
