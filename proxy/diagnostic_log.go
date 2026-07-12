package proxy

import (
	"kiro-go/config"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDiagnosticLogLimit = 200

type diagnosticLogEntry struct {
	ID             string `json:"id"`
	Timestamp      int64  `json:"timestamp"`
	RequestID      string `json:"requestId,omitempty"`
	Protocol       string `json:"protocol"`
	Model          string `json:"model,omitempty"`
	AccountID      string `json:"accountId,omitempty"`
	AccountEmail   string `json:"accountEmail,omitempty"`
	StatusCode     int    `json:"statusCode,omitempty"`
	Error          string `json:"error,omitempty"`
	RequestSummary string `json:"requestSummary,omitempty"`
}

type diagnosticLog struct {
	mu      sync.RWMutex
	limit   int
	entries []diagnosticLogEntry
	nextID  uint64
}

func newDiagnosticLog(limit int) *diagnosticLog {
	if limit <= 0 {
		limit = defaultDiagnosticLogLimit
	}
	return &diagnosticLog{limit: limit}
}

func (l *diagnosticLog) configure(limit int) {
	if limit <= 0 {
		limit = defaultDiagnosticLogLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
	if len(l.entries) > limit {
		l.entries = append([]diagnosticLogEntry(nil), l.entries[len(l.entries)-limit:]...)
	}
}

func (l *diagnosticLog) add(entry diagnosticLogEntry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	entry.ID = "diag_" + strconv.FormatUint(l.nextID, 10)
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().Unix()
	}
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.limit {
		l.entries = l.entries[len(l.entries)-l.limit:]
	}
}

func (l *diagnosticLog) list(limit int) []diagnosticLogEntry {
	if l == nil {
		return []diagnosticLogEntry{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if limit <= 0 || limit > l.limit {
		limit = l.limit
	}
	if limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]diagnosticLogEntry, limit)
	for i := 0; i < limit; i++ {
		out[i] = l.entries[len(l.entries)-1-i]
	}
	return out
}

func (h *Handler) recordDiagnosticFailure(entry diagnosticLogEntry) {
	cfg := config.GetDiagnosticConfig()
	if !cfg.Enabled {
		return
	}
	if h.diagnosticLog == nil {
		h.diagnosticLog = newDiagnosticLog(cfg.MaxEntries)
	}
	h.diagnosticLog.configure(cfg.MaxEntries)
	if !cfg.IncludeRequestSummary {
		entry.RequestSummary = ""
	}
	entry.AccountEmail = redactDiagnosticText(entry.AccountEmail)
	entry.Error = truncateDiagnosticText(redactDiagnosticText(entry.Error), 2000)
	entry.RequestSummary = truncateDiagnosticText(redactDiagnosticText(entry.RequestSummary), 4000)
	h.diagnosticLog.add(entry)
}

func (h *Handler) recordDiagnosticFailureForPayload(protocol, model string, account *config.Account, statusCode int, err error, payload *KiroPayload) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	entry := diagnosticLogEntry{
		Protocol:       protocol,
		Model:          model,
		StatusCode:     statusCode,
		Error:          message,
		RequestSummary: summarizeKiroPayload(payload),
	}
	if payload != nil {
		entry.RequestID = requestIDFromContext(payload.requestContext)
	}
	if account != nil {
		entry.AccountID = account.ID
		entry.AccountEmail = account.Email
	}
	h.recordDiagnosticFailure(entry)
}

func summarizeKiroPayload(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	parts := []string{}
	if current := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.Content); current != "" {
		parts = append(parts, "current: "+current)
	}
	for _, item := range payload.ConversationState.History {
		if item.UserInputMessage != nil {
			if text := strings.TrimSpace(item.UserInputMessage.Content); text != "" {
				parts = append(parts, "user: "+text)
			}
		}
		if item.AssistantResponseMessage != nil {
			if text := strings.TrimSpace(item.AssistantResponseMessage.Content); text != "" {
				parts = append(parts, "assistant: "+text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func truncateDiagnosticText(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if maxRunes <= 0 || text == "" {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "...[truncated]"
}

var diagnosticRedactors = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"'{}]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)("?(?:accessToken|refreshToken|kiroApiKey|clientSecret|apiKey|password|token)"?\s*[:=]\s*")[^"]+(")`), `${1}[REDACTED]${2}`},
	{regexp.MustCompile(`(?i)('?(?:accessToken|refreshToken|kiroApiKey|clientSecret|apiKey|password|token)'?\s*[:=]\s*')[^']+(')`), `${1}[REDACTED]${2}`},
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`), `sk-[REDACTED]`},
	{regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), `[EMAIL_REDACTED]`},
}

func redactDiagnosticText(text string) string {
	if text == "" {
		return ""
	}
	for _, redactor := range diagnosticRedactors {
		text = redactor.re.ReplaceAllString(text, redactor.repl)
	}
	return text
}
