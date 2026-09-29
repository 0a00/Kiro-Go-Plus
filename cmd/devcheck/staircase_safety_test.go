package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaircaseFailureStopsEscalationAndRunsRecovery(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
		}
		w.WriteHeader(503)
	}))
	defer s.Close()
	r := &runner{opts: options{baseURL: s.URL, timeout: time.Second, concurrencySteps: []int{1, 5, 20}, requests: 2, loadMaxTokens: 32, postLoadRecovery: true}, apiKey: "fixture", client: s.Client(), model: "test"}
	r.runStaircase(context.Background())
	if len(r.results) != 4 || r.results[0].Status != statusFail || r.results[1].Status != statusSkip || r.results[2].Status != statusSkip || r.results[3].Name != "post-load-recovery" {
		t.Fatalf("unsafe escalation %+v", r.results)
	}
	if posts.Load() != 3 {
		t.Fatalf("sent %d posts, want 2 first-level + 1 recovery", posts.Load())
	}
}

func TestShortMatrixSlowResponseWarnsWithoutMaskingFailures(t *testing.T) {
	for _, tt := range []struct {
		stream       bool
		latency      int64
		status, want string
	}{
		{true, 121000, statusPass, statusWarn}, {false, 121000, statusPass, statusWarn},
		{true, 1000, statusPass, statusPass}, {false, 30000, statusPass, statusPass},
		{true, 121000, statusFail, statusFail},
	} {
		r := scenarioResult{Stream: tt.stream, TTFTMillis: tt.latency, TotalMillis: tt.latency, Status: tt.status}
		warnSlowMatrixResponse(&r)
		if r.Status != tt.want {
			t.Fatalf("got %s want %s", r.Status, tt.want)
		}
	}
}
