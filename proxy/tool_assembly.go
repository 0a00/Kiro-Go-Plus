package proxy

import (
	"sync"
	"time"
)

type toolAssemblySnapshot struct {
	ToolUseID     string
	Name          string
	ArgumentBytes int
	FragmentCount int
	Elapsed       time.Duration
}

type toolAssemblyMonitor struct {
	mu           sync.Mutex
	timeout      time.Duration
	onTimeout    func(toolAssemblySnapshot)
	timer        *time.Timer
	generation   uint64
	active       bool
	startedAt    time.Time
	toolUseID    string
	name         string
	bytes        int
	fragments    int
	timedOut     *toolAssemblySnapshot
	maxElapsed   time.Duration
	hasElapsed   bool
	maxBytes     int
	maxFragments int
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
	originalActivity := target.OnToolUseActivity

	wrapped.OnToolUseActivity = func() {
		monitor.activity()
		if originalActivity != nil {
			originalActivity()
		}
	}

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
	if m.active && m.toolUseID == "" {
		m.toolUseID = toolUseID
		m.name = name
		m.bytes = 0
		m.fragments = 0
		m.mu.Unlock()
		return
	}
	if m.active {
		m.recordElapsedLocked(now)
	}
	m.stopTimerLocked()
	m.generation++
	generation := m.generation
	m.active = true
	m.startedAt = now
	m.toolUseID = toolUseID
	m.name = name
	m.bytes = 0
	m.fragments = 0
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
		m.fragments++
		if m.bytes > m.maxBytes {
			m.maxBytes = m.bytes
		}
		if m.fragments > m.maxFragments {
			m.maxFragments = m.fragments
		}
	}
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) activity() {
	if m == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		return
	}
	m.generation++
	generation := m.generation
	m.active = true
	m.startedAt = now
	m.toolUseID = ""
	m.name = ""
	m.bytes = 0
	m.fragments = 0
	m.timedOut = nil
	if m.timeout > 0 {
		m.timer = time.AfterFunc(m.timeout, func() {
			m.fire(generation)
		})
	}
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) stop(toolUseID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.active && (m.toolUseID == "" || toolUseID == "" || m.toolUseID == toolUseID) {
		m.recordElapsedLocked(time.Now())
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
	if m.active {
		m.recordElapsedLocked(time.Now())
	}
	m.active = false
	m.generation++
	m.stopTimerLocked()
	m.mu.Unlock()
}

func (m *toolAssemblyMonitor) MaxElapsed() (time.Duration, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxElapsed, m.hasElapsed
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

func (m *toolAssemblyMonitor) MaxArguments() (bytes, fragments int) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxBytes, m.maxFragments
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
		FragmentCount: m.fragments,
		Elapsed:       time.Since(m.startedAt),
	}
	m.recordDurationLocked(snapshot.Elapsed)
	m.active = false
	m.timedOut = &snapshot
	m.timer = nil
	onTimeout := m.onTimeout
	m.mu.Unlock()
	if onTimeout != nil {
		onTimeout(snapshot)
	}
}

func (m *toolAssemblyMonitor) recordElapsedLocked(now time.Time) {
	if m.startedAt.IsZero() {
		return
	}
	m.recordDurationLocked(now.Sub(m.startedAt))
}

func (m *toolAssemblyMonitor) recordDurationLocked(elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	if !m.hasElapsed || elapsed > m.maxElapsed {
		m.maxElapsed = elapsed
		m.hasElapsed = true
	}
}

func stopAndRecordToolAssembly(payload *KiroPayload, monitor *toolAssemblyMonitor) {
	if monitor == nil {
		return
	}
	monitor.Stop()
	if elapsed, ok := monitor.MaxElapsed(); ok && payload != nil {
		payload.recordToolAssembly(elapsed)
	}
	if payload != nil {
		argumentBytes, fragmentCount := monitor.MaxArguments()
		payload.recordToolStreamMetrics(argumentBytes, fragmentCount)
	}
}

func (m *toolAssemblyMonitor) stopTimerLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}
