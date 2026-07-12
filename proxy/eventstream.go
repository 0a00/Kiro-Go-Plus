package proxy

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"
)

const (
	eventStreamPreludeSize = 12
	eventStreamMinFrame    = eventStreamPreludeSize + 4
	eventStreamMaxFrame    = 16 * 1024 * 1024
)

// EventStreamErrorKind identifies failures while decoding AWS EventStream data.
type EventStreamErrorKind string

const (
	EventStreamTruncated          EventStreamErrorKind = "truncated"
	EventStreamInvalidLength      EventStreamErrorKind = "invalid_length"
	EventStreamFrameTooLarge      EventStreamErrorKind = "frame_too_large"
	EventStreamPreludeCRCMismatch EventStreamErrorKind = "prelude_crc_mismatch"
	EventStreamMessageCRCMismatch EventStreamErrorKind = "message_crc_mismatch"
	EventStreamInvalidHeaders     EventStreamErrorKind = "invalid_headers"
	EventStreamInvalidPayload     EventStreamErrorKind = "invalid_payload"
	EventStreamIncompleteToolUse  EventStreamErrorKind = "incomplete_tool_use"
)

// EventStreamError is returned for malformed or incomplete upstream frames.
type EventStreamError struct {
	Kind    EventStreamErrorKind
	Message string
	Cause   error
}

