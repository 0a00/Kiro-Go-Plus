package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"time"
)

type closeTrackingEventStream struct {
	reader *bytes.Reader
	closed bool
}

func (b *closeTrackingEventStream) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *closeTrackingEventStream) Close() error {
	b.closed = true
	return nil
}

func TestParseEventStreamValidCRCFrames(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 10.0}),
	}, nil))

	var output strings.Builder
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
		OnComplete: func(_, _ int) {
			completed = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if output.String() != "hello" || !completed {
		t.Fatalf("unexpected parse result: output=%q completed=%v", output.String(), completed)
	}
}

func TestParseEventStreamPreservesCacheUsageBreakdown(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "hello",
			"usage": map[string]interface{}{
				"input_tokens":                1000,
				"output_tokens":               25,
				"uncached_input_tokens":       300,
				"cache_read_input_tokens":     500,
				"cache_creation_input_tokens": 200,
				"cache_creation": map[string]interface{}{
					"ephemeral_5m_input_tokens": 150,
					"ephemeral_1h_input_tokens": 50,
				},
			},
		}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 10.0}),
	}, nil))

	var got KiroTokenUsage
	if err := parseEventStream(stream, &KiroStreamCallback{OnUsage: func(usage KiroTokenUsage) { got = usage }}); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !got.HasCacheBreakdown || got.InputTokens != 1000 || got.OutputTokens != 25 ||
		got.UncachedInputTokens != 300 || got.CacheReadInputTokens != 500 || got.CacheCreationInputTokens != 200 ||
		got.CacheCreation5mTokens != 150 || got.CacheCreation1hTokens != 50 {
		t.Fatalf("unexpected cache usage: %+v", got)
	}
}

func TestParseEventStreamPreservesThinkingUsage(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "hello",
			"usage": map[string]interface{}{
				"input_tokens":  10,
				"output_tokens": 20,
				"output_tokens_details": map[string]interface{}{
					"reasoning_tokens": 7,
				},
			},
		}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}),
	}, nil))

	var got KiroTokenUsage
	if err := parseEventStream(stream, &KiroStreamCallback{OnUsage: func(usage KiroTokenUsage) { got = usage }}); err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	if !got.HasThinkingBreakdown || got.ThinkingTokens != 7 {
		t.Fatalf("unexpected thinking usage: %+v", got)
	}
}

func TestParseEventStreamRejectsPreludeCRC(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})
	frame[8] ^= 0xff
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader(frame), nil), EventStreamPreludeCRCMismatch)
}

func TestParseEventStreamRejectsMessageCRC(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})
	frame[len(frame)-1] ^= 0xff
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader(frame), nil), EventStreamMessageCRCMismatch)
}

func TestParseEventStreamRejectsOversizedFrameBeforeAllocation(t *testing.T) {
	prelude := make([]byte, eventStreamPreludeSize)
	binary.BigEndian.PutUint32(prelude[0:4], eventStreamMaxFrame+1)
	binary.BigEndian.PutUint32(prelude[4:8], 0)
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader(prelude), nil), EventStreamFrameTooLarge)
}

func TestParseEventStreamRejectsPartialPrelude(t *testing.T) {
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader([]byte{0, 0, 0, 16}), nil), EventStreamTruncated)
}

func TestParseEventStreamRejectsPartialFrame(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader(frame[:len(frame)-3]), nil), EventStreamTruncated)
}

func TestParseEventStreamRejectsInvalidJSONPayload(t *testing.T) {
	frame := awsEventStreamRawFrame(t, "assistantResponseEvent", []byte(`{"content":`))
	assertEventStreamErrorKind(t, parseEventStream(bytes.NewReader(frame), nil), EventStreamInvalidPayload)
}

func TestParseEventStreamPreservesInvalidCompletedToolJSON(t *testing.T) {
	frame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_bad",
		"name":      "write_file",
		"input":     `{"path":`,
		"stop":      true,
	})
	var toolUse KiroToolUse
	err := parseEventStream(bytes.NewReader(frame), &KiroStreamCallback{OnToolUse: func(value KiroToolUse) { toolUse = value }})
	if err != nil {
		t.Fatalf("expected invalid arguments to be preserved, got %v", err)
	}
	if toolUse.Input["_raw_arguments"] != `{"path":` {
		t.Fatalf("unexpected preserved arguments: %+v", toolUse.Input)
	}
}

func TestParseEventStreamMarksTextWithoutCompletionSignalTruncated(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"})
	var reason string
	var completed bool
	err := parseEventStream(bytes.NewReader(frame), &KiroStreamCallback{
		OnTruncated: func(value string) { reason = value },
		OnComplete:  func(_, _ int) { completed = true },
	})
	assertEventStreamErrorKind(t, err, EventStreamIncompleteResponse)
	if reason == "" || completed {
		t.Fatalf("expected failed truncated stream, reason=%q completed=%v", reason, completed)
	}
}

