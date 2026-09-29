package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestUpstreamReadAndFrameGapsDifferWithoutCountingRetries(t *testing.T) {
	timing := newRequestFirstContentTimer(time.Now())
	reader := newUpstreamObservedReader(strings.NewReader(""), timing)
	start := reader.lastRead
	for i := 1; i <= 10; i++ {
		reader.readAt(start.Add(time.Duration(i) * 100 * time.Millisecond))
	}
	reader.frame(start.Add(time.Second))
	var entry requestLogEntry
	timing.Apply(&entry)
	if *entry.MaxUpstreamReadGapMs != 100 || *entry.MaxUpstreamFrameGapMs != 1000 {
		t.Fatalf("read/frame gap conflated: %+v", entry)
	}
	// A new attempt owns new timestamps: retry backoff must not become an
	// upstream body-read gap.
	retry := newUpstreamObservedReader(strings.NewReader(""), timing)
	retry.readAt(retry.lastRead.Add(50 * time.Millisecond))
	if timing.maxUpstreamReadGapMs.Load() != 100 {
		t.Fatal("retry changed upstream gap")
	}
}

func TestUpstreamObservationTracksTrailingStall(t *testing.T) {
	timing := newRequestFirstContentTimer(time.Now())
	r := newUpstreamObservedReader(strings.NewReader(""), timing)
	r.lastRead = time.Now().Add(-time.Second)
	r.lastFrame = r.lastRead
	r.finish()
	if timing.maxUpstreamReadGapMs.Load() < 1000 || timing.maxUpstreamFrameGapMs.Load() < 1000 {
		t.Fatal("trailing stall not observed")
	}
}

func TestToolDispatchDelayDoesNotIncludeDiscardedAttempt(t *testing.T) {
	timing := newRequestFirstContentTimer(time.Now().Add(-time.Hour))
	callback, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnToolUse: func(KiroToolUse) {}, upstreamTiming: timing,
	}, nil, false, false, false, false)
	callback.OnToolUseStart("discarded", "Write")
	gate.toolStarts["discarded"] = time.Now().Add(-time.Hour)
	callback.OnToolUseStart("valid", "Write")
	gate.toolStarts["valid"] = time.Now().Add(-50 * time.Millisecond)
	callback.OnToolUse(KiroToolUse{ToolUseID: "valid", Name: "Write", Input: map[string]interface{}{}})
	wait := timing.firstToolDispatchDelayMs.Load()
	if wait < 50 || wait > 1000 {
		t.Fatalf("dispatch delay includes discarded attempt: %d", wait)
	}
}

func TestLongRequestTimelinePreservesGapErrorAndTail(t *testing.T) {
	trace := &requestDetailTrace{startedAt: time.Now(), maxEvents: 32}
	trace.recordEventLocked("tool_start", 0)
	for i := 0; i < 5000; i++ {
		if i == 700 {
			trace.lastEventAt = time.Now().Add(-3 * time.Second)
		}
		if i == 1000 {
			trace.recordEventLocked("error", 0)
		}
		trace.recordEventLocked("tool_delta", 7)
	}
	trace.recordEventLocked("tool_stop", 0)
	trace.recordEventLocked("complete", 0)
	if len(trace.timeline) != 32 || trace.droppedEvents != trace.eventSequence-len(trace.timeline) {
		t.Fatalf("unbounded or inconsistent timeline: %d dropped=%d", len(trace.timeline), trace.droppedEvents)
	}
	var gap, failure bool
	previous := 0
	for _, event := range trace.timeline {
		gap = gap || event.IdleGapMs >= 3000
		failure = failure || event.Type == "error"
		if event.Sequence <= previous {
			t.Fatal("nonmonotonic retained events")
		}
		previous = event.Sequence
	}
	if !gap || !failure || trace.timeline[0].Type != "tool_start" || trace.timeline[len(trace.timeline)-1].Type != "complete" {
		t.Fatalf("important events discarded: %+v", trace.timeline)
	}
}
