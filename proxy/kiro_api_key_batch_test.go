package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func decodeKiroAPIKeyBatchResponse(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Success bool                    `json:"success"`
	Counts  kiroAPIKeyBatchCounts   `json:"counts"`
	Results []kiroAPIKeyBatchResult `json:"results"`
} {
	t.Helper()
	var response struct {
		Success bool                    `json:"success"`
		Counts  kiroAPIKeyBatchCounts   `json:"counts"`
		Results []kiroAPIKeyBatchResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode batch response: %v; body=%s", err, recorder.Body.String())
	}
	return response
}

func TestKiroAPIKeyBatchAddsOnePerLineWithPartialResults(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var callsMu sync.Mutex
	var calls []string
	stubKiroAPIKeyProbe(t, func(_ context.Context, key, region, proxyURL string) (*config.AccountInfo, error) {
		callsMu.Lock()
		calls = append(calls, key+"|"+region+"|"+proxyURL)
		callsMu.Unlock()
		return &config.AccountInfo{
			Email:            key + "@example.invalid",
			UserId:           "user-" + key[len("ksk_"):],
			SubscriptionType: "POWER",
		}, nil
	})

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader(`{
		"keys":"ksk_batch_one\n\nksk_batch_two\nksk_batch_one\nnot-a-kiro-key",
		"nicknamePrefix":"pool",
		"region":"us-east-1",
		"proxyURL":"direct",
		"enabled":false
	}`))
	h.apiAddKiroAPIKeysBatch(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeKiroAPIKeyBatchResponse(t, recorder)
	if response.Success || response.Counts.Total != 5 || response.Counts.NonEmpty != 4 ||
		response.Counts.IgnoredEmptyLines != 1 || response.Counts.Created != 2 ||
		response.Counts.Duplicates != 1 || response.Counts.Failed != 1 {
		t.Fatalf("unexpected batch counts: %+v response=%+v", response.Counts, response)
	}
	if len(response.Results) != 4 || response.Results[1].Status != "created" ||
		response.Results[2].Status != "duplicate" || response.Results[3].Status != "failed" {
		t.Fatalf("unexpected per-line results: %+v", response.Results)
	}
	if strings.Contains(recorder.Body.String(), "ksk_batch_one") || strings.Contains(recorder.Body.String(), "ksk_batch_two") {
		t.Fatalf("batch response leaked a raw Kiro API key: %s", recorder.Body.String())
	}

	accounts := config.GetAccounts()
	byKey := make(map[string]config.Account)
	for _, account := range accounts {
		byKey[account.KiroApiKey] = account
	}
	for key, wantNickname := range map[string]string{
		"ksk_batch_one": "pool-1",
		"ksk_batch_two": "pool-3",
	} {
		account, ok := byKey[key]
		if !ok || account.Nickname != wantNickname || account.Region != "us-east-1" || account.ProxyURL != "direct" || account.Enabled {
			t.Fatalf("unexpected persisted account for %s: %+v", key, account)
		}
	}
	callsMu.Lock()
	callCount := len(calls)
	for _, call := range calls {
		if !strings.HasSuffix(call, "|us-east-1|direct") {
			t.Fatalf("probe did not receive common region/proxy: %s", call)
		}
	}
	callsMu.Unlock()
	if callCount != 2 {
		t.Fatalf("probe call count=%d, want 2", callCount)
	}

	duplicateRecorder := httptest.NewRecorder()
	h.apiAddKiroAPIKeysBatch(duplicateRecorder, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader(`{"keys":"ksk_batch_one"}`)))
	if duplicateRecorder.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", duplicateRecorder.Code, duplicateRecorder.Body.String())
	}
	duplicateResponse := decodeKiroAPIKeyBatchResponse(t, duplicateRecorder)
	if duplicateResponse.Counts.Duplicates != 1 || duplicateResponse.Counts.Created != 0 || len(config.GetAccounts()) != 2 {
		t.Fatalf("existing duplicate was not skipped: %+v accounts=%d", duplicateResponse, len(config.GetAccounts()))
	}
}

func TestKiroAPIKeyBatchAcceptsPlainTextAndBoundsInput(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, region, _ string) (*config.AccountInfo, error) {
		return &config.AccountInfo{Email: "plain@example.invalid", UserId: "plain-user"}, nil
	})
	h := &Handler{pool: accountpool.GetPool()}

	plain := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader("ksk_plain_one\r\nksk_plain_two\r\n"))
	request.Header.Set("Content-Type", "text/plain")
	h.apiAddKiroAPIKeysBatch(plain, request)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain-text status=%d body=%s", plain.Code, plain.Body.String())
	}
	response := decodeKiroAPIKeyBatchResponse(t, plain)
	if response.Counts.Created != 2 || response.Counts.Failed != 0 {
		t.Fatalf("unexpected plain-text result: %+v", response)
	}

	tooMany := httptest.NewRecorder()
	tooManyBody := strings.Repeat("ksk_x\n", maxKiroAPIKeyBatchEntries+1)
	h.apiAddKiroAPIKeysBatch(tooMany, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader(tooManyBody)))
	if tooMany.Code != http.StatusBadRequest || !strings.Contains(tooMany.Body.String(), "at most") {
		t.Fatalf("too-many status=%d body=%s", tooMany.Code, tooMany.Body.String())
	}

	tooLarge := httptest.NewRecorder()
	h.apiAddKiroAPIKeysBatch(tooLarge, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader(strings.Repeat("x", int(maxKiroAPIKeyBatchBodyBytes)+1))))
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too-large status=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}
}

func TestKiroAPIKeyBatchRedactsProbeErrors(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const key = "ksk_sensitive_batch_key"
	stubKiroAPIKeyProbe(t, func(_ context.Context, keyValue, _, _ string) (*config.AccountInfo, error) {
		return nil, fmt.Errorf("upstream rejected credential %s", keyValue)
	})
	h := &Handler{pool: accountpool.GetPool()}
	recorder := httptest.NewRecorder()
	h.apiAddKiroAPIKeysBatch(recorder, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/kiro-api-keys/batch", strings.NewReader(`{"keys":"`+key+`","region":"us-east-1"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeKiroAPIKeyBatchResponse(t, recorder)
	if response.Counts.Failed != 1 || len(response.Results) != 1 || response.Results[0].Status != "failed" {
		t.Fatalf("unexpected failure result: %+v", response)
	}
	if strings.Contains(recorder.Body.String(), key) {
		t.Fatalf("probe error leaked raw key: %s", recorder.Body.String())
	}
}

func TestKiroAPIKeyBatchRejectsInvalidKeyLengthsBeforeProbe(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var calls int
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, _, _ string) (*config.AccountInfo, error) {
		calls++
		return &config.AccountInfo{}, nil
	})
	h := &Handler{pool: accountpool.GetPool()}
	longKey := "ksk_" + strings.Repeat("a", maxKiroAPIKeyBatchKeyBytes)
	recorder := httptest.NewRecorder()
	h.apiAddKiroAPIKeysBatch(recorder, httptest.NewRequest(
		http.MethodPost,
		"/admin/api/accounts/kiro-api-keys/batch",
		strings.NewReader("ksk_\n"+longKey),
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeKiroAPIKeyBatchResponse(t, recorder)
	if response.Counts.Failed != 2 || len(response.Results) != 2 || calls != 0 {
		t.Fatalf("invalid keys were not rejected before probing: calls=%d response=%+v", calls, response)
	}
}
