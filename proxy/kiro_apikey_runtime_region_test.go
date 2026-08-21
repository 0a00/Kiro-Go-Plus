package proxy

import (
	"bytes"
	"context"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallKiroAPIRecoversAPIKeyRuntimeRegionAndPersistsAfterSuccess(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	account := config.Account{
		ID: "api-region", Email: "region@example.test", AuthMethod: "api_key",
		KiroApiKey: "ksk_test", AccessToken: "ksk_test", Region: "us-east-1", Enabled: true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accountpool.GetPool().Reload()

	originalProbe := probeKiroAPIKeyRegion
	probeKiroAPIKeyRegion = func(_ context.Context, _ string, region, _ string) (*config.AccountInfo, error) {
		if region == "eu-central-1" {
			return &config.AccountInfo{Email: account.Email}, nil
		}
		return nil, &UpstreamError{Kind: UpstreamErrorForbidden, StatusCode: http.StatusForbidden}
	}
	t.Cleanup(func() { probeKiroAPIKeyRegion = originalProbe })

	originalClient := kiroHttpStore.Load()
	var hosts []string
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if len(hosts) == 1 {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"message":"The bearer token included in the request is invalid."}`)),
			}, nil
		}
		body := bytes.Join([][]byte{
			awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "recovered"}),
			awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
		}, nil)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})
	t.Cleanup(func() { kiroHttpStore.Store(originalClient) })

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{Content: "hello", ModelID: "claude-sonnet-4.6", Origin: "AI_EDITOR"}
	var output strings.Builder
	if err := CallKiroAPI(&account, payload, &KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }}); err != nil {
		t.Fatalf("region recovery failed: %v", err)
	}
	if output.String() != "recovered" || len(hosts) != 2 || hosts[0] != "q.us-east-1.amazonaws.com" || hosts[1] != "q.eu-central-1.amazonaws.com" {
		t.Fatalf("unexpected recovery output=%q hosts=%v", output.String(), hosts)
	}
	if account.Region != "eu-central-1" || config.GetAccounts()[0].Region != "eu-central-1" {
		t.Fatalf("confirmed region was not persisted: local=%q config=%q", account.Region, config.GetAccounts()[0].Region)
	}
	poolAccounts := accountpool.GetPool().GetAllAccounts()
	if len(poolAccounts) != 1 || poolAccounts[0].Region != "eu-central-1" {
		t.Fatalf("pool region was not refreshed: %+v", poolAccounts)
	}
}

func TestCallKiroAPIRollsBackUnconfirmedAPIKeyRegion(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)
	account := config.Account{ID: "api-rollback", AuthMethod: "api_key", KiroApiKey: "ksk_test", AccessToken: "ksk_test", Region: "us-east-1", Enabled: true}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	originalProbe := probeKiroAPIKeyRegion
	probeKiroAPIKeyRegion = func(_ context.Context, _ string, region, _ string) (*config.AccountInfo, error) {
		if region == "eu-central-1" {
			return &config.AccountInfo{}, nil
		}
		return nil, &UpstreamError{Kind: UpstreamErrorForbidden}
	}
	t.Cleanup(func() { probeKiroAPIKeyRegion = originalProbe })
	originalClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"message":"invalid bearer token"}`)),
		}, nil
	})})
	t.Cleanup(func() { kiroHttpStore.Store(originalClient) })

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{Content: "hello", ModelID: "claude-sonnet-4.6", Origin: "AI_EDITOR"}
	if err := CallKiroAPI(&account, payload, &KiroStreamCallback{}); err == nil {
		t.Fatal("expected runtime confirmation to fail")
	}
	if account.Region != "us-east-1" || config.GetAccounts()[0].Region != "us-east-1" {
		t.Fatalf("unconfirmed region was retained: local=%q config=%q", account.Region, config.GetAccounts()[0].Region)
	}
}
