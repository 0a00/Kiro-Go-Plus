package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func TestStreamTerminationCauseClassifiesWrappedErrorsWithoutRawData(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{nil, ""}, {io.EOF, "eof"}, {io.ErrUnexpectedEOF, "unexpected_eof"},
		{context.Canceled, "context_canceled"}, {context.DeadlineExceeded, "deadline_exceeded"},
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "connection_reset"},
		{syscall.EPIPE, "broken_pipe"}, {&net.DNSError{IsTimeout: true}, "transport_timeout"},
		{errors.New("private cause at example.invalid"), "unknown"},
	} {
		err := tc.cause
		if err != nil {
			err = newToolOutputTruncatedError("fixture", &EventStreamError{Kind: EventStreamIncompleteToolUse, Cause: fmt.Errorf("wrapped: %w", err)})
		}
		if got := streamTerminationCause(err); got != tc.want {
			t.Fatalf("cause=%q, want %q", got, tc.want)
		}
	}
	for _, tc := range []struct {
		kind UpstreamErrorKind
		want string
	}{
		{UpstreamErrorToolAssemblyTimeout, "tool_argument_idle_timeout"},
		{UpstreamErrorStreamIdleTimeout, "stream_idle_timeout"},
		{UpstreamErrorActionableTimeout, "actionable_output_timeout"},
		{UpstreamErrorFirstTokenTimeout, "first_output_timeout"},
	} {
		if got := streamTerminationCause(&UpstreamError{Kind: tc.kind, Cause: context.DeadlineExceeded}); got != tc.want {
			t.Fatalf("got %q", got)
		}
	}
}

func TestIncompleteToolEOFAndResetRetainCauseInAdminAttempt(t *testing.T) {
	frame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "fixture", "name": "Edit", "input": `{"new_string":"`})
	for _, tc := range []struct {
		cause error
		want  string
	}{{io.EOF, "eof"}, {io.ErrUnexpectedEOF, "unexpected_eof"}, {syscall.ECONNRESET, "connection_reset"}} {
		err := parseEventStream(io.MultiReader(bytes.NewReader(frame), terminationErrorReader{tc.cause}), &KiroStreamCallback{})
		if !errors.Is(err, tc.cause) {
			t.Fatal("parser lost terminal cause")
		}
		var streamErr *EventStreamError
		if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
			t.Fatalf("unexpected parser error: %v", err)
		}
		wrapped := newToolOutputTruncatedError("fixture", streamErr)
		trace := newRequestDetailTrace(httptest.NewRequest("POST", "/v1/messages", nil), "claude.messages.stream", nil, config.DefaultRequestDetailMaxBytes)
		trace.recordAttempt("", "", "fixture", "example.invalid", time.Now(), 200, "partial_stream_error", wrapped, "tool_output_truncated")
		if len(trace.attempts) != 1 || trace.attempts[0].TerminationCause != tc.want {
			t.Fatal("missing admin terminal cause")
		}
		encoded, e := json.Marshal(trace.attempts[0])
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Contains(encoded, []byte(`"terminationCause":"`+tc.want+`"`)) {
			t.Fatal("classification not serialized")
		}
		if mapDownstreamError(wrapped).Status != 502 {
			t.Fatal("diagnostics changed status")
		}
	}
}

type terminationErrorReader struct{ err error }

func (r terminationErrorReader) Read([]byte) (int, error) { return 0, r.err }
