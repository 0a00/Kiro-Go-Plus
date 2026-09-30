package main

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"syscall"
)

const maxLoadFailureDetails = 32

// Deliberately omit error strings, URLs, response bodies and tool arguments.
// Wrapped net/url errors can contain credentials or private request data.
type loadFailureDetail struct {
	Protocol       string `json:"protocol"`
	Workload       string `json:"workload,omitempty"`
	Category       string `json:"category"`
	Stage          string `json:"stage"`
	Cause          string `json:"cause,omitempty"`
	RequestID      string `json:"requestId,omitempty"`
	HTTPStatus     int    `json:"httpStatus,omitempty"`
	Stream         bool   `json:"stream"`
	TotalMillis    int64  `json:"totalMillis"`
	HeadersMillis  int64  `json:"headersMillis"`
	TTFTMillis     int64  `json:"ttftMillis"`
	Events         int    `json:"events,omitempty"`
	Heartbeats     int    `json:"heartbeats,omitempty"`
	SemanticOutput bool   `json:"semanticOutput"`
	Terminal       bool   `json:"terminal"`
}

var diagnosticIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func safeDiagnosticID(id string) string {
	if diagnosticIDPattern.MatchString(id) {
		return id
	}
	return ""
}

func transportFailureCause(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errResponseTooLarge):
		return "diagnostic_limit"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "network_timeout"
	}
	return "transport_other"
}

func describeLoadFailure(response apiResponse, sample loadSample) loadFailureDetail {
	stage := response.errorStage
	if stage == "" {
		stage = "validate_response"
	}
	return loadFailureDetail{
		Protocol: sample.protocol, Workload: sample.workload, Category: sample.category,
		Stage: stage, Cause: transportFailureCause(response.err), RequestID: safeDiagnosticID(response.requestID),
		HTTPStatus: response.statusCode, Stream: sample.stream, TotalMillis: sample.duration,
		HeadersMillis: sample.headers, TTFTMillis: sample.ttft, Events: response.stream.events,
		Heartbeats: response.stream.heartbeats, SemanticOutput: response.stream.semanticOutput, Terminal: response.stream.terminal,
	}
}
