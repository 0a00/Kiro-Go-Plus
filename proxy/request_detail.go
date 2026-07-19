package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	requestDetailStateVersion    = 1
	requestDetailSaveDelay       = 2 * time.Second
	requestDetailPersistenceFile = "request_details.json"
	maxRequestDetailStoreBytes   = 64 << 20
	requestDetailStoreReserve    = 64 << 10
	maxRequestDetailAttempts     = 256
	maxRequestDetailTools        = 128
)

type requestDetailContextKey struct{}

type requestDetailStatusWriter struct {
	http.ResponseWriter
	statusCode      int
	responsePreview []byte
}

type requestDetailFlushingWriter struct {
	*requestDetailStatusWriter
	flusher http.Flusher
}

func (w *requestDetailStatusWriter) WriteHeader(statusCode int) {
	if w.statusCode == 0 {
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *requestDetailStatusWriter) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	if len(w.responsePreview) < 4096 {
		remaining := 4096 - len(w.responsePreview)
		if remaining > len(data) {
			remaining = len(data)
		}
		w.responsePreview = append(w.responsePreview, data[:remaining]...)
	}
	return w.ResponseWriter.Write(data)
}

func (w *requestDetailStatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestDetailFlushingWriter) Flush() {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	w.flusher.Flush()
}

type requestDetail struct {
	Version            int                    `json:"version"`
	RequestID          string                 `json:"requestId"`
	Timestamp          int64                  `json:"timestamp"`
	Protocol           string                 `json:"protocol"`
	Model              string                 `json:"model,omitempty"`
	APIKeyID           string                 `json:"apiKeyId,omitempty"`
	APIKeyName         string                 `json:"apiKeyName,omitempty"`
	AccountID          string                 `json:"accountId,omitempty"`
	AccountEmail       string                 `json:"accountEmail,omitempty"`
	Endpoint           string                 `json:"endpoint,omitempty"`
	AccountSelectionMs int64                  `json:"accountSelectionMs,omitempty"`
	AccountAttempts    int                    `json:"accountAttempts,omitempty"`
	RouteAffinityHit   bool                   `json:"routeAffinityHit,omitempty"`
	Status             string                 `json:"status"`
	StatusCode         int                    `json:"statusCode"`
	DurationMs         int64                  `json:"durationMs"`
	Request            requestDetailRequest   `json:"request"`
	Response           requestDetailResponse  `json:"response"`
	Attempts           []requestDetailAttempt `json:"attempts,omitempty"`
	Timeline           []requestDetailEvent   `json:"timeline,omitempty"`
	DroppedEvents      int                    `json:"droppedEvents,omitempty"`
	TruncatedFields    []string               `json:"truncatedFields,omitempty"`
}

type requestDetailRequest struct {
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Headers       map[string]string `json:"headers,omitempty"`
	BodyJSON      string            `json:"bodyJson,omitempty"`
	BodyBytes     int               `json:"bodyBytes"`
	BodySHA256    string            `json:"bodySha256"`
	BodyTruncated bool              `json:"bodyTruncated,omitempty"`
}

