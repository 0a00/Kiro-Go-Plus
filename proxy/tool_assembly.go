package proxy

import (
	"sync"
	"time"
)

type toolAssemblySnapshot struct {
	ToolUseID     string
	Name          string
	ArgumentBytes int
	Elapsed       time.Duration
}

type toolAssemblyMonitor struct {
	mu         sync.Mutex
	timeout    time.Duration
	onTimeout  func(toolAssemblySnapshot)
	timer      *time.Timer
	generation uint64
	active     bool
	startedAt  time.Time
	toolUseID  string
	name       string
	bytes      int
	timedOut   *toolAssemblySnapshot
}

func wrapToolAssemblyMonitor(target *KiroStreamCallback, timeout time.Duration, onTimeout func(toolAssemblySnapshot)) (*KiroStreamCallback, *toolAssemblyMonitor) {
	if target == nil {
		target = &KiroStreamCallback{}
	}
	monitor := &toolAssemblyMonitor{timeout: timeout, onTimeout: onTimeout}
	wrapped := *target
	originalStart := target.OnToolUseStart
	originalDelta := target.OnToolUseDelta
	originalStop := target.OnToolUseStop

	wrapped.OnToolUseStart = func(toolUseID, name string) {
		monitor.start(toolUseID, name)
		if originalStart != nil {
			originalStart(toolUseID, name)
		}
	}
	wrapped.OnToolUseDelta = func(toolUseID, input string) {
		monitor.add(toolUseID, input)
		if originalDelta != nil {
			originalDelta(toolUseID, input)
		}
	}
	wrapped.OnToolUseStop = func(toolUseID string) {
		monitor.stop(toolUseID)
		if originalStop != nil {
			originalStop(toolUseID)
		}
	}
	return &wrapped, monitor
}

func (m *toolAssemblyMonitor) start(toolUseID, name string) {
	if m == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.stopTimerLocked()
	m.generation++
	generation := m.generation
	m.active = true
	m.startedAt = now
	m.toolUseID = toolUseID
	m.name = name
	m.bytes = 0
	m.timedOut = nil
	if m.timeout > 0 {
		m.timer = time.AfterFunc(m.timeout, func() {
			m.fire(generation)
		})
	}
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) add(toolUseID, input string) {
	if m == nil || input == "" {
		return
	}
	m.mu.Lock()
	if m.active {
		if toolUseID != "" && m.toolUseID != toolUseID {
			m.toolUseID = toolUseID
		}
		m.bytes += len(input)
	}
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) stop(toolUseID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.active && (m.toolUseID == "" || toolUseID == "" || m.toolUseID == toolUseID) {
		m.active = false
		m.generation++
		m.stopTimerLocked()
	}
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active = false
	m.generation++
	m.stopTimerLocked()
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) TimedOut() (toolAssemblySnapshot, bool) {
	if m == nil {
		return toolAssemblySnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timedOut == nil {
		return toolAssemblySnapshot{}, false
	}
	return *m.timedOut, true
}

func (m *toolAssemblyMonitor) fire(generation uint64) {
	m.mu.Lock()
	if !m.active || m.generation != generation {
		m.mu.Unlock()
		return
	}
	snapshot := toolAssemblySnapshot{
		ToolUseID:     m.toolUseID,
		Name:          m.name,
		ArgumentBytes: m.bytes,
		Elapsed:       time.Since(m.startedAt),
	}
	m.active = false
	m.timedOut = &snapshot
	m.timer = nil
	onTimeout := m.onTimeout
	m.mu.Unlock()
	if onTimeout != nil {
		onTimeout(snapshot)
	}
}

func (m *toolAssemblyMonitor) stopTimerLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}
