package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestThinkingSourceReasoningFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be accepted first")
	}
	if source != thinkingSourceReasoningEvent {
		t.Fatalf("expected source to be reasoning, got %v", source)
	}
	if allowTagSource(&source) {
		t.Fatalf("expected tag source to be rejected after reasoning source selected")
	}
}

func TestApiGetAccountsExposesExplicitOutcomeCounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "outcome-counts",
		Enabled:      true,
		RequestCount: 17,
		ErrorCount:   5,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool}
	rec := httptest.NewRecorder()
	h.apiGetAccounts(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var accounts []struct {
		RequestCount int `json:"requestCount"`
		ErrorCount   int `json:"errorCount"`
		SuccessCount int `json:"successCount"`
		FailureCount int `json:"failureCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	got := accounts[0]
	if got.SuccessCount != 17 || got.FailureCount != 5 || got.RequestCount != 17 || got.ErrorCount != 5 {
		t.Fatalf("unexpected account counters: %+v", got)
	}
}

func TestThinkingConfigAPIUpdatesTokenDefaults(t *testing.T) {
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/thinking", strings.NewReader(`{
		"suffix":"-thinking",
		"openaiFormat":"reasoning_content",
		"claudeFormat":"thinking",
		"defaultBudgetTokens":4000,
		"budgetCapTokens":10000,
		"defaultMaxOutputTokens":64000,
		"defaultContextWindowTokens":1000000,
		"bufferToolStreams":true,
		"enforceAgentToolUse":true
	}`))
	(&Handler{}).apiUpdateThinkingConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetThinkingConfig()
	if got.DefaultMaxOutputTokens != 64000 || got.DefaultContextWindowTokens != 1000000 {
		t.Fatalf("unexpected persisted token defaults: %+v", got)
	}

	invalid := httptest.NewRecorder()
	(&Handler{}).apiUpdateThinkingConfig(invalid, httptest.NewRequest(http.MethodPost, "/admin/api/thinking", strings.NewReader(`{"defaultContextWindowTokens":512}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid context window to return 400, got %d", invalid.Code)
	}
}

func TestRetryConfigAPIAcceptsUnlimitedAccountPolling(t *testing.T) {
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })

	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 0
	retry.MaxRetryDurationSeconds = 1200
	retry.ToolAssemblyTimeoutSeconds = 240
	body, err := json.Marshal(retry)
	if err != nil {
		t.Fatalf("marshal retry config: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/retry", bytes.NewReader(body))
	(&Handler{}).apiUpdateRetryConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := config.GetRetryConfig().MaxAccountAttempts; got != 0 {
		t.Fatalf("max account attempts = %d, want 0", got)
	}
	got := config.GetRetryConfig()
	if got.MaxRetryDurationSeconds != 1200 || got.ToolAssemblyTimeoutSeconds != 240 {
		t.Fatalf("retry timeout settings were not persisted: %+v", got)
	}
}

func TestRetryConfigAPIPreservesNewTimeoutsWhenOmitted(t *testing.T) {
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })

	retry := config.GetRetryConfig()
	retry.MaxRetryDurationSeconds = 1200
	retry.ToolAssemblyTimeoutSeconds = 240
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("seed retry config: %v", err)
	}
	body, err := json.Marshal(retry)
	if err != nil {
		t.Fatalf("marshal retry config: %v", err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse retry config: %v", err)
	}
	delete(document, "maxRetryDurationSeconds")
	delete(document, "toolAssemblyTimeoutSeconds")
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal legacy retry config: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/retry", bytes.NewReader(body))
	(&Handler{}).apiUpdateRetryConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := config.GetRetryConfig()
	if got.MaxRetryDurationSeconds != 1200 || got.ToolAssemblyTimeoutSeconds != 240 {
		t.Fatalf("omitted timeout settings were reset: %+v", got)
	}
}

