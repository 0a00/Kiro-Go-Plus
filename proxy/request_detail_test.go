package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestDetailSanitizesSecretsBinaryAndToolArguments(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4.5",
		"password":"do-not-store",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"keep this prompt; Authorization: Bearer secret-token-value"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8gd29ybGQ="}},
			{"type":"tool_use","id":"tool-1","name":"Write","input":{"path":"secret.txt","content":"raw arguments"}}
		]}]
	}`)

	body, truncated := sanitizeRequestDetailBody(raw, config.DefaultRequestDetailMaxBytes)
	if truncated {
		t.Fatal("sanitized request was unexpectedly truncated")
	}
	for _, forbidden := range []string{"do-not-store", "secret-token-value", "aGVsbG8gd29ybGQ=", "raw arguments", "secret.txt"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("sanitized body retained %q: %s", forbidden, body)
		}
	}
	for _, required := range []string{"keep this prompt", `"redacted": "binary"`, `"mimeType": "image/png"`, `"redacted": "tool_arguments"`, `"sha256"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("sanitized body missing %q: %s", required, body)
		}
	}
}

func TestRequestDetailStorePersistenceEvictionAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_details.json")
	store, err := newPersistentRequestDetailStore(2, config.DefaultRequestDetailMaxBytes, path)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	for i, requestID := range []string{"req-one", "req-two", "req-three"} {
		if !store.add(requestDetail{
			Version:    requestDetailStateVersion,
			RequestID:  requestID,
			Timestamp:  int64(i + 1),
			Protocol:   "claude.messages",
			Status:     "success",
			StatusCode: http.StatusOK,
		}) {
			t.Fatalf("add %s", requestID)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush store: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("request detail mode = %o, want 600", got)
	}

	restored, err := newPersistentRequestDetailStore(2, config.DefaultRequestDetailMaxBytes, path)
	if err != nil {
		t.Fatalf("restore store: %v", err)
	}
	if restored.has("req-one") || !restored.has("req-two") || !restored.has("req-three") {
		t.Fatalf("unexpected restored eviction state")
	}
	raw, ok := restored.get("req-three")
	if !ok || !json.Valid(raw) {
		t.Fatalf("restored detail is invalid: %s", raw)
	}
	if deleted := restored.clear(); deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
}

func TestRequestDetailStoreBoundsIndividualRecords(t *testing.T) {
	store := newRequestDetailStore(10, config.MinRequestDetailMaxBytes)
	detail := requestDetail{
		Version:    requestDetailStateVersion,
		RequestID:  "req-large",
		Protocol:   "openai.responses",
		Status:     "success",
		StatusCode: http.StatusOK,
		Request: requestDetailRequest{
			BodyJSON: strings.Repeat("request-data", 8000),
		},
		Response: requestDetailResponse{
			VisibleOutput:  strings.Repeat("visible-data", 8000),
			ThinkingOutput: strings.Repeat("thinking-data", 8000),
		},
	}
	if !store.add(detail) {
		t.Fatal("bounded store rejected a trimmable detail")
	}
	raw, ok := store.get("req-large")
	if !ok {
		t.Fatal("bounded detail missing")
	}
	if len(raw) > config.MinRequestDetailMaxBytes {
		t.Fatalf("detail bytes = %d, max = %d", len(raw), config.MinRequestDetailMaxBytes)
	}
	if !strings.Contains(string(raw), "truncatedFields") {
		t.Fatalf("bounded detail did not report truncation: %s", raw)
	}
}

