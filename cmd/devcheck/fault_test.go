package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFaultFixturesRejectEmptyCorruptTruncatedAndConflictingStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		match  string
	}{
		{name: "corrupt JSON frame", stream: "data: {not-json}\n\n", match: "invalid JSON"},
		{name: "truncated JSON frame", stream: "event: message_stop\ndata: {\"type\":", match: "invalid JSON"},
		{
			name: "conflicting terminal events",
			stream: strings.Join([]string{
				"event: message_stop", `data: {"type":"message_stop"}`, "",
				"event: response.failed", `data: {"type":"response.failed","error":{"message":"late failure"}}`, "",
			}, "\n"),
			match: "conflicting terminal",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consumeSSE(strings.NewReader(tc.stream), time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error = %v, want %q", err, tc.match)
			}
		})
	}

	stats, err := consumeSSE(strings.NewReader(""), time.Now())
	if err != nil {
		t.Fatalf("empty stream parse: %v", err)
	}
	result := streamScenarioResult("empty-stream", "fixture", "test-model", apiResponse{statusCode: http.StatusOK, stream: stats})
	if result.Status != statusFail || !strings.Contains(result.Detail, "terminal") {
		t.Fatalf("empty stream was not rejected: %+v", result)
	}
}

func TestFaultFixturesClassifyRateLimitServerTimeoutAndProtocolFailures(t *testing.T) {
	tests := []struct {
		name     string
		response apiResponse
		stream   bool
		want     string
	}{
		{name: "429", response: apiResponse{statusCode: http.StatusTooManyRequests}, want: "http_429"},
		{name: "500", response: apiResponse{statusCode: http.StatusBadGateway}, want: "http_5xx"},
		{name: "timeout", response: apiResponse{err: context.DeadlineExceeded}, want: "timeout"},
		{name: "cancellation", response: apiResponse{err: context.Canceled}, want: "canceled"},
		{name: "empty JSON", response: apiResponse{statusCode: http.StatusOK, body: []byte(`{"choices":[]}`)}, want: "empty_response"},
		{name: "empty SSE", response: apiResponse{statusCode: http.StatusOK}, stream: true, want: "stream_protocol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sample := classifyLoadSample(tc.response, tc.stream)
			if sample.success || sample.category != tc.want {
				t.Fatalf("sample = %+v, want category %q", sample, tc.want)
			}
		})
	}
}

func TestRunnerSeparatesResponseHeadersFromFirstSSEAndSemanticOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL}, apiKey: "test", client: server.Client(), userAgent: "devcheck-test"}
	response := r.post(context.Background(), "/v1/messages", map[string]interface{}{"stream": true}, true, true)
	if response.err != nil {
		t.Fatalf("request: %v", response.err)
	}
	if response.headers < 15*time.Millisecond || response.stream.firstEvent < 35*time.Millisecond || response.stream.firstSemantic < 55*time.Millisecond {
		t.Fatalf("timing stages were collapsed: headers=%s event=%s semantic=%s", response.headers, response.stream.firstEvent, response.stream.firstSemantic)
	}
}

func TestAnthropicAndMCPToolResultRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name       string
		run        func(*runner, context.Context)
		wantTool   string
		wantInput  string
		wantMarker string
	}{
		{name: "anthropic function", run: (*runner).runAnthropicFunction, wantTool: "dev_echo", wantInput: `{"value":"FUNCTION_OK"}`, wantMarker: "FUNCTION_RESULT_OK"},
		{name: "MCP zero argument", run: (*runner).runMCPZeroArgument, wantTool: "mcp__memory__read_graph", wantInput: `{}`, wantMarker: "MCP_RESULT_OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var payload map[string]interface{}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					body := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_fixture\",\"name\":%q,\"input\":%s}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", tc.wantTool, tc.wantInput)
					_, _ = w.Write([]byte(body))
					return
				}
				encoded, _ := json.Marshal(payload["messages"])
				if !strings.Contains(string(encoded), `"tool_use_id":"toolu_fixture"`) || !strings.Contains(string(encoded), `"type":"tool_result"`) {
					http.Error(w, "tool result linkage missing", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, tc.wantMarker)
			}))
			defer server.Close()

			r := &runner{opts: options{baseURL: server.URL, timeout: time.Second}, apiKey: "test", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test"}
			tc.run(r, context.Background())
			if requests.Load() != 2 || len(r.results) != 1 || r.results[0].Status != statusPass {
				t.Fatalf("round trip = requests %d results %+v", requests.Load(), r.results)
			}
		})
	}
}

func TestBoundedSoakStopsSchedulingWithoutCancelingInflightRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(15 * time.Millisecond)
		var payload map[string]interface{}
		_ = json.NewDecoder(req.Body).Decode(&payload)
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK"}]}`))
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second}, apiKey: "test", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test"}
	samples, scheduled, durationReached := r.executeSoak(context.Background(), 2, 100, 32, 45*time.Millisecond)
	if !durationReached || scheduled == 0 || scheduled >= 100 || len(samples) != scheduled {
		t.Fatalf("soak bounds = reached %v scheduled %d samples %d", durationReached, scheduled, len(samples))
	}
	for _, sample := range samples {
		if !sample.success {
			t.Fatalf("in-flight request was canceled at duration cap: %+v", sample)
		}
	}
}

func TestBuildLoadResultReportsPercentilesAndFailureCategories(t *testing.T) {
	samples := []loadSample{
		{duration: 10, success: true},
		{duration: 20, stream: true, success: true, ttft: 5},
		{duration: 30, category: "http_429"},
		{duration: 40, category: "http_5xx"},
	}
	result := buildLoadResult("fixture", "model", 2, 5, samples)
	if result.Status != statusFail || result.Successes != 2 || result.P50Millis != 20 || result.P95Millis != 30 || result.P99Millis != 30 {
		t.Fatalf("unexpected load summary: %+v", result)
	}
	if result.FailureCategories["http_429"] != 1 || result.FailureCategories["http_5xx"] != 1 || result.FailureCategories["not_started_or_canceled"] != 1 {
		t.Fatalf("unexpected failure categories: %+v", result.FailureCategories)
	}
}

func TestParseOptionsSupportsMatrixStaircaseSoakAndScenarioSelection(t *testing.T) {
	opts, err := parseOptions([]string{
		"--suite", "matrix", "--models", "claude-sonnet-5,claude-sonnet-5-thinking",
		"--concurrency-levels", "1,5,100", "--scenarios", "chat-stream,responses-stream",
		"--soak-duration", "10s", "--soak-max-requests", "20", "--soak-token-budget", "640",
	})
	if err != nil {
		t.Fatalf("parse options: %v", err)
	}
	if len(opts.models) != 2 || len(opts.concurrencySteps) != 3 || len(opts.scenarioFilter) != 2 {
		t.Fatalf("parsed options: %+v", opts)
	}
	for _, args := range [][]string{
		{"--suite", "unknown"},
		{"--concurrency-levels", "0,5"},
		{"--scenarios", "not-a-scenario"},
		{"--model", "one", "--models", "two"},
		{"--soak-token-budget", "31"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}

func TestClassifyLoadSampleKeepsWrappedCancellationKinds(t *testing.T) {
	sample := classifyLoadSample(apiResponse{err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded)}, false)
	if sample.category != "timeout" {
		t.Fatalf("wrapped timeout category = %q", sample.category)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", context.Canceled), context.Canceled) {
		t.Fatal("sanity check for wrapped cancellation failed")
	}
}