func TestRetryConfigAPIRejectsMissingAccountAttempts(t *testing.T) {
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })

	retry := config.GetRetryConfig()
	body, err := json.Marshal(retry)
	if err != nil {
		t.Fatalf("marshal retry config: %v", err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse retry config: %v", err)
	}
	delete(document, "maxAccountAttempts")
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal partial retry config: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/retry", bytes.NewReader(body))
	(&Handler{}).apiUpdateRetryConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServeHTTPRejectsOversizedRequestBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.ContentLength = maxRequestBodyBytes + 1
	rec := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestServeHTTPRejectsOversizedChunkedRequestBody(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(make([]byte, maxRequestBodyBytes+1)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiPromptCacheConfigUpdatesRuntimeTracker(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{promptCache: newPromptCacheTrackerWithSettings(time.Hour, 1)}
	req := httptest.NewRequest(http.MethodPost, "/prompt-cache", strings.NewReader(`{
		"enabled": false,
		"namespaceMode": "account_api_key",
		"cacheReadEfficiencyMin": 0.5,
		"cacheReadEfficiencyMax": 0.6,
		"kvCacheTtlSecs": 120,
		"maxEntriesPerAccount": 123,
		"maxEntriesTotal": 456
	}`))
	rec := httptest.NewRecorder()

	h.apiUpdatePromptCache(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetPromptCacheConfig()
	if got.Enabled || got.NamespaceMode != config.PromptCacheNamespaceAccountAPIKey {
		t.Fatalf("expected persisted disabled/account_api_key policy, got enabled=%v namespace=%q", got.Enabled, got.NamespaceMode)
	}
	if got.CacheReadEfficiencyMin != 0.5 || got.CacheReadEfficiencyMax != 0.6 {
		t.Fatalf("expected persisted cache efficiency range 0.5-0.6, got %v-%v", got.CacheReadEfficiencyMin, got.CacheReadEfficiencyMax)
	}
	if got.KvCacheTTLSecs != 120 {
		t.Fatalf("expected persisted kvCacheTtlSecs 120, got %d", got.KvCacheTTLSecs)
	}
	if got.MaxEntriesPerAccount != 123 || got.MaxEntriesTotal != 456 {
		t.Fatalf("expected persisted cache limits 123/456, got %d/%d", got.MaxEntriesPerAccount, got.MaxEntriesTotal)
	}
	h.promptCache.settingsMu.RLock()
	defer h.promptCache.settingsMu.RUnlock()
	if h.promptCache.enabled || h.promptCache.namespaceMode != config.PromptCacheNamespaceAccountAPIKey {
		t.Fatalf("expected runtime disabled/account_api_key policy, got enabled=%v namespace=%q", h.promptCache.enabled, h.promptCache.namespaceMode)
	}
	if h.promptCache.readEfficiencyMin != 0.5 || h.promptCache.readEfficiencyMax != 0.6 {
		t.Fatalf("expected runtime read efficiency range 0.5-0.6, got %v-%v", h.promptCache.readEfficiencyMin, h.promptCache.readEfficiencyMax)
	}
	if h.promptCache.maxSupportedTTL != 120*time.Second {
		t.Fatalf("expected runtime ttl 120s, got %v", h.promptCache.maxSupportedTTL)
	}
	if h.promptCache.maxEntriesPerAccount != 123 || h.promptCache.maxEntriesTotal != 456 {
		t.Fatalf("expected runtime cache limits 123/456, got %d/%d", h.promptCache.maxEntriesPerAccount, h.promptCache.maxEntriesTotal)
	}
}

func TestApiPromptCacheReportsStatsAndClearsState(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	tracker.Update("acct-1", &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	})
	tracker.RecordUsage(promptCacheUsage{CacheReadInputTokens: 100}, true)
	h := &Handler{promptCache: tracker}

	getRecorder := httptest.NewRecorder()
	h.apiGetPromptCache(getRecorder, httptest.NewRequest(http.MethodGet, "/prompt-cache", nil))
	var response struct {
		Stats promptCacheStats `json:"stats"`
	}
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Stats.Entries != 1 || response.Stats.CacheHits != 1 {
		t.Fatalf("unexpected prompt cache stats: %+v", response.Stats)
	}

	clearRecorder := httptest.NewRecorder()
	h.apiClearPromptCache(clearRecorder, httptest.NewRequest(http.MethodDelete, "/prompt-cache", nil))
	if clearRecorder.Code != http.StatusOK || tracker.entryCountValue() != 0 || tracker.Stats().TrackedRequests != 0 {
		t.Fatalf("prompt cache was not cleared: code=%d stats=%+v", clearRecorder.Code, tracker.Stats())
	}
}

func TestDueAutoRefreshAccountsFiltersAndUsesAdaptiveStaleness(t *testing.T) {
	now := time.Now()
	h := &Handler{autoRefreshFail: map[string]int64{"cooling": now.Add(time.Hour).Unix()}}
	autoRefresh := config.AutoRefreshConfig{
		IntervalMinutes:           30,
		TokenRefreshBeforeSeconds: 120,
	}
	accounts := []config.Account{
		{ID: "disabled", Enabled: false, AccessToken: "token"},
		{ID: "fresh", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: now.Add(-5 * time.Minute).Unix()},
		{ID: "active-stale", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: now.Add(-31 * time.Minute).Unix()},
		{ID: "idle-fresh", Enabled: true, AccessToken: "token", LastRefresh: now.Add(-2 * time.Hour).Unix()},
		{ID: "token-due", Enabled: true, AccessToken: "token", RefreshToken: "refresh", ExpiresAt: now.Add(time.Minute).Unix(), LastRefresh: now.Unix()},
		{ID: "cooling", Enabled: true, AccessToken: "token", LastRefresh: 0},
		{ID: "no-creds", Enabled: true},
	}

	due := h.dueAutoRefreshAccounts(accounts, autoRefresh)
	ids := make([]string, 0, len(due))
	for _, account := range due {
		ids = append(ids, account.ID)
	}
	if got := strings.Join(ids, ","); got != "token-due,active-stale" {
		t.Fatalf("unexpected due accounts: %s", got)
	}
}

func TestAutoRefreshSelectionPrioritizesExpiringTokens(t *testing.T) {
	now := time.Now()
	h := &Handler{}
	autoRefresh := config.AutoRefreshConfig{IntervalMinutes: 30, TokenRefreshBeforeSeconds: 120}
	accounts := []config.Account{
		{ID: "regular", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: 0},
		{ID: "expires-later", Enabled: true, AccessToken: "token", RefreshToken: "refresh", ExpiresAt: now.Add(time.Minute).Unix()},
		{ID: "missing-token", Enabled: true, RefreshToken: "refresh", LastRefresh: now.Unix()},
	}
	due := h.dueAutoRefreshAccounts(accounts, autoRefresh)
	selected := h.nextAutoRefreshAccounts(due, 2)
	if len(selected) != 2 || selected[0].ID != "missing-token" || selected[1].ID != "expires-later" {
		t.Fatalf("expected urgent token refreshes first, got %+v", selected)
	}
}

func TestRebuildCachedModelsDropsRemovedModels(t *testing.T) {
	h := &Handler{modelsByAccount: map[string][]ModelInfo{
		"a": {{ModelId: "model-a"}, {ModelId: "model-shared"}},
		"b": {{ModelId: "model-shared"}},
	}}
	h.modelsCacheMu.Lock()
	h.rebuildCachedModelsLocked()
	if len(h.cachedModels) != 2 {
		h.modelsCacheMu.Unlock()
		t.Fatalf("expected two aggregate models, got %d", len(h.cachedModels))
	}
	h.modelsByAccount["a"] = []ModelInfo{{ModelId: "model-a"}}
	h.modelsByAccount["b"] = nil
	h.rebuildCachedModelsLocked()
	models := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.Unlock()
	if len(models) != 1 || models[0].ModelId != "model-a" {
		t.Fatalf("removed model remained in aggregate: %+v", models)
	}
}

func TestNextAutoRefreshAccountsRotatesWhenMaxPerRunSet(t *testing.T) {
	h := &Handler{}
	accounts := []config.Account{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
		{ID: "d"},
	}

	first := h.nextAutoRefreshAccounts(accounts, 2)
	second := h.nextAutoRefreshAccounts(accounts, 2)
	third := h.nextAutoRefreshAccounts(accounts, 2)

	gotIDs := func(items []*config.Account) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.ID)
		}
		return out
	}
	if got := strings.Join(gotIDs(first), ","); got != "a,b" {
		t.Fatalf("expected first window a,b, got %s", got)
	}
	if got := strings.Join(gotIDs(second), ","); got != "c,d" {
		t.Fatalf("expected second window c,d, got %s", got)
	}
	if got := strings.Join(gotIDs(third), ","); got != "a,b" {
		t.Fatalf("expected third window a,b, got %s", got)
	}
}

