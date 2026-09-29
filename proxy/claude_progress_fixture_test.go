package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in, loopback-only real-handler fixture for the interactive Claude Code
// regression. It never loads real accounts or calls a real upstream.
func TestClaudeCodeProgressUIFixture(t *testing.T) {
	dir := os.Getenv("KIRO_DEV_UI_FIXTURE_DIR")
	if dir == "" {
		t.Skip("interactive fixture not requested")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("fixture directory must be absolute")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatal("fixture directory must exist")
	}
	workspace := filepath.Join(dir, "gateway")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload KiroPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if ctx != nil && len(ctx.ToolResults) > 0 {
			writeIntegrityText(t, w, "FIXTURE_COMPLETED", true)
			return
		}
		send := func(event string, data map[string]interface{}) {
			_, _ = w.Write(awsEventStreamFrame(t, event, data))
			w.(http.Flusher).Flush()
		}
		wait := func(d time.Duration) bool {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				return true
			case <-r.Context().Done():
				return false
			}
		}
		start, _ := json.Marshal(map[string]int64{"at": time.Now().UnixMilli()})
		if err := os.WriteFile(filepath.Join(dir, "gateway-started.json"), start, 0600); err != nil {
			t.Error(err)
			return
		}
		send("assistantResponseEvent", map[string]interface{}{"content": "<thinking>I will verify a disposable file edit."})
		if !wait(2 * time.Second) {
			return
		}
		send("assistantResponseEvent", map[string]interface{}{"content": "</thinking>\n\nI will expand the file with more test content."})
		args, _ := json.Marshal(map[string]string{"file_path": filepath.Join(workspace, "progress.txt"), "old_string": "", "new_string": strings.Repeat("progress fixture\n", 10)})
		send("toolUseEvent", map[string]interface{}{"toolUseId": "fixture-edit", "name": "Edit", "input": string(args[:30])})
		if !wait(12 * time.Second) {
			return
		}
		send("toolUseInputEvent", map[string]interface{}{"input": string(args[30:])})
		send("toolUseStopEvent", map[string]interface{}{})
		send("metadataEvent", map[string]interface{}{"stopReason": "tool_use"})
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	enableBalancedTestMode(t)
	kiroHttpStore.Store(&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{}})
	server := httptest.NewServer(h)
	defer server.Close()
	if err := os.WriteFile(filepath.Join(dir, "gateway-url.txt"), []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "fixture-stop")); err == nil {
				return
			}
		case <-deadline.C:
			t.Fatal("interactive fixture exceeded two-minute bound")
		}
	}
}
