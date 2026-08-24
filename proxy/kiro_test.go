package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseEventStreamPreservesRepeatedTextDeltas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		field     string
		chunks    []string
		want      string
		reasoning bool
	}{
		{name: "assistant repeated", eventType: "assistantResponseEvent", field: "content", chunks: []string{"666", "666", "666", "6"}, want: "6666666666"},
		{name: "assistant prefix shaped", eventType: "assistantResponseEvent", field: "content", chunks: []string{"6", "66"}, want: "666"},
		{name: "reasoning repeated", eventType: "reasoningContentEvent", field: "text", chunks: []string{"ha", "ha", "ha"}, want: "hahaha", reasoning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream bytes.Buffer
			for _, chunk := range tc.chunks {
				stream.Write(awsEventStreamFrame(t, tc.eventType, map[string]interface{}{tc.field: chunk}))
			}
			stream.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))

			var got strings.Builder
			err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
				OnText: func(text string, reasoning bool) {
					if reasoning == tc.reasoning {
						got.WriteString(text)
					}
				},
			})
			if err != nil {
				t.Fatalf("parse repeated deltas: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("stream content corrupted: got %q want %q", got.String(), tc.want)
			}
		})
	}
}

func TestParseEventStreamRecoversCompletePendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	truncated := false
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnTruncated: func(string) { truncated = true },
	})
	if err != nil {
		t.Fatalf("expected complete tool use to be recovered, got %v", err)
	}
	if len(toolUses) != 1 || toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected recovered tool uses: %#v", toolUses)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("unexpected recovered tool input: %#v", toolUses[0].Input)
	}
	if truncated {
		t.Fatal("a complete recovered tool call must not be marked as truncated")
	}
}

func TestParseEventStreamHandlesSeparateToolFramesAndStopEvent(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseStartEvent", map[string]interface{}{
			"toolUseId": "toolu_separate",
			"name":      "Write",
		}),
		awsEventStreamFrame(t, "toolUseInputEvent", map[string]interface{}{
			"input": `{"file_path":"index.html","content":"ok"}`,
		}),
		awsEventStreamFrame(t, "toolUseStopEvent", map[string]interface{}{}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}),
	}, nil))

	var toolUses []KiroToolUse
	var starts, stops int
	var deltas strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUseStart: func(toolUseID, name string) { starts++ },
		OnToolUseDelta: func(toolUseID, input string) { deltas.WriteString(input) },
		OnToolUseStop:  func(toolUseID string) { stops++ },
		OnToolUse:      func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	if err != nil {
		t.Fatalf("parse separate tool frames: %v", err)
	}
	if starts != 1 || stops != 1 || len(toolUses) != 1 {
		t.Fatalf("unexpected tool callbacks: starts=%d stops=%d uses=%d", starts, stops, len(toolUses))
	}
	if got := toolUses[0].Input["content"]; got != "ok" {
		t.Fatalf("unexpected tool input: %#v", toolUses[0].Input)
	}
	if !strings.Contains(deltas.String(), `"index.html"`) {
		t.Fatalf("missing tool delta: %q", deltas.String())
	}
}

func TestParseEventStreamWaitsForRealToolIDBeforeStreaming(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseStartEvent", map[string]interface{}{
			"name":  "Write",
			"input": `{"content":"first`,
		}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_upstream",
			"name":      "Write",
			"input":     ` second"}`,
			"stop":      true,
		}),
	}, nil))

	var callbackIDs []string
	var toolUse KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUseStart: func(toolUseID, _ string) { callbackIDs = append(callbackIDs, toolUseID) },
		OnToolUseDelta: func(toolUseID, _ string) { callbackIDs = append(callbackIDs, toolUseID) },
		OnToolUseStop:  func(toolUseID string) { callbackIDs = append(callbackIDs, toolUseID) },
		OnToolUse:      func(value KiroToolUse) { toolUse = value },
	})
	if err != nil {
		t.Fatalf("parse generated tool id stream: %v", err)
	}
	if len(callbackIDs) != 4 {
		t.Fatalf("callback id count = %d, want 4: %#v", len(callbackIDs), callbackIDs)
	}
	for _, id := range callbackIDs {
		if id != "toolu_upstream" {
			t.Fatalf("tool id changed during stream: %#v", callbackIDs)
		}
	}
	if toolUse.ToolUseID != "toolu_upstream" || toolUse.Input["content"] != "first second" {
		t.Fatalf("unexpected completed tool use: %#v callbackIDs=%#v", toolUse, callbackIDs)
	}
}

