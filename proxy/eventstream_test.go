package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
	"time"
)

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
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if reason == "" || !completed {
		t.Fatalf("expected truncated completion, reason=%q completed=%v", reason, completed)
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
