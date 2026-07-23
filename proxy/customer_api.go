package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strconv"
)

const (
	defaultCustomerLogLimit = 100
	maxCustomerLogLimit     = 500
)

func customerKeyStatus(entry config.ApiKeyEntry) string {
	if overToken, overCredit := config.ApiKeyOverLimit(entry); overToken || overCredit {
		return "exhausted"
	}
	if !entry.Enabled {
		return "disabled"
	}
	return "active"
}

func customerCreditsRemaining(entry config.ApiKeyEntry) float64 {
	if entry.CreditLimit <= 0 {
		return -1
	}
	remaining := entry.CreditLimit - entry.CreditsUsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

func customerTokensRemaining(entry config.ApiKeyEntry) int64 {
	if entry.TokenLimit <= 0 {
		return -1
	}
	remaining := entry.TokenLimit - entry.TokensUsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (h *Handler) authenticateCustomerKey(w http.ResponseWriter, r *http.Request) *config.ApiKeyEntry {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	provided := extractProvidedKey(r)
	entry := config.FindApiKeyByValue(provided)
	if provided != "" && entry != nil {
		return entry
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="kiro"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
	return nil
}

type customerStatsView struct {
	Status           string  `json:"status"`
	Version          string  `json:"version"`
	KeyStatus        string  `json:"keyStatus"`
	RequestsCount    int64   `json:"requestsCount"`
	TokensUsed       int64   `json:"tokensUsed"`
	CreditsUsed      float64 `json:"creditsUsed"`
	TokenLimit       int64   `json:"tokenLimit"`
	CreditLimit      float64 `json:"creditLimit"`
	TokensRemaining  int64   `json:"tokensRemaining"`
	CreditsRemaining float64 `json:"creditsRemaining"`
	LastUsedAt       int64   `json:"lastUsedAt,omitempty"`
}

func (h *Handler) handleCustomerStats(w http.ResponseWriter, r *http.Request) {
	entry := h.authenticateCustomerKey(w, r)
	if entry == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(customerStatsView{
		Status:           "ok",
		Version:          config.Version,
		KeyStatus:        customerKeyStatus(*entry),
		RequestsCount:    entry.RequestsCount,
		TokensUsed:       entry.TokensUsed,
		CreditsUsed:      entry.CreditsUsed,
		TokenLimit:       entry.TokenLimit,
		CreditLimit:      entry.CreditLimit,
		TokensRemaining:  customerTokensRemaining(*entry),
		CreditsRemaining: customerCreditsRemaining(*entry),
		LastUsedAt:       entry.LastUsedAt,
	})
}

type customerMeView struct {
	Name               string  `json:"name,omitempty"`
	Status             string  `json:"status"`
	CreatedAt          int64   `json:"createdAt"`
	LastUsedAt         int64   `json:"lastUsedAt,omitempty"`
	TokenLimit         int64   `json:"tokenLimit"`
	CreditLimit        float64 `json:"creditLimit"`
	TokensUsed         int64   `json:"tokensUsed"`
	CreditsUsed        float64 `json:"creditsUsed"`
	TokensRemaining    int64   `json:"tokensRemaining"`
	CreditsRemaining   float64 `json:"creditsRemaining"`
	RequestsCount      int64   `json:"requestsCount"`
	RequestsPerMinute  int     `json:"requestsPerMinute"`
	TokensPerMinute    int64   `json:"tokensPerMinute"`
	MaxConcurrency     int     `json:"maxConcurrency"`
	QueueCapacity      int     `json:"queueCapacity"`
	QueueTimeoutMillis int     `json:"queueTimeoutMs"`
}

func (h *Handler) handleCustomerMe(w http.ResponseWriter, r *http.Request) {
	entry := h.authenticateCustomerKey(w, r)
	if entry == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(customerMeView{
		Name:               entry.Name,
		Status:             customerKeyStatus(*entry),
		CreatedAt:          entry.CreatedAt,
		LastUsedAt:         entry.LastUsedAt,
		TokenLimit:         entry.TokenLimit,
		CreditLimit:        entry.CreditLimit,
		TokensUsed:         entry.TokensUsed,
		CreditsUsed:        entry.CreditsUsed,
		TokensRemaining:    customerTokensRemaining(*entry),
		CreditsRemaining:   customerCreditsRemaining(*entry),
		RequestsCount:      entry.RequestsCount,
		RequestsPerMinute:  entry.RequestsPerMinute,
		TokensPerMinute:    entry.TokensPerMinute,
		MaxConcurrency:     entry.MaxConcurrency,
		QueueCapacity:      entry.QueueCapacity,
		QueueTimeoutMillis: entry.QueueTimeoutMs,
	})
}

type customerRequestLogView struct {
	Timestamp                int64   `json:"timestamp"`
	Protocol                 string  `json:"protocol"`
	Model                    string  `json:"model"`
	Status                   string  `json:"status"`
	StatusCode               int     `json:"statusCode"`
	ErrorCategory            string  `json:"errorCategory,omitempty"`
	UpstreamFirstActivityMs  *int64  `json:"upstreamFirstActivityMs,omitempty"`
	FirstSSEEventMs          *int64  `json:"firstSseEventMs,omitempty"`
	FirstThinkingMs          *int64  `json:"firstThinkingMs,omitempty"`
	FirstVisibleTextMs       *int64  `json:"firstVisibleTextMs,omitempty"`
	FirstToolOutputMs        *int64  `json:"firstToolOutputMs,omitempty"`
	FirstContentMs           *int64  `json:"firstContentMs,omitempty"`
	MaxStreamGapMs           *int64  `json:"maxStreamGapMs,omitempty"`
	HeartbeatCount           int     `json:"heartbeatCount,omitempty"`
	DurationMs               int64   `json:"durationMs"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	ThinkingTokens           int     `json:"thinkingTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CacheStatus              string  `json:"cacheStatus,omitempty"`
	WebSearchRequests        int     `json:"webSearchRequests,omitempty"`
	ToolUseCount             int     `json:"toolUseCount,omitempty"`
	Credits                  float64 `json:"credits,omitempty"`
}

func customerRequestErrorCategory(entry requestLogEntry) string {
	if entry.Status == "canceled" || entry.StatusCode == 499 {
		return "canceled"
	}
	switch entry.StatusCode {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication"
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "upstream"
	}
	if entry.StatusCode >= 500 {
		return "server"
	}
	if entry.StatusCode >= 400 {
		return "request"
	}
	if entry.Status != "" && entry.Status != "success" {
		return "error"
	}
	return ""
}

func customerRequestLog(entry requestLogEntry) customerRequestLogView {
	return customerRequestLogView{
		Timestamp:                entry.Timestamp,
		Protocol:                 entry.Protocol,
		Model:                    entry.Model,
		Status:                   entry.Status,
		StatusCode:               entry.StatusCode,
		ErrorCategory:            customerRequestErrorCategory(entry),
		UpstreamFirstActivityMs:  entry.UpstreamFirstActivityMs,
		FirstSSEEventMs:          entry.FirstSSEEventMs,
		FirstThinkingMs:          entry.FirstThinkingMs,
		FirstVisibleTextMs:       entry.FirstVisibleTextMs,
		FirstToolOutputMs:        entry.FirstToolOutputMs,
		FirstContentMs:           entry.FirstContentMs,
		MaxStreamGapMs:           entry.MaxStreamGapMs,
		HeartbeatCount:           entry.HeartbeatCount,
		DurationMs:               entry.DurationMs,
		InputTokens:              entry.InputTokens,
		OutputTokens:             entry.OutputTokens,
		ThinkingTokens:           entry.ThinkingTokens,
		CacheReadInputTokens:     entry.CacheReadInputTokens,
		CacheCreationInputTokens: entry.CacheCreationInputTokens,
		CacheStatus:              entry.CacheStatus,
		WebSearchRequests:        entry.WebSearchRequests,
		ToolUseCount:             entry.ToolUseCount,
		Credits:                  entry.Credits,
	}
}

func (h *Handler) handleCustomerLogs(w http.ResponseWriter, r *http.Request) {
	entry := h.authenticateCustomerKey(w, r)
	if entry == nil {
		return
	}
	limit := defaultCustomerLogLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = parsed
	}
	if limit > maxCustomerLogLimit {
		limit = maxCustomerLogLimit
	}

	logs := []requestLogEntry{}
	if h.requestLog != nil {
		logs = h.requestLog.listForAPIKey(entry.ID, limit)
	}
	out := make([]customerRequestLogView, 0, len(logs))
	for _, request := range logs {
		out = append(out, customerRequestLog(request))
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":  out,
		"count": len(out),
		"limit": limit,
	})
}