func TestParseEventStreamRecoversCompleteToolAtCleanEOFAfterTelemetry(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_completion",
			"name":      "Write",
			"input":     `{"file_path":"index.html","content":"complete"}`,
		}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}),
	}, nil))

	var toolUses []KiroToolUse
	if err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	}); err != nil {
		t.Fatalf("recover complete tool at clean EOF: %v", err)
	}
	if len(toolUses) != 1 || toolUses[0].ToolUseID != "toolu_completion" {
		t.Fatalf("unexpected recovered tool use: %#v", toolUses)
	}
}

func TestParseEventStreamRejectsIncompletePendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "write",
		"input":     `{"file_path":"index.html","content":`,
	}))

	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
		t.Fatalf("expected incomplete tool-use error, got %#v", err)
	}
	if len(toolUses) != 0 {
		t.Fatalf("incomplete tool use must not be emitted, got %d", len(toolUses))
	}
}

func TestParseEventStreamRejectsPendingToolUseWithoutArgumentsOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "Write",
	}))

	err := parseEventStream(stream, &KiroStreamCallback{})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
		t.Fatalf("expected missing arguments to remain incomplete, got %#v", err)
	}
}

func TestParseEventStreamRecoversSchemaDeclaredZeroArgumentToolOnEOF(t *testing.T) {
	const toolName = "mcpMemoryReadGraphH123"
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_zero",
		"name":      toolName,
	}))
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}))

	var toolUses []KiroToolUse
	var stops int
	err := parseEventStreamWithOptions(stream, &KiroStreamCallback{
		OnToolUse:     func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
		OnToolUseStop: func(string) { stops++ },
	}, options)
	if err != nil {
		t.Fatalf("recover zero-argument tool: %v", err)
	}
	if len(toolUses) != 1 || toolUses[0].Name != toolName || len(toolUses[0].Input) != 0 || stops != 1 {
		t.Fatalf("unexpected recovered tool callbacks: uses=%#v stops=%d", toolUses, stops)
	}
}

func TestParseEventStreamRecoversSchemaDeclaredZeroArgumentToolOnCompletion(t *testing.T) {
	const toolName = "mcpFilesystemListAllowedDirectoriesH123"
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_zero",
			"name":      toolName,
		}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}),
	}, nil))
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}))

	var toolUses []KiroToolUse
	err := parseEventStreamWithOptions(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	}, options)
	if err != nil {
		t.Fatalf("recover zero-argument tool on completion: %v", err)
	}
	if len(toolUses) != 1 || len(toolUses[0].Input) != 0 {
		t.Fatalf("unexpected recovered tool use: %#v", toolUses)
	}
}

func TestParseEventStreamDoesNotRecoverZeroArgumentToolAtIncompatibleTerminalBoundary(t *testing.T) {
	const toolName = "mcpMemoryReadGraphH123"
	toolFrame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_zero",
		"name":      toolName,
	})
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}))

	tests := []struct {
		name     string
		terminal []byte
	}{
		{name: "end turn", terminal: awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"})},
		{name: "max tokens metadata", terminal: awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "max_tokens"})},
		{name: "max output tokens metadata", terminal: awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "max_output_tokens"})},
		{name: "content length exception", terminal: awsEventStreamExceptionFrame(t, "ContentLengthExceededException", map[string]interface{}{"message": "limit"})},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var toolUses []KiroToolUse
			err := parseEventStreamWithOptions(bytes.NewReader(bytes.Join([][]byte{toolFrame, tc.terminal}, nil)), &KiroStreamCallback{
				OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
			}, options)
			var streamErr *EventStreamError
			if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
				t.Fatalf("expected incomplete zero-argument tool, got %#v", err)
			}
			if len(toolUses) != 0 {
				t.Fatalf("incompatible terminal boundary emitted tool use: %#v", toolUses)
			}
		})
	}
}

