package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Provider generation can stall after a short prefix. A delayed but complete
// large tail must survive both delivery modes; EOF must never authorize it.
func TestLargeToolPrefixAndTailPreserveIntegrityAcrossDeliveryModes(t *testing.T) {
	for _, mode := range []string{"balanced", "live"} {
		for _, size := range []int{64 << 10, 256 << 10} {
			for _, complete := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/complete=%v", mode, size, complete), func(t *testing.T) {
					input, err := json.Marshal(map[string]string{"value": strings.Repeat("x", size)})
					if err != nil {
						t.Fatal(err)
					}
					var hits atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						writeIntegrityText(t, w, "Preparing a complete tool call for the requested file update.", false)
						_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
							"toolUseId": "fixture-large", "name": "Edit", "input": string(input[:90]),
						}))
						w.(http.Flusher).Flush()
						time.Sleep(20 * time.Millisecond)
						if !complete {
							return
						}
						for offset := 90; offset < len(input); offset += 8192 {
							end := min(offset+8192, len(input))
							_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{"input": string(input[offset:end])}))
						}
						_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
						_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
					}))
					defer upstream.Close()
					h := setupStreamIntegrityPathTest(t, upstream)
					if mode == "balanced" {
						enableBalancedTestMode(t)
					} else {
						enableLiveTestMode(t)
					}
					rec := httptest.NewRecorder()
					h.handleClaudeMessages(rec, balancedRequest("Edit", ""))
					body := rec.Body.String()
					if hits.Load() != 1 {
						t.Fatalf("replayed visible response %d times", hits.Load())
					}
					var args strings.Builder
					for _, line := range strings.Split(body, "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event struct {
							Delta struct {
								Type  string `json:"type"`
								Input string `json:"partial_json"`
							} `json:"delta"`
						}
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						if event.Delta.Type == "input_json_delta" {
							args.WriteString(event.Delta.Input)
						}
					}
					if complete {
						if args.String() != string(input) || strings.Count(body, "event: message_stop") != 1 || strings.Contains(body, "event: error") {
							t.Fatalf("complete large tool corrupted: size=%d delivered=%d", len(input), args.Len())
						}
					} else {
						if !strings.Contains(body, "event: error") || strings.Contains(body, "event: message_stop") || strings.Contains(body, `"stop_reason":"tool_use"`) {
							t.Fatal("incomplete tool was reported as success")
						}
						if mode == "balanced" && args.Len() != 0 {
							t.Fatal("buffered incomplete arguments escaped")
						}
					}
				})
			}
		}
	}
}
