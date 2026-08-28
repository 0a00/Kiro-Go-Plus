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
	active       map[string]*toolAssemblyActive
	nextGen      uint64
	timedOut     *toolAssemblySnapshot
	maxElapsed   time.Duration
	hasElapsed   bool
	maxBytes     int
	maxFragments int
}

type toolAssemblyActive struct {
	toolUseID      string
	name           string
	startedAt      time.Time
	lastActivityAt time.Time
	bytes          int
	fragments      int
	generation     uint64
	timer          *time.Timer
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timedOut != nil {
		return
	}
	if m.active == nil {
		m.active = make(map[string]*toolAssemblyActive)
	}
	if existing := m.active[toolUseID]; existing != nil {
		if name != "" {
			existing.name = name
		}
		m.touchLocked(existing)
		return
	}
	if toolUseID != "" {
		if placeholder := m.active[""]; placeholder != nil {
			delete(m.active, "")
			if placeholder.timer != nil {
				placeholder.timer.Stop()
			}
			placeholder.toolUseID = toolUseID
			placeholder.name = name
			m.active[toolUseID] = placeholder
			m.touchLocked(placeholder)
			m.armLocked(placeholder)
			return
		}
	}
	m.addActiveLocked(toolUseID, name, time.Now())
}

func (m *toolAssemblyMonitor) addActiveLocked(toolUseID, name string, startedAt time.Time) *toolAssemblyActive {
	if m.active == nil {
		m.active = make(map[string]*toolAssemblyActive)
	}
	state := &toolAssemblyActive{
		toolUseID:      toolUseID,
		name:           name,
		startedAt:      startedAt,
		lastActivityAt: startedAt,
	}
	m.active[toolUseID] = state
	m.armLocked(state)
	return state
}

func (m *toolAssemblyMonitor) touchLocked(state *toolAssemblyActive) {
	if state == nil {
		return
	}
	state.lastActivityAt = time.Now()
}

func (m *toolAssemblyMonitor) armLocked(state *toolAssemblyActive) {
	if state == nil || m.timeout <= 0 {
		return
	}
	if state.lastActivityAt.IsZero() {
		state.lastActivityAt = time.Now()
	}
	m.nextGen++
	state.generation = m.nextGen
	remaining := m.timeout - time.Since(state.lastActivityAt)
	if remaining < 0 {
		remaining = 0
	}
	toolUseID := state.toolUseID
	generation := state.generation
	state.timer = time.AfterFunc(remaining, func() {
		m.fire(toolUseID, generation)
	})
}

func (m *toolAssemblyMonitor) stateForInputLocked(toolUseID string) *toolAssemblyActive {
	if state := m.active[toolUseID]; state != nil {
		return state
	}
	if toolUseID != "" {
		if placeholder := m.active[""]; placeholder != nil {
			delete(m.active, "")
			if placeholder.timer != nil {
				placeholder.timer.Stop()
			}
			placeholder.toolUseID = toolUseID
			m.active[toolUseID] = placeholder
			m.touchLocked(placeholder)
			m.armLocked(placeholder)
			return placeholder
		}
	}
	return nil
}

func (m *toolAssemblyMonitor) add(toolUseID, input string) {
	if m == nil || input == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timedOut != nil {
		return
	}
	state := m.stateForInputLocked(toolUseID)
	if state == nil {
		state = m.addActiveLocked(toolUseID, "", time.Now())
	}
	state.bytes += len(input)
	state.fragments++
	if state.bytes > m.maxBytes {
		m.maxBytes = state.bytes
	}
	if state.fragments > m.maxFragments {
		m.maxFragments = state.fragments
	}
	m.touchLocked(state)
}

func (m *toolAssemblyMonitor) activity() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timedOut != nil {
		return
	}
	if m.active == nil {
		m.active = make(map[string]*toolAssemblyActive)
	}
	if len(m.active) == 0 {
		m.addActiveLocked("", "", time.Now())
		return
	}
	for _, state := range m.active {
		m.touchLocked(state)
	}
}

func (m *toolAssemblyMonitor) stop(toolUseID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.active[toolUseID]
	if state == nil && toolUseID != "" {
		state = m.active[""]
	}
	if state == nil {
		return
	}
	m.recordDurationLocked(time.Since(state.startedAt))
	if state.timer != nil {
		state.timer.Stop()
	}
	delete(m.active, state.toolUseID)
}

func (m *toolAssemblyMonitor) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, state := range m.active {
		m.recordDurationLocked(now.Sub(state.startedAt))
		if state.timer != nil {
			state.timer.Stop()
		}
		delete(m.active, id)
	}
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

func (m *toolAssemblyMonitor) fire(toolUseID string, generation uint64) {
	m.mu.Lock()
	state := m.active[toolUseID]
	if state == nil || state.generation != generation || m.timedOut != nil {
		m.mu.Unlock()
		return
	}
	idleFor := time.Since(state.lastActivityAt)
	if idleFor < 0 {
		idleFor = 0
	}
	if idleFor < m.timeout {
		m.armLocked(state)
		m.mu.Unlock()
		return
	}
	snapshot := toolAssemblySnapshot{
		ToolUseID:     state.toolUseID,
		Name:          state.name,
		ArgumentBytes: state.bytes,
		FragmentCount: state.fragments,
		Elapsed:       time.Since(state.startedAt),
	}
	m.recordDurationLocked(snapshot.Elapsed)
	m.timedOut = &snapshot
	for id, active := range m.active {
		if active.timer != nil {
			active.timer.Stop()
		}
		delete(m.active, id)
	}
	onTimeout := m.onTimeout
	m.mu.Unlock()
	if onTimeout != nil {
		onTimeout(snapshot)
	}
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