func TestApiRuntimeConfigUpdatesConfigAndLogger(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	prevLevel := logger.GetLevel()
	defer logger.SetLevel(prevLevel)
	logger.SetLevel(logger.LevelInfo)

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/runtime-config", strings.NewReader(`{
		"host": "127.0.0.1",
		"port": 9090,
		"logLevel": "debug",
		"kiroVersion": "0.12.0",
		"systemVersion": "linux#6.1",
		"nodeVersion": "22.23.0"
	}`))
	rec := httptest.NewRecorder()

	h.apiUpdateRuntimeConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if logger.GetLevel() != logger.LevelDebug {
		t.Fatalf("expected logger level debug, got %s", logger.LevelName(logger.GetLevel()))
	}
	got := config.GetRuntimeConfig()
	if got.Host != "127.0.0.1" || got.Port != 9090 || got.LogLevel != "debug" {
		t.Fatalf("unexpected runtime config: %+v", got)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["restartRequired"] != true {
		t.Fatalf("expected restartRequired=true for host/port change, got %#v", body["restartRequired"])
	}
}

func TestClaudeStreamReportsRealInputTokensAtCompletion(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "cc-account",
		Enabled:     true,
		AccessToken: "token-cc",
		ProfileArn:  "arn:aws:codewhisperer:profile/cc",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "streamed ok",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.23,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:  newRequestLog(defaultRequestLogLimit),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "streamed ok") {
		t.Fatalf("expected streamed content, got:\n%s", body)
	}
	if !strings.Contains(body, `"input_tokens":2460`) {
		t.Fatalf("expected final usage to report real input_tokens 2460, got:\n%s", body)
	}
	entries := h.requestLog.list(1)
	if len(entries) != 1 || entries[0].Protocol != "claude.messages.stream" {
		t.Fatalf("unexpected request log protocol: %+v", entries)
	}
	if entries[0].FirstContentMs == nil {
		t.Fatalf("request log did not capture first-content latency: %+v", entries[0])
	}
	if *entries[0].FirstContentMs < 0 || *entries[0].FirstContentMs > entries[0].DurationMs {
		t.Fatalf("invalid first-content latency: %+v", entries[0])
	}
}

func TestClaudeLiveModeCommitsInferredTextBeforeUpstreamCompletes(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateThinkingConfig("-thinking", "reasoning_content", "thinking", 4000, 10000, 0, 0, false, true); err != nil {
		t.Fatalf("enable live tool streams: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "stream-account",
		Enabled:     true,
		AccessToken: "token-stream",
		ProfileArn:  "arn:aws:codewhisperer:profile/stream",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	releaseUpstream := make(chan struct{})
	firstChunk := "first streamed chunk with enough substantive text to pass the downstream thinking tag safety buffer"
	secondChunk := firstChunk + " and second chunk"
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": firstChunk,
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": secondChunk,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:  newRequestLog(defaultRequestLogLimit),
	}
	server := httptest.NewServer(h)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"messages":[{"role":"user","content":"任务目标：请创建一个 HTML 文件并写入工作区。"}],
		"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object","properties":{"content":{"type":"string"}}}}]
	}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("start streamed request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	reader := bufio.NewReader(resp.Body)
	var first strings.Builder
	for !strings.Contains(first.String(), "first streamed chunk") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read first streamed chunk: %v body=%s", readErr, first.String())
		}
		first.WriteString(line)
	}
	if strings.Contains(first.String(), "second chunk") {
		t.Fatalf("upstream completed before first chunk was observed: %s", first.String())
	}

	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if !strings.Contains(string(rest), "second chunk") || !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("remaining stream is incomplete: %s", rest)
	}
}

func TestClaudeLiveToolStreamEmitsArgumentDeltasBeforeCompletion(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateThinkingConfig("-thinking", "reasoning_content", "thinking", 4000, 10000, 0, 0, false, true); err != nil {
		t.Fatalf("enable live tool streams: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "live-tool-account", Enabled: true, AccessToken: "token-live-tool", ProfileArn: "arn:aws:codewhisperer:profile/live-tool",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseStartEvent", map[string]interface{}{
			"toolUseId": "toolu_live",
			"name":      "Write",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{
			"input": `{"file_path":"index.html","content":"first-live-fragment`,
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{
			"input": ` second-live-fragment"}`,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), requestLog: newRequestLog(defaultRequestLogLimit)}
	server := httptest.NewServer(h)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"messages":[{"role":"user","content":"任务目标：请创建一个 HTML 文件并写入工作区。"}],
		"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}]
	}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("start streamed request: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var first strings.Builder
	for !strings.Contains(first.String(), "first-live-fragment") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read first tool delta: %v body=%s", readErr, first.String())
		}
		first.WriteString(line)
	}
	if strings.Contains(first.String(), "second-live-fragment") || strings.Contains(first.String(), "message_stop") {
		t.Fatalf("tool stream completed before first argument delta was observed: %s", first.String())
	}

	time.Sleep(40 * time.Millisecond)
	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining tool stream: %v", err)
	}
	full := first.String() + string(rest)
	if !strings.Contains(full, "second-live-fragment") || !strings.Contains(full, "message_stop") {
		t.Fatalf("remaining tool stream is incomplete: %s", full)
	}
	if strings.Count(full, "event: content_block_start") != 1 || strings.Count(full, `"type":"input_json_delta"`) != 2 || strings.Count(full, "event: content_block_stop") != 1 {
		t.Fatalf("tool stream emitted duplicate or missing blocks: %s", full)
	}
	entries := h.requestLog.list(1)
	if len(entries) != 1 || entries[0].FirstContentMs == nil || *entries[0].FirstContentMs >= entries[0].DurationMs {
		t.Fatalf("live tool latency was not captured before completion: %+v", entries)
	}
	if entries[0].UpstreamFirstActivityMs == nil || *entries[0].UpstreamFirstActivityMs > *entries[0].FirstContentMs {
		t.Fatalf("upstream activity latency is missing or later than downstream content: %+v", entries[0])
	}
	if entries[0].ToolAssemblyMs == nil || *entries[0].ToolAssemblyMs < 30 {
		t.Fatalf("tool assembly duration was not captured: %+v", entries[0])
	}
}

