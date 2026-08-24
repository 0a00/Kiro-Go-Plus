package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testRunner(server *httptest.Server) *runner {
	return &runner{
		opts:      options{baseURL: server.URL, timeout: 2 * time.Second},
		apiKey:    "fixture-key",
		client:    server.Client(),
		model:     "claude-sonnet-5",
		thinking:  "claude-sonnet-5-thinking",
		startedAt: time.Now(),
		userAgent: "devcheck-test",
	}
}

func writeSSEFixture(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = fmt.Fprint(w, event)
	}
}

func TestExtractTokenUsageAcrossSupportedProtocols(t *testing.T) {
	tests := []struct {
		name string
		body string
		want tokenUsage
	}{
		{
			name: "anthropic",
			body: `{"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}`,
			want: tokenUsage{inputTokens: 10, outputTokens: 3, cacheReadTokens: 80, cacheCreationTokens: 20},
		},
		{
			name: "chat",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":70,"cache_creation_tokens":15},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			want: tokenUsage{inputTokens: 100, outputTokens: 4, cacheReadTokens: 70, cacheCreationTokens: 15, reasoningTokens: 2},
		},
		{
			name: "responses",
			body: `{"response":{"usage":{"input_tokens":120,"output_tokens":8,"input_tokens_details":{"cached_tokens":90,"cache_creation_tokens":12},"output_tokens_details":{"reasoning_tokens":6}}}}`,
			want: tokenUsage{inputTokens: 120, outputTokens: 8, cacheReadTokens: 90, cacheCreationTokens: 12, reasoningTokens: 6},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := extractTokenUsageFromBody([]byte(test.body))
			if got != test.want {
				t.Fatalf("usage = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestBuildLoadProbeCoversProtocolsAndTransferModes(t *testing.T) {
	wantProtocols := []string{"anthropic", "anthropic", "openai", "openai", "responses", "responses"}
	wantPaths := []string{"/v1/messages", "/v1/messages", "/v1/chat/completions", "/v1/chat/completions", "/v1/responses", "/v1/responses"}
	for index := range wantProtocols {
		probe := buildLoadProbe("claude-sonnet-5", index, 32)
		if probe.protocol != wantProtocols[index] || probe.path != wantPaths[index] || probe.stream != (index%2 == 1) {
			t.Fatalf("probe %d = %+v", index, probe)
		}
		if stream, _ := probe.payload["stream"].(bool); stream != probe.stream {
			t.Fatalf("probe %d stream payload = %v", index, stream)
		}
	}
}

func TestResponsesFunctionAndCustomToolRoundTrips(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(payload)
		body := string(encoded)
		w.Header().Set("X-Request-Id", "req_fixture_responses")
		if strings.Contains(body, "function_call_output") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"RESPONSES_RESULT_OK"}]}]}`))
			return
		}
		if strings.Contains(body, "custom_tool_call_output") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"CUSTOM_RESULT_OK"}]}]}`))
			return
		}
		if strings.Contains(body, "responses_echo") {
			writeSSEFixture(w,
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"call_id\":\"call_function\",\"type\":\"function_call\",\"name\":\"responses_echo\"}}\n\n",
				"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"value\\\":\\\"RESPONSES_FUNCTION_OK\\\"}\"}\n\n",
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"call_id\":\"call_function\",\"type\":\"function_call\",\"name\":\"responses_echo\",\"arguments\":\"{\\\"value\\\":\\\"RESPONSES_FUNCTION_OK\\\"}\"}}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		writeSSEFixture(w,
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"ct_1\",\"call_id\":\"call_custom\",\"type\":\"custom_tool_call\",\"name\":\"dev_exec\"}}\n\n",
			"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"delta\":\"CUSTOM_TOOL_OK\"}\n\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"call_id\":\"call_custom\",\"type\":\"custom_tool_call\",\"name\":\"dev_exec\",\"input\":\"CUSTOM_TOOL_OK\"}}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	r := testRunner(server)
	r.runResponsesFunction(context.Background())
	r.runResponsesCustomTool(context.Background())
	if len(r.results) != 2 {
		t.Fatalf("results = %+v", r.results)
	}
	for _, result := range r.results {
		if result.Status != statusPass || result.ToolCalls != 1 || result.RequestID != "req_fixture_responses" {
			t.Fatalf("roundtrip failed: %+v", result)
		}
	}
}

func TestPromptCacheReuseReportsColdAndWarmUsage(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := make(map[string]interface{})
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(body["system"])
		if len(encoded) < 10000 || !strings.Contains(string(encoded), "cache_control") {
			http.Error(w, "cache prefix missing", http.StatusBadRequest)
			return
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", fmt.Sprintf("req_cache_%d", attempt))
		if attempt == 1 {
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"CACHE_OK"}],"usage":{"input_tokens":20,"output_tokens":2,"cache_creation_input_tokens":1500}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"CACHE_OK"}],"usage":{"input_tokens":20,"output_tokens":2,"cache_read_input_tokens":1500}}`))
	}))
	defer server.Close()

	r := testRunner(server)
	r.runPromptCacheReuse(context.Background())
	if len(r.results) != 1 || r.results[0].Status != statusPass || r.results[0].CacheReadTokens != 1500 {
		t.Fatalf("cache result = %+v", r.results)
	}
	if !strings.Contains(r.results[0].Detail, "hit_ratio=98.7%") || attempts.Load() != 3 {
		t.Fatalf("cache diagnostics = %+v attempts=%d", r.results[0], attempts.Load())
	}
}

func TestMultimodalAccountingIgnoresEncodedSize(t *testing.T) {
	var mu sync.Mutex
	encodedSizes := make(map[string][]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		protocol := "anthropic"
		data := ""
		switch req.URL.Path {
		case "/v1/chat/completions":
			protocol = "openai"
			messages, _ := payload["messages"].([]interface{})
			message, _ := messages[0].(map[string]interface{})
			content, _ := message["content"].([]interface{})
			imageBlock, _ := content[0].(map[string]interface{})
			imageURL, _ := imageBlock["image_url"].(map[string]interface{})
			data, _ = imageURL["url"].(string)
		case "/v1/responses":
			protocol = "responses"
			input, _ := payload["input"].([]interface{})
			message, _ := input[0].(map[string]interface{})
			content, _ := message["content"].([]interface{})
			imageBlock, _ := content[0].(map[string]interface{})
			data, _ = imageBlock["image_url"].(string)
		default:
			messages, _ := payload["messages"].([]interface{})
			message, _ := messages[0].(map[string]interface{})
			content, _ := message["content"].([]interface{})
			imageBlock, _ := content[0].(map[string]interface{})
			source, _ := imageBlock["source"].(map[string]interface{})
			data, _ = source["data"].(string)
		}
		mu.Lock()
		encodedSizes[protocol] = append(encodedSizes[protocol], len(data))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch protocol {
		case "openai":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"IMAGE_OK"}}],"usage":{"prompt_tokens":240,"completion_tokens":2}}`))
		case "responses":
			_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"IMAGE_OK"}]}],"usage":{"input_tokens":240,"output_tokens":2}}`))
		default:
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"IMAGE_OK"}],"usage":{"input_tokens":240,"output_tokens":2}}`))
		}
	}))
	defer server.Close()

	r := testRunner(server)
	r.runMultimodalAccounting(context.Background())
	if len(r.results) != 3 {
		t.Fatalf("multimodal result = %+v", r.results)
	}
	for _, result := range r.results {
		if result.Status != statusPass {
			t.Fatalf("multimodal result = %+v", result)
		}
		sizes := encodedSizes[result.Protocol]
		if len(sizes) != 2 || sizes[1] < sizes[0]*10 {
			t.Fatalf("%s encoded image sizes do not exercise byte variance: %v", result.Protocol, sizes)
		}
	}
}

