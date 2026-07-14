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
	"testing"
	"time"
)

func TestRequestLogPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.json")
	upstreamActivityMs := int64(45)
	firstContentMs := int64(123)
	toolAssemblyMs := int64(78)
	log, err := newPersistentRequestLog(2, path)
	if err != nil {
		t.Fatalf("create persistent log: %v", err)
	}
	log.add(requestLogEntry{Protocol: "one", Timestamp: 1, RequestToolNames: []string{"Read"}})
	log.add(requestLogEntry{Protocol: "two", Timestamp: 2})
	log.add(requestLogEntry{
		Protocol:                "three",
		Timestamp:               3,
		UpstreamFirstActivityMs: &upstreamActivityMs,
		FirstContentMs:          &firstContentMs,
		ToolAssemblyMs:          &toolAssemblyMs,
	})
	if err := log.Flush(); err != nil {
		t.Fatalf("flush request log: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat request log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("request log mode = %o, want 600", got)
	}

	restored, err := newPersistentRequestLog(2, path)
	if err != nil {
		t.Fatalf("restore request log: %v", err)
	}
	got := restored.list(10)
	if len(got) != 2 || got[0].Protocol != "three" || got[1].Protocol != "two" {
		t.Fatalf("unexpected restored entries: %+v", got)
	}
	if got[0].ID != 3 || got[1].ID != 2 {
		t.Fatalf("unexpected restored IDs: %+v", got)
	}
	if got[0].FirstContentMs == nil || *got[0].FirstContentMs != firstContentMs {
		t.Fatalf("first-content latency was not restored: %+v", got[0])
	}
	if got[0].UpstreamFirstActivityMs == nil || *got[0].UpstreamFirstActivityMs != upstreamActivityMs || got[0].ToolAssemblyMs == nil || *got[0].ToolAssemblyMs != toolAssemblyMs {
		t.Fatalf("stream diagnostics were not restored: %+v", got[0])
	}
	if got[1].FirstContentMs != nil {
		t.Fatalf("legacy entry should not gain first-content latency: %+v", got[1])
	}
	restored.add(requestLogEntry{Protocol: "four", Timestamp: 4})
	got = restored.list(1)
	if len(got) != 1 || got[0].ID != 4 {
		t.Fatalf("restored log did not continue IDs: %+v", got)
	}
	if err := restored.Flush(); err != nil {
		t.Fatalf("flush restored request log: %v", err)
	}
}

func TestRequestFirstContentTimerIgnoresBlankAndKeepsFirstValue(t *testing.T) {
	timer := newRequestFirstContentTimer(time.Now().Add(-time.Second))
	timer.MarkText(" \n\t")
	if got := timer.Value(); got != nil {
		t.Fatalf("blank content recorded latency: %d", *got)
	}

	timer.MarkText("first token")
	first := timer.Value()
	if first == nil || *first < 900 {
		t.Fatalf("unexpected first-content latency: %v", first)
	}
	timer.Mark()
	if got := timer.Value(); got == nil || *got != *first {
		t.Fatalf("first-content latency changed: first=%v got=%v", first, got)
	}
}

func TestRequestLogPersistenceRejectsCorruptFileAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":`), 0o600); err != nil {
		t.Fatalf("write corrupt request log: %v", err)
	}

	log, err := newPersistentRequestLog(10, path)
	if err == nil {
		t.Fatal("expected corrupt request log to report an error")
	}
	if got := log.list(10); len(got) != 0 {
		t.Fatalf("corrupt request log restored entries: %+v", got)
	}
	log.add(requestLogEntry{Protocol: "recovered", Timestamp: 1})
	if err := log.Flush(); err != nil {
		t.Fatalf("replace corrupt request log: %v", err)
	}
	restored, err := newPersistentRequestLog(10, path)
	if err != nil {
		t.Fatalf("restore recovered request log: %v", err)
	}
	if got := restored.list(1); len(got) != 1 || got[0].Protocol != "recovered" {
		t.Fatalf("unexpected recovered entries: %+v", got)
	}
}

func TestRequestLogKeepsNewestFirstWithLimit(t *testing.T) {
	log := newRequestLog(2)
	log.add(requestLogEntry{Protocol: "one", Timestamp: 1})
	log.add(requestLogEntry{Protocol: "two", Timestamp: 2})
	log.add(requestLogEntry{Protocol: "three", Timestamp: 3})

	got := log.list(10)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Protocol != "three" || got[1].Protocol != "two" {
		t.Fatalf("expected newest retained entries first, got %+v", got)
	}
}

