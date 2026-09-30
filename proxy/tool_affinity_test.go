package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFailedToolClassificationDoesNotIncludeUnrelatedErrors(t *testing.T) {
	for _, err := range []error{newToolAssemblyTimeoutError("fixture", "Edit", 90, time.Minute), newToolOutputTruncatedError("fixture", nil)} {
		if !failedToolCall(fmt.Errorf("wrapped: %w", err)) {
			t.Fatal("tool failure not recognized")
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, newStreamIdleTimeoutError("fixture", time.Minute), newEmptyResponseError("fixture", true)} {
		if failedToolCall(err) {
			t.Fatalf("unrelated error treated as tool failure: %v", err)
		}
	}
}

func TestTruncatedToolDropsAffinityWithoutReplayingCommittedText(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeIntegrityText(t, w, "Preparing the file.", false)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "tool_fixture", "name": "Edit", "input": `{"new_string":"`}))
	}))
	defer server.Close()
	h := setupStreamIntegrityPathTest(t, server)
	up := config.GetUpstreamProtectionConfig()
	up.Enabled = true
	up.RouteAffinityTTLSeconds = 3600
	up.RouteAffinityMaxEntries = 20000
	if err := config.UpdateUpstreamProtectionConfig(up); err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	account, guard, err := h.pool.AcquireForModel("claude-sonnet-4.5", "tool-session", nil)
	if err != nil || account == nil || guard == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer guard.Release()
	payload := &KiroPayload{transparentClaudeCode: true, streamTextWithBufferedTools: true}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	var delivered string
	err = h.callKiroAPIWithHealth(account, payload, &KiroStreamCallback{OnText: func(text string, _ bool) { delivered += text }}, guard)
	if !failedToolCall(err) || hits != 1 || delivered != "Preparing the file." {
		t.Fatalf("unexpected replay or error: hits=%d text=%q err=%v", hits, delivered, err)
	}
	guard.Release()
	_, next, err := h.pool.AcquireForModel("claude-sonnet-4.5", "tool-session", nil)
	if err != nil || next == nil {
		t.Fatalf("next: %v", err)
	}
	defer next.Release()
	if next.AffinityHit() {
		t.Fatal("client retry stayed pinned to truncated-tool account")
	}
}
