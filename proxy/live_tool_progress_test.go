package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func enableLiveTestMode(t *testing.T) {
	t.Helper()
	if err := config.UpdateThinkingConfigWithToolStreamMode("-thinking", "reasoning_content", "thinking", 4000, 10000, 0, 0, config.ToolStreamModeLive, true); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLargeToolStartsBeforeSilentTailAndCompletesOnce(t *testing.T) {
	for _, size := range []int{40 << 10, 80 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			input, _ := json.Marshal(map[string]string{"value": strings.Repeat("a", size)})
			const prefix = 24
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "large", "name": "Write", "input": string(input[:prefix]),
				}))
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				for pos := prefix; pos < len(input); pos += 256 {
					_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{
						"input": string(input[pos:min(pos+256, len(input))]),
					}))
				}
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
				_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
			}))
			defer upstream.Close()
			h := setupStreamIntegrityPathTest(t, upstream)
			enableLiveTestMode(t)
			server := httptest.NewServer(h)
			defer server.Close()
			r := balancedRequest("Write", "")
			req, _ := http.NewRequest("POST", server.URL+r.URL.Path, r.Body)
			req.Header = r.Header
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var early strings.Builder
			for !strings.Contains(early.String(), "input_json_delta") {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("no live tool before tail release: %v", err)
				}
				early.WriteString(line)
			}
			if strings.Contains(early.String(), "content_block_stop") || !strings.Contains(early.String(), `"name":"Write"`) {
				t.Fatalf("invalid early events: %s", early.String())
			}
			time.Sleep(40 * time.Millisecond)
			unblock()
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			all := early.String() + string(rest)
			var raw strings.Builder
			for _, line := range strings.Split(all, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event struct {
					Delta struct {
						Type  string `json:"type"`
						Input string `json:"partial_json"`
					} `json:"delta"`
				}
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
				if event.Delta.Type == "input_json_delta" {
					raw.WriteString(event.Delta.Input)
				}
			}
			if calls.Load() != 1 || raw.String() != string(input) || strings.Count(all, "event: content_block_stop") != 1 ||
				strings.Count(all, "event: message_stop") != 1 || strings.Contains(all, "event: error") {
				t.Fatalf("invalid live completion: calls=%d argument bytes=%d", calls.Load(), raw.Len())
			}
			logs := h.requestLog.list(1)
			if len(logs) != 1 || logs[0].MaxUpstreamReadGapMs == nil || *logs[0].MaxUpstreamReadGapMs < 30 ||
				logs[0].MaxUpstreamFrameGapMs == nil || *logs[0].MaxUpstreamFrameGapMs < 30 ||
				logs[0].FirstToolDispatchDelayMs == nil || *logs[0].FirstToolDispatchDelayMs >= *logs[0].ToolAssemblyMs {
				t.Fatalf("missing/distorted wait metrics: %+v", logs)
			}
		})
	}
}

func TestLiveMalformedOrTruncatedToolNeverSendsStopOrReplays(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "broken", "name": "Write", "input": `{"value":"unfinished`, "stop": stop,
				}))
			}))
			defer upstream.Close()
			h := setupStreamIntegrityPathTest(t, upstream)
			enableLiveTestMode(t)
			rec := httptest.NewRecorder()
			h.handleClaudeMessages(rec, balancedRequest("Write", ""))
			body := rec.Body.String()
			if calls.Load() != 1 || !strings.Contains(body, "input_json_delta") || !strings.Contains(body, "event: error") ||
				strings.Contains(body, "content_block_stop") || strings.Contains(body, "message_stop") {
				t.Fatalf("invalid live tool completion/replay: calls=%d body=%s", calls.Load(), body)
			}
		})
	}
}

func TestToolCompletionRequiresJSONObject(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `"value"`, `{"x":`, `{"x":1} extra`} {
		t.Run(input, func(t *testing.T) {
			frame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "invalid", "name": "Write", "input": input, "stop": true,
			})
			completed := false
			err := parseEventStream(bytes.NewReader(frame), &KiroStreamCallback{
				OnToolUseStop: func(string) { completed = true },
				OnToolUse:     func(KiroToolUse) { completed = true },
			})
			assertEventStreamErrorKind(t, err, EventStreamInvalidPayload)
			if completed {
				t.Fatal("invalid object authorized tool completion")
			}
		})
	}
}

func TestLivePartialToolCancellationClosesUpstreamWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "cancel", "name": "Write", "input": `{"value":"partial`,
		}))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableLiveTestMode(t)
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := balancedRequest("Write", "")
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+r.URL.Path, r.Body)
	req.Header = r.Header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "input_json_delta") {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream remained connected after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("canceled partial tool replayed")
	}
}

func TestLiveInterleavedToolsDoNotCloseMalformedFollowingTool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, event := range []map[string]interface{}{
			{"toolUseId": "first", "name": "Write", "input": `{"value":`},
			{"toolUseId": "second", "name": "Write", "input": `{"value":`},
			{"toolUseId": "first", "input": `"valid"}`, "stop": true},
			{"toolUseId": "second", "input": `"broken`, "stop": true},
		} {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", event))
		}
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableLiveTestMode(t)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, balancedRequest("Write", ""))
	body := rec.Body.String()
	if strings.Count(body, "event: content_block_stop") != 1 || strings.Contains(body, "message_stop") ||
		!strings.Contains(body, "event: error") || strings.Count(body, `"id":"first"`) != 1 || strings.Count(body, `"id":"second"`) != 1 {
		t.Fatalf("invalid interleaved tool lifecycle: %s", body)
	}
}