type requestDetailResponse struct {
	VisibleOutput            string                 `json:"visibleOutput,omitempty"`
	VisibleOutputBytes       int                    `json:"visibleOutputBytes,omitempty"`
	VisibleOutputTruncated   bool                   `json:"visibleOutputTruncated,omitempty"`
	ThinkingOutput           string                 `json:"thinkingOutput,omitempty"`
	ThinkingOutputBytes      int                    `json:"thinkingOutputBytes,omitempty"`
	ThinkingOutputTruncated  bool                   `json:"thinkingOutputTruncated,omitempty"`
	Tools                    []requestDetailToolUse `json:"tools,omitempty"`
	InputTokens              int                    `json:"inputTokens,omitempty"`
	OutputTokens             int                    `json:"outputTokens,omitempty"`
	ThinkingTokens           int                    `json:"thinkingTokens,omitempty"`
	UncachedInputTokens      int                    `json:"uncachedInputTokens,omitempty"`
	CacheReadInputTokens     int                    `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int                    `json:"cacheCreationInputTokens,omitempty"`
	CacheCreation5mTokens    int                    `json:"cacheCreation5mTokens,omitempty"`
	CacheCreation1hTokens    int                    `json:"cacheCreation1hTokens,omitempty"`
	HasCacheBreakdown        bool                   `json:"hasCacheBreakdown,omitempty"`
	StopReason               string                 `json:"stopReason,omitempty"`
	TruncationReason         string                 `json:"truncationReason,omitempty"`
	Error                    string                 `json:"error,omitempty"`
	Credits                  float64                `json:"credits,omitempty"`
	ContextUsagePercentage   *float64               `json:"contextUsagePercentage,omitempty"`
}

type requestDetailAttempt struct {
	Sequence             int    `json:"sequence"`
	AccountID            string `json:"accountId,omitempty"`
	AccountEmail         string `json:"accountEmail,omitempty"`
	Endpoint             string `json:"endpoint"`
	Host                 string `json:"host,omitempty"`
	StartedMs            int64  `json:"startedMs"`
	DurationMs           int64  `json:"durationMs"`
	Status               string `json:"status"`
	StatusCode           int    `json:"statusCode,omitempty"`
	Error                string `json:"error,omitempty"`
	RetryReason          string `json:"retryReason,omitempty"`
	RetryAcrossEndpoints bool   `json:"retryAcrossEndpoints,omitempty"`
	RetryAcrossAccounts  bool   `json:"retryAcrossAccounts,omitempty"`
}

type requestDetailEvent struct {
	Sequence  int    `json:"sequence"`
	Type      string `json:"type"`
	ElapsedMs int64  `json:"elapsedMs"`
	IdleGapMs int64  `json:"idleGapMs"`
	Bytes     int    `json:"bytes,omitempty"`
}

type requestDetailToolUse struct {
	ToolUseID       string `json:"toolUseId,omitempty"`
	Name            string `json:"name,omitempty"`
	ArgumentBytes   int    `json:"argumentBytes,omitempty"`
	ArgumentSHA256  string `json:"argumentSha256,omitempty"`
	FragmentCount   int    `json:"fragmentCount,omitempty"`
	FirstFragmentMs *int64 `json:"firstFragmentMs,omitempty"`
	LastFragmentMs  *int64 `json:"lastFragmentMs,omitempty"`
	Completed       bool   `json:"completed,omitempty"`
}

type boundedDetailText struct {
	data      []byte
	total     int
	limit     int
	truncated bool
}

func (c *boundedDetailText) append(value string) {
	c.total += len(value)
	if value == "" || c.limit <= len(c.data) {
		if value != "" {
			c.truncated = true
		}
		return
	}
	remaining := c.limit - len(c.data)
	chunk := truncateUTF8Bytes(value, remaining)
	c.data = append(c.data, chunk...)
	if len(chunk) < len(value) {
		c.truncated = true
	}
}

func (c *boundedDetailText) string() string {
	return string(c.data)
}

type requestDetailToolState struct {
	detail   requestDetailToolUse
	hasher   hash.Hash
	sawDelta bool
}

type requestDetailTrace struct {
	mu             sync.Mutex
	startedAt      time.Time
	requestID      string
	protocol       string
	request        requestDetailRequest
	maxBytes       int
	maxEvents      int
	visible        boundedDetailText
	thinking       boundedDetailText
	tools          map[string]*requestDetailToolState
	toolOrder      []string
	attempts       []requestDetailAttempt
	timeline       []requestDetailEvent
	droppedEvents  int
	lastEventAt    time.Time
	lastProgressAt time.Time
	usage          KiroTokenUsage
	stopReason     string
	truncation     string
	err            string
	credits        float64
	contextUsage   *float64
	finalized      bool
}

type storedRequestDetail struct {
	requestID string
	timestamp int64
	data      json.RawMessage
}

type requestDetailStore struct {
	mu             sync.RWMutex
	limit          int
	maxDetailBytes int
	entries        []storedRequestDetail
	totalBytes     int
	path           string
	persistMu      sync.Mutex
	writeMu        sync.Mutex
	saveTimer      *time.Timer
}

type persistedRequestDetails struct {
	Version int               `json:"version"`
	SavedAt int64             `json:"savedAt"`
	Entries []json.RawMessage `json:"entries"`
}

func requestDetailPath() string {
	return filepath.Join(config.GetConfigDir(), requestDetailPersistenceFile)
}

func newRequestDetailStore(limit, maxDetailBytes int) *requestDetailStore {
	limit, maxDetailBytes = normalizeRequestDetailLimits(limit, maxDetailBytes)
	return &requestDetailStore{
		limit:          limit,
		maxDetailBytes: maxDetailBytes,
		entries:        make([]storedRequestDetail, 0, limit),
	}
}

func newPersistentRequestDetailStore(limit, maxDetailBytes int, path string) (*requestDetailStore, error) {
	store := newRequestDetailStore(limit, maxDetailBytes)
	store.path = strings.TrimSpace(path)
	if store.path == "" {
		return store, nil
	}
	return store, store.loadFrom(store.path)
}

func normalizeRequestDetailLimits(limit, maxDetailBytes int) (int, int) {
	if limit < config.MinRequestDetailMaxEntries {
		limit = config.DefaultRequestDetailMaxEntries
	}
	if limit > config.MaxRequestDetailMaxEntries {
		limit = config.MaxRequestDetailMaxEntries
	}
	if maxDetailBytes < config.MinRequestDetailMaxBytes {
		maxDetailBytes = config.DefaultRequestDetailMaxBytes
	}
	if maxDetailBytes > config.MaxRequestDetailMaxBytes {
		maxDetailBytes = config.MaxRequestDetailMaxBytes
	}
	return limit, maxDetailBytes
}

func (s *requestDetailStore) loadFrom(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open request details: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure request details: %w", err)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxRequestDetailStoreBytes+1))
	if err != nil {
		return fmt.Errorf("read request details: %w", err)
	}
	if len(data) > maxRequestDetailStoreBytes {
		return fmt.Errorf("request details exceed %d bytes", maxRequestDetailStoreBytes)
	}
	var state persistedRequestDetails
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode request details: %w", err)
	}
	if state.Version != requestDetailStateVersion {
		return fmt.Errorf("unsupported request detail version %d", state.Version)
	}

	entries := make([]storedRequestDetail, 0, len(state.Entries))
	totalBytes := 0
	for _, raw := range state.Entries {
		if len(raw) == 0 || len(raw) > s.maxDetailBytes {
			continue
		}
		var envelope struct {
			RequestID string `json:"requestId"`
			Timestamp int64  `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || strings.TrimSpace(envelope.RequestID) == "" {
			continue
		}
		copyRaw := append(json.RawMessage(nil), raw...)
		entries = append(entries, storedRequestDetail{requestID: envelope.RequestID, timestamp: envelope.Timestamp, data: copyRaw})
		totalBytes += len(copyRaw)
	}
	if len(entries) > s.limit {
		entries = entries[len(entries)-s.limit:]
		totalBytes = requestDetailEntriesBytes(entries)
	}
	for totalBytes > maxRequestDetailStoreBytes-requestDetailStoreReserve && len(entries) > 0 {
		totalBytes -= len(entries[0].data)
		entries = entries[1:]
	}
	s.mu.Lock()
	s.entries = append([]storedRequestDetail(nil), entries...)
	s.totalBytes = totalBytes
	s.mu.Unlock()
	return nil
}

