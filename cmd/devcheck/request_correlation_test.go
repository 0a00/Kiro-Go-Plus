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

func TestHeaderEOFStillCarriesClientRequestID(t *testing.T) {
	ids := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ids <- req.Header.Get("X-Request-Id")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL}, apiKey: "fixture", client: server.Client()}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		response := r.post(ctx, "/v1/messages", map[string]string{"input": "fixture"}, true, false)
		cancel()
		id := <-ids
		if response.err == nil || response.requestID != id || !strings.HasPrefix(id, "devcheck_") || seen[id] {
			t.Fatalf("correlation lost: %+v", response)
		}
		seen[id] = true
	}
}

func TestThinkingUpdatesProbeCannotHideAClientRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		var payload struct {
			Thinking struct {
				Display string `json:"display"`
			} `json:"thinking"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.Thinking.Display != "updates" {
			t.Error("missing display probe")
		}
		http.Error(w, `{"error":{"message":"fixture rejection"}}`, 400)
	}))
	defer server.Close()
	r := &runner{opts: options{baseURL: server.URL, timeout: time.Second}, apiKey: "fixture", client: server.Client(), thinking: "fixture-thinking"}
	r.runThinkingDisplayUpdates(context.Background())
	if calls != 1 || len(r.results) != 1 || r.results[0].Status != statusFail {
		t.Fatalf("rejection hidden: %+v", r.results)
	}
}