func TestClaudeLiveThinkingDoesNotRetryAfterDownstreamCommit(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateThinkingConfig("-thinking", "reasoning_content", "thinking", 4000, 10000, 0, 0, false, true); err != nil {
		t.Fatalf("enable live tool streams: %v", err)
	}
	for _, account := range []config.Account{
		{ID: "live-failure-first", Enabled: true, AccessToken: "token-live-first", ProfileArn: "arn:aws:codewhisperer:profile/live-first"},
		{ID: "live-failure-second", Enabled: true, AccessToken: "token-live-second", ProfileArn: "arn:aws:codewhisperer:profile/live-second"},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": "committed-live-reasoning",
		}))
		_, _ = w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), requestLog: newRequestLog(defaultRequestLogLimit)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"max_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"任务目标：请创建一个 HTML 文件并写入工作区。"}],
		"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object","properties":{"content":{"type":"string"}}}}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if upstreamCalls != 1 {
		t.Fatalf("live stream retried after downstream commit: calls=%d body=%s", upstreamCalls, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "committed-live-reasoning") || !strings.Contains(body, "event: error") {
		t.Fatalf("partial live stream did not end with an SSE error: %s", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("failed live stream incorrectly reported normal completion: %s", body)
	}
	entries := h.requestLog.list(1)
	if len(entries) != 1 || entries[0].Status != "failed" || entries[0].FirstContentMs == nil {
		t.Fatalf("failed live stream diagnostics are incomplete: %+v", entries)
	}
}

func TestClaudeInferredToolStreamEmitsThinkingHeartbeatBeforeToolCompletes(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	oldHeartbeatInterval := claudeStreamHeartbeatInterval
	claudeStreamHeartbeatInterval = 10 * time.Millisecond
	defer func() { claudeStreamHeartbeatInterval = oldHeartbeatInterval }()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "thinking-stream-account", Enabled: true, AccessToken: "token-thinking", ProfileArn: "arn:aws:codewhisperer:profile/thinking",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_write",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"`,
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_write",
			"name":      "Write",
			"input":     `complete"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), requestLog: newRequestLog(defaultRequestLogLimit)}
	server := httptest.NewServer(h)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"max_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"任务目标：请创建一个 HTML 文件并写入工作区。"}],
		"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}]
	}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("start streamed request: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var first strings.Builder
	for !strings.Contains(first.String(), `"type":"thinking_delta"`) {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read thinking heartbeat: %v body=%s", readErr, first.String())
		}
		first.WriteString(line)
	}
	if strings.Contains(first.String(), `"type":"tool_use"`) {
		t.Fatalf("tool completed before thinking was observed: %s", first.String())
	}

	time.Sleep(40 * time.Millisecond)
	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if !strings.Contains(string(rest), `"type":"tool_use"`) || !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("remaining tool stream is incomplete: %s", rest)
	}
	entries := h.requestLog.list(1)
	if len(entries) != 1 || entries[0].FirstContentMs == nil {
		t.Fatalf("request log did not capture completed tool latency: %+v", entries)
	}
	if *entries[0].FirstContentMs < 30 {
		t.Fatalf("blank thinking heartbeat counted as first content: %+v", entries[0])
	}
}

