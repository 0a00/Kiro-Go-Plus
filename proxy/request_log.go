package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

const defaultRequestLogLimit = 1000

type requestLogEntry struct {
	ID                       uint64  `json:"id"`
	Timestamp                int64   `json:"timestamp"`
	RequestID                string  `json:"requestId,omitempty"`
	Protocol                 string  `json:"protocol"`
	Model                    string  `json:"model"`
	AccountID                string  `json:"accountId,omitempty"`
	AccountEmail             string  `json:"accountEmail,omitempty"`
	Endpoint                 string  `json:"endpoint,omitempty"`
	Status                   string  `json:"status"`
	StatusCode               int     `json:"statusCode"`
	DurationMs               int64   `json:"durationMs"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	Credits                  float64 `json:"credits,omitempty"`
	Error                    string  `json:"error,omitempty"`
}

type requestLog struct {
	mu      sync.RWMutex
	nextID  atomic.Uint64
	limit   int
	entries []requestLogEntry
}

func newRequestLog(limit int) *requestLog {
	if limit <= 0 {
		limit = defaultRequestLogLimit
	}
	return &requestLog{limit: limit, entries: make([]requestLogEntry, 0, limit)}
}

func (l *requestLog) add(entry requestLogEntry) {
	if l == nil {
		return
	}
	entry.ID = l.nextID.Add(1)
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().Unix()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	if overflow := len(l.entries) - l.limit; overflow > 0 {
		copy(l.entries, l.entries[overflow:])
		l.entries = l.entries[:l.limit]
	}
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

func (h *Handler) recordRequestLog(entry requestLogEntry) {
	if h == nil {
		return
	}
	if h.requestLog == nil {
		h.requestLog = newRequestLog(defaultRequestLogLimit)
	}
	h.requestLog.add(entry)
}

func (h *Handler) recordRequestLogForPayload(payload *KiroPayload, entry requestLogEntry) {
	if payload != nil {
		entry.RequestID = requestIDFromContext(payload.requestContext)
		entry.Endpoint = payload.successfulEndpoint()
	}
	h.recordRequestLog(entry)
}

func requestDurationMs(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
