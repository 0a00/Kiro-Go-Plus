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
}
