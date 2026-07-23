package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	defaultRequestLogLimit      = config.DefaultRequestLogMaxEntries
	requestLogStateVersion      = 1
	requestLogSaveDelay         = 2 * time.Second
	maxPersistedRequestLogBytes = 64 << 20
	requestLogPersistenceFile   = "request_log.json"
)

type requestLogEntry struct {
	ID                       uint64   `json:"id"`
	Timestamp                int64    `json:"timestamp"`
	RequestID                string   `json:"requestId,omitempty"`
	APIKeyID                 string   `json:"apiKeyId,omitempty"`
	APIKeyName               string   `json:"apiKeyName,omitempty"`
	Protocol                 string   `json:"protocol"`
	Model                    string   `json:"model"`
	AccountID                string   `json:"accountId,omitempty"`
	AccountEmail             string   `json:"accountEmail,omitempty"`
	Endpoint                 string   `json:"endpoint,omitempty"`
	AccountSelectionMs       int64    `json:"accountSelectionMs,omitempty"`
	AccountAttempts          int      `json:"accountAttempts,omitempty"`
	RouteAffinityHit         bool     `json:"routeAffinityHit,omitempty"`
	Status                   string   `json:"status"`
	StatusCode               int      `json:"statusCode"`
	UpstreamFirstActivityMs  *int64   `json:"upstreamFirstActivityMs,omitempty"`
	FirstSSEEventMs          *int64   `json:"firstSseEventMs,omitempty"`
	FirstThinkingMs          *int64   `json:"firstThinkingMs,omitempty"`
	FirstVisibleTextMs       *int64   `json:"firstVisibleTextMs,omitempty"`
	FirstToolOutputMs        *int64   `json:"firstToolOutputMs,omitempty"`
	FirstContentMs           *int64   `json:"firstContentMs,omitempty"`
	MaxStreamGapMs           *int64   `json:"maxStreamGapMs,omitempty"`
	HeartbeatCount           int      `json:"heartbeatCount,omitempty"`
	ToolAssemblyMs           *int64   `json:"toolAssemblyMs,omitempty"`
	DurationMs               int64    `json:"durationMs"`
	InputTokens              int      `json:"inputTokens,omitempty"`
	OutputTokens             int      `json:"outputTokens,omitempty"`
	ThinkingTokens           int      `json:"thinkingTokens,omitempty"`
	CacheReadInputTokens     int      `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int      `json:"cacheCreationInputTokens,omitempty"`
	CacheStatus              string   `json:"cacheStatus,omitempty"`
	CacheMissReason          string   `json:"cacheMissReason,omitempty"`
	CacheSource              string   `json:"cacheSource,omitempty"`
	CacheMatchedInputTokens  int      `json:"cacheMatchedInputTokens,omitempty"`
	CacheEligibleInputTokens int      `json:"cacheEligibleInputTokens,omitempty"`
	CacheReadEfficiency      float64  `json:"cacheReadEfficiency,omitempty"`
	WebSearchRequests        int      `json:"webSearchRequests,omitempty"`
	VisibleOutputChars       int      `json:"visibleOutputChars,omitempty"`
	ThinkingOutputChars      int      `json:"thinkingOutputChars,omitempty"`
	RequestToolCount         int      `json:"requestToolCount,omitempty"`
	RequestToolNames         []string `json:"requestToolNames,omitempty"`
	ToolUseRequired          bool     `json:"toolUseRequired,omitempty"`
	ToolUsePolicy            string   `json:"toolUsePolicy,omitempty"`
	ToolUseCount             int      `json:"toolUseCount,omitempty"`
	ToolArgumentBytes        int      `json:"toolArgumentBytes,omitempty"`
	ToolFragmentCount        int      `json:"toolFragmentCount,omitempty"`
	ToolTruncationCount      int      `json:"toolTruncationCount,omitempty"`
	ToolRecoveryAttempts     int      `json:"toolRecoveryAttempts,omitempty"`
	StopReason               string   `json:"stopReason,omitempty"`
	Credits                  float64  `json:"credits,omitempty"`
	Error                    string   `json:"error,omitempty"`
	DetailAvailable          bool     `json:"detailAvailable,omitempty"`
}

type requestLog struct {
	mu        sync.RWMutex
	nextID    atomic.Uint64
	limit     int
	entries   []requestLogEntry
	path      string
	persistMu sync.Mutex
	writeMu   sync.Mutex
	saveTimer *time.Timer
}

type persistedRequestLog struct {
	Version int               `json:"version"`
	SavedAt int64             `json:"savedAt"`
	Entries []requestLogEntry `json:"entries"`
}

func newRequestLog(limit int) *requestLog {
	if limit <= 0 {
		limit = defaultRequestLogLimit
	}
	return &requestLog{limit: limit, entries: make([]requestLogEntry, 0, limit)}
}

func (l *requestLog) configure(limit int) {
	if l == nil {
		return
	}
	if limit <= 0 {
		limit = defaultRequestLogLimit
	}
	l.mu.Lock()
	l.limit = limit
	if overflow := len(l.entries) - limit; overflow > 0 {
		l.entries = append([]requestLogEntry(nil), l.entries[overflow:]...)
	}
	l.mu.Unlock()
	l.scheduleSave()
}

func requestLogPath() string {
	return filepath.Join(config.GetConfigDir(), requestLogPersistenceFile)
}

func newPersistentRequestLog(limit int, path string) (*requestLog, error) {
	log := newRequestLog(limit)
	log.path = strings.TrimSpace(path)
	if log.path == "" {
		return log, nil
	}
	return log, log.loadFrom(log.path)
}

func (l *requestLog) loadFrom(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open request log: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxPersistedRequestLogBytes+1))
	if err != nil {
		return fmt.Errorf("read request log: %w", err)
	}
	if len(data) > maxPersistedRequestLogBytes {
		return fmt.Errorf("request log exceeds %d bytes", maxPersistedRequestLogBytes)
	}
	var state persistedRequestLog
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode request log: %w", err)
	}
	if state.Version != requestLogStateVersion {
		return fmt.Errorf("unsupported request log version %d", state.Version)
	}
	entries := state.Entries
	if len(entries) > l.limit {
		entries = entries[len(entries)-l.limit:]
	}
	entries = append([]requestLogEntry(nil), entries...)
	var maxID uint64
	for i := range entries {
		if entries[i].ID == 0 {
			maxID++
			entries[i].ID = maxID
		} else if entries[i].ID > maxID {
			maxID = entries[i].ID
		}
		if entries[i].RequestToolNames != nil {
			entries[i].RequestToolNames = append([]string(nil), entries[i].RequestToolNames...)
		}
	}
	l.mu.Lock()
	l.entries = entries
	l.mu.Unlock()
	l.nextID.Store(maxID)
	return nil
}

func (l *requestLog) add(entry requestLogEntry) {
	if l == nil {
		return
	}
	entry.ID = l.nextID.Add(1)
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().Unix()
	}
	if entry.RequestToolNames != nil {
		entry.RequestToolNames = append([]string(nil), entry.RequestToolNames...)
	}

	l.mu.Lock()
	l.entries = append(l.entries, entry)
	if overflow := len(l.entries) - l.limit; overflow > 0 {
		copy(l.entries, l.entries[overflow:])
		l.entries = l.entries[:l.limit]
	}
	l.mu.Unlock()
	l.scheduleSave()
}

func (l *requestLog) scheduleSave() {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return
	}
	l.persistMu.Lock()
	if l.saveTimer != nil {
		l.persistMu.Unlock()
		return
	}
	path := l.path
	l.saveTimer = time.AfterFunc(requestLogSaveDelay, func() {
		l.persistMu.Lock()
		l.saveTimer = nil
		l.persistMu.Unlock()
		if err := l.saveTo(path); err != nil {
			logger.Warnf("[RequestLog] Failed to persist request log: %v", err)
		}
	})
	l.persistMu.Unlock()
}

func (l *requestLog) Flush() error {
	if l == nil || strings.TrimSpace(l.path) == "" {
		return nil
	}
	l.persistMu.Lock()
	if l.saveTimer != nil {
		l.saveTimer.Stop()
		l.saveTimer = nil
	}
	path := l.path
	l.persistMu.Unlock()
	return l.saveTo(path)
}

func (l *requestLog) saveTo(path string) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	l.mu.RLock()
	entries := append([]requestLogEntry(nil), l.entries...)
	for i := range entries {
		if entries[i].RequestToolNames != nil {
			entries[i].RequestToolNames = append([]string(nil), entries[i].RequestToolNames...)
		}
	}
	l.mu.RUnlock()

	data, err := json.MarshalIndent(persistedRequestLog{
		Version: requestLogStateVersion,
		SavedAt: time.Now().Unix(),
		Entries: entries,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode request log: %w", err)
	}
	if len(data) > maxPersistedRequestLogBytes {
		return fmt.Errorf("encoded request log exceeds %d bytes", maxPersistedRequestLogBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create request log directory: %w", err)
	}
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create request log temp file: %w", err)
	}
	removeTemp := true
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure request log temp file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write request log: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync request log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close request log: %w", err)
	}
	file = nil
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit request log: %w", err)
	}
	removeTemp = false
	return nil
}

func (l *requestLog) list(limit int) []requestLogEntry {
	if l == nil {
		return []requestLogEntry{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]requestLogEntry, 0, limit)
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

func (l *requestLog) listForAPIKey(apiKeyID string, limit int) []requestLogEntry {
	apiKeyID = strings.TrimSpace(apiKeyID)
	if l == nil || apiKeyID == "" || limit <= 0 {
		return []requestLogEntry{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]requestLogEntry, 0, limit)
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		if l.entries[i].APIKeyID != apiKeyID {
			continue
		}
		entry := l.entries[i]
		if entry.RequestToolNames != nil {
			entry.RequestToolNames = append([]string(nil), entry.RequestToolNames...)
		}
		out = append(out, entry)
	}
	return out
}

func (h *Handler) recordRequestLog(entry requestLogEntry) {
	if h == nil {
		return
	}
	if h.requestLog == nil {
		h.requestLog = newRequestLog(config.GetRequestLogConfig().MaxEntries)
	}
	h.requestLog.add(entry)
}

func (h *Handler) recordCanceledRequestForPayload(payload *KiroPayload, protocol, model string, startedAt time.Time, firstContentMs *int64, err error) {
	if h == nil || err == nil {
		return
	}
	canceled := classifyRequestCancellation("", err)
	mapped := mapDownstreamError(canceled)
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:      time.Now().Unix(),
		Protocol:       protocol,
		Model:          model,
		Status:         "canceled",
		StatusCode:     mapped.Status,
		FirstContentMs: firstContentMs,
		DurationMs:     requestDurationMs(startedAt),
		Error:          canceled.Error(),
	})
}

func (h *Handler) recordRequestLogForPayload(payload *KiroPayload, entry requestLogEntry) {
	if payload != nil {
		entry.RequestID = requestIDFromContext(payload.requestContext)
		if entry.Endpoint == "" {
			entry.Endpoint = payload.successfulEndpoint()
		}
		entry.APIKeyID = apiKeyIDFromContext(payload.requestContext)
		if apiKey := config.GetApiKeyEntry(entry.APIKeyID); apiKey != nil {
			entry.APIKeyName = apiKey.Name
		}
		context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if context != nil && len(context.Tools) > 0 {
			entry.RequestToolCount = len(context.Tools)
			entry.RequestToolNames = make([]string, 0, len(context.Tools))
			for _, tool := range context.Tools {
				name := tool.ToolSpecification.Name
				if original, ok := payload.ToolNameMap[name]; ok {
					name = original
				}
				entry.RequestToolNames = append(entry.RequestToolNames, name)
			}
		}
		entry.ToolUseRequired = payload.requireToolUse
		entry.ToolUsePolicy = payload.toolUsePolicy
		upstreamActivityMs, toolAssemblyMs, toolArgumentBytes, toolFragmentCount, toolTruncationCount, toolRecoveryAttempts := payload.streamMetrics()
		if entry.UpstreamFirstActivityMs == nil {
			entry.UpstreamFirstActivityMs = upstreamActivityMs
		}
		if entry.ToolAssemblyMs == nil {
			entry.ToolAssemblyMs = toolAssemblyMs
		}
		entry.ToolArgumentBytes = toolArgumentBytes
		entry.ToolFragmentCount = toolFragmentCount
		entry.ToolTruncationCount = toolTruncationCount
		entry.ToolRecoveryAttempts = toolRecoveryAttempts
		entry.AccountSelectionMs, entry.AccountAttempts, entry.RouteAffinityHit = payload.accountSelectionMetrics()
		payload.requestTimingTracker().Apply(&entry)
		payload.applyPromptCacheDiagnostic(&entry)
		entry.DetailAvailable = h.recordRequestDetailForContext(payload.requestContext, entry)
	}
	h.recordRequestLog(entry)
}

func (h *Handler) recordRequestLogForContext(ctx context.Context, entry requestLogEntry) {
	entry.RequestID = requestIDFromContext(ctx)
	entry.APIKeyID = apiKeyIDFromContext(ctx)
	if apiKey := config.GetApiKeyEntry(entry.APIKeyID); apiKey != nil {
		entry.APIKeyName = apiKey.Name
	}
	entry.DetailAvailable = h.recordRequestDetailForContext(ctx, entry)
	h.recordRequestLog(entry)
}

func requestDurationMs(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

type requestFirstContentTimer struct {
	startedAt          time.Time
	firstContentMs     atomic.Int64
	firstSSEEventMs    atomic.Int64
	firstThinkingMs    atomic.Int64
	firstVisibleTextMs atomic.Int64
	firstToolOutputMs  atomic.Int64
	lastSSEEventNanos  atomic.Int64
	maxStreamGapMs     atomic.Int64
	heartbeatCount     atomic.Int64
}

func newRequestFirstContentTimer(startedAt time.Time) *requestFirstContentTimer {
	timer := &requestFirstContentTimer{startedAt: startedAt}
	timer.firstContentMs.Store(-1)
	timer.firstSSEEventMs.Store(-1)
	timer.firstThinkingMs.Store(-1)
	timer.firstVisibleTextMs.Store(-1)
	timer.firstToolOutputMs.Store(-1)
	timer.lastSSEEventNanos.Store(-1)
	timer.maxStreamGapMs.Store(-1)
	return timer
}

func (t *requestFirstContentTimer) MarkText(text string) {
	t.MarkVisibleText(text)
}

func (t *requestFirstContentTimer) MarkOutput(text string, isThinking bool) {
	if isThinking {
		t.MarkThinking(text)
		return
	}
	t.MarkVisibleText(text)
}

func (t *requestFirstContentTimer) MarkThinking(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	t.markFirst(&t.firstThinkingMs)
	t.markFirst(&t.firstContentMs)
}

func (t *requestFirstContentTimer) MarkVisibleText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	t.markFirst(&t.firstVisibleTextMs)
	t.markFirst(&t.firstContentMs)
}

func (t *requestFirstContentTimer) Mark() {
	t.MarkToolOutput()
}

func (t *requestFirstContentTimer) MarkToolOutput() {
	if t == nil {
		return
	}
	t.markFirst(&t.firstToolOutputMs)
	t.markFirst(&t.firstContentMs)
}

func (t *requestFirstContentTimer) MarkSSEEvent(heartbeat bool) {
	if t == nil {
		return
	}
	now := time.Now()
	elapsed := now.Sub(t.startedAt).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	t.firstSSEEventMs.CompareAndSwap(-1, elapsed)
	if heartbeat {
		t.heartbeatCount.Add(1)
	}
	elapsedNanos := now.Sub(t.startedAt).Nanoseconds()
	previous := t.lastSSEEventNanos.Swap(elapsedNanos)
	if previous >= 0 {
		gapMs := (elapsedNanos - previous) / int64(time.Millisecond)
		if gapMs < 0 {
			gapMs = 0
		}
		for {
			current := t.maxStreamGapMs.Load()
			if gapMs <= current || t.maxStreamGapMs.CompareAndSwap(current, gapMs) {
				break
			}
		}
	}
}

func (t *requestFirstContentTimer) markFirst(target *atomic.Int64) {
	if t == nil || target == nil {
		return
	}
	elapsed := time.Since(t.startedAt).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	target.CompareAndSwap(-1, elapsed)
}

func (t *requestFirstContentTimer) Value() *int64 {
	if t == nil {
		return nil
	}
	elapsed := t.firstContentMs.Load()
	if elapsed < 0 {
		return nil
	}
	return &elapsed
}

func (t *requestFirstContentTimer) Apply(entry *requestLogEntry) {
	if t == nil || entry == nil {
		return
	}
	setRequestTimingValue(&entry.FirstContentMs, t.firstContentMs.Load())
	setRequestTimingValue(&entry.FirstSSEEventMs, t.firstSSEEventMs.Load())
	setRequestTimingValue(&entry.FirstThinkingMs, t.firstThinkingMs.Load())
	setRequestTimingValue(&entry.FirstVisibleTextMs, t.firstVisibleTextMs.Load())
	setRequestTimingValue(&entry.FirstToolOutputMs, t.firstToolOutputMs.Load())
	setRequestTimingValue(&entry.MaxStreamGapMs, t.maxStreamGapMs.Load())
	if entry.HeartbeatCount == 0 {
		entry.HeartbeatCount = int(t.heartbeatCount.Load())
	}
}

func setRequestTimingValue(target **int64, elapsed int64) {
	if target == nil || *target != nil || elapsed < 0 {
		return
	}
	value := elapsed
	*target = &value
}

func outputCharCount(text string) int {
	return utf8.RuneCountInString(text)
}