func TestParseEventStreamReadsStopReasonKeyVariants(t *testing.T) {
	for _, key := range []string{"stopReason", "stop_reason"} {
		t.Run(key, func(t *testing.T) {
			stream := bytes.NewReader(bytes.Join([][]byte{
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "done"}),
				awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{key: "max_tokens"}),
			}, nil))
			var reason string
			if err := parseEventStream(stream, &KiroStreamCallback{OnStopReason: func(value string) { reason = value }}); err != nil {
				t.Fatalf("parse stop reason: %v", err)
			}
			if reason != "max_tokens" {
				t.Fatalf("stop reason=%q, want max_tokens", reason)
			}
		})
	}
}

func TestParseEventStreamTreatsContentLengthExceededAsMaxTokens(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial but valid"}),
		awsEventStreamExceptionFrame(t, "ContentLengthExceededException", map[string]interface{}{
			"message": "response reached the configured output limit",
		}),
	}, nil))
	var output strings.Builder
	var stopReason string
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText:       func(text string, _ bool) { output.WriteString(text) },
		OnStopReason: func(reason string) { stopReason = reason },
		OnComplete:   func(_, _ int) { completed = true },
	})
	if err != nil {
		t.Fatalf("max-token exception returned an error: %v", err)
	}
	if output.String() != "partial but valid" || stopReason != "max_tokens" || !completed {
		t.Fatalf("unexpected completion: output=%q reason=%q completed=%v", output.String(), stopReason, completed)
	}
}

func TestParseEventStreamContentLengthExceededRecoversCompleteToolPrefix(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_complete", "name": "Write", "input": `{"path":"index.html"}`,
		}),
		awsEventStreamExceptionFrame(t, "ContentLengthExceededException", map[string]interface{}{"message": "limit"}),
	}, nil))
	var tools []KiroToolUse
	var stopReason string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse:    func(tool KiroToolUse) { tools = append(tools, tool) },
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil {
		t.Fatalf("complete pending tool was not recovered: %v", err)
	}
	if len(tools) != 1 || tools[0].ToolUseID != "toolu_complete" || stopReason != "max_tokens" {
		t.Fatalf("unexpected recovered completion: tools=%+v reason=%q", tools, stopReason)
	}
}

func TestParseEventStreamRejectsSimilarContentLengthException(t *testing.T) {
	err := parseEventStream(bytes.NewReader(awsEventStreamExceptionFrame(
		t, "SomeContentLengthExceededException", map[string]interface{}{"message": "fatal"},
	)), &KiroStreamCallback{})
	if _, ok := asUpstreamError(err); !ok {
		t.Fatalf("near-match exception was not fatal: %#v", err)
	}
}

func TestStreamIdleReaderCancelsOnInactivity(t *testing.T) {
	timedOut := make(chan struct{}, 1)
	reader := newStreamIdleReader(strings.NewReader(""), 20*time.Millisecond, func() {
		timedOut <- struct{}{}
	})
	defer reader.Stop()

	select {
	case <-timedOut:
	case <-time.After(time.Second):
		t.Fatal("idle reader did not fire")
	}
}

func TestParseAndCloseEventStreamClosesBodyOnCallbackPanic(t *testing.T) {
	body := &closeTrackingEventStream{
		reader: bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})),
	}
	panicked := false
	func() {
		defer func() {
			panicked = recover() != nil
		}()
		_ = parseAndCloseEventStream(body, time.Second, nil, &KiroStreamCallback{
			OnText: func(string, bool) { panic("callback failed") },
		})
	}()
	if !panicked {
		t.Fatal("expected callback panic")
	}
	if !body.closed {
		t.Fatal("upstream body was not closed after callback panic")
	}
	if _, err := body.Read(nil); err != nil && err != io.EOF {
		t.Fatalf("unexpected tracking body state: %v", err)
	}
}

func assertEventStreamErrorKind(t *testing.T, err error, want EventStreamErrorKind) {
	t.Helper()
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != want {
		t.Fatalf("expected EventStream error %q, got %#v", want, err)
	}
}

func awsEventStreamRawFrame(t *testing.T, eventType string, payload []byte) []byte {
	t.Helper()
	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := eventStreamPreludeSize + len(headers) + len(payload) + 4
	frame := make([]byte, eventStreamPreludeSize, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	frame = append(frame, headers...)
	frame = append(frame, payload...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

func awsEventStreamExceptionFrame(t *testing.T, exceptionType string, payload map[string]interface{}) []byte {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal exception payload: %v", err)
	}
	headers := make([]byte, 0)
	headers = appendEventStreamStringHeader(headers, ":message-type", "exception")
	headers = appendEventStreamStringHeader(headers, ":exception-type", exceptionType)

	totalLength := eventStreamPreludeSize + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, eventStreamPreludeSize, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

func appendEventStreamStringHeader(dst []byte, name, value string) []byte {
	dst = append(dst, byte(len(name)))
	dst = append(dst, name...)
	dst = append(dst, byte(7))
	dst = append(dst, byte(len(value)>>8), byte(len(value)))
	return append(dst, value...)
}