func (s *requestDetailStore) configure(limit, maxDetailBytes int) {
	if s == nil {
		return
	}
	limit, maxDetailBytes = normalizeRequestDetailLimits(limit, maxDetailBytes)
	s.mu.Lock()
	changed := s.limit != limit || s.maxDetailBytes != maxDetailBytes
	s.limit = limit
	s.maxDetailBytes = maxDetailBytes
	originalCount := len(s.entries)
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if len(entry.data) <= maxDetailBytes {
			filtered = append(filtered, entry)
		}
	}
	s.entries = filtered
	if overflow := len(s.entries) - limit; overflow > 0 {
		s.entries = append([]storedRequestDetail(nil), s.entries[overflow:]...)
	}
	changed = changed || len(s.entries) != originalCount
	s.totalBytes = requestDetailEntriesBytes(s.entries)
	s.mu.Unlock()
	if changed {
		s.scheduleSave()
	}
}

func (s *requestDetailStore) add(detail requestDetail) bool {
	if s == nil || strings.TrimSpace(detail.RequestID) == "" {
		return false
	}
	detail = boundRequestDetail(detail, s.currentMaxDetailBytes())
	raw, err := json.Marshal(detail)
	if err != nil || len(raw) == 0 || len(raw) > s.currentMaxDetailBytes() {
		return false
	}

	entry := storedRequestDetail{
		requestID: detail.RequestID,
		timestamp: detail.Timestamp,
		data:      append(json.RawMessage(nil), raw...),
	}
	s.mu.Lock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].requestID == entry.requestID {
			s.totalBytes -= len(s.entries[i].data)
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
		}
	}
	s.entries = append(s.entries, entry)
	s.totalBytes += len(entry.data)
	for (len(s.entries) > s.limit || s.totalBytes > maxRequestDetailStoreBytes-requestDetailStoreReserve) && len(s.entries) > 0 {
		s.totalBytes -= len(s.entries[0].data)
		s.entries = s.entries[1:]
	}
	s.mu.Unlock()
	s.scheduleSave()
	return true
}

func (s *requestDetailStore) currentMaxDetailBytes() int {
	if s == nil {
		return config.DefaultRequestDetailMaxBytes
	}
	s.mu.RLock()
	value := s.maxDetailBytes
	s.mu.RUnlock()
	return value
}

func (s *requestDetailStore) has(requestID string) bool {
	if s == nil || requestID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].requestID == requestID {
			return true
		}
	}
	return false
}

func (s *requestDetailStore) get(requestID string) (json.RawMessage, bool) {
	if s == nil || requestID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].requestID == requestID {
			return append(json.RawMessage(nil), s.entries[i].data...), true
		}
	}
	return nil, false
}

func (s *requestDetailStore) clear() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	count := len(s.entries)
	s.entries = nil
	s.totalBytes = 0
	s.mu.Unlock()
	s.scheduleSave()
	return count
}

func (s *requestDetailStore) stats() (int, int) {
	if s == nil {
		return 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries), s.totalBytes
}

func (s *requestDetailStore) scheduleSave() {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return
	}
	s.persistMu.Lock()
	if s.saveTimer != nil {
		s.persistMu.Unlock()
		return
	}
	path := s.path
	s.saveTimer = time.AfterFunc(requestDetailSaveDelay, func() {
		s.persistMu.Lock()
		s.saveTimer = nil
		s.persistMu.Unlock()
		if err := s.saveTo(path); err != nil {
			logger.Warnf("[RequestDetail] Failed to persist request details: %v", err)
		}
	})
	s.persistMu.Unlock()
}

