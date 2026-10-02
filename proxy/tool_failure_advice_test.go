package proxy

import (
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolFailureAdvicePreservesErrorKindStatusAndRetryPolicy(t *testing.T) {
	cause := &EventStreamError{Kind: EventStreamIncompleteToolUse, ToolName: "Edit", ArgumentBytes: 90, FragmentCount: 13}
	for _, tc := range []struct {
		err    *UpstreamError
		kind   UpstreamErrorKind
		status int
	}{
		{newToolAssemblyTimeoutError("fixture", "Edit", 90, 180*time.Second), UpstreamErrorToolAssemblyTimeout, http.StatusGatewayTimeout},
		{newToolOutputTruncatedError("fixture", cause), UpstreamErrorToolOutputTruncated, http.StatusBadGateway},
	} {
		if tc.err.Kind != tc.kind || mapDownstreamError(tc.err).Status != tc.status || tc.err.ToolName != "Edit" || tc.err.ArgumentBytes != 90 {
			t.Fatalf("error metadata changed: %+v", tc.err)
		}
		if !shouldRetryAcrossAccounts(tc.err) || !shouldRetryAcrossEndpoints(tc.err) || circuitEligibleFailure(tc.err) {
			t.Fatal("advice must not change existing retry/circuit policy")
		}
		if !strings.Contains(tc.err.Error(), "smaller complete edits") || !strings.Contains(tc.err.Error(), "preserve any required atomic operation") {
			t.Fatalf("missing qualified recovery advice: %v", tc.err)
		}
		for _, unsupported := range []string{"8192", "size limit exceeded", "tool_result", "successfully executed"} {
			if strings.Contains(tc.err.Error(), unsupported) {
				t.Fatalf("unsupported claim: %s", unsupported)
			}
		}
	}
	if !errors.Is(newToolOutputTruncatedError("fixture", cause), cause) {
		t.Fatal("lost upstream error cause")
	}
}

func TestToolFailureAdviceDoesNotTellReadOnlyOrUnknownToolsToEdit(t *testing.T) {
	for _, name := range []string{"", "Read", "WebSearch", "credit_check", "mcp__memory__read_graph"} {
		err := newToolAssemblyTimeoutError("fixture", name, 10, time.Minute)
		if strings.Contains(err.Error(), "file changes") {
			t.Fatalf("unexpected file advice for %q", name)
		}
	}
}

func TestClaudeStreamToolFailureIncludesAdviceWithoutReplayingOrCompletingTool(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeIntegrityText(t, w, "Preparing a file change with a complete tool call; inspect the resulting patch before executing it.", false)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tool-fixture", "name": "Edit", "input": `{"new_string":"`,
		}))
	}))
	defer server.Close()
	h := setupStreamIntegrityPathTest(t, server)
	if err := config.UpdateClaudeCodeTransparentMode(true); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5","stream":true,"max_tokens":32000,
		"tools":[{"name":"Edit","description":"Edit a file","input_schema":{"type":"object","properties":{"new_string":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"Create a large file."}]
	}`))
	req.Header.Set("User-Agent", "claude-code/fixture")
	recorder := httptest.NewRecorder()
	h.handleClaudeMessages(recorder, req)
	body := recorder.Body.String()
	if hits.Load() != 1 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "smaller complete edits") {
		t.Fatalf("missing failure advice or replayed stream: hits=%d body=%s", hits.Load(), body)
	}
	if strings.Contains(body, "message_stop") || strings.Contains(body, `"type":"tool_result"`) || strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("incomplete tool was reported as completed: %s", body)
	}
}
