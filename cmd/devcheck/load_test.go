package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLoadControlsAndHighConcurrencyGuard(t *testing.T) {
	if _, err := parseOptions([]string{"--suite", "load", "--concurrency", "101"}); err == nil {
		t.Fatal("concurrency above the safety limit was accepted")
	}
	opts, err := parseOptions([]string{
		"--suite", "load", "--concurrency", "250", "--allow-high-load",
		"--concurrency-levels", "1,250", "--load-profile", "realistic", "--load-max-tokens", "256",
		"--load-pattern", "fixed", "--target-rps", "12.5", "--warmup-requests", "3",
	})
	if err != nil {
		t.Fatalf("valid load controls rejected: %v", err)
	}
	if opts.concurrency != 250 || len(opts.concurrencySteps) != 2 || opts.loadProfile != "realistic" || opts.loadPattern != "fixed" || opts.targetRPS != 12.5 || opts.warmupRequests != 3 {
		t.Fatalf("unexpected parsed load controls: %+v", opts)
	}
	for _, args := range [][]string{
		{"--suite", "load", "--target-rps", "5"},
		{"--suite", "load", "--load-pattern", "fixed", "--target-rps", "0.01"},
		{"--suite", "load", "--load-profile", "realistic", "--load-max-tokens", "64"},
		{"--suite", "soak", "--load-max-tokens", "4096", "--soak-token-budget", "1024"},
		{"--suite", "load", "--allow-high-load", "--concurrency", "1001"},
		{"--suite", "staircase", "--load-pattern", "fixed", "--target-rps", "5"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("invalid load controls accepted: %v", args)
		}
	}
}

func TestRealisticLoadProfileCoversOperationalWorkloads(t *testing.T) {
	r := &runner{
		opts: options{loadProfile: "realistic", webSearch: false}, model: "claude-sonnet-5",
		thinking: "claude-sonnet-5-thinking", startedAt: time.Unix(100, 0),
	}
	seen := make(map[string]bool)
	for index := 0; index < realisticLoadCycle; index++ {
		probe := r.buildConfiguredLoadProbe(index, 256)
		seen[probe.workload] = true
		if probe.expectedMarker == "" || probe.payload == nil {
			t.Fatalf("probe %d is incomplete: %+v", index, probe)
		}
	}
	for _, workload := range []string{"protocol-marker", "thinking", "long-stream", "function-tool", "mcp-tool", "image", "cache", "skill-context"} {
		if !seen[workload] {
			t.Fatalf("realistic profile omitted %q: %v", workload, seen)
		}
	}
	toolProbe := r.buildConfiguredLoadProbe(8, 256)
	if toolProbe.validation != loadValidationTool || toolProbe.expectedTool != "load_echo" {
		t.Fatalf("function workload validation = %+v", toolProbe)
	}
}

func TestLoadArrivalIntervalRampIsBoundedAndIncreasing(t *testing.T) {
	start := loadArrivalInterval("ramp", 20, 0, 10*time.Second)
	middle := loadArrivalInterval("ramp", 20, 5*time.Second, 10*time.Second)
	end := loadArrivalInterval("ramp", 20, 10*time.Second, 10*time.Second)
	if start <= middle || middle <= end || end <= 0 {
		t.Fatalf("ramp intervals are not decreasing: start=%s middle=%s end=%s", start, middle, end)
	}
	if got := loadArrivalInterval("fixed", 20, 0, time.Second); got != 50*time.Millisecond {
		t.Fatalf("fixed interval = %s, want 50ms", got)
	}
}

