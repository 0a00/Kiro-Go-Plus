package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEMetricsDistinguishEventThinkingTextToolAndGap(t *testing.T) {
	stats := sseStats{tools: make(map[int]*sseToolState)}
	if err := stats.record("message_start", `{"type":"message_start"}`, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := stats.record("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`, 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := stats.record("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`, 90*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := stats.record("message_stop", `{"type":"message_stop"}`, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if stats.firstEvent != 10*time.Millisecond || stats.firstSemantic != 40*time.Millisecond || stats.firstThinking != 40*time.Millisecond || stats.firstText != 90*time.Millisecond {
		t.Fatalf("unexpected first-event timings: %+v", stats)
	}
	if stats.maxEventGap != 50*time.Millisecond || !stats.terminal {
		t.Fatalf("unexpected gap or terminal state: %+v", stats)
	}
}

func TestConsumeSSESupportsChatAndResponsesStreams(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		wantText  int
		wantThink int
		wantTools int
		wantArgs  string
		wantTool  string
	}{
		{
			name: "chat completions",
			stream: strings.Join([]string{
				`data: {"choices":[{"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
				"",
				`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
				"",
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"dev_echo","arguments":"{\"value\":\"OK\"}"}}]},"finish_reason":null}]}`,
				"",
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n"),
			wantText: 1, wantThink: 1, wantTools: 1, wantArgs: `{"value":"OK"}`, wantTool: "dev_echo",
		},
		{
			name: "responses",
			stream: strings.Join([]string{
				"event: response.created", `data: {"type":"response.created"}`, "",
				"event: response.reasoning_summary_text.delta", `data: {"type":"response.reasoning_summary_text.delta","delta":"think"}`, "",
				"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"answer"}`, "",
				"event: response.output_item.added", `data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"dev_echo"}}`, "",
				"event: response.function_call_arguments.delta", `data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"value\":\"OK\"}"}`, "",
				"event: response.output_item.done", `data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"dev_echo","arguments":"{\"value\":\"OK\"}"}}`, "",
				"event: response.completed", `data: {"type":"response.completed"}`, "",
				"data: [DONE]", "",
			}, "\n"),
			wantText: 1, wantThink: 1, wantTools: 1, wantArgs: `{"value":"OK"}`, wantTool: "dev_echo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := consumeSSE(strings.NewReader(tc.stream), time.Now())
			if err != nil {
				t.Fatalf("consume stream: %v", err)
			}
			if !stats.terminal || stats.contentDeltas != tc.wantText || stats.thinkingDeltas != tc.wantThink || stats.toolCalls != tc.wantTools {
				t.Fatalf("unexpected stream stats: %+v", stats)
			}
			arguments, found := stats.toolArguments(tc.wantTool)
			if !found || arguments != tc.wantArgs || !stats.hasSingleCompleteTool(tc.wantTool) {
				t.Fatalf("tool = found %v args %q stats %+v", found, arguments, stats)
			}
		})
	}
}

func TestStreamScenarioWarnsWhenProtocolReportsOutputLimit(t *testing.T) {
	streams := []string{
		strings.Join([]string{
			"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"partial"}`, "",
			"event: response.incomplete", `data: {"type":"response.incomplete"}`, "",
			"data: [DONE]", "",
		}, "\n"),
		strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`, "",
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`, "",
			"data: [DONE]", "",
		}, "\n"),
		strings.Join([]string{
			"event: content_block_delta", `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`, "",
			"event: message_delta", `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`, "",
			"event: message_stop", `data: {"type":"message_stop"}`, "",
		}, "\n"),
	}
	for index, stream := range streams {
		stats, err := consumeSSE(strings.NewReader(stream), time.Now())
		if err != nil {
			t.Fatalf("stream %d: %v", index, err)
		}
		result := streamScenarioResult("limit", "fixture", "model", apiResponse{statusCode: http.StatusOK, stream: stats})
		if result.Status != statusWarn || !strings.Contains(result.Detail, "output limit") {
			t.Fatalf("stream %d result = %+v stats=%+v", index, result, stats)
		}
	}
}

func TestProtocolMatrixCoversThreeAPIsAndBothTransferModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		stream, _ := payload["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			switch req.URL.Path {
			case "/v1/messages":
				_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"MATRIX_OK"}]}`))
			case "/v1/chat/completions":
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"MATRIX_OK"}}]}`))
			case "/v1/responses":
				_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"MATRIX_OK"}]}]}`))
			default:
				http.NotFound(w, req)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch req.URL.Path {
		case "/v1/messages":
			_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"MATRIX_OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"MATRIX_OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		case "/v1/responses":
			_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"MATRIX_OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	r := &runner{
		opts: options{baseURL: server.URL, timeout: time.Second}, apiKey: "test-key", client: server.Client(),
		selected: []string{"claude-sonnet-5", "claude-sonnet-5-thinking"}, userAgent: "devcheck-test",
	}
	r.runProtocolMatrix(context.Background())
	if len(r.results) != 12 {
		t.Fatalf("matrix results = %d, want 12: %+v", len(r.results), r.results)
	}
	for _, result := range r.results {
		if result.Status != statusPass {
			t.Fatalf("matrix scenario failed: %+v", result)
		}
	}
}