func (s *requestDetailStore) Flush() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil
	}
	s.persistMu.Lock()
	if s.saveTimer != nil {
		s.saveTimer.Stop()
		s.saveTimer = nil
	}
	path := s.path
	s.persistMu.Unlock()
	return s.saveTo(path)
}

func (s *requestDetailStore) saveTo(path string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	entries := make([]json.RawMessage, len(s.entries))
	for i := range s.entries {
		entries[i] = append(json.RawMessage(nil), s.entries[i].data...)
	}
	s.mu.RUnlock()

	data, err := json.Marshal(persistedRequestDetails{
		Version: requestDetailStateVersion,
		SavedAt: time.Now().Unix(),
		Entries: entries,
	})
	if err != nil {
		return fmt.Errorf("encode request details: %w", err)
	}
	if len(data) > maxRequestDetailStoreBytes {
		return fmt.Errorf("encoded request details exceed %d bytes", maxRequestDetailStoreBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create request detail directory: %w", err)
	}
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create request detail temp file: %w", err)
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
		return fmt.Errorf("secure request detail temp file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write request details: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync request details: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close request details: %w", err)
	}
	file = nil
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit request details: %w", err)
	}
	removeTemp = false
	return nil
}

func requestDetailEntriesBytes(entries []storedRequestDetail) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.data)
	}
	return total
}

func (h *Handler) ensureRequestDetailStore() *requestDetailStore {
	if h == nil {
		return nil
	}
	cfg := config.GetRequestLogConfig()
	h.requestDetailsMu.Lock()
	defer h.requestDetailsMu.Unlock()
	if h.requestDetails == nil {
		h.requestDetails = newRequestDetailStore(cfg.DetailedMaxEntries, cfg.MaxDetailBytes)
	} else {
		h.requestDetails.configure(cfg.DetailedMaxEntries, cfg.MaxDetailBytes)
	}
	return h.requestDetails
}

func (h *Handler) attachRequestDetailTrace(r *http.Request, protocol string, body []byte) *http.Request {
	if r == nil {
		return r
	}
	cfg := config.GetRequestLogConfig()
	if !cfg.DetailedLogEnabled {
		return r
	}
	trace := newRequestDetailTrace(r, protocol, body, cfg.MaxDetailBytes)
	return r.WithContext(context.WithValue(r.Context(), requestDetailContextKey{}, trace))
}

func requestDetailTraceFromContext(ctx context.Context) *requestDetailTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(requestDetailContextKey{}).(*requestDetailTrace)
	return trace
}

func wrapRequestDetailResponseWriter(w http.ResponseWriter, ctx context.Context) (http.ResponseWriter, *requestDetailStatusWriter) {
	if requestDetailTraceFromContext(ctx) == nil {
		return w, nil
	}
	tracker := &requestDetailStatusWriter{ResponseWriter: w}
	if flusher, ok := w.(http.Flusher); ok {
		return &requestDetailFlushingWriter{requestDetailStatusWriter: tracker, flusher: flusher}, tracker
	}
	return tracker, tracker
}

func (h *Handler) finalizeUnrecordedRequestDetail(ctx context.Context, tracker *requestDetailStatusWriter, startedAt time.Time, protocol, model string) {
	trace := requestDetailTraceFromContext(ctx)
	if trace == nil || tracker == nil || trace.isFinalized() {
		return
	}
	statusCode := tracker.statusCode
	if statusCode == 0 {
		if ctx != nil && ctx.Err() != nil {
			statusCode = 499
		} else {
			statusCode = http.StatusInternalServerError
		}
	}
	status := "failed"
	if statusCode >= 200 && statusCode < 400 {
		status = "success"
	} else if statusCode >= 400 && statusCode < 500 {
		status = "rejected"
	}
	h.recordRequestLogForContext(ctx, requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   protocol,
		Model:      model,
		Status:     status,
		StatusCode: statusCode,
		DurationMs: requestDurationMs(startedAt),
		Error:      requestDetailResponseError(statusCode, tracker.responsePreview),
	})
}

func requestDetailResponseError(statusCode int, preview []byte) string {
	if statusCode < 400 || len(preview) == 0 {
		return ""
	}
	var decoded struct {
		Error interface{} `json:"error"`
	}
	if json.Unmarshal(preview, &decoded) == nil {
		switch value := decoded.Error.(type) {
		case string:
			return truncateDiagnosticText(redactRequestDetailText(value), 2000)
		case map[string]interface{}:
			if message, _ := value["message"].(string); message != "" {
				return truncateDiagnosticText(redactRequestDetailText(message), 2000)
			}
		}
	}
	return truncateDiagnosticText(redactRequestDetailText(string(preview)), 2000)
}