func writeLoadResponse(w http.ResponseWriter, req *http.Request, marker string, stream, wrong, truncated bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		if wrong {
			marker = "WRONG_MARKER"
		}
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", marker)
		if !truncated {
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if wrong {
		marker = "WRONG_MARKER"
	}
	_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
}

func loadFixtureMarker(req *http.Request) string {
	marker, _ := loadFixtureRequest(req)
	return marker
}

func loadFixtureRequest(req *http.Request) (string, bool) {
	body, _ := io.ReadAll(req.Body)
	var payload map[string]interface{}
	_ = json.Unmarshal(body, &payload)
	stream, _ := payload["stream"].(bool)
	return loadMarkerFromPayload(string(body)), stream
}

func TestExecuteLoadClassifiesConcurrentUpstreamFaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		marker, stream := loadFixtureRequest(req)
		index, _ := strconv.Atoi(strings.TrimPrefix(marker, "LOAD_OK_"))
		switch index % 5 {
		case 0:
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case 1:
			w.WriteHeader(http.StatusBadGateway)
			return
		case 2:
			writeLoadResponse(w, req, marker, stream, false, index%2 == 1)
			return
		case 3:
			writeLoadResponse(w, req, marker, stream, true, false)
			return
		default:
			writeLoadResponse(w, req, marker, stream, false, false)
		}
	}))
	defer server.Close()
	r := &runner{
		opts:   options{baseURL: server.URL, timeout: time.Second, loadProfile: "marker", loadPattern: "closed"},
		apiKey: "fixture", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	execution := r.executeLoad(context.Background(), 5, 20, 32)
	result := buildLoadExecutionResult("fault-load", r.model, 5, 20, execution)
	if result.Successes == result.Requests || result.DistinctRequestIDs != 0 {
		t.Fatalf("faults were not represented correctly: %+v", result)
	}
	if !hasFailureSuffix(result.FailureCategories, "http_429") || !hasFailureSuffix(result.FailureCategories, "http_5xx") || !hasFailureSuffix(result.FailureCategories, "stream_protocol") || !hasFailureSuffix(result.FailureCategories, "marker_mismatch") {
		t.Fatalf("missing fault categories: %+v", result.FailureCategories)
	}
}

func hasFailureSuffix(categories map[string]int, suffix string) bool {
	for name, count := range categories {
		if count > 0 && strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

type closeTrackingBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *closeTrackingBody) Close() error {
	b.closed.Add(1)
	return nil
}

type loadFixtureRoundTripper struct {
	closed atomic.Int32
	delay  time.Duration
}

func (t *loadFixtureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.delay > 0 {
		timer := time.NewTimer(t.delay)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, req.Context().Err()
		}
	}
	marker, stream := loadFixtureRequest(req)
	body := fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, marker)
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
		body = fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", marker)
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}},
		Body: &closeTrackingBody{Reader: strings.NewReader(body), closed: &t.closed}, Request: req,
	}, nil
}

func TestExecuteLoadClosesEveryResponseBody(t *testing.T) {
	transport := &loadFixtureRoundTripper{}
	r := &runner{
		opts:   options{baseURL: "http://fixture", timeout: time.Second, loadProfile: "marker", loadPattern: "closed"},
		apiKey: "fixture", client: &http.Client{Transport: transport}, model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	execution := r.executeLoad(context.Background(), 4, 12, 32)
	if len(execution.samples) != 12 || transport.closed.Load() != 12 {
		t.Fatalf("response bodies closed=%d samples=%d", transport.closed.Load(), len(execution.samples))
	}
	for _, sample := range execution.samples {
		if !sample.success {
			t.Fatalf("fixture request failed: %+v", sample)
		}
	}
}

func TestPostLoadRecoveryChecksHealthAndMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
			return
		}
		marker := loadFixtureMarker(req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
	}))
	defer server.Close()
	r := &runner{
		opts:   options{baseURL: server.URL, timeout: time.Second, loadMaxTokens: 32, postLoadRecovery: true},
		apiKey: "fixture", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	r.runPostLoadRecovery(context.Background())
	if len(r.results) != 1 || r.results[0].Status != statusPass {
		t.Fatalf("recovery result = %+v", r.results)
	}
}

