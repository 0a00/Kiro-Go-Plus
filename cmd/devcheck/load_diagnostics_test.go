package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadFailureDetailsAreBoundedAndDoNotExposeWrappedErrors(t *testing.T) {
	secret := "private-fixture-value"
	err := &url.Error{Op: "Post", URL: "https://example.invalid/?token=" + secret, Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	response := apiResponse{err: err, errorStage: "read_stream", statusCode: 200, requestID: "req_fixture", total: time.Second, body: []byte(secret)}
	sample := classifyLoadSample(response, loadProbe{protocol: "anthropic", stream: true})
	samples := make([]loadSample, maxLoadFailureDetails+5)
	for i := range samples {
		samples[i] = sample
	}
	result := buildLoadResult("diagnostics", "fixture", 1, len(samples), samples)
	if len(result.FailureDetails) != maxLoadFailureDetails || result.FailureDetailsOmitted != 5 {
		t.Fatalf("unbounded diagnostics: %+v", result)
	}
	first := result.FailureDetails[0]
	if first.Stage != "read_stream" || first.Cause != "connection_reset" || first.RequestID != "req_fixture" || first.HTTPStatus != 200 {
		t.Fatalf("missing diagnosis: %+v", first)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "example.invalid") {
		t.Fatal("private wrapped error leaked")
	}
	response.requestID = "unsafe\r\nheader"
	if got := describeLoadFailure(response, sample).RequestID; got != "" {
		t.Fatalf("unsafe request id: %q", got)
	}
}

func TestLoadFailureCauseDistinguishesTransportAndContent(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{context.Canceled, "canceled"}, {io.ErrUnexpectedEOF, "unexpected_eof"}, {io.EOF, "eof"},
		{syscall.ECONNREFUSED, "connection_refused"}, {syscall.EPIPE, "broken_pipe"},
		{&net.DNSError{Err: "private", Name: "private"}, "dns"}, {errors.New("private"), "transport_other"},
	} {
		if got := transportFailureCause(tc.err); got != tc.want {
			t.Fatalf("cause %s, want %s", got, tc.want)
		}
	}
	sample := classifyLoadSample(apiResponse{statusCode: 200, body: []byte(`{"content":[{"type":"text","text":"wrong"}]}`)}, loadProbe{expectedMarker: "EXPECTED"})
	if sample.category != "marker_mismatch" || sample.failureDetail.Cause != "" || sample.failureDetail.Stage != "validate_response" {
		t.Fatalf("content confused with transport: %+v", sample)
	}
}

func TestLoadFailureIncludesTruncatedStreamEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req_truncated")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "10000")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL}, client: server.Client()}
	response := r.post(context.Background(), "/v1/messages", map[string]string{}, false, true)
	sample := classifyLoadSample(response, loadProbe{stream: true})
	if sample.category != "stream_truncated" || sample.failureDetail.Cause != "unexpected_eof" || sample.failureDetail.Stage != "read_stream" || !sample.failureDetail.SemanticOutput || sample.failureDetail.Terminal {
		t.Fatalf("missing partial stream evidence: %+v", sample)
	}
}

func TestPacedSoakCoversDurationAndPreservesQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[{"type":"text","text":"fixture"}]}`)
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second, soakInterval: 25 * time.Millisecond}, client: server.Client(), model: "fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	execution := r.executeSoakExecution(ctx, 2, 100, 32, 110*time.Millisecond)
	if !execution.soakDurationReached || execution.scheduled < 1 || execution.scheduled > 5 || execution.scheduleWall < 100*time.Millisecond {
		t.Fatalf("pacing not applied: %+v", execution)
	}
	if len(execution.samples) != execution.scheduled {
		t.Fatal("lost in-flight requests")
	}
	execution = r.executeSoakExecution(ctx, 2, 1, 32, 110*time.Millisecond)
	if execution.scheduled != 1 || execution.soakDurationReached {
		t.Fatal("quota cap must not pretend to cover duration")
	}
}

func TestSoakQuotaOnlyCompletionIsWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(req.Body).Decode(&payload)
		data, _ := json.Marshal(payload)
		marker := loadMarkerFromPayload(string(data))
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second, concurrency: 1, loadMaxTokens: 32, soakDuration: time.Second, soakMaxRequests: 1, soakTokenBudget: 32}, client: server.Client(), model: "fixture"}
	r.runSoak(context.Background())
	if len(r.results) != 1 || r.results[0].Status != statusWarn || r.results[0].SoakDurationReached || r.results[0].SoakStopReason != "request/token cap" {
		t.Fatalf("unexpected soak result: %+v", r.results)
	}
}

func TestPacedSoakCancellationDoesNotWaitForInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{}`)
		cancel()
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second, soakInterval: time.Minute}, client: server.Client(), model: "fixture"}
	started := time.Now()
	execution := r.executeSoakExecution(ctx, 1, 10, 32, time.Minute)
	if execution.scheduled != 1 || execution.soakDurationReached || time.Since(started) > time.Second {
		t.Fatalf("pacing ignored cancellation: %+v", execution)
	}
}

func TestSoakIntervalValidatedAndIncludedInBaseline(t *testing.T) {
	t.Setenv("KIRO_DEV_API_KEY", "fixture")
	for _, value := range []string{"-1s", "25h"} {
		if _, err := parseOptions([]string{"--soak-interval", value}); err == nil {
			t.Fatalf("invalid interval accepted: %s", value)
		}
	}
	opts, err := parseOptions([]string{"--suite", "soak", "--soak-interval", "3s"})
	if err != nil || opts.soakInterval != 3*time.Second {
		t.Fatalf("pacing parse: %v", err)
	}
	baseline := devReport{SoakMillis: opts.soakDuration.Milliseconds(), SoakMaxRequests: opts.soakMaxRequests, SoakTokenBudget: opts.soakTokenBudget}
	if err := validateLoadBaselineMetadata(opts, baseline); err == nil {
		t.Fatal("different pacing accepted as comparable")
	}
	before := configurationFingerprint(opts)
	opts.soakInterval = 0
	if before == configurationFingerprint(opts) {
		t.Fatal("pacing missing from configuration fingerprint")
	}
}

func TestDefaultAcceptanceModelPrefersSonnet45Aliases(t *testing.T) {
	for _, name := range []string{"claude-sonnet-4-5", "claude-sonnet-4.5"} {
		if got := selectClaudeModel([]string{"claude-haiku-4-5", "claude-sonnet-5", name}); got != name {
			t.Fatalf("default=%s want=%s", got, name)
		}
	}
}