func newRequestDetailTrace(r *http.Request, protocol string, body []byte, maxBytes int) *requestDetailTrace {
	if maxBytes < config.MinRequestDetailMaxBytes {
		maxBytes = config.DefaultRequestDetailMaxBytes
	}
	requestBodyLimit := maxBytes * 45 / 100
	outputLimit := maxBytes * 15 / 100
	maxEvents := maxBytes / 1024
	if maxEvents < 16 {
		maxEvents = 16
	}
	if maxEvents > 512 {
		maxEvents = 512
	}
	bodyJSON, bodyTruncated := sanitizeRequestDetailBody(body, requestBodyLimit)
	hashSum := sha256.Sum256(body)
	now := time.Now()
	return &requestDetailTrace{
		startedAt: now,
		requestID: requestIDFromContext(r.Context()),
		protocol:  protocol,
		request: requestDetailRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			Headers:       allowlistedRequestDetailHeaders(r.Header),
			BodyJSON:      bodyJSON,
			BodyBytes:     len(body),
			BodySHA256:    hex.EncodeToString(hashSum[:]),
			BodyTruncated: bodyTruncated,
		},
		maxBytes:  maxBytes,
		maxEvents: maxEvents,
		visible:   boundedDetailText{limit: outputLimit},
		thinking:  boundedDetailText{limit: outputLimit},
		tools:     make(map[string]*requestDetailToolState),
	}
}

func (t *requestDetailTrace) recordText(text string, thinking bool) {
	if t == nil {
		return
	}
	redacted := redactRequestDetailText(text)
	t.mu.Lock()
	if thinking {
		t.thinking.append(redacted)
		t.recordEventLocked("thinking", len(text))
	} else {
		t.visible.append(redacted)
		t.recordEventLocked("text", len(text))
	}
	t.mu.Unlock()
}

func (t *requestDetailTrace) isFinalized() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	finalized := t.finalized
	t.mu.Unlock()
	return finalized
}