func TestClaudeInferredToolStreamRetriesAfterPreambleTransportFailure(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, account := range []config.Account{
		{ID: "stream-retry-first", Enabled: true, AccessToken: "token-first", ProfileArn: "arn:aws:codewhisperer:profile/first"},
		{ID: "stream-retry-second", Enabled: true, AccessToken: "token-second", ProfileArn: "arn:aws:codewhisperer:profile/second"},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
		if upstreamCalls == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
				"text": "reasoning from the failed attempt",
			}))
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": "I will create the requested file now.",
			}))
			_, _ = w.Write([]byte{0x00, 0x01, 0x02})
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_write",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"complete"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:  newRequestLog(defaultRequestLogLimit),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"max_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{"role":"user","content":"任务目标：请创建一个 HTML 文件并写入工作区。"}],
		"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstreamCalls)
	}
	if strings.Count(body, "event: message_start") != 1 {
		t.Fatalf("expected one immediate message_start, body=%s", body)
	}
	if strings.Contains(body, "I will create the requested file now") {
		t.Fatalf("failed-attempt preamble leaked downstream: %s", body)
	}
	if !strings.Contains(body, "reasoning from the failed attempt") {
		t.Fatalf("retryable thinking was not streamed downstream: %s", body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("retried tool stream is incomplete: %s", body)
	}
}