func TestRequestDetailTraceCapturesOutputUsageToolsAttemptsAndTimeline(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer never-store-this")
	req.Header.Set("User-Agent", "detail-test")
	req = req.WithContext(context.WithValue(req.Context(), requestIDContextKey{}, "req-trace"))
	trace := newRequestDetailTrace(req, "claude.messages.stream", []byte(`{"model":"test"}`), config.DefaultRequestDetailMaxBytes)
	trace.recordText("visible", false)
	trace.recordText("thinking", true)
	trace.recordToolUseStart("tool-1", "Write")
	trace.recordToolUseDelta("tool-1", `{"path":`)
	trace.recordToolUseDelta("tool-1", `"a.txt"}`)
	trace.recordToolUseStop("tool-1")
	trace.recordUsage(KiroTokenUsage{InputTokens: 10, OutputTokens: 4, CacheReadInputTokens: 3, HasCacheBreakdown: true})
	trace.recordAttempt("account-1", "user@example.com", "runtime", "example.com", time.Now().Add(-20*time.Millisecond), http.StatusOK, "success", nil, "")

	detail, ok := trace.finalize(requestLogEntry{
		RequestID:  "req-trace",
		Protocol:   "claude.messages.stream",
		Model:      "test",
		Status:     "success",
		StatusCode: http.StatusOK,
		DurationMs: 25,
	})
	if !ok {
		t.Fatal("trace did not finalize")
	}
	if _, exists := detail.Request.Headers["Authorization"]; exists || detail.Request.Headers["User-Agent"] != "detail-test" {
		t.Fatalf("unexpected captured headers: %+v", detail.Request.Headers)
	}
	if detail.Response.VisibleOutput != "visible" || detail.Response.ThinkingOutput != "thinking" {
		t.Fatalf("unexpected output capture: %+v", detail.Response)
	}
	if detail.Response.InputTokens != 10 || detail.Response.OutputTokens != 4 || detail.Response.CacheReadInputTokens != 3 {
		t.Fatalf("unexpected usage: %+v", detail.Response)
	}
	if len(detail.Response.Tools) != 1 || detail.Response.Tools[0].ArgumentBytes == 0 || detail.Response.Tools[0].ArgumentSHA256 == "" {
		t.Fatalf("unexpected tool metadata: %+v", detail.Response.Tools)
	}
	if len(detail.Attempts) != 1 || detail.Attempts[0].Status != "success" || len(detail.Timeline) == 0 {
		t.Fatalf("missing attempt or timeline: attempts=%+v timeline=%+v", detail.Attempts, detail.Timeline)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if strings.Contains(string(raw), "a.txt") || strings.Contains(string(raw), `\"path\":`) {
		t.Fatalf("raw tool arguments were persisted: %s", raw)
	}
}

func TestRequestDetailRecordsToolFragmentsBeforeBufferedCommit(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test"}`))
	trace := newRequestDetailTrace(req, "claude.messages.stream", []byte(`{"model":"test"}`), config.DefaultRequestDetailMaxBytes)
	dispatched := 0
	target := &KiroStreamCallback{
		OnToolUse:   func(KiroToolUse) { dispatched++ },
		detailTrace: trace,
	}
	wrapper, _ := wrapMeaningfulStreamCallback(target, nil, true, true, false, false)
	wrapper.OnToolUseStart("tool-buffered", "Write")
	wrapper.OnToolUseDelta("tool-buffered", `{"path":`)
	time.Sleep(5 * time.Millisecond)
	wrapper.OnToolUseDelta("tool-buffered", `"a.txt"}`)
	if dispatched != 0 {
		t.Fatalf("buffered tool dispatched before completion: %d", dispatched)
	}
	wrapper.OnToolUse(KiroToolUse{ToolUseID: "tool-buffered", Name: "Write", Input: map[string]interface{}{"path": "a.txt"}})
	if dispatched != 1 {
		t.Fatalf("completed tool dispatch count = %d, want 1", dispatched)
	}

	detail, ok := trace.finalize(requestLogEntry{RequestID: "req-buffered", Protocol: "claude.messages.stream", Status: "success", StatusCode: http.StatusOK})
	if !ok || len(detail.Response.Tools) != 1 {
		t.Fatalf("missing buffered tool detail: %+v", detail.Response.Tools)
	}
	tool := detail.Response.Tools[0]
	if tool.FragmentCount != 2 || tool.FirstFragmentMs == nil || tool.LastFragmentMs == nil || *tool.LastFragmentMs <= *tool.FirstFragmentMs {
		t.Fatalf("tool fragment timing was not captured at receipt time: %+v", tool)
	}
}

func TestRequestDetailsCaptureInferenceProtocols(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	logConfig := config.GetRequestLogConfig()
	logConfig.DetailedLogEnabled = true
	if err := config.UpdateRequestLogConfig(logConfig); err != nil {
		t.Fatalf("enable detailed logging: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "detail-account",
		Email:       "detail@example.com",
		Enabled:     true,
		AccessToken: "test-token",
		ProfileArn:  "arn:aws:codewhisperer:profile/detail",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "detail-ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{
		pool:           pool,
		promptCache:    newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:     newRequestLog(defaultRequestLogLimit),
		requestDetails: newRequestDetailStore(logConfig.DetailedMaxEntries, logConfig.MaxDetailBytes),
	}

	tests := []struct {
		path string
		body string
	}{
		{"/v1/messages", `{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/chat/completions", `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"claude-sonnet-4.5","input":"hello","store":false}`},
		{"/v1/messages/count_tokens", `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}]}`},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", tc.path, rec.Code, rec.Body.String())
		}
	}

	entries := h.requestLog.list(10)
	if len(entries) != len(tests) {
		t.Fatalf("request log count = %d, want %d: %+v", len(entries), len(tests), entries)
	}
	wantProtocols := map[string]bool{
		"claude.messages.stream": false,
		"openai.chat":            false,
		"openai.responses":       false,
		"claude.count_tokens":    false,
	}
	for _, entry := range entries {
		raw, ok := h.requestDetails.get(entry.RequestID)
		if !ok {
			t.Fatalf("missing detail for %+v", entry)
		}
		var detail requestDetail
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		if _, known := wantProtocols[detail.Protocol]; known {
			wantProtocols[detail.Protocol] = true
		}
		if detail.Protocol != "claude.count_tokens" && len(detail.Attempts) == 0 {
			t.Fatalf("protocol %s did not record upstream attempts", detail.Protocol)
		}
	}
	for protocol, seen := range wantProtocols {
		if !seen {
			t.Fatalf("protocol %s was not captured: %+v", protocol, entries)
		}
	}
}

func TestRequestDetailsCaptureLocallyRejectedRequest(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	logConfig := config.GetRequestLogConfig()
	logConfig.DetailedLogEnabled = true
	if err := config.UpdateRequestLogConfig(logConfig); err != nil {
		t.Fatalf("enable detailed logging: %v", err)
	}
	h := &Handler{
		requestLog:     newRequestLog(defaultRequestLogLimit),
		requestDetails: newRequestDetailStore(logConfig.DetailedMaxEntries, logConfig.MaxDetailBytes),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":128,
		"thinking":{"type":"disabled","budget_tokens":64},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", rec.Code, rec.Body.String())
	}
	entries := h.requestLog.list(1)
	if len(entries) != 1 || entries[0].Protocol != "claude.count_tokens" || entries[0].Status != "rejected" || entries[0].StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected rejected request metadata: %+v", entries)
	}
	raw, ok := h.requestDetails.get(entries[0].RequestID)
	if !ok {
		t.Fatalf("rejected request detail missing: %+v", entries[0])
	}
	var detail requestDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode rejected detail: %v", err)
	}
	if !strings.Contains(detail.Response.Error, "not supported") {
		t.Fatalf("rejected detail error missing: %+v", detail.Response)
	}
}

func TestRequestDetailAdminAPIsAndRequestListLinkage(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	store := newRequestDetailStore(10, config.DefaultRequestDetailMaxBytes)
	if !store.add(requestDetail{Version: 1, RequestID: "req-admin", Protocol: "test", Status: "success"}) {
		t.Fatal("add detail")
	}
	h := &Handler{
		requestLog:     newRequestLog(defaultRequestLogLimit),
		requestDetails: store,
	}
	h.requestLog.add(requestLogEntry{RequestID: "req-admin", Protocol: "test", Status: "success", StatusCode: http.StatusOK})

	listRec := httptest.NewRecorder()
	h.apiGetRequests(listRec, httptest.NewRequest(http.MethodGet, "/requests?limit=10", nil))
	var list struct {
		Requests []requestLogEntry `json:"requests"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode request list: %v", err)
	}
	if len(list.Requests) != 1 || !list.Requests[0].DetailAvailable {
		t.Fatalf("detail linkage missing: %+v", list.Requests)
	}

	getRec := httptest.NewRecorder()
	h.apiGetRequestDetail(getRec, httptest.NewRequest(http.MethodGet, "/request-details?id=req-admin", nil), true)
	if getRec.Code != http.StatusOK || !json.Valid(getRec.Body.Bytes()) {
		t.Fatalf("get detail status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("missing download header: %v", getRec.Header())
	}

	clearRec := httptest.NewRecorder()
	h.apiClearRequestDetails(clearRec, httptest.NewRequest(http.MethodDelete, "/request-details", nil))
	if clearRec.Code != http.StatusOK || store.has("req-admin") {
		t.Fatalf("clear detail status=%d body=%s", clearRec.Code, clearRec.Body.String())
	}
}

func TestRequestDetailStoreConcurrentAccess(t *testing.T) {
	store := newRequestDetailStore(1000, config.DefaultRequestDetailMaxBytes)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				requestID := strings.Join([]string{"req", string(rune('a' + worker)), string(rune('a' + i%26))}, "-")
				store.add(requestDetail{Version: 1, RequestID: requestID, Protocol: "test", Status: "success"})
				_, _ = store.get(requestID)
				_ = store.has(requestID)
			}
		}()
	}
	wg.Wait()
	count, bytes := store.stats()
	if count == 0 || bytes == 0 {
		t.Fatalf("concurrent store remained empty: count=%d bytes=%d", count, bytes)
	}
}