func TestParseEventStreamTelemetryDoesNotFinalizePendingZeroArgumentTool(t *testing.T) {
	const toolName = "mcpMemoryReadGraphH123"
	toolFrame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_zero",
		"name":      toolName,
	})
	telemetry := awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})
	corrupt := awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0})
	corrupt[len(corrupt)-1] ^= 0xff
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}))

	var toolUses []KiroToolUse
	err := parseEventStreamWithOptions(bytes.NewReader(bytes.Join([][]byte{toolFrame, telemetry, corrupt}, nil)), &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	}, options)
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamMessageCRCMismatch {
		t.Fatalf("expected trailing CRC failure, got %#v", err)
	}
	if len(toolUses) != 0 {
		t.Fatalf("telemetry finalized a pending zero-argument tool: %#v", toolUses)
	}
}

func TestMergeToolUseRecoveryStateKeepsDominantCause(t *testing.T) {
	outputLimitCause := errors.New("output limit")
	toolUseCause := errors.New("tool use")
	boundary, cause := mergeToolUseRecoveryState(
		toolUseRecoveryOutputLimit, outputLimitCause,
		toolUseRecoveryExplicitToolUse, toolUseCause,
	)
	if boundary != toolUseRecoveryOutputLimit || !errors.Is(cause, outputLimitCause) {
		t.Fatalf("dominant recovery state was replaced: boundary=%v cause=%v", boundary, cause)
	}

	conflictCause := errors.New("conflicting stop")
	boundary, cause = mergeToolUseRecoveryState(boundary, cause, toolUseRecoveryConflictingStop, conflictCause)
	if boundary != toolUseRecoveryConflictingStop || !errors.Is(cause, conflictCause) {
		t.Fatalf("stronger recovery state was not applied: boundary=%v cause=%v", boundary, cause)
	}
}

func TestParseEventStreamDoesNotRecoverEmptyParameterizedTool(t *testing.T) {
	const toolName = "read_file"
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_parameterized",
		"name":      toolName,
	}))
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"path"},
	}))

	err := parseEventStreamWithOptions(stream, &KiroStreamCallback{}, options)
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
		t.Fatalf("expected parameterized tool to remain incomplete, got %#v", err)
	}
}

func TestParseEventStreamReadsWrappedToolUseEvent(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseEvent": map[string]interface{}{
			"toolUseId": "toolu_wrapped",
			"name":      "lookup",
			"input":     `{"query":"kiro"}`,
			"stop":      true,
		},
	}))

	var got KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{OnToolUse: func(toolUse KiroToolUse) { got = toolUse }})
	if err != nil {
		t.Fatalf("parse wrapped tool-use event: %v", err)
	}
	if got.ToolUseID != "toolu_wrapped" || got.Name != "lookup" || got.Input["query"] != "kiro" {
		t.Fatalf("wrapped tool-use event was not preserved: %#v", got)
	}
}

func TestParseEventStreamRecoversCompleteToolUseBeforeTruncatedFrame(t *testing.T) {
	completeTool := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "write",
		"input":     `{"file_path":"index.html","content":"complete"}`,
	})
	stream := bytes.NewReader(append(completeTool, []byte{0, 0, 0, 20}...))

	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("expected complete tool use to survive truncated tail, got %v", err)
	}
	if len(toolUses) != 1 || toolUses[0].Input["content"] != "complete" {
		t.Fatalf("unexpected recovered tool use: %#v", toolUses)
	}
}

func TestParseEventStreamPreservesTruncatedFrameErrorForIncompleteToolUse(t *testing.T) {
	incompleteTool := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "write",
		"input":     `{"file_path":"index.html","content":`,
	})
	stream := bytes.NewReader(append(incompleteTool, []byte{0, 0, 0, 20}...))

	err := parseEventStream(stream, &KiroStreamCallback{})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
		t.Fatalf("expected incomplete tool-use error, got %#v", err)
	}
	if streamErr.ToolName != "write" || streamErr.ArgumentBytes != len(`{"file_path":"index.html","content":`) || streamErr.FragmentCount != 1 {
		t.Fatalf("incomplete tool diagnostics were lost: %+v", streamErr)
	}
	var cause *EventStreamError
	if !errors.As(streamErr.Cause, &cause) || cause.Kind != EventStreamTruncated {
		t.Fatalf("expected truncated frame cause, got %#v", streamErr.Cause)
	}
}

func TestParseEventStreamDoesNotMaskCorruptFrameAfterIncompleteToolUse(t *testing.T) {
	incompleteTool := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "Write",
		"input":     `{"file_path":"index.html","content":`,
	})
	corrupt := awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})
	corrupt[len(corrupt)-1] ^= 0xff

	err := parseEventStream(bytes.NewReader(append(incompleteTool, corrupt...)), &KiroStreamCallback{})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamMessageCRCMismatch {
		t.Fatalf("expected message CRC error, got %#v", err)
	}
}

