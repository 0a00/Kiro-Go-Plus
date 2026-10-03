package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func enableBalancedTestMode(t *testing.T) {
	t.Helper()
	if err := config.UpdateThinkingConfigWithToolStreamMode("-thinking", "reasoning_content", "thinking", 4000, 10000, 0, 0, config.ToolStreamModeBalanced, true); err != nil {
		t.Fatal(err)
	}
}

func TestBalancedPolicyBuffersEveryToolWithoutBufferingText(t *testing.T) {
	for _, transparent := range []bool{false, true} {
		for _, name := range []string{"Write", "Bash", "Read", "mcp__sheet__resolve", ""} {
			payload := &KiroPayload{transparentClaudeCode: transparent}
			req := &ClaudeRequest{Stream: true}
			if name != "" {
				req.Tools = []ClaudeTool{{Name: name}}
			}
			configureClaudeToolStreaming(payload, req, true, claudeThinkingResponseOptions{}, config.ThinkingConfig{ToolStreamMode: config.ToolStreamModeBalanced})
			if payload.deferTextUntilComplete || payload.streamToolUseDeltas || payload.streamThinkingPrecommit || !payload.streamTextWithBufferedTools {
				t.Fatalf("unexpected balanced policy for %s (transparent=%v)", name, transparent)
			}
		}
	}
}

func TestBalancedWhitespacePreservationDoesNotCommitEmptyResponse(t *testing.T) {
	var output strings.Builder
	callback, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	}, nil, false, false, false, false)
	gate.preserveTextWhitespace = true
	callback.OnText("    ", false)
	if gate.hasEmittedOutput() || gate.hasActionableOutput() {
		t.Fatal("whitespace-only response committed")
	}
	callback.OnText("code", false)
	callback.OnText("\n", false)
	if output.String() != "    code\n" {
		t.Fatalf("indentation lost: %q", output.String())
	}
}

func balancedRequest(tool, choice string) *http.Request {
	body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":4096,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"Use the test tool."}],"tools":[{"name":%q,"input_schema":{"type":"object","properties":{"value":{"type":"string"}}}}]%s}`, tool, choice)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("User-Agent", "claude-code/2.1.281")
	return r
}

func TestBalancedTextAndThinkingArriveBeforeValidatedMCPTool(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "planning"}))
		for _, text := range []string{"hello", " ", "world", "\n"} {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": text}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "call1", "name": "mcp__sheet__resolve", "input": `{"value":"partial`}))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{"input": ` complete"}`}))
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer upstream.Close()
	defer close(release)
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	server := httptest.NewServer(h)
	defer server.Close()
	r := balancedRequest("mcp__sheet__resolve", "")
	request, _ := http.NewRequest("POST", server.URL+r.URL.Path, r.Body)
	request.Header = r.Header
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var early strings.Builder
	for !strings.Contains(early.String(), `"text":"world"`) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("no text before tool release: %v", err)
		}
		early.WriteString(line)
	}
	if !strings.Contains(early.String(), "thinking_delta") || strings.Contains(early.String(), `"type":"tool_use"`) || strings.Contains(early.String(), "partial") {
		t.Fatalf("incorrect early stream: %s", early.String())
	}
	// Unblock once, leaving deferred cleanup safe even on an earlier assertion.
	release <- struct{}{}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	body := early.String() + string(rest)
	if !strings.Contains(body, `"partial_json":"{\"value\":\"partial complete\"}"`) || !strings.Contains(body, "message_stop") {
		t.Fatalf("incomplete final tool: %s", body)
	}
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
		if event.Delta.Type == "text_delta" {
			text.WriteString(event.Delta.Text)
		}
	}
	if text.String() != "hello world\n" {
		t.Fatalf("whitespace altered: %q", text.String())
	}
}

func TestBalancedTruncationAfterVisibleOutputNeverReplays(t *testing.T) {
	for _, malformedStop := range []bool{false, true} {
		t.Run(fmt.Sprint(malformedStop), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "visible once"}))
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "broken", "name": "Write", "input": `{"value":"unfinished`, "stop": malformedStop}))
			}))
			defer upstream.Close()
			h := setupStreamIntegrityPathTest(t, upstream)
			enableBalancedTestMode(t)
			rec := httptest.NewRecorder()
			h.handleClaudeMessages(rec, balancedRequest("Write", ""))
			body := rec.Body.String()
			if calls.Load() != 1 || strings.Count(body, "visible once") != 1 || strings.Contains(body, "unfinished") || strings.Count(body, `"id":"broken"`) != 1 || strings.Contains(body, "input_json_delta") || strings.Count(body, "event: content_block_stop") != 1 || strings.Contains(body, "message_stop") || !strings.Contains(body, "event: error") {
				t.Fatalf("unsafe replay or tool output: calls=%d body=%s", calls.Load(), body)
			}
		})
	}
}

func TestBalancedExplicitToolChoiceDoesNotAcceptTextOnlyCompletion(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeIntegrityText(t, w, "no tool emitted", true)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, balancedRequest("Write", `,"tool_choice":{"type":"tool","name":"Write"}`))
	if calls.Load() != 1 || !strings.Contains(rec.Body.String(), "event: error") || strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("explicit tool accepted text or replayed: %s", rec.Body.String())
	}
}

func TestBalancedCompleteToolThenTruncationDoesNotDuplicateFirstTool(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "complete1", "name": "Write", "input": `{"value":"valid"}`, "stop": true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "incomplete2", "name": "Write", "input": `{"value":"unfinished`,
		}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, balancedRequest("Write", ""))
	body := rec.Body.String()
	if calls.Load() != 1 || strings.Count(body, "complete1") != 1 || strings.Count(body, "incomplete2") != 1 ||
		strings.Count(body, "input_json_delta") != 1 || strings.Count(body, "event: content_block_stop") != 1 ||
		!strings.Contains(body, "event: error") || strings.Contains(body, "message_stop") {
		t.Fatalf("complete tool duplicated or incomplete tool exposed: calls=%d body=%s", calls.Load(), body)
	}
}

func TestBalancedTruncationBeforeOutputRetriesWithoutLeakingTool(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "discarded", "name": "Write", "input": `{"value":"unfinished`,
			}))
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "recovered", "name": "Write", "input": `{"value":"complete"}`, "stop": true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	_ = config.UpdateEndpointFallback(true)
	kiroEndpoints = append(kiroEndpoints, kiroEndpoint{Key: "backup", Name: "backup", URL: upstream.URL})
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, balancedRequest("Write", ""))
	body := rec.Body.String()
	if calls.Load() != 2 || strings.Contains(body, "discarded") || strings.Contains(body, "unfinished") ||
		strings.Contains(body, "event: error") || !strings.Contains(body, "recovered") || !strings.Contains(body, "message_stop") {
		t.Fatalf("unsafe pre-output recovery: calls=%d body=%s", calls.Load(), body)
	}
}