func TestRequestLogConfigureAppliesLimitImmediately(t *testing.T) {
	log := newRequestLog(3)
	log.add(requestLogEntry{Protocol: "one"})
	log.add(requestLogEntry{Protocol: "two"})
	log.add(requestLogEntry{Protocol: "three"})

	log.configure(2)
	got := log.list(10)
	if len(got) != 2 || got[0].Protocol != "three" || got[1].Protocol != "two" {
		t.Fatalf("unexpected entries after shrinking limit: %+v", got)
	}

	log.configure(4)
	log.add(requestLogEntry{Protocol: "four"})
	log.add(requestLogEntry{Protocol: "five"})
	if got := log.list(10); len(got) != 4 || got[0].Protocol != "five" {
		t.Fatalf("unexpected entries after expanding limit: %+v", got)
	}
}

func TestApiRequestLogConfigUpdatesRuntimeLimit(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	log := newRequestLog(200)
	for i := 0; i < 101; i++ {
		log.add(requestLogEntry{Protocol: "request"})
	}
	h := &Handler{requestLog: log}
	req := httptest.NewRequest(http.MethodPost, "/request-log", strings.NewReader(`{"maxEntries":100}`))
	rec := httptest.NewRecorder()

	h.apiUpdateRequestLogConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := config.GetRequestLogConfig().MaxEntries; got != 100 {
		t.Fatalf("persisted max entries = %d, want 100", got)
	}
	if got := h.requestLog.list(200); len(got) != 100 {
		t.Fatalf("runtime request log length = %d, want 100", len(got))
	}

	badReq := httptest.NewRequest(http.MethodPost, "/request-log", strings.NewReader(`{"maxEntries":99}`))
	badRec := httptest.NewRecorder()
	h.apiUpdateRequestLogConfig(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid limit to return 400, got %d", badRec.Code)
	}

	badDetailReq := httptest.NewRequest(http.MethodPost, "/request-log", strings.NewReader(`{"maxEntries":100,"detailedMaxEntries":1001,"maxDetailBytes":262144}`))
	badDetailRec := httptest.NewRecorder()
	h.apiUpdateRequestLogConfig(badDetailRec, badDetailReq)
	if badDetailRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid detail limit to return 400, got %d", badDetailRec.Code)
	}
}

func TestRequestLogAttributesAPIKeyFromPayloadContext(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{Name: "agent-test", Key: "sk-agent-test", Enabled: true})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}

	h := &Handler{requestLog: newRequestLog(defaultRequestLogLimit)}
	payload := &KiroPayload{requestContext: context.WithValue(context.Background(), apiKeyContextKey{}, entry.ID)}
	h.recordRequestLogForPayload(payload, requestLogEntry{Protocol: "claude.messages.stream", Status: "success", StatusCode: 200})

	got := h.requestLog.list(1)
	if len(got) != 1 || got[0].APIKeyID != entry.ID || got[0].APIKeyName != entry.Name {
		t.Fatalf("unexpected API key attribution: %+v", got)
	}
}

func TestRequestLogCapturesRequestedToolPolicy(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{requestLog: newRequestLog(defaultRequestLogLimit)}
	payload := &KiroPayload{
		requireToolUse: true,
		toolUsePolicy:  toolUsePolicyExplicit,
		ToolNameMap:    map[string]string{"writeH123": "mcp__workspace__Write"},
	}
	payload.beginStreamMetrics(time.Now())
	payload.recordToolStreamMetrics(16384, 24)
	payload.recordToolTruncation(12000, 18, true)
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "writeH123"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}

	h.recordRequestLogForPayload(payload, requestLogEntry{Protocol: "claude.messages", Status: "success", StatusCode: 200})
	got := h.requestLog.list(1)
	if len(got) != 1 || got[0].RequestToolCount != 1 || !got[0].ToolUseRequired {
		t.Fatalf("unexpected tool request metadata: %+v", got)
	}
	if got[0].ToolUsePolicy != toolUsePolicyExplicit {
		t.Fatalf("unexpected tool policy metadata: %+v", got[0])
	}
	if len(got[0].RequestToolNames) != 1 || got[0].RequestToolNames[0] != "mcp__workspace__Write" {
		t.Fatalf("expected restored tool name, got %+v", got[0].RequestToolNames)
	}
	if got[0].ToolArgumentBytes != 16384 || got[0].ToolFragmentCount != 24 || got[0].ToolTruncationCount != 1 || got[0].ToolRecoveryAttempts != 1 {
		t.Fatalf("expected long-tool metrics, got %+v", got[0])
	}
}

func TestAdminRequestsEndpointReturnsRequestLog(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{
		pool:       accountpool.GetPool(),
		requestLog: newRequestLog(defaultRequestLogLimit),
	}
	h.recordRequestLog(requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   "openai.chat",
		Model:      "claude-sonnet-4.5",
		Status:     "success",
		StatusCode: 200,
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/requests?limit=1", nil)
	req.Header.Set("X-Admin-Password", "changeme")
	rec := httptest.NewRecorder()

	h.handleAdminAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Requests []requestLogEntry `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Requests) != 1 || body.Requests[0].Protocol != "openai.chat" {
		t.Fatalf("unexpected response: %+v", body)
	}
}