func TestCorrelateLoadSamplesUsesOnlySafeCustomerLogFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/logs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"logs":[{"requestId":"req-correlation","endpoint":"runtime","accountSelectionMs":37,"accountAttempts":2,"routeAffinityHit":true,"cacheStatus":"hit","toolUseCount":1}]}`))
			return
		}
		marker := loadFixtureMarker(req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-correlation")
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
	}))
	defer server.Close()
	r := &runner{
		opts:   options{baseURL: server.URL, timeout: time.Second, loadProfile: "marker", loadPattern: "closed"},
		apiKey: "fixture", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	execution := r.executeLoad(context.Background(), 1, 1, 32)
	r.correlateLoadSamples(context.Background(), execution.samples)
	if len(execution.samples) != 1 || !execution.samples[0].correlated || execution.samples[0].endpoint != "runtime" || execution.samples[0].accountAttempts != 2 || !execution.samples[0].affinityHit || execution.samples[0].cacheStatus != "hit" || execution.samples[0].toolUses != 1 {
		t.Fatalf("load correlation = %+v", execution.samples)
	}
}

func TestLoadMarkerBoundaryDoesNotAcceptLongerMarker(t *testing.T) {
	if containsLoadMarker("anything", "") {
		t.Fatal("empty marker was accepted")
	}
	if containsLoadMarker("prefix LOAD_OK_10 suffix", "LOAD_OK_1") {
		t.Fatal("marker prefix was accepted as a longer marker")
	}
	if !containsLoadMarker("prefix LOAD_OK_1; suffix", "LOAD_OK_1") {
		t.Fatal("valid marker boundary was rejected")
	}
}

func TestLoadCorrelationNormalizesEndpointAndBoundsMetrics(t *testing.T) {
	entry, ok := normalizeLoadCorrelationLog(loadCorrelationLog{
		RequestID: " req-1 ", Endpoint: "https://runtime.example.invalid/private?token=secret",
		AccountSelectionMs: -1, AccountAttempts: 1_000_001, ToolUseCount: -4, CacheStatus: "unknown",
	})
	if !ok || entry.RequestID != "req-1" || entry.Endpoint != "runtime" || entry.AccountSelectionMs != 0 || entry.AccountAttempts != 10000 || entry.ToolUseCount != 0 || entry.CacheStatus != "" {
		t.Fatalf("normalized correlation entry = %+v, ok=%v", entry, ok)
	}
	if _, ok := normalizeLoadCorrelationLog(loadCorrelationLog{}); ok {
		t.Fatal("empty correlation request ID was accepted")
	}
}

func TestExecuteLoadOpenLoopCountsClientOverload(t *testing.T) {
	transport := &loadFixtureRoundTripper{delay: 40 * time.Millisecond}
	r := &runner{
		opts:   options{baseURL: "http://fixture", timeout: time.Second, loadProfile: "marker", loadPattern: "fixed"},
		apiKey: "fixture", client: &http.Client{Transport: transport}, model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	execution := r.executeLoadPattern(context.Background(), 1, 20, 32, "fixed", 1000, 0, 0)
	result := buildLoadExecutionResult("open-loop", r.model, 1, 20, execution)
	if execution.scheduled != 20 || execution.dropped == 0 {
		t.Fatalf("open-loop scheduling = scheduled %d dropped %d", execution.scheduled, execution.dropped)
	}
	if result.ArrivalRPS <= result.AchievedRPS {
		t.Fatalf("open-loop rates hid dropped arrivals: arrival %.2f achieved %.2f", result.ArrivalRPS, result.AchievedRPS)
	}
	if !hasFailureSuffix(result.FailureCategories, "client_overload") {
		t.Fatalf("client overload was not reported: %+v", result.FailureCategories)
	}
}

func TestLoadHelpersGuardInvalidInputs(t *testing.T) {
	if got := loadSuiteTimeout("closed", 0, 0, 0, 0); got <= 0 {
		t.Fatalf("invalid closed timeout = %s", got)
	}
	if got := loadSuiteTimeout("ramp", 1, 10, 10, time.Second, 2*time.Second); got <= 3*time.Second {
		t.Fatalf("ramp timeout was not conservatively bounded: %s", got)
	}
	if got := loadArrivalInterval("fixed", math.NaN(), time.Second, 0); got != 0 {
		t.Fatalf("NaN arrival interval = %s", got)
	}
	if got := percentile([]int64{3, 1, 2}, math.NaN()); got != 1 {
		t.Fatalf("NaN percentile = %d", got)
	}

	r := &runner{opts: options{timeout: time.Second, loadPattern: "fixed"}}
	execution := r.executeLoadPattern(context.Background(), 0, -1, 0, "fixed", 0, 0, 0)
	if len(execution.samples) != 0 || execution.scheduled != 0 {
		t.Fatalf("invalid load execution was not empty: %+v", execution)
	}
	samples, scheduled, durationReached := r.executeSoak(context.Background(), 0, 10, 32, time.Second)
	if len(samples) != 0 || scheduled != 0 || durationReached {
		t.Fatalf("invalid soak execution = samples %d scheduled %d duration=%v", len(samples), scheduled, durationReached)
	}
}

func TestMarkDuplicateLoadRequestIDsRejectsCrossTalk(t *testing.T) {
	samples := []loadSample{
		{requestID: "same", success: true},
		{requestID: "same", success: true},
		{requestID: "unique", success: true},
	}
	markDuplicateLoadRequestIDs(samples)
	if !samples[0].success || samples[1].success || samples[1].category != "duplicate_request_id" || !samples[2].success {
		t.Fatalf("duplicate request IDs were not isolated: %+v", samples)
	}
}

func TestResponseSizeErrorsHaveDedicatedLoadCategory(t *testing.T) {
	_, err := readBounded(strings.NewReader("123456789"), 8)
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("bounded read error = %v, want response size sentinel", err)
	}
	sample := classifyLoadSample(apiResponse{err: err}, loadProbe{protocol: "anthropic"})
	if sample.success || sample.category != "response_too_large" {
		t.Fatalf("oversized response category = %+v", sample)
	}
}
