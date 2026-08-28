package proxy

import (
	"testing"
	"time"
)

func TestToolAssemblyMonitorTimesOutGrowingIncompleteTool(t *testing.T) {
	timedOut := make(chan toolAssemblySnapshot, 1)
	callback, monitor := wrapToolAssemblyMonitor(&KiroStreamCallback{}, 20*time.Millisecond, func(snapshot toolAssemblySnapshot) {
		timedOut <- snapshot
	})
	defer monitor.Stop()

	callback.OnToolUseStart("toolu_write", "Write")
	callback.OnToolUseDelta("toolu_write", `{"content":"partial`)

	select {
	case snapshot := <-timedOut:
		if snapshot.Name != "Write" || snapshot.ArgumentBytes == 0 || snapshot.Elapsed < 15*time.Millisecond {
			t.Fatalf("unexpected timeout snapshot: %+v", snapshot)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tool assembly monitor did not time out")
	}
	if snapshot, ok := monitor.TimedOut(); !ok || snapshot.ToolUseID != "toolu_write" {
		t.Fatalf("monitor did not retain timeout details: %+v, %v", snapshot, ok)
	}
}

func TestToolAssemblyMonitorStopsAfterCompleteTool(t *testing.T) {
	timedOut := make(chan toolAssemblySnapshot, 1)
	callback, monitor := wrapToolAssemblyMonitor(&KiroStreamCallback{}, 20*time.Millisecond, func(snapshot toolAssemblySnapshot) {
		timedOut <- snapshot
	})
	defer monitor.Stop()

	callback.OnToolUseStart("toolu_write", "Write")
	callback.OnToolUseDelta("toolu_write", `{"content":"complete"}`)
	callback.OnToolUseStop("toolu_write")

	select {
	case snapshot := <-timedOut:
		t.Fatalf("completed tool timed out: %+v", snapshot)
	case <-time.After(60 * time.Millisecond):
	}
	if snapshot, ok := monitor.TimedOut(); ok {
		t.Fatalf("completed tool retained timeout: %+v", snapshot)
	}
	if elapsed, ok := monitor.MaxElapsed(); !ok || elapsed <= 0 {
		t.Fatalf("completed tool assembly duration was not retained: elapsed=%s ok=%v", elapsed, ok)
	}
}

func TestToolAssemblyMonitorTracksInterleavedToolsIndependently(t *testing.T) {
	timedOut := make(chan toolAssemblySnapshot, 1)
	callback, monitor := wrapToolAssemblyMonitor(&KiroStreamCallback{}, 40*time.Millisecond, func(snapshot toolAssemblySnapshot) {
		timedOut <- snapshot
	})
	defer monitor.Stop()

	callback.OnToolUseStart("toolu_a", "first")
	callback.OnToolUseDelta("toolu_a", `{"value":`)
	callback.OnToolUseStart("toolu_b", "second")
	callback.OnToolUseDelta("toolu_b", `{"value":2}`)
	callback.OnToolUseStop("toolu_b")

	select {
	case snapshot := <-timedOut:
		if snapshot.ToolUseID != "toolu_a" || snapshot.Name != "first" || snapshot.ArgumentBytes == 0 {
			t.Fatalf("wrong interleaved tool timed out: %+v", snapshot)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unfinished first tool was lost when second tool completed")
	}
}

func TestToolAssemblyMonitorRenewsGrowingToolAfterConfiguredInterval(t *testing.T) {
	timedOut := make(chan toolAssemblySnapshot, 1)
	const timeout = 200 * time.Millisecond
	callback, monitor := wrapToolAssemblyMonitor(&KiroStreamCallback{}, timeout, func(snapshot toolAssemblySnapshot) {
		timedOut <- snapshot
	})
	defer monitor.Stop()

	callback.OnToolUseStart("toolu_growing", "Write")
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		callback.OnToolUseDelta("toolu_growing", "x")
		select {
		case snapshot := <-timedOut:
			t.Fatalf("active tool timed out while receiving fragments: %+v", snapshot)
		default:
		}
	}

	select {
	case snapshot := <-timedOut:
		t.Fatalf("active tool timed out immediately after a fragment: %+v", snapshot)
	case <-time.After(75 * time.Millisecond):
	}
	select {
	case snapshot := <-timedOut:
		if snapshot.Elapsed < timeout {
			t.Fatalf("tool timed out before its total elapsed time exceeded the idle limit: %+v", snapshot)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("tool did not time out after becoming idle")
	}
}

func TestToolAssemblyMonitorRenewsOnRepeatedStartAndActivity(t *testing.T) {
	timedOut := make(chan toolAssemblySnapshot, 1)
	const timeout = 120 * time.Millisecond
	callback, monitor := wrapToolAssemblyMonitor(&KiroStreamCallback{}, timeout, func(snapshot toolAssemblySnapshot) {
		timedOut <- snapshot
	})
	defer monitor.Stop()

	callback.OnToolUseStart("toolu_repeat", "Write")
	time.Sleep(70 * time.Millisecond)
	callback.OnToolUseStart("toolu_repeat", "Write")
	time.Sleep(70 * time.Millisecond)
	callback.OnToolUseActivity()
	select {
	case snapshot := <-timedOut:
		t.Fatalf("repeated start/activity did not renew the tool: %+v", snapshot)
	default:
	}

	select {
	case snapshot := <-timedOut:
		if snapshot.ToolUseID != "toolu_repeat" {
			t.Fatalf("unexpected timed-out tool: %+v", snapshot)
		}
	case <-time.After(220 * time.Millisecond):
		t.Fatal("tool did not time out after activity stopped")
	}
}