func (e *EventStreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("AWS EventStream %s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("AWS EventStream %s", e.Kind)
}

func (e *EventStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type eventStreamFrame struct {
	headers map[string]string
	payload []byte
}

func (f *eventStreamFrame) header(name string) string {
	if f == nil {
		return ""
	}
	return f.headers[name]
}

func readEventStreamFrame(r io.Reader) (*eventStreamFrame, error) {
	var prelude [eventStreamPreludeSize]byte
	n, err := io.ReadFull(r, prelude[:])
	if err != nil {
		if err == io.EOF && n == 0 {
			return nil, io.EOF
		}
		return nil, &EventStreamError{
			Kind:    EventStreamTruncated,
			Message: fmt.Sprintf("incomplete prelude: read %d of %d bytes", n, eventStreamPreludeSize),
			Cause:   err,
		}
	}

	totalLength := binary.BigEndian.Uint32(prelude[0:4])
	headersLength := binary.BigEndian.Uint32(prelude[4:8])
	if totalLength < eventStreamMinFrame {
		return nil, &EventStreamError{
			Kind:    EventStreamInvalidLength,
			Message: fmt.Sprintf("frame length %d is below minimum %d", totalLength, eventStreamMinFrame),
		}
	}
	if totalLength > eventStreamMaxFrame {
		return nil, &EventStreamError{
			Kind:    EventStreamFrameTooLarge,
			Message: fmt.Sprintf("frame length %d exceeds maximum %d", totalLength, eventStreamMaxFrame),
		}
	}
	if headersLength > totalLength-eventStreamMinFrame {
		return nil, &EventStreamError{
			Kind:    EventStreamInvalidLength,
			Message: fmt.Sprintf("headers length %d exceeds frame payload boundary", headersLength),
		}
	}

	expectedPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	actualPreludeCRC := crc32.ChecksumIEEE(prelude[0:8])
	if actualPreludeCRC != expectedPreludeCRC {
		return nil, &EventStreamError{
			Kind: EventStreamPreludeCRCMismatch,
			Message: fmt.Sprintf("expected 0x%08x, calculated 0x%08x",
				expectedPreludeCRC, actualPreludeCRC),
		}
	}

	remainingLength := int(totalLength) - eventStreamPreludeSize
	remaining := make([]byte, remainingLength)
	n, err = io.ReadFull(r, remaining)
	if err != nil {
		return nil, &EventStreamError{
			Kind:    EventStreamTruncated,
			Message: fmt.Sprintf("incomplete frame: read %d of %d remaining bytes", n, remainingLength),
			Cause:   err,
		}
	}

	expectedMessageCRC := binary.BigEndian.Uint32(remaining[len(remaining)-4:])
	checksum := crc32.NewIEEE()
	_, _ = checksum.Write(prelude[:])
	_, _ = checksum.Write(remaining[:len(remaining)-4])
	actualMessageCRC := checksum.Sum32()
	if actualMessageCRC != expectedMessageCRC {
		return nil, &EventStreamError{
			Kind: EventStreamMessageCRCMismatch,
			Message: fmt.Sprintf("expected 0x%08x, calculated 0x%08x",
				expectedMessageCRC, actualMessageCRC),
		}
	}

	headerEnd := int(headersLength)
	headers, err := parseEventStreamHeaders(remaining[:headerEnd])
	if err != nil {
		return nil, err
	}
	payloadEnd := len(remaining) - 4
	return &eventStreamFrame{
		headers: headers,
		payload: remaining[headerEnd:payloadEnd],
	}, nil
}

func parseEventStreamHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for offset := 0; offset < len(data); {
		nameLength := int(data[offset])
		offset++
		if nameLength == 0 {
			return nil, invalidEventStreamHeaders("header name is empty")
		}
		if offset+nameLength+1 > len(data) {
			return nil, invalidEventStreamHeaders("header name or value type is truncated")
		}

		name := string(data[offset : offset+nameLength])
		offset += nameLength
		valueType := data[offset]
		offset++

		switch valueType {
		case 0, 1:
			// Boolean values have no payload.
		case 2:
			if offset+1 > len(data) {
				return nil, invalidEventStreamHeaders("byte header is truncated")
			}
			offset++
		case 3:
			if offset+2 > len(data) {
				return nil, invalidEventStreamHeaders("short header is truncated")
			}
			offset += 2
		case 4:
			if offset+4 > len(data) {
				return nil, invalidEventStreamHeaders("integer header is truncated")
			}
			offset += 4
		case 5, 8:
			if offset+8 > len(data) {
				return nil, invalidEventStreamHeaders("long header is truncated")
			}
			offset += 8
		case 6, 7:
			if offset+2 > len(data) {
				return nil, invalidEventStreamHeaders("variable-length header size is truncated")
			}
			valueLength := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLength > len(data) {
				return nil, invalidEventStreamHeaders("variable-length header value is truncated")
			}
			if valueType == 7 {
				headers[name] = string(data[offset : offset+valueLength])
			}
			offset += valueLength
		case 9:
			if offset+16 > len(data) {
				return nil, invalidEventStreamHeaders("UUID header is truncated")
			}
			offset += 16
		default:
			return nil, invalidEventStreamHeaders(fmt.Sprintf("unsupported header value type %d", valueType))
		}
	}
	return headers, nil
}

func invalidEventStreamHeaders(message string) error {
	return &EventStreamError{Kind: EventStreamInvalidHeaders, Message: message}
}

// streamIdleReader cancels an upstream request when no body bytes arrive for
// the configured interval. It does not impose a total duration on the stream.
type streamIdleReader struct {
	reader    io.Reader
	timeout   time.Duration
	onTimeout func()

	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	fired   bool
}

func newStreamIdleReader(reader io.Reader, timeout time.Duration, onTimeout func()) *streamIdleReader {
	r := &streamIdleReader{reader: reader, timeout: timeout, onTimeout: onTimeout}
	if timeout > 0 {
		r.timer = time.AfterFunc(timeout, r.fire)
	}
	return r
}

func (r *streamIdleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.touch()
	}
	return n, err
}

func (r *streamIdleReader) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
}

func (r *streamIdleReader) touch() {
	if r == nil || r.timeout <= 0 {
		return
	}
	r.mu.Lock()
	if !r.stopped && !r.fired && r.timer != nil {
		r.timer.Reset(r.timeout)
	}
	r.mu.Unlock()
}

func (r *streamIdleReader) fire() {
	r.mu.Lock()
	if r.stopped || r.fired {
		r.mu.Unlock()
		return
	}
	r.fired = true
	onTimeout := r.onTimeout
	r.mu.Unlock()
	if onTimeout != nil {
		onTimeout()
	}
}