func TestThinkingOutputLimitAndLongStreamScenarios(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(payload)
		body := string(encoded)
		if strings.Contains(body, "Calculate 211") {
			if req.URL.Path == "/v1/chat/completions" {
				writeSSEFixture(w,
					"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"reason\"},\"finish_reason\":null}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\"7807\"},\"finish_reason\":null}]}\n\n",
					"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
					"data: [DONE]\n\n",
				)
				return
			}
			writeSSEFixture(w,
				"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"reason\"}\n\n",
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"7807\"}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		if strings.Contains(body, "Write 100 numbered") {
			switch req.URL.Path {
			case "/v1/messages":
				writeSSEFixture(w,
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n",
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
				)
			case "/v1/chat/completions":
				writeSSEFixture(w,
					"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n",
					"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n",
					"data: [DONE]\n\n",
				)
			case "/v1/responses":
				writeSSEFixture(w,
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
					"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
					"data: [DONE]\n\n",
				)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for index := 0; index < 4; index++ {
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"line %d\\n\"}}\n\n", index)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	r := testRunner(server)
	r.runThinkingProtocols(context.Background())
	r.runOutputLimit(context.Background())
	r.runLongStream(context.Background())
	if len(r.results) != 6 {
		t.Fatalf("results = %+v", r.results)
	}
	for _, result := range r.results {
		if result.Status != statusPass {
			t.Fatalf("advanced stream scenario failed: %+v", result)
		}
	}
}
