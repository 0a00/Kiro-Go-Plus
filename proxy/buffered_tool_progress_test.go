package proxy

import (
	"bufio"
	"context"
	"encoding/json"
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

func TestBalancedToolAnnouncementRequiresRecognizedStreamingClient(t *testing.T) {
	for _, tc := range []struct {
		name, mode, agent  string
		stream, beta, want bool
	}{
		{"cli", "balanced", "claude-code/2", true, false, true},
		{"forwarded", "balanced", "Go-http-client/1.1", true, true, true},
		{"generic", "balanced", "Go-http-client/1.1", true, false, false},
		{"safe", "safe", "claude-code/2", true, false, false},
		{"live", "live", "claude-code/2", true, false, false},
		{"nonstream", "balanced", "claude-code/2", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &KiroPayload{}
			r := &ClaudeRequest{Stream: tc.stream, ClientUserAgent: tc.agent, ClientClaudeCodeBeta: tc.beta}
			configureClaudeToolStreaming(p, r, false, claudeThinkingResponseOptions{}, config.ThinkingConfig{ToolStreamMode: tc.mode})
			if p.announceBufferedToolStarts != tc.want {
				t.Fatalf("announce=%v, want %v", p.announceBufferedToolStarts, tc.want)
			}
		})
	}
}

// Withhold the tail until the downstream observes the start: no timing-based
// success assertion can accidentally accept a fully buffered implementation.
func TestBalancedToolAnnouncementPrecedesValidatedArguments(t *testing.T) {
	for _, outcome := range []string{"complete", "truncated", "cancel", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var calls atomic.Int32
			canceled := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload KiroPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				name := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.Name
				writeIntegrityText(t, w, "Preparing the requested tool.", false)
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "progress", "name": name, "input": `{"value":"pending`,
				}))
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					canceled <- struct{}{}
					return
				}
				if outcome != "complete" {
					return
				}
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{"input": ` complete"}`}))
				_, _ = w.Write(awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}))
				_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
			}))
			defer upstream.Close()
			defer unblock()
			h := setupStreamIntegrityPathTest(t, upstream)
			enableBalancedTestMode(t)
			if outcome == "timeout" {
				retry := config.GetRetryConfig()
				retry.ToolArgumentIdleTimeoutSeconds = 1
				if err := config.UpdateRetryConfig(retry); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(h)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := balancedRequest("mcp__workspace__edit-file", "")
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL+r.URL.Path, r.Body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = r.Header
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var early strings.Builder
			for !strings.Contains(early.String(), `"id":"progress"`) {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("missing early start: %v", err)
				}
				early.WriteString(line)
			}
			if !strings.Contains(early.String(), `"name":"mcp__workspace__edit-file"`) ||
				strings.Contains(early.String(), "input_json_delta") || strings.Contains(early.String(), "pending") ||
				strings.Count(early.String(), "event: content_block_stop") != 1 {
				t.Fatalf("premature arguments/completion or wrong name: %s", early.String())
			}
			if outcome == "cancel" {
				cancel()
				_ = resp.Body.Close()
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("upstream not canceled")
				}
				if calls.Load() != 1 {
					t.Fatal("canceled tool replayed")
				}
				return
			}
			if outcome != "timeout" {
				unblock()
			}
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			body := early.String() + string(rest)
			if calls.Load() != 1 || strings.Count(body, `"id":"progress"`) != 1 {
				t.Fatal("duplicate tool or upstream replay")
			}
			if outcome == "complete" {
				if strings.Count(body, "input_json_delta") != 1 || !strings.Contains(body, `\"pending complete\"`) ||
					strings.Count(body, "event: content_block_stop") != 2 || strings.Count(body, "event: message_stop") != 1 {
					t.Fatalf("invalid completed lifecycle: %s", body)
				}
			} else if !strings.Contains(body, "event: error") || strings.Contains(body, "input_json_delta") ||
				strings.Contains(body, "message_stop") || strings.Count(body, "event: content_block_stop") != 1 {
				t.Fatalf("incomplete tool received arguments or completion: %s", body)
			}
		})
	}
}

func TestBalancedToolAnnouncementsStayPairedWhenInterleaved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeIntegrityText(t, w, "Preparing two tools.", false)
		for _, event := range []map[string]interface{}{
			{"toolUseId": "one", "name": "Edit", "input": `{"value":`},
			{"toolUseId": "two", "name": "Edit", "input": `{"value":`},
			{"toolUseId": "two", "input": `"second"}`, "stop": true},
			{"toolUseId": "one", "input": `"first"}`, "stop": true},
		} {
			_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", event))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, balancedRequest("Edit", ""))
	ids, inputs, stops := map[int]string{}, map[string]string{}, map[int]int{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				ID string `json:"id"`
			} `json:"content_block"`
			Delta struct {
				Input string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		if e.Block.ID != "" {
			if ids[e.Index] != "" {
				t.Fatal("duplicate start")
			}
			ids[e.Index] = e.Block.ID
		}
		if e.Delta.Input != "" {
			if ids[e.Index] == "" || stops[e.Index] != 0 {
				t.Fatal("delta outside tool")
			}
			inputs[ids[e.Index]] += e.Delta.Input
		}
		if e.Type == "content_block_stop" {
			stops[e.Index]++
		}
	}
	if len(ids) != 2 || inputs["one"] != `{"value":"first"}` || inputs["two"] != `{"value":"second"}` {
		t.Fatalf("crossed/missing arguments: %+v", inputs)
	}
	for index := range ids {
		if stops[index] != 1 {
			t.Fatal("missing or duplicate stop")
		}
	}
}
