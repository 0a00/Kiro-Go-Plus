package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadServerStatsParsingPreservesZeroAndRejectsInvalidCounters(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    loadServerStats
		wantErr bool
	}{
		{name: "zero is valid", body: `{"requestsCount":0,"tokensUsed":0}`, want: loadServerStats{valid: true}},
		{name: "customer counters", body: `{"requestsCount":12,"tokensUsed":345}`, want: loadServerStats{requests: 12, tokens: 345, valid: true}},
		{name: "legacy counters", body: `{"totalRequests":12,"totalTokens":345}`, want: loadServerStats{requests: 12, tokens: 345, valid: true}},
		{name: "missing counter", body: `{"requestsCount":12}`, wantErr: true},
		{name: "negative counter", body: `{"requestsCount":-1,"tokensUsed":0}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var payload loadServerStatsPayload
			if err := json.Unmarshal([]byte(tc.body), &payload); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			got, err := normalizeLoadServerStatsPayload(payload)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error=%v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("stats = %+v, want %+v", got, tc.want)
			}
		})
	}

	if _, reset := loadServerStatsDelta(loadServerStats{requests: 5, tokens: 10, valid: true}, loadServerStats{requests: 4, tokens: 11, valid: true}); !reset {
		t.Fatal("counter rollback was not reported")
	}
	if delta, reset := loadServerStatsDelta(loadServerStats{requests: 5, tokens: 10, valid: true}, loadServerStats{requests: 7, tokens: 15, valid: true}); reset || delta.requests != 2 || delta.tokens != 5 {
		t.Fatalf("counter delta = %+v reset=%v", delta, reset)
	}
}

func TestLoadExecutionSeparatesScheduleDeadlineFromInflightDrain(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if startedOnce.CompareAndSwap(false, true) {
			close(started)
		}
		<-release
		marker := loadFixtureMarker(req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, marker)
	}))
	defer server.Close()

	r := &runner{
		opts:   options{baseURL: server.URL, timeout: 500 * time.Millisecond, loadProfile: "marker"},
		apiKey: "fixture", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	scheduleParent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	resultCh := make(chan loadExecution, 1)
	go func() {
		resultCh <- r.executeLoadPatternWithParents(scheduleParent, context.Background(), context.Background(), 1, 10, 32, "closed", 0, 0, 0)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("load request did not start")
	}
	select {
	case <-scheduleParent.Done():
	case <-time.After(time.Second):
		t.Fatal("schedule deadline did not expire")
	}
	close(release)
	execution := <-resultCh
	if execution.scheduled != 1 || len(execution.samples) != 1 || !execution.samples[0].success {
		t.Fatalf("in-flight request was not drained: scheduled=%d samples=%d sample=%+v", execution.scheduled, len(execution.samples), execution.samples)
	}
}

func TestLoadServerStatsAreCrossCheckedWithoutLeakingCredentials(t *testing.T) {
	var statsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/stats" {
			call := statsCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				_, _ = w.Write([]byte(`{"requestsCount":10,"tokensUsed":100}`))
			} else {
				_, _ = w.Write([]byte(`{"requestsCount":12,"tokensUsed":140}`))
			}
			return
		}
		marker, stream := loadFixtureRequest(req)
		writeLoadResponse(w, req, marker, stream, false, false)
	}))
	defer server.Close()

	r := &runner{
		opts:   options{baseURL: server.URL, timeout: time.Second, loadProfile: "marker", collectServerStats: true, resourceSampleInterval: 0},
		apiKey: "fixture-secret", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test",
	}
	execution := r.executeLoad(context.Background(), 2, 2, 32)
	result := buildLoadExecutionResult("stats-load", r.model, 2, 2, execution)
	if statsCalls.Load() != 2 || result.ServerStatsSamples != 2 || result.ServerStatsRequestsDelta != 2 || result.ServerStatsTokensDelta != 40 || result.ServerStatsCounterReset {
		t.Fatalf("server stats cross-check = %+v calls=%d", result, statsCalls.Load())
	}
}

func TestLoadStreamFaultsMeasureWireGapAndRejectDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, ": heartbeat\n\n")
		flusher.Flush()
		if req.URL.Query().Get("fault") == "disconnect" {
			_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"PARTIAL\"}}\n\n")
			flusher.Flush()
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, err := hijacker.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"SLOW_OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second}, apiKey: "fixture", client: server.Client(), model: "claude-sonnet-5", userAgent: "devcheck-test"}

	slow := r.post(context.Background(), "/v1/messages", claudePayload(r.model, true, "slow", 32), true, true)
	if slow.err != nil || !slow.stream.terminal || slow.stream.maxActivityGap < 20*time.Millisecond {
		t.Fatalf("slow stream metrics = err=%v stats=%+v", slow.err, slow.stream)
	}
	disconnected := r.post(context.Background(), "/v1/messages?fault=disconnect", claudePayload(r.model, true, "disconnect", 32), true, true)
	sample := classifyLoadSample(disconnected, loadProbe{protocol: "anthropic", stream: true})
	if sample.success || sample.category != "stream_truncated" {
		t.Fatalf("disconnected stream classification = %+v response=%+v", sample, disconnected)
	}
}

func TestLoadStreamTimeoutAfterSemanticOutputIsClassifiedAsTruncated(t *testing.T) {
	response := apiResponse{
		err:    fmt.Errorf("upstream: %w", context.DeadlineExceeded),
		stream: sseStats{semanticOutput: true, terminal: false},
	}
	sample := classifyLoadSample(response, loadProbe{protocol: "anthropic", stream: true})
	if sample.success || sample.category != "stream_truncated" {
		t.Fatalf("timed-out partial stream = %+v", sample)
	}
}

func TestLoadBaselineUsesRelativeSuccessDropAndRequiresMatchingProfiles(t *testing.T) {
	path := t.TempDir() + "/baseline.json"
	baseline := devReport{
		Suite: "load", LoadProfile: "marker", LoadPattern: "closed", LoadMaxTokens: 32,
		Concurrency: 2, Requests: 100,
		Results: []scenarioResult{{Name: "load-a", Protocol: "load", Model: "model", Requests: 100, P95Millis: 100, TTFTP95Millis: 50, SuccessRate: 100}},
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	r := &runner{opts: options{suite: "load", loadProfile: "marker", loadPattern: "closed", loadMaxTokens: 32, concurrency: 2, requests: 100, baselineTolerancePercent: 10}, results: []scenarioResult{{Name: "load-a", Protocol: "load", Model: "model", Requests: 100, P95Millis: 109, TTFTP95Millis: 55, SuccessRate: 91}}}
	if err := r.compareLoadBaseline(path); err != nil {
		t.Fatal(err)
	}
	if r.baselineRegressions != 0 || r.results[0].Status == statusFail {
		t.Fatalf("within-tolerance baseline was rejected: %+v regressions=%d", r.results, r.baselineRegressions)
	}

	r.results[0].SuccessRate = 89
	if err := r.compareLoadBaseline(path); err != nil {
		t.Fatal(err)
	}
	if r.baselineRegressions != 1 || r.results[0].Status != statusFail {
		t.Fatalf("relative success regression was missed: %+v regressions=%d", r.results, r.baselineRegressions)
	}

	r.results = []scenarioResult{{Name: "load-new", Protocol: "load", Model: "model", Requests: 100, SuccessRate: 100}}
	if err := r.compareLoadBaseline(path); err != nil {
		t.Fatal(err)
	}
	if r.baselineMissing != 2 || r.results[0].Status != statusFail {
		t.Fatalf("missing baseline profiles were not counted strictly: %+v missing=%d", r.results, r.baselineMissing)
	}
}

func TestLoadResourceSamplerHandlesNilContextAndSaturatingDeltas(t *testing.T) {
	sampler := newLoadResourceSampler(nil, 0)
	sampler.stop()
	if samples, _, _ := sampler.summary(); samples < 2 {
		t.Fatalf("resource sampler did not record start/stop observations: %d", samples)
	}
	maxInt64 := uint64(^uint64(0) >> 1)
	if got := signedUint64Delta(^uint64(0), 0); got != int64(maxInt64) {
		t.Fatalf("positive resource delta overflowed: %d", got)
	}
	if got := signedUint64Delta(0, ^uint64(0)); got != -int64(maxInt64) {
		t.Fatalf("negative resource delta overflowed: %d", got)
	}
}

func TestLoadResourceThresholdsAreAppliedAfterExecution(t *testing.T) {
	r := &runner{opts: options{maxClientGoroutineGrowth: 1, maxClientHeapGrowthMB: 1}}
	result := r.finalizeLoadResult(scenarioResult{
		Protocol: "load", Requests: 1, ClientGoroutineDelta: 2, ClientHeapDeltaBytes: 2 * 1024 * 1024,
	})
	if result.Status != statusFail || len(result.ThresholdFailures) != 2 {
		t.Fatalf("resource thresholds = %+v", result)
	}
	if !strings.Contains(result.Detail, "threshold_failures=") {
		t.Fatalf("threshold detail missing: %q", result.Detail)
	}
}

func TestRequiredServerStatsTurnCollectionFailuresIntoThresholdFailures(t *testing.T) {
	r := &runner{opts: options{requireServerStats: true}}
	result := r.finalizeLoadResult(scenarioResult{
		Protocol: "load", Requests: 2, Status: statusPass,
		ServerStatsErrors: 1, ServerStatsSamples: 1,
	})
	if result.Status != statusFail || len(result.ThresholdFailures) != 2 {
		t.Fatalf("required server stats = %+v", result)
	}
}

func TestLoadBaselineMetadataMustMatchCurrentWorkload(t *testing.T) {
	baseline := devReport{
		Suite: "load", LoadProfile: "marker", LoadPattern: "closed", LoadMaxTokens: 32,
		Concurrency: 5, Requests: 10,
	}
	opts := options{suite: "load", loadProfile: "realistic", loadPattern: "closed", loadMaxTokens: 32, concurrency: 5, requests: 10}
	if err := validateLoadBaselineMetadata(opts, baseline); err == nil {
		t.Fatal("baseline with a different workload profile was accepted")
	}
	baseline.LoadProfile = "realistic"
	if err := validateLoadBaselineMetadata(opts, baseline); err != nil {
		t.Fatalf("matching baseline metadata rejected: %v", err)
	}
}

func TestLoadBaselineMetadataCoversScheduleAndSuiteBounds(t *testing.T) {
	opts := options{
		suite: "staircase", loadProfile: "marker", loadPattern: "closed", loadMaxTokens: 64,
		concurrency: 5, requests: 10, rampDuration: 2 * time.Minute, warmupRequests: 3,
		concurrencySteps: []int{1, 5, 10}, staircaseHold: 30 * time.Second,
		staircaseCooldown: 5 * time.Second, staircaseMaxRequests: 300,
	}
	baseline := devReport{
		Suite: "staircase", LoadProfile: "marker", LoadPattern: "closed", LoadMaxTokens: 64,
		Concurrency: 5, Requests: 10, RampMillis: 120000, WarmupRequests: 3,
		ConcurrencyLevels: []int{1, 5, 10}, StaircaseHoldMillis: 30000,
		StaircaseCooldownMillis: 5000, StaircaseMaxRequests: 300,
	}
	if err := validateLoadBaselineMetadata(opts, baseline); err != nil {
		t.Fatalf("matching staircase metadata rejected: %v", err)
	}
	mutations := []func(*devReport){
		func(report *devReport) { report.RampMillis++ },
		func(report *devReport) { report.WarmupRequests++ },
		func(report *devReport) { report.ConcurrencyLevels = []int{1, 10} },
		func(report *devReport) { report.StaircaseHoldMillis++ },
		func(report *devReport) { report.StaircaseCooldownMillis++ },
		func(report *devReport) { report.StaircaseMaxRequests++ },
	}
	for index, mutate := range mutations {
		candidate := baseline
		candidate.ConcurrencyLevels = append([]int(nil), baseline.ConcurrencyLevels...)
		mutate(&candidate)
		if err := validateLoadBaselineMetadata(opts, candidate); err == nil {
			t.Fatalf("staircase metadata mutation %d was accepted", index)
		}
	}

	soakOpts := options{
		suite: "soak", loadProfile: "marker", loadPattern: "closed", loadMaxTokens: 64,
		concurrency: 5, requests: 10, rampDuration: time.Minute, warmupRequests: 2,
		soakDuration: 10 * time.Minute, soakMaxRequests: 500, soakTokenBudget: 16000,
	}
	soakBaseline := devReport{
		Suite: "soak", LoadProfile: "marker", LoadPattern: "closed", LoadMaxTokens: 64,
		Concurrency: 5, Requests: 10, RampMillis: 60000, WarmupRequests: 2,
		SoakMillis: 600000, SoakMaxRequests: 500, SoakTokenBudget: 16000,
	}
	if err := validateLoadBaselineMetadata(soakOpts, soakBaseline); err != nil {
		t.Fatalf("matching soak metadata rejected: %v", err)
	}
	for name, mutate := range map[string]func(*devReport){
		"duration": func(report *devReport) { report.SoakMillis++ },
		"requests": func(report *devReport) { report.SoakMaxRequests++ },
		"tokens":   func(report *devReport) { report.SoakTokenBudget++ },
	} {
		candidate := soakBaseline
		mutate(&candidate)
		if err := validateLoadBaselineMetadata(soakOpts, candidate); err == nil {
			t.Fatalf("soak %s mutation was accepted", name)
		}
	}
}

func TestLoadThresholdsDoNotReclassifyRecoveryProbe(t *testing.T) {
	r := &runner{opts: options{minSuccessRate: 99, maxClientGoroutineGrowth: 1, maxClientHeapGrowthMB: 1}}
	result := r.finalizeLoadResult(scenarioResult{Protocol: "load", Name: "post-load-recovery", Status: statusPass})
	if result.Status != statusPass || len(result.ThresholdFailures) != 0 {
		t.Fatalf("recovery probe was treated as aggregate load: %+v", result)
	}
}
