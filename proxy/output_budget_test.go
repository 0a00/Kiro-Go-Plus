package proxy

import (
	"bytes"
	"strings"
	"testing"
)

func TestClientOutputTokenLimiterStopsPlainTextStream(t *testing.T) {
	payload := &KiroPayload{
		clientOutputTokenLimit:   24,
		enforceClientOutputLimit: true,
	}
	var output strings.Builder
	var stopReason string
	completed := false
	callback := applyClientOutputTokenLimit(payload, &KiroStreamCallback{
		OnText:       func(text string, _ bool) { output.WriteString(text) },
		OnStopReason: func(reason string) { stopReason = reason },
		OnComplete:   func(_, _ int) { completed = true },
	})
	streamCallback, _ := wrapMeaningfulStreamCallback(callback, nil, false, false, false, false)
	streamCallback, _ = wrapToolAssemblyMonitor(streamCallback, 0, nil)
	stream := bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": strings.Repeat("word ", 100)}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "tail"}),
	}, nil)
	if err := parseEventStream(bytes.NewReader(stream), streamCallback); err != nil {
		t.Fatalf("parse output-limited stream: %v", err)
	}
	if output.Len() == 0 || estimateWireTokens(output.String()) > 24 {
		t.Fatalf("output exceeded client budget: bytes=%d tokens=%d", output.Len(), estimateWireTokens(output.String()))
	}
	if stopReason != "max_tokens" || !completed || callback.outputLimitReached == nil || !callback.outputLimitReached() {
		t.Fatalf("output limit did not produce a clean terminal state: reason=%q completed=%v", stopReason, completed)
	}
}

func TestClientOutputTokenLimiterDisabledForToolCapablePayload(t *testing.T) {
	// Handlers leave enforcement disabled whenever the client supplied tools;
	// preserving structured arguments is more important than a hard text cap.
	payload := &KiroPayload{clientOutputTokenLimit: 24}
	original := &KiroStreamCallback{OnText: func(string, bool) {}}
	if got := applyClientOutputTokenLimit(payload, original); got != original {
		t.Fatal("tool-capable payload unexpectedly installed a plain-text output limiter")
	}
}
