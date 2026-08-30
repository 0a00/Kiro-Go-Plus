package proxy

import (
	"strings"
	"testing"
)

// TestPayloadExceedsByteCap covers the configurable ceiling, including the
// documented "0 disables the byte check" case, where only token limits apply.
func TestPayloadExceedsByteCap(t *testing.T) {
	original := maxPayloadBytes
	t.Cleanup(func() { maxPayloadBytes = original })

	maxPayloadBytes = 1000
	if payloadExceedsByteCap(1000) {
		t.Fatal("a payload exactly at the ceiling must be accepted")
	}
	if !payloadExceedsByteCap(1001) {
		t.Fatal("a payload above the ceiling must be rejected")
	}

	maxPayloadBytes = 0
	for _, size := range []int{0, 1000, 10 << 20} {
		if payloadExceedsByteCap(size) {
			t.Fatalf("ceiling 0 must disable the byte check, but %d was rejected", size)
		}
	}
}

// TestRaisedByteCapKeepsLargeHistory shows the practical effect of the override:
// a payload that the default ceiling would trim survives a raised ceiling.
func TestRaisedByteCapKeepsLargeHistory(t *testing.T) {
	original := maxPayloadBytes
	t.Cleanup(func() { maxPayloadBytes = original })

	build := func() *KiroPayload {
		req := &ClaudeRequest{Model: "claude-opus-5"}
		for i := 0; i < 60; i++ {
			req.Messages = append(req.Messages,
				ClaudeMessage{Role: "user", Content: strings.Repeat("a", 12_000)},
				ClaudeMessage{Role: "assistant", Content: strings.Repeat("b", 12_000)},
			)
		}
		req.Messages = append(req.Messages, ClaudeMessage{Role: "user", Content: "e agora?"})
		return ClaudeToKiro(req, false)
	}

	maxPayloadBytes = 900 * 1024
	trimmed := build()

	maxPayloadBytes = 4 << 20
	untrimmed := build()

	if len(untrimmed.ConversationState.History) <= len(trimmed.ConversationState.History) {
		t.Fatalf("raising the ceiling should keep more history: default=%d raised=%d",
			len(trimmed.ConversationState.History), len(untrimmed.ConversationState.History))
	}
}
