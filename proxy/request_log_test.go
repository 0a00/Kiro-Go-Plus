package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

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
	h := &Handler{requestLog: newRequestLog(defaultRequestLogLimit)}
	payload := &KiroPayload{
		requireToolUse: true,
		toolUsePolicy:  toolUsePolicyExplicit,
		ToolNameMap:    map[string]string{"writeH123": "mcp__workspace__Write"},
	}
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
