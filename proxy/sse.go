package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// prepareSSEHeaders tells browsers and reverse proxies that the response is a
// live event stream. In particular, no-transform and X-Accel-Buffering avoid
// intermediary buffering that can make an otherwise incremental response look
// like a single burst at the client.
func prepareSSEHeaders(w http.ResponseWriter) {
	if w == nil {
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
}

// writeSSEComment writes a protocol-level SSE comment. Comments are ignored
// by SSE consumers, but still reset idle timers in proxies and clients.
func writeSSEComment(w http.ResponseWriter, flusher http.Flusher, comment string) {
	if w == nil || flusher == nil {
		return
	}
	if comment == "" {
		comment = "keep-alive"
	}
	_, _ = fmt.Fprintf(w, ": %s\n\n", comment)
	flusher.Flush()
}

// startSSECommentHeartbeat starts a bounded-lifetime heartbeat for a stream.
// The emit callback owns any writer lock; this keeps the helper usable by the
// different protocol handlers without allowing concurrent ResponseWriter use.
func startSSECommentHeartbeat(ctx context.Context, interval time.Duration, emit func()) (stop func()) {
	if interval <= 0 || emit == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				emit()
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		stopOnce.Do(func() { close(done) })
		<-finished
	}
}
