package proxy

import (
	"io"
	"sync/atomic"
	"time"
)

// Measure application-level body reads separately from complete EventStream
// frames. Small reads can keep arriving while a large frame is still incomplete.
// These are not packet timestamps and do not identify the source of a delay.
type upstreamObservedReader struct {
	reader    io.Reader
	timing    *requestFirstContentTimer
	lastRead  time.Time
	lastFrame time.Time
}

func newUpstreamObservedReader(reader io.Reader, timing *requestFirstContentTimer) *upstreamObservedReader {
	now := time.Now()
	return &upstreamObservedReader{reader: reader, timing: timing, lastRead: now, lastFrame: now}
}

func (r *upstreamObservedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.readAt(time.Now())
	}
	return n, err
}

func (r *upstreamObservedReader) readAt(now time.Time) {
	storeMaxGap(&r.timing.maxUpstreamReadGapMs, now.Sub(r.lastRead))
	r.lastRead = now
}

func (r *upstreamObservedReader) frame(now time.Time) {
	storeMaxGap(&r.timing.maxUpstreamFrameGapMs, now.Sub(r.lastFrame))
	r.lastFrame = now
}

func (r *upstreamObservedReader) finish() {
	// Include a final stalled read/partial frame on EOF, timeout or cancellation.
	now := time.Now()
	storeMaxGap(&r.timing.maxUpstreamReadGapMs, now.Sub(r.lastRead))
	storeMaxGap(&r.timing.maxUpstreamFrameGapMs, now.Sub(r.lastFrame))
}

func storeMaxGap(target *atomic.Int64, duration time.Duration) {
	value := max(int64(0), duration.Milliseconds())
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}
