package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBalancedClosesDeliveredTextWhileToolIsStillIncomplete(t *testing.T) {
	for _, nativeThinking := range []bool{false, true} {
		t.Run(map[bool]string{true: "native", false: "tags"}[nativeThinking], func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if nativeThinking {
					_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "Plan the edit."}))
				} else {
					_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>Plan the edit.</thinking>"}))
				}
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "\n\nI will expand the file."}))
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "edit1", "name": "Edit", "input": `{"value":"pending`,
				}))
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{"input": ` finished"}`}))
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
				// Later text must start a new block, not append to the closed one.
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Later text."}))
				_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
			}))
			defer upstream.Close()
			defer unblock()
			h := setupStreamIntegrityPathTest(t, upstream)
			enableBalancedTestMode(t)
			server := httptest.NewServer(h)
			defer server.Close()
			r := balancedRequest("Edit", "")
			req, _ := http.NewRequest("POST", server.URL+r.URL.Path, r.Body)
			req.Header = r.Header
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var early strings.Builder
			var stoppedText bool
			var preambleIndex = -1
			for !stoppedText {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("text block remained open during tool wait: %v", err)
				}
				early.WriteString(line)
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var e struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
				if strings.Contains(e.Delta.Text, "expand the file") {
					preambleIndex = e.Index
				}
				stoppedText = preambleIndex >= 0 && e.Type == "content_block_stop" && e.Index == preambleIndex
			}
			if strings.Contains(early.String(), `"type":"tool_use"`) || strings.Contains(early.String(), "pending") || strings.Contains(early.String(), "message_stop") {
				t.Fatalf("tool/message ended before validation: %s", early.String())
			}
			unblock()
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			all := early.String() + string(rest)
			starts, stops := map[int]string{}, map[int]int{}
			var text strings.Builder
			for _, line := range strings.Split(all, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var e struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
					Block struct {
						Type string `json:"type"`
					} `json:"content_block"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
				switch e.Type {
				case "content_block_start":
					if starts[e.Index] != "" {
						t.Fatal("duplicate start")
					}
					starts[e.Index] = e.Block.Type
				case "content_block_stop":
					stops[e.Index]++
				case "content_block_delta":
					if starts[e.Index] == "" || stops[e.Index] > 0 {
						t.Fatal("delta outside block")
					}
					if e.Delta.Type == "text_delta" {
						text.WriteString(e.Delta.Text)
					}
				}
			}
			for index := range starts {
				if stops[index] != 1 {
					t.Fatalf("unbalanced block %d", index)
				}
			}
			if text.String() != "\n\nI will expand the file.Later text." || strings.Count(all, "event: message_stop") != 1 || strings.Count(all, "input_json_delta") != 1 {
				t.Fatalf("output changed: %s", all)
			}
		})
	}
}

func TestBufferedToolBoundaryDoesNotExposeOrCommitSilentAttempts(t *testing.T) {
	var boundaries, toolCalls int
	callback, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		onBufferedToolStart: func(string, string) bool { boundaries++; return false }, OnText: func(string, bool) {}, OnToolUse: func(KiroToolUse) { toolCalls++ },
	}, nil, false, false, false, false)
	callback.OnToolUseStart("first", "Edit")
	callback.OnToolUseDelta("first", `{"value":`)
	if boundaries != 0 || toolCalls != 0 || gate.hasEmittedOutput() || gate.hasActionableOutput() {
		t.Fatal("silent incomplete tool committed")
	}
	callback.OnText("visible", false)
	callback.OnToolUseStart("second", "Edit")
	if boundaries != 1 || toolCalls != 0 {
		t.Fatal("missing text-only boundary")
	}
}

func TestBufferedToolBoundaryDoesNotFlushSafeModePendingText(t *testing.T) {
	var boundaries int
	callback, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		onBufferedToolStart: func(string, string) bool { boundaries++; return false }, OnText: func(string, bool) {},
	}, nil, true, true, true, false)
	callback.OnText("Let me edit the file.", false)
	callback.OnToolUseStart("first", "Edit")
	if boundaries != 0 || gate.hasEmittedOutput() {
		t.Fatal("safe-mode buffer flushed before complete tool")
	}
}
