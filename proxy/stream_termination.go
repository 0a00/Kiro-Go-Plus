package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

// Record only allowlisted causes, never raw transport errors or peer addresses.
// EOF describes what the reader saw; it does not identify who closed the stream.
func streamTerminationCause(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := asUpstreamError(err); ok {
		switch e.Kind {
		case UpstreamErrorToolAssemblyTimeout:
			return "tool_argument_idle_timeout"
		case UpstreamErrorStreamIdleTimeout:
			return "stream_idle_timeout"
		case UpstreamErrorActionableTimeout:
			return "actionable_output_timeout"
		case UpstreamErrorFirstTokenTimeout:
			return "first_output_timeout"
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "transport_timeout"
	}
	return "unknown"
}