func TestRemovedClaudeAliasesReturnNotFound(t *testing.T) {
	h := &Handler{}
	for _, path := range []string{
		"/cc/v1/messages",
		"/messages",
		"/anthropic/v1/messages",
		"/cc/v1/messages/count_tokens",
		"/messages/count_tokens",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClaudeNonStreamRetriesNextAccountAfterPreResponseFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "first",
		Enabled:     true,
		AccessToken: "token-first",
		ProfileArn:  "arn:aws:codewhisperer:profile/first",
	}); err != nil {
		t.Fatalf("add first account: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "second",
		Enabled:     true,
		AccessToken: "token-second",
		ProfileArn:  "arn:aws:codewhisperer:profile/second",
	}); err != nil {
		t.Fatalf("add second account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		// Fail the first attempted account (whichever it is) so the handler
		// is forced to add it to `excluded` and retry the other one.
		if len(requestTokens) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "retried successfully",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 2 {
		t.Fatalf("expected two account attempts, got %v", requestTokens)
	}
	if requestTokens[0] == requestTokens[1] {
		t.Fatalf("expected first account to be excluded before retry, got %v", requestTokens)
	}
	tokenSet := map[string]bool{requestTokens[0]: true, requestTokens[1]: true}
	if !tokenSet["token-first"] || !tokenSet["token-second"] {
		t.Fatalf("expected both accounts to be tried, got %v", requestTokens)
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "retried successfully" {
		t.Fatalf("expected retried response content, got %#v", resp.Content)
	}
}

func TestClaudeNonStreamDisablesSuspendedAccountsAndKeepsTrying(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("acc-%d", i)
		if err := config.AddAccount(config.Account{
			ID:          id,
			Email:       id + "@example.test",
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		if len(requestTokens) < 4 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Your User ID temporarily is suspended.","reason":null}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "usable account",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to reach usable account, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 4 {
		t.Fatalf("expected four account attempts, got %v", requestTokens)
	}
	accounts := config.GetAccounts()
	banned := 0
	for _, account := range accounts {
		if account.BanStatus == "BANNED" {
			banned++
		}
	}
	if banned != 3 {
		t.Fatalf("expected first three suspended accounts to be banned, got %d", banned)
	}
}

func TestThinkingSourceTagFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected tag source to be accepted first")
	}
	if source != thinkingSourceTagBlock {
		t.Fatalf("expected source to be tag, got %v", source)
	}
	if allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be rejected after tag source selected")
	}
}

