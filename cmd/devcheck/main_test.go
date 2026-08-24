package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if !found {
		t.Fatal("tool call was not recorded")
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || len(input) != 0 {
		t.Fatalf("unexpected zero-argument input %q: %v", arguments, err)
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
		opts:    options{baseURL: "http://127.0.0.1:8080", suite: "smoke"},
		apiKey:  "report-secret",
		model:   "claude-sonnet-5",
		results: []scenarioResult{{Name: "health", Status: statusPass}},
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report permissions = %o, want 600", info.Mode().Perm())
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
	if got := percentile([]int64{50, 10, 30, 20, 40}, 0.95); got != 40 {
		t.Fatalf("p95 = %d, want nearest-rank floor value 40", got)
	}
}
