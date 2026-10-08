package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type deliveryTestWriter struct {
	header          http.Header
	mode            string
	writes, flushes int
}

func (w *deliveryTestWriter) Header() http.Header { return w.header }
func (w *deliveryTestWriter) WriteHeader(int)     {}
func (w *deliveryTestWriter) Write(b []byte) (int, error) {
	w.writes++
	switch w.mode {
	case "write":
		return 0, errors.New("private transport details")
	case "short":
		return len(b) / 2, nil
	}
	return len(b), nil
}
func (w *deliveryTestWriter) Flush() { _ = w.FlushError() }
func (w *deliveryTestWriter) FlushError() error {
	w.flushes++
	if w.mode == "flush" {
		return errors.New("private flush details")
	}
	return nil
}

func TestJSONDeliveryReportsWriteFlushAndCancellation(t *testing.T) {
	for _, mode := range []string{"ok", "write", "short", "flush", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			base := &deliveryTestWriter{header: make(http.Header), mode: mode}
			// Exercise the detail wrapper's error-returning flush, not just the base.
			w := &requestDetailFlushingWriter{requestDetailStatusWriter: &requestDetailStatusWriter{ResponseWriter: base}, flusher: base}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			entry := requestLogEntry{Status: "success", StatusCode: 200}
			writeJSONWithDelivery(ctx, w, map[string]string{"text": "fixture"}, &entry, time.Now())
			if mode == "ok" {
				if entry.Status != "success" || entry.Delivery.Status != "flushed" || base.flushes != 1 {
					t.Fatalf("unexpected success: %+v", entry)
				}
			} else {
				if entry.Status != "delivery_failed" || entry.Delivery.Status != "failed" || strings.Contains(entry.Error, "private") {
					t.Fatalf("unexpected failure: %+v", entry)
				}
				if mode == "cancel" && (base.writes != 0 || entry.StatusCode != 499) {
					t.Fatal("canceled response was written")
				}
				if mode == "short" && entry.Delivery.Cause != "short_write" {
					t.Fatal("short write lost")
				}
				if mode == "flush" && entry.Delivery.Stage != "flush" {
					t.Fatal("flush error lost")
				}
			}
		})
	}
}

type deliveryNoFlushWriter struct {
	header http.Header
	writes int
}

func (w *deliveryNoFlushWriter) Header() http.Header         { return w.header }
func (w *deliveryNoFlushWriter) WriteHeader(int)             {}
func (w *deliveryNoFlushWriter) Write(b []byte) (int, error) { w.writes++; return len(b), nil }

func TestJSONDeliveryDoesNotClaimUnsupportedFlushOrEncodeSuccess(t *testing.T) {
	w := &deliveryNoFlushWriter{header: make(http.Header)}
	entry := requestLogEntry{Status: "success", StatusCode: 200}
	writeJSONWithDelivery(context.Background(), w, map[string]string{"text": "fixture"}, &entry, time.Now())
	if entry.Delivery.Status != "written" || entry.Status != "success" {
		t.Fatalf("unsupported flush misreported: %+v", entry)
	}
	writeJSONWithDelivery(context.Background(), w, func() {}, &entry, time.Now())
	if entry.Status != "delivery_failed" || entry.Delivery.Stage != "encode" || entry.StatusCode != 500 {
		t.Fatalf("encode failure lost: %+v", entry)
	}
}

func TestNonStreamDeliveryFailureLogsWithoutRetryAcrossProtocols(t *testing.T) {
	for _, detailed := range []bool{false, true} {
		for _, protocol := range []string{"messages", "chat/completions", "responses"} {
			t.Run(fmt.Sprint(detailed)+protocol, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					writeIntegrityText(t, w, "fixture response", true)
				}))
				defer upstream.Close()
				h := setupStreamIntegrityPathTest(t, upstream)
				cfg := config.GetRequestLogConfig()
				cfg.DetailedLogEnabled = detailed
				if err := config.UpdateRequestLogConfig(cfg); err != nil {
					t.Fatal(err)
				}
				input := `"messages":[{"role":"user","content":"test"}]`
				if protocol == "responses" {
					input = `"input":"test","store":false`
				}
				body := `{"model":"claude-sonnet-4.5","max_tokens":128,` + input + `}`
				w := &deliveryTestWriter{header: make(http.Header), mode: "flush"}
				h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(body)))
				logs := h.requestLog.list(10)
				if calls.Load() != 1 || len(logs) != 1 || logs[0].Status != "delivery_failed" || logs[0].Delivery.Stage != "flush" {
					t.Fatalf("write failure retried/lost: calls=%d logs=%+v", calls.Load(), logs)
				}
				if logs[0].OutputTokens == 0 {
					t.Fatal("upstream usage erased on delivery failure")
				}
				if detailed {
					raw, ok := h.ensureRequestDetailStore().get(logs[0].RequestID)
					var d requestDetail
					if !ok || json.Unmarshal(raw, &d) != nil || d.Delivery == nil || d.Delivery.Stage != "flush" {
						t.Fatal("detail finalized before write")
					}
				}
			})
		}
	}
}

func TestThinkingDisplayUpdatesPreservesActualReasoning(t *testing.T) {
	for _, kind := range []string{"enabled", "adaptive"} {
		for _, stream := range []bool{false, true} {
			t.Run(kind+fmt.Sprint(stream), func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "fixture reasoning"}))
					_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "fixture answer"}))
					_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
				}))
				defer upstream.Close()
				h := setupStreamIntegrityPathTest(t, upstream)
				thinking := map[string]interface{}{"type": kind, "display": "updates"}
				if kind == "enabled" {
					thinking["budget_tokens"] = 1024
				}
				body, _ := json.Marshal(map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 2048, "stream": stream, "thinking": thinking, "messages": []map[string]string{{"role": "user", "content": "test"}}})
				r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
				r.Header.Set("User-Agent", "claude-cli/2.1.289 (external, sdk-cli)")
				r.Header.Set("anthropic-beta", "thinking-display-updates-2026-08-18")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "fixture reasoning") || !strings.Contains(w.Body.String(), "fixture answer") {
					t.Fatalf("updates failed: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	for _, tc := range []struct {
		kind, display string
		valid         bool
	}{{"enabled", "UPDATES", true}, {"adaptive", "updates", true}, {"disabled", "updates", false}, {"enabled", "unknown", false}} {
		cfg := &ClaudeThinkingConfig{Type: tc.kind, Display: tc.display}
		if tc.kind == "enabled" {
			cfg.BudgetTokens = 1024
		}
		if (validateClaudeThinkingConfig(cfg, 2048) == "") != tc.valid {
			t.Fatalf("bad validation for %+v", tc)
		}
	}
	opts := resolveClaudeThinkingResponseOptions(&ClaudeThinkingConfig{Display: "updates"}, "think")
	if opts.Format != "thinking" || opts.OmitDisplay {
		t.Fatal("updates altered or hid reasoning")
	}
}