func TestThinkingSourceSameSourceRemainsAllowed(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected initial tag source selection to succeed")
	}
	if !allowTagSource(&source) {
		t.Fatalf("expected repeated tag source selection to stay allowed")
	}

	source = thinkingSourceUnknown
	if !allowReasoningSource(&source) {
		t.Fatalf("expected initial reasoning source selection to succeed")
	}
	if !allowReasoningSource(&source) {
		t.Fatalf("expected repeated reasoning source selection to stay allowed")
	}
}

func TestValidateOpenAIRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg != "" {
		t.Fatalf("expected tool-result final turn to be valid, got %q", msg)
	}
}

func TestValidateClaudeRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateClaudeRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestResolveClaudeThinkingModeHonorsRequestThinking(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		thinking     *ClaudeThinkingConfig
		wantModel    string
		wantThinking bool
	}{
		{
			name:         "adaptive request enables thinking",
			model:        "claude-sonnet-4.6",
			thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			wantModel:    "claude-sonnet-4.6",
			wantThinking: true,
		},
		{
			name:         "enabled request enables thinking",
			model:        "claude-opus-4.5",
			thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			wantModel:    "claude-opus-4.5",
			wantThinking: true,
		},
		{
			name:         "disabled request keeps thinking off",
			model:        "claude-opus-4.7",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-opus-4.7",
			wantThinking: false,
		},
		{
			name:         "suffix remains supported when thinking is disabled",
			model:        "claude-sonnet-4.5-thinking",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := resolveClaudeThinkingMode(tc.model, tc.thinking, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("expected model %q, got %q", tc.wantModel, gotModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("expected thinking=%v, got %v", tc.wantThinking, gotThinking)
			}
		})
	}
}

func TestCloneClaudeRequestForThinkingInjectsPromptWithoutMutatingOriginal(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.6",
		System: "Follow the user instructions.",
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected cloned system prompt to be structured blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	gotPrompt := extractSystemPrompt(cloned.System)
	expected := ThinkingModePrompt + "\n\nFollow the user instructions."
	if gotPrompt != expected {
		t.Fatalf("expected injected system prompt %q, got %q", expected, gotPrompt)
	}
	if original, ok := req.System.(string); !ok || original != "Follow the user instructions." {
		t.Fatalf("expected original request system prompt to stay unchanged, got %#v", req.System)
	}
}

func TestCloneClaudeRequestForThinkingPreservesStructuredSystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "cached system",
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
					"ttl":  "5m",
				},
			},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok || first["text"] != ThinkingModePrompt+"\n" {
		t.Fatalf("expected first block to be thinking prompt, got %#v", blocks[0])
	}
	second, ok := blocks[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected original system block to remain a map, got %T", blocks[1])
	}
	cacheControl, ok := second["cache_control"].(map[string]interface{})
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("expected original cache_control to be preserved, got %#v", second["cache_control"])
	}
}

func TestThinkingPromptAffectsClaudeTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	baseTokens := estimateClaudeRequestInputTokens(req)
	thinkingTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(req, true))

	if thinkingTokens <= baseTokens {
		t.Fatalf("expected thinking tokens (%d) to exceed base tokens (%d)", thinkingTokens, baseTokens)
	}
}

func TestValidateClaudeThinkingConfig(t *testing.T) {
	tests := []struct {
		name        string
		thinking    *ClaudeThinkingConfig
		maxTokens   int
		expectError bool
	}{
		{
			name:        "adaptive is valid",
			thinking:    &ClaudeThinkingConfig{Type: "adaptive"},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled requires budget",
			thinking:    &ClaudeThinkingConfig{Type: "enabled"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled requires at least 1024 budget tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 512},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled rejects max tokens zero",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			maxTokens:   0,
			expectError: true,
		},
		{
			name:        "enabled budget must stay below max tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "disabled rejects display",
			thinking:    &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "missing type is rejected",
			thinking:    &ClaudeThinkingConfig{},
			maxTokens:   4096,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errMsg := validateClaudeThinkingConfig(tc.thinking, tc.maxTokens)
			if tc.expectError && errMsg == "" {
				t.Fatalf("expected validation error")
			}
			if !tc.expectError && errMsg != "" {
				t.Fatalf("expected thinking config to be valid, got %q", errMsg)
			}
		})
	}
}

func TestResolveClaudeThinkingResponseOptions(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *ClaudeThinkingConfig
		defaultFmt string
		wantFmt    string
		wantOmit   bool
	}{
		{
			name:       "default config is preserved when display unset",
			thinking:   &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			defaultFmt: "think",
			wantFmt:    "think",
			wantOmit:   false,
		},
		{
			name:       "summarized forces official thinking blocks",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "summarized"},
			defaultFmt: "reasoning_content",
			wantFmt:    "thinking",
			wantOmit:   false,
		},
		{
			name:       "omitted forces official thinking blocks and hides content",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "omitted"},
			defaultFmt: "think",
			wantFmt:    "thinking",
			wantOmit:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := resolveClaudeThinkingResponseOptions(tc.thinking, tc.defaultFmt)
			if opts.Format != tc.wantFmt {
				t.Fatalf("expected format %q, got %q", tc.wantFmt, opts.Format)
			}
			if opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("expected omitDisplay=%v, got %v", tc.wantOmit, opts.OmitDisplay)
			}
		})
	}
}

func TestMergeUniqueModelsPreservesUnionAcrossAccounts(t *testing.T) {
	base := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"TEXT"}},
	}
	incoming := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"image"}},
		{ModelId: "claude-opus-4-7", InputTypes: []string{"text"}},
	}

	merged := mergeUniqueModels(base, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique models, got %d", len(merged))
	}
	if !modelSupportsImage(merged[0].InputTypes) {
		t.Fatalf("expected merged input types to preserve image capability, got %#v", merged[0].InputTypes)
	}
	if merged[1].ModelId != "claude-opus-4-7" {
		t.Fatalf("expected second model to be claude-opus-4-7, got %q", merged[1].ModelId)
	}
}

func TestBuildAnthropicModelsResponseGeneratesThinkingVariants(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-sonnet-4.5",
		InputTypes: []string{"text", "image"},
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens:  200000,
			MaxOutputTokens: 64000,
		},
	}}, "-thinking")

	if len(models) != 2 {
		t.Fatalf("expected base model and thinking variant, got %d", len(models))
	}
	if models[0]["id"] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected base model id: %#v", models[0]["id"])
	}
	if models[1]["id"] != "claude-sonnet-4.5-thinking" {
		t.Fatalf("unexpected thinking model id: %#v", models[1]["id"])
	}
	if supportsImage, ok := models[0]["supports_image"].(bool); !ok || !supportsImage {
		t.Fatalf("expected image capability to be preserved, got %#v", models[0]["supports_image"])
	}
	if models[0]["context_window"] != 200000 || models[0]["max_output_tokens"] != 64000 {
		t.Fatalf("expected upstream token limits, got %#v", models[0])
	}
}