func (t *requestDetailTrace) recordProgress() {
	if t == nil {
		return
	}
	t.mu.Lock()
	now := time.Now()
	if !t.lastProgressAt.IsZero() && now.Sub(t.lastProgressAt) < 250*time.Millisecond {
		t.mu.Unlock()
		return
	}
	t.lastProgressAt = now
	t.recordEventLocked("progress", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordToolUseStart(toolUseID, name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	state := t.toolStateLocked(toolUseID, name)
	state.detail.Name = redactRequestDetailText(name)
	t.recordEventLocked("tool_start", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordToolUseDelta(toolUseID, input string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	state := t.toolStateLocked(toolUseID, "")
	nowMs := t.elapsedMsLocked(time.Now())
	if state.detail.FirstFragmentMs == nil {
		value := nowMs
		state.detail.FirstFragmentMs = &value
	}
	value := nowMs
	state.detail.LastFragmentMs = &value
	state.sawDelta = true
	state.detail.ArgumentBytes += len(input)
	state.detail.FragmentCount++
	_, _ = state.hasher.Write([]byte(input))
	t.recordEventLocked("tool_delta", len(input))
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordToolUseStop(toolUseID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	state := t.toolStateLocked(toolUseID, "")
	state.detail.Completed = true
	t.recordEventLocked("tool_stop", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordToolUse(toolUse KiroToolUse) {
	if t == nil {
		return
	}
	t.mu.Lock()
	state := t.toolStateLocked(toolUse.ToolUseID, toolUse.Name)
	if toolUse.Name != "" {
		state.detail.Name = redactRequestDetailText(toolUse.Name)
	}
	if !state.sawDelta {
		if raw, err := json.Marshal(toolUse.Input); err == nil {
			state.detail.ArgumentBytes = len(raw)
			state.detail.FragmentCount = 1
			_, _ = state.hasher.Write(raw)
		}
	}
	state.detail.Completed = true
	t.recordEventLocked("tool_use", state.detail.ArgumentBytes)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordUsage(usage KiroTokenUsage) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.usage = usage
	t.recordEventLocked("usage", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordComplete(inputTokens, outputTokens int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if inputTokens > 0 {
		t.usage.InputTokens = inputTokens
	}
	if outputTokens > 0 {
		t.usage.OutputTokens = outputTokens
	}
	t.recordEventLocked("complete", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordTruncated(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.truncation = truncateDiagnosticText(redactRequestDetailText(reason), 1000)
	t.recordEventLocked("truncated", len(reason))
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordError(err error) {
	if t == nil || err == nil {
		return
	}
	t.mu.Lock()
	t.err = truncateDiagnosticText(redactRequestDetailText(diagnosticErrorMessage(err)), 4000)
	t.recordEventLocked("error", len(err.Error()))
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordCredits(credits float64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.credits = credits
	t.recordEventLocked("credits", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordContextUsage(percentage float64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	value := percentage
	t.contextUsage = &value
	t.recordEventLocked("context_usage", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) recordAttempt(accountID, accountEmail, endpoint, host string, startedAt time.Time, statusCode int, status string, err error, retryReason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if len(t.attempts) >= maxRequestDetailAttempts {
		t.droppedEvents++
		t.mu.Unlock()
		return
	}
	attempt := requestDetailAttempt{
		Sequence:     len(t.attempts) + 1,
		AccountID:    accountID,
		AccountEmail: redactRequestDetailText(accountEmail),
		Endpoint:     endpoint,
		Host:         host,
		StartedMs:    t.elapsedMsLocked(startedAt),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       status,
		StatusCode:   statusCode,
		RetryReason:  retryReason,
	}
	if attempt.DurationMs < 0 {
		attempt.DurationMs = 0
	}
	if err != nil {
		attempt.Error = truncateDiagnosticText(redactRequestDetailText(diagnosticErrorMessage(err)), 2000)
		attempt.RetryAcrossEndpoints = shouldRetryAcrossEndpoints(err)
		attempt.RetryAcrossAccounts = shouldRetryAcrossAccounts(err)
	}
	t.attempts = append(t.attempts, attempt)
	t.recordEventLocked("upstream_attempt", 0)
	t.mu.Unlock()
}

func (t *requestDetailTrace) toolStateLocked(toolUseID, name string) *requestDetailToolState {
	key := strings.TrimSpace(toolUseID)
	if key == "" {
		key = "tool_" + fmt.Sprint(len(t.toolOrder)+1)
	}
	if state := t.tools[key]; state != nil {
		if state.detail.Name == "" && name != "" {
			state.detail.Name = name
		}
		return state
	}
	if len(t.toolOrder) >= maxRequestDetailTools {
		key = t.toolOrder[len(t.toolOrder)-1]
		return t.tools[key]
	}
	state := &requestDetailToolState{
		detail: requestDetailToolUse{ToolUseID: toolUseID, Name: name},
		hasher: sha256.New(),
	}
	t.tools[key] = state
	t.toolOrder = append(t.toolOrder, key)
	return state
}

func (t *requestDetailTrace) recordEventLocked(kind string, size int) {
	now := time.Now()
	if len(t.timeline) >= t.maxEvents {
		t.droppedEvents++
		t.lastEventAt = now
		return
	}
	idleGap := now.Sub(t.startedAt).Milliseconds()
	if !t.lastEventAt.IsZero() {
		idleGap = now.Sub(t.lastEventAt).Milliseconds()
	}
	if idleGap < 0 {
		idleGap = 0
	}
	t.timeline = append(t.timeline, requestDetailEvent{
		Sequence:  len(t.timeline) + 1,
		Type:      kind,
		ElapsedMs: t.elapsedMsLocked(now),
		IdleGapMs: idleGap,
		Bytes:     size,
	})
	t.lastEventAt = now
}

func (t *requestDetailTrace) elapsedMsLocked(at time.Time) int64 {
	value := at.Sub(t.startedAt).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func (t *requestDetailTrace) finalize(entry requestLogEntry) (requestDetail, bool) {
	if t == nil {
		return requestDetail{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalized {
		return requestDetail{}, false
	}
	t.finalized = true
	tools := make([]requestDetailToolUse, 0, len(t.toolOrder))
	for _, key := range t.toolOrder {
		state := t.tools[key]
		if state == nil {
			continue
		}
		detail := state.detail
		if detail.ArgumentBytes > 0 {
			detail.ArgumentSHA256 = hex.EncodeToString(state.hasher.Sum(nil))
		}
		tools = append(tools, detail)
	}
	requestID := entry.RequestID
	if requestID == "" {
		requestID = t.requestID
	}
	protocol := entry.Protocol
	if protocol == "" {
		protocol = t.protocol
	}
	responseError := t.err
	if responseError == "" {
		responseError = truncateDiagnosticText(redactRequestDetailText(entry.Error), 4000)
	}
	usage := t.usage
	if usage.InputTokens == 0 {
		usage.InputTokens = entry.InputTokens
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = entry.OutputTokens
	}
	if usage.ThinkingTokens == 0 {
		usage.ThinkingTokens = entry.ThinkingTokens
	}
	if usage.CacheReadInputTokens == 0 {
		usage.CacheReadInputTokens = entry.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens == 0 {
		usage.CacheCreationInputTokens = entry.CacheCreationInputTokens
	}
	detail := requestDetail{
		Version:            requestDetailStateVersion,
		RequestID:          requestID,
		Timestamp:          t.startedAt.Unix(),
		Protocol:           protocol,
		Model:              entry.Model,
		APIKeyID:           entry.APIKeyID,
		APIKeyName:         entry.APIKeyName,
		AccountID:          entry.AccountID,
		AccountEmail:       redactRequestDetailText(entry.AccountEmail),
		Endpoint:           entry.Endpoint,
		AccountSelectionMs: entry.AccountSelectionMs,
		AccountAttempts:    entry.AccountAttempts,
		RouteAffinityHit:   entry.RouteAffinityHit,
		Status:             entry.Status,
		StatusCode:         entry.StatusCode,
		DurationMs:         entry.DurationMs,
		Request:            t.request,
		Attempts:           append([]requestDetailAttempt(nil), t.attempts...),
		Timeline:           append([]requestDetailEvent(nil), t.timeline...),
		DroppedEvents:      t.droppedEvents,
		Response: requestDetailResponse{
			VisibleOutput:            t.visible.string(),
			VisibleOutputBytes:       t.visible.total,
			VisibleOutputTruncated:   t.visible.truncated,
			ThinkingOutput:           t.thinking.string(),
			ThinkingOutputBytes:      t.thinking.total,
			ThinkingOutputTruncated:  t.thinking.truncated,
			Tools:                    tools,
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			ThinkingTokens:           usage.ThinkingTokens,
			UncachedInputTokens:      usage.UncachedInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheCreation5mTokens:    usage.CacheCreation5mTokens,
			CacheCreation1hTokens:    usage.CacheCreation1hTokens,
			HasCacheBreakdown:        usage.HasCacheBreakdown,
			StopReason:               entry.StopReason,
			TruncationReason:         t.truncation,
			Error:                    responseError,
			Credits:                  t.credits,
			ContextUsagePercentage:   t.contextUsage,
		},
	}
	if detail.DurationMs <= 0 {
		detail.DurationMs = time.Since(t.startedAt).Milliseconds()
	}
	if detail.Request.BodyTruncated {
		detail.TruncatedFields = append(detail.TruncatedFields, "request.bodyJson")
	}
	if detail.Response.VisibleOutputTruncated {
		detail.TruncatedFields = append(detail.TruncatedFields, "response.visibleOutput")
	}
	if detail.Response.ThinkingOutputTruncated {
		detail.TruncatedFields = append(detail.TruncatedFields, "response.thinkingOutput")
	}
	if detail.DroppedEvents > 0 {
		detail.TruncatedFields = append(detail.TruncatedFields, "timeline")
	}
	return boundRequestDetail(detail, t.maxBytes), true
}

func (h *Handler) recordRequestDetailForContext(ctx context.Context, entry requestLogEntry) bool {
	trace := requestDetailTraceFromContext(ctx)
	if trace == nil {
		return false
	}
	detail, ok := trace.finalize(entry)
	if !ok {
		return h.ensureRequestDetailStore().has(entry.RequestID)
	}
	return h.ensureRequestDetailStore().add(detail)
}

func boundRequestDetail(detail requestDetail, maxBytes int) requestDetail {
	if maxBytes < config.MinRequestDetailMaxBytes {
		maxBytes = config.DefaultRequestDetailMaxBytes
	}
	for attempts := 0; attempts < 24; attempts++ {
		raw, err := json.Marshal(detail)
		if err == nil && len(raw) <= maxBytes {
			return detail
		}
		switch {
		case len(detail.Timeline) > 8:
			detail.DroppedEvents += len(detail.Timeline) / 2
			detail.Timeline = detail.Timeline[len(detail.Timeline)/2:]
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "timeline")
		case len(detail.Request.BodyJSON) > 1024:
			detail.Request.BodyJSON = truncateUTF8Bytes(detail.Request.BodyJSON, len(detail.Request.BodyJSON)/2)
			detail.Request.BodyTruncated = true
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "request.bodyJson")
		case len(detail.Response.VisibleOutput) > 512:
			detail.Response.VisibleOutput = truncateUTF8Bytes(detail.Response.VisibleOutput, len(detail.Response.VisibleOutput)/2)
			detail.Response.VisibleOutputTruncated = true
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "response.visibleOutput")
		case len(detail.Response.ThinkingOutput) > 512:
			detail.Response.ThinkingOutput = truncateUTF8Bytes(detail.Response.ThinkingOutput, len(detail.Response.ThinkingOutput)/2)
			detail.Response.ThinkingOutputTruncated = true
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "response.thinkingOutput")
		case len(detail.Attempts) > 16:
			detail.Attempts = detail.Attempts[len(detail.Attempts)/2:]
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "attempts")
		case len(detail.Response.Tools) > 16:
			detail.Response.Tools = detail.Response.Tools[:len(detail.Response.Tools)/2]
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "response.tools")
		case len(detail.Request.Headers) > 0:
			detail.Request.Headers = nil
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "request.headers")
		default:
			detail.Request.BodyJSON = "[detail omitted because it exceeded the configured byte limit]"
			detail.Response.VisibleOutput = ""
			detail.Response.ThinkingOutput = ""
			detail.Timeline = nil
			detail.Attempts = nil
			detail.Response.Tools = nil
			detail.TruncatedFields = appendUniqueDetailField(detail.TruncatedFields, "detail")
		}
	}
	return detail
}

func appendUniqueDetailField(fields []string, field string) []string {
	for _, existing := range fields {
		if existing == field {
			return fields
		}
	}
	return append(fields, field)
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}

var requestDetailSensitiveRedactors = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"'{}]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)("?(?:accessToken|refreshToken|idToken|sessionToken|kiroApiKey|clientSecret|apiKey|password)"?\s*[:=]\s*")[^"]+(")`), `${1}[REDACTED]${2}`},
	{regexp.MustCompile(`(?i)('?(?:accessToken|refreshToken|idToken|sessionToken|kiroApiKey|clientSecret|apiKey|password)'?\s*[:=]\s*')[^']+(')`), `${1}[REDACTED]${2}`},
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`), `sk-[REDACTED]`},
}

func redactRequestDetailText(value string) string {
	for _, redactor := range requestDetailSensitiveRedactors {
		value = redactor.re.ReplaceAllString(value, redactor.repl)
	}
	return value
}

func allowlistedRequestDetailHeaders(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	allowed := map[string]bool{
		"accept":                    true,
		"content-type":              true,
		"user-agent":                true,
		"x-request-id":              true,
		"x-conversation-id":         true,
		"x-client-user":             true,
		"anthropic-version":         true,
		"anthropic-beta":            true,
		"anthropic-organization-id": true,
		"openai-organization":       true,
	}
	result := make(map[string]string)
	for key, values := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if !allowed[lower] && !strings.HasPrefix(lower, "x-stainless-") {
			continue
		}
		result[http.CanonicalHeaderKey(key)] = truncateDiagnosticText(redactRequestDetailText(strings.Join(values, ", ")), 2000)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func sanitizeRequestDetailBody(raw []byte, limit int) (string, bool) {
	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		value = redactRequestDetailText(string(raw))
	} else {
		value = sanitizeRequestDetailJSONValue(value)
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		encoded = []byte(`"[unable to encode sanitized request]"`)
	}
	if limit <= 0 || len(encoded) <= limit {
		return string(encoded), false
	}
	return truncateUTF8Bytes(string(encoded), limit), true
}

func sanitizeRequestDetailJSONValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		kind := strings.ToLower(strings.TrimSpace(detailMapString(typed, "type")))
		mimeType := detailMapString(typed, "media_type")
		if mimeType == "" {
			mimeType = detailMapString(typed, "mime_type")
		}
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			if isSensitiveRequestDetailKey(key) {
				result[key] = "[REDACTED]"
				continue
			}
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if isToolArgumentField(typed, kind, lowerKey) {
				result[key] = requestDetailPayloadSummary("tool_arguments", child)
				continue
			}
			if text, ok := child.(string); ok {
				if summary, matched := requestDetailBinarySummary(text, mimeType, kind, lowerKey); matched {
					result[key] = summary
					continue
				}
			}
			result[key] = sanitizeRequestDetailJSONValue(child)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for i := range typed {
			result[i] = sanitizeRequestDetailJSONValue(typed[i])
		}
		return result
	case string:
		if summary, matched := requestDetailBinarySummary(typed, "", "", ""); matched {
			return summary
		}
		return redactRequestDetailText(typed)
	default:
		return value
	}
}

func detailMapString(values map[string]interface{}, key string) string {
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			text, _ := value.(string)
			return text
		}
	}
	return ""
}

func isSensitiveRequestDetailKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.TrimSpace(key)))
	switch normalized {
	case "authorization", "proxyauthorization", "apikey", "xapikey", "kiroapikey", "password", "passwd", "clientsecret", "secret", "accesstoken", "refreshtoken", "idtoken", "sessiontoken", "credential", "credentials", "cookie", "setcookie", "token":
		return true
	default:
		return false
	}
}

func isToolArgumentField(values map[string]interface{}, kind, key string) bool {
	if key == "input" && (kind == "tool_use" || kind == "tooluse") {
		return true
	}
	if key != "arguments" {
		return false
	}
	if strings.Contains(kind, "function_call") || strings.Contains(kind, "tool_call") {
		return true
	}
	return strings.TrimSpace(detailMapString(values, "name")) != ""
}

func requestDetailPayloadSummary(kind string, value interface{}) map[string]interface{} {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(fmt.Sprint(value))
	}
	sum := sha256.Sum256(raw)
	return map[string]interface{}{
		"redacted": kind,
		"bytes":    len(raw),
		"sha256":   hex.EncodeToString(sum[:]),
	}
}

func requestDetailBinarySummary(value, mimeHint, kind, key string) (map[string]interface{}, bool) {
	trimmed := strings.TrimSpace(value)
	mimeType := strings.TrimSpace(mimeHint)
	encoded := trimmed
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		comma := strings.IndexByte(trimmed, ',')
		if comma < 0 || !strings.Contains(strings.ToLower(trimmed[:comma]), ";base64") {
			return nil, false
		}
		meta := trimmed[5:comma]
		if semi := strings.IndexByte(meta, ';'); semi >= 0 {
			meta = meta[:semi]
		}
		if mimeType == "" {
			mimeType = meta
		}
		encoded = trimmed[comma+1:]
	} else {
		binaryField := key == "data" && (kind == "base64" || strings.Contains(kind, "image") || strings.Contains(kind, "document"))
		if !binaryField {
			return nil, false
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		decoded = []byte(encoded)
	}
	sum := sha256.Sum256(decoded)
	result := map[string]interface{}{
		"redacted":     "binary",
		"bytes":        len(decoded),
		"encodedBytes": len(encoded),
		"sha256":       hex.EncodeToString(sum[:]),
	}
	if mimeType != "" {
		result["mimeType"] = mimeType
	}
	return result, true
}

func requestDetailRetryReason(err error) string {
	if err == nil {
		return ""
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		return string(upstreamErr.Kind)
	}
	return "transport_error"
}
