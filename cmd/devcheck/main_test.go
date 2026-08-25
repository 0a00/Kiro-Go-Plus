package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumeSSECountsThinkingAsFirstSemanticOutput(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first thought"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	stats, err := consumeSSE(strings.NewReader(stream), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("consume SSE: %v", err)
	}
	if !stats.terminal || stats.events != 4 || stats.thinkingDeltas != 1 || stats.contentDeltas != 1 {
		t.Fatalf("unexpected stream stats: %+v", stats)
	}
	if stats.firstSemantic <= 0 {
		t.Fatal("thinking delta was not counted as first semantic output")
	}
}

func TestConsumeSSEPreservesZeroArgumentToolInput(t *testing.T) {
	stream := strings.Join([]string{
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__memory__read_graph","input":{}}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	stats, err := consumeSSE(strings.NewReader(stream), time.Now())
	if err != nil {
		t.Fatalf("consume SSE: %v", err)
	}
	arguments, found := stats.toolArguments("mcp__memory__read_graph")
	if !found || !stats.hasSingleCompleteTool("mcp__memory__read_graph") {
		t.Fatal("one complete tool call was not recorded")
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || len(input) != 0 {
		t.Fatalf("unexpected zero-argument input %q: %v", arguments, err)
	}
}

func TestConsumeSSERejectsMalformedJSONAndToolLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		match  string
	}{
		{
			name: "malformed JSON",
			stream: strings.Join([]string{
				"event: content_block_delta",
				`data: {"type":"content_block_delta"`,
				"",
			}, "\n"),
			match: "invalid JSON",
		},
		{
			name: "tool delta before start",
			stream: strings.Join([]string{
				"event: content_block_delta",
				`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				"",
			}, "\n"),
			match: "before start",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consumeSSE(strings.NewReader(tc.stream), time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("consumeSSE() error = %v, want %q", err, tc.match)
			}
		})
	}
}

func TestConsumeSSEObserverRunsOnlyAfterSemanticOutput(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	events := 0
	stats, err := consumeSSEWithObserver(strings.NewReader(stream), time.Now(), func() { events++ })
	if err != nil {
		t.Fatalf("consume SSE: %v", err)
	}
	if events != 1 || stats.events != 3 || stats.contentChars != 2 || !stats.terminal {
		t.Fatalf("unexpected observer result: callbacks=%d stats=%+v", events, stats)
	}
}

func TestRunnerCancelsEstablishedStreamAfterSemanticOutput(t *testing.T) {
	serverCanceled := make(chan struct{})
	prematureCancel := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-req.Context().Done():
			prematureCancel <- struct{}{}
			close(serverCanceled)
			return
		case <-time.After(50 * time.Millisecond):
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-req.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	r := &runner{
		opts:      options{baseURL: server.URL},
		apiKey:    "dev-secret",
		client:    server.Client(),
		userAgent: "devcheck-test",
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer timeoutCancel()
	ctx, cancelRequest := context.WithCancel(timeoutCtx)
	response := r.postWithSSEObserver(ctx, "/v1/messages", map[string]interface{}{"stream": true}, true, true, cancelRequest)
	if !errors.Is(response.err, context.Canceled) || response.stream.events != 2 {
		t.Fatalf("stream cancellation result = err=%v stats=%+v", response.err, response.stream)
	}
	select {
	case <-prematureCancel:
		t.Fatal("message_start triggered cancellation before semantic output")
	default:
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not reach the server request context")
	}
}

func TestRunLoadMixesStreamAndNonStreamRequests(t *testing.T) {
	var streamRequests atomic.Int32
	var nonStreamRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(payload)
		marker := loadMarkerFromPayload(string(encoded))
		if marker == "" {
			http.Error(w, "load marker missing", http.StatusBadRequest)
			return
		}
		if streaming, _ := payload["stream"].(bool); streaming {
			streamRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", marker)
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
	}))
	defer server.Close()

	r := &runner{
		opts: options{
			baseURL: server.URL, timeout: time.Second, concurrency: 2, requests: 4,
		},
		apiKey: "dev-secret", client: server.Client(), model: "claude-sonnet-4.6", userAgent: "devcheck-test",
	}
	r.runLoad(context.Background())
	if len(r.results) != 1 || r.results[0].Status != statusPass {
		t.Fatalf("unexpected load result: %+v", r.results)
	}
	if streamRequests.Load() != 2 || nonStreamRequests.Load() != 2 {
		t.Fatalf("load mix = stream %d, non-stream %d", streamRequests.Load(), nonStreamRequests.Load())
	}
}

func TestExpectedAuthenticationRejectionIsStrict(t *testing.T) {
	valid := apiResponse{
		statusCode: http.StatusUnauthorized,
		body:       []byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid or missing API key"}}`),
	}
	if !isExpectedAuthenticationRejection(valid) {
		t.Fatal("canonical local API-key rejection was not recognized")
	}
	for _, response := range []apiResponse{
		{statusCode: http.StatusForbidden, body: valid.body},
		{statusCode: http.StatusUnauthorized, body: []byte(`{"error":{"type":"account_suspended","message":"account suspended"}}`)},
		{statusCode: http.StatusUnauthorized, body: []byte(`not-json`)},
	} {
		if isExpectedAuthenticationRejection(response) {
			t.Fatalf("unexpected response was accepted as local authentication rejection: %+v", response)
		}
	}
}

func TestRunnerAuthenticationHeadersAreRequestScoped(t *testing.T) {
	var authenticatedHeaders, anonymousHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/authenticated" {
			authenticatedHeaders = req.Header.Clone()
		} else {
			anonymousHeaders = req.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	r := &runner{
		opts:      options{baseURL: server.URL},
		apiKey:    "dev-secret",
		client:    server.Client(),
		userAgent: "devcheck-test",
	}
	if response := r.get(context.Background(), "/authenticated", true); response.err != nil {
		t.Fatalf("authenticated request: %v", response.err)
	}
	if response := r.get(context.Background(), "/anonymous", false); response.err != nil {
		t.Fatalf("anonymous request: %v", response.err)
	}
	if authenticatedHeaders.Get("Authorization") != "Bearer dev-secret" || authenticatedHeaders.Get("X-Api-Key") != "dev-secret" {
		t.Fatalf("missing authenticated headers: %v", authenticatedHeaders)
	}
	if anonymousHeaders.Get("Authorization") != "" || anonymousHeaders.Get("X-Api-Key") != "" {
		t.Fatalf("credential leaked to anonymous request: %v", anonymousHeaders)
	}
}

func TestRunnerDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	redirectTargetReached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		redirectTargetReached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL+"/credential-target", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	r := &runner{
		opts:      options{baseURL: origin.URL},
		apiKey:    "redirect-secret",
		client:    newHTTPClient(),
		userAgent: "devcheck-test",
	}
	response := r.get(context.Background(), "/redirect", true)
	if response.err != nil {
		t.Fatalf("redirect response: %v", response.err)
	}
	if response.statusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", response.statusCode, http.StatusTemporaryRedirect)
	}
	if redirectTargetReached {
		t.Fatal("client followed redirect while carrying credentials")
	}
}

func TestWriteReportUsesPrivatePermissionsAndOmitsAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("create broad report: %v", err)
	}
	r := &runner{
		opts:          options{baseURL: "http://127.0.0.1:8080", suite: "smoke"},
		apiKey:        "report-secret",
		model:         "claude-sonnet-5",
		serverVersion: "1.2.48",
		results:       []scenarioResult{{Name: "health", Status: statusPass}},
	}
	if err := r.writeReport(path); err != nil {
		t.Fatalf("write report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(data), r.apiKey) {
		t.Fatal("API key leaked into report")
	}
	var report devReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.ServerVersion != "1.2.48" || !strings.HasPrefix(report.ConfigurationFingerprint, "sha256:") {
		t.Fatalf("missing report correlation metadata: %+v", report)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteReportReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	path := filepath.Join(directory, "report.json")
	if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := &runner{
		opts:    options{baseURL: "http://127.0.0.1:8080", suite: "smoke"},
		apiKey:  "report-secret",
		results: []scenarioResult{{Name: "health", Status: statusPass}},
	}
	if err := r.writeReport(path); err != nil {
		t.Fatalf("write report through symlink: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "sentinel" {
		t.Fatalf("symlink target was modified: %q", data)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("report path remained a symlink")
	}
}

func TestParseOptionsRejectsRemoteCredentialTargetByDefault(t *testing.T) {
	if _, err := parseOptions([]string{"--base-url", "https://example.com"}); err == nil {
		t.Fatal("remote base URL was accepted without explicit approval")
	}
	opts, err := parseOptions([]string{"--base-url", "https://example.com", "--allow-remote"})
	if err != nil || !opts.allowRemote {
		t.Fatalf("explicit remote base URL was rejected: opts=%+v err=%v", opts, err)
	}
	if _, err := parseOptions([]string{"--base-url", "http://user:secret@127.0.0.1:8080"}); err == nil {
		t.Fatal("userinfo in base URL was accepted")
	}
}

func TestSelectClaudeModelAndPercentile(t *testing.T) {
	models := []string{"claude-opus-5-thinking", "claude-sonnet-4.6", "claude-sonnet-5"}
	if got := selectClaudeModel(models); got != "claude-sonnet-5" {
		t.Fatalf("selected model = %q", got)
	}
	if got := percentile([]int64{50, 10, 30, 20, 40}, 0.95); got != 50 {
		t.Fatalf("p95 = %d, want nearest-rank value 50", got)
	}
}

func loadMarkerFromPayload(payload string) string {
	start := strings.Index(payload, "LOAD_OK_")
	if start < 0 {
		return ""
	}
	end := start + len("LOAD_OK_")
	for end < len(payload) && payload[end] >= '0' && payload[end] <= '9' {
		end++
	}
	return payload[start:end]
}