func TestParseEventStreamDoesNotMaskCorruptFrameAfterCompleteToolUse(t *testing.T) {
	completeTool := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_complete",
		"name":      "read_file",
		"input":     `{"path":"README.md"}`,
	})
	corrupt := awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})
	corrupt[len(corrupt)-1] ^= 0xff

	var toolUses []KiroToolUse
	err := parseEventStream(bytes.NewReader(append(completeTool, corrupt...)), &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamMessageCRCMismatch {
		t.Fatalf("expected message CRC error, got %#v", err)
	}
	if len(toolUses) != 0 {
		t.Fatalf("corrupt trailing frame must not emit pending tool uses: %#v", toolUses)
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
	}, nil))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	pending := &pendingToolUseSet{}
	err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, pending, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("unexpected tool-use error: %v", err)
	}

	if !pending.empty() {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}
	pending := &pendingToolUseSet{}

	err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, pending, callback)
	if err != nil {
		t.Fatalf("unexpected first tool-use error: %v", err)
	}
	err = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, pending, callback)
	if err != nil {
		t.Fatalf("unexpected completed tool-use error: %v", err)
	}

	if !pending.empty() {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestHandleToolUseEventIgnoresEmptyObjectBeforeArgumentFragments(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{OnToolUse: func(toolUse KiroToolUse) {
		toolUses = append(toolUses, toolUse)
	}}
	pending := &pendingToolUseSet{}

	err := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_fragmented",
		"name":      "read_file",
		"input":     map[string]interface{}{},
	}, pending, callback)
	if err != nil {
		t.Fatalf("unexpected initial tool-use error: %v", err)
	}
	err = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_fragmented",
		"name":      "read_file",
		"input":     `{"path":"README.md"}`,
		"stop":      true,
	}, pending, callback)
	if err != nil {
		t.Fatalf("unexpected completed tool-use error: %v", err)
	}
	if !pending.empty() || len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, pending=%v uses=%d", pending.order, len(toolUses))
	}
	if got := toolUses[0].Input["path"]; got != "README.md" {
		t.Fatalf("expected valid joined arguments, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport, err := buildKiroTransport("http://proxy.local:8080")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	transport, err := buildKiroTransport("")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected empty proxy setting to retain environment proxy resolution")
	}
}

func TestBuildKiroTransportDirectBypassesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport, err := buildKiroTransport("direct")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	if transport.Proxy != nil {
		t.Fatalf("expected direct transport to have no proxy function")
	}
}

func TestBuildKiroTransportRejectsMalformedProxyInsteadOfDirectFallback(t *testing.T) {
	if _, err := buildKiroTransport("http://proxy-without-port"); err == nil {
		t.Fatal("expected malformed proxy to fail transport construction")
	}
	if _, err := GetClientForProxy("socks5://:1080"); err == nil {
		t.Fatal("expected malformed account proxy to fail closed")
	}
}

func TestInitKiroHttpClientUsesIdleTimeoutForStreamsAndShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf("expected no total streaming timeout, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func TestKiroIDEEndpointIgnoresOAuthAuthenticationRegion(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	}

	got := ep.ResolveURL(&config.Account{AuthMethod: "idc", Region: "eu-west-1"})
	if got != "https://q.us-east-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("expected default data-plane endpoint, got %q", got)
	}
}

func TestKiroIDEEndpointUsesAPIKeyRegion(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	}

	got := ep.ResolveURL(&config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test", Region: "eu-west-1"})
	if got != "https://q.eu-west-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("expected API key region endpoint, got %q", got)
	}
}

func TestKiroIDEEndpointDefaultsToUSEast1(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	}

	got := ep.ResolveURL(&config.Account{})
	if got != "https://q.us-east-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("expected default endpoint, got %q", got)
	}
}

func TestNonKiroIDEEndpointKeepsConfiguredURL(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "http://example.test/generate",
		Origin: "AI_EDITOR",
		Name:   "test",
	}

	got := ep.ResolveURL(&config.Account{Region: "eu-west-1"})
	if got != ep.URL {
		t.Fatalf("expected configured URL to be preserved, got %q", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
