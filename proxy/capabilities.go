package proxy

import (
	"encoding/json"
	"errors"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func capabilityFeatureForPayload(payload *KiroPayload) string {
	if payload != nil {
		message := payload.ConversationState.CurrentMessage.UserInputMessage
		if payload.requireToolUse || payload.requireActionableOutput ||
			(message.UserInputMessageContext != nil && len(message.UserInputMessageContext.Tools) > 0) {
			return pool.CapabilityTools
		}
		model := strings.ToLower(strings.TrimSpace(message.ModelID))
		if strings.HasSuffix(model, "-thinking") || strings.HasSuffix(model, ".thinking") {
			return pool.CapabilityThinking
		}
	}
	return pool.CapabilityText
}

func capabilityModelForPayload(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	return strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
}

func recordKiroCapabilityObservation(account *config.Account, endpoint string, payload *KiroPayload, status pool.CapabilityStatus, reason error, startedAt time.Time) {
	if account == nil || strings.TrimSpace(endpoint) == "" {
		return
	}
	model := capabilityModelForPayload(payload)
	if model == "" {
		return
	}
	message := ""
	if reason != nil {
		message = reason.Error()
	}
	pool.GetPool().RecordCapability(account.ID, normalizeCapabilityEndpoint(endpoint), model, capabilityFeatureForPayload(payload), status, message, time.Since(startedAt))
}

func capabilityFeatureForProbe(kind modelHealthProbeKind) string {
	switch kind {
	case modelHealthProbeThinking:
		return pool.CapabilityThinking
	case modelHealthProbeTools:
		return pool.CapabilityTools
	default:
		return pool.CapabilityText
	}
}

func normalizeCapabilityEndpoint(endpoint string) string {
	value := strings.ToLower(strings.TrimSpace(endpoint))
	switch {
	case strings.Contains(value, "runtime"):
		return "runtime"
	case strings.Contains(value, "codewhisperer"):
		return "codewhisperer"
	case strings.Contains(value, "amazonq") || strings.Contains(value, "amazon q"):
		return "amazonq"
	case strings.Contains(value, "kiro") || value == "q":
		return "kiro"
	case strings.Contains(value, "websearch") || strings.Contains(value, "web search"):
		return "websearch"
	default:
		return value
	}
}

func capabilityStatusForError(err error) pool.CapabilityStatus {
	if err == nil {
		return pool.CapabilitySupported
	}
	if strings.Contains(strings.ToLower(err.Error()), "profilearn is required") ||
		strings.Contains(strings.ToLower(err.Error()), "profile arn is required") {
		return pool.CapabilityProfileRequired
	}
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.Kind {
		case UpstreamErrorProfileUnavailable:
			return pool.CapabilityProfileRequired
		case UpstreamErrorModelUnavailable:
			return pool.CapabilityModelUnavailable
		case UpstreamErrorRateLimit:
			return pool.CapabilityRateLimited
		case UpstreamErrorQuota:
			return pool.CapabilityQuota
		case UpstreamErrorTokenExpired, UpstreamErrorAuthRevoked:
			return pool.CapabilityAuthFailed
		case UpstreamErrorTransient, UpstreamErrorEndpointUnavailable, UpstreamErrorFirstTokenTimeout,
			UpstreamErrorStreamIdleTimeout, UpstreamErrorActionableTimeout, UpstreamErrorToolAssemblyTimeout,
			UpstreamErrorStreamTruncated, UpstreamErrorRetryBudget, UpstreamErrorEmptyResponse:
			return pool.CapabilityTransient
		}
	}
	return pool.CapabilityUnavailable
}

func capabilityEndpointFromError(err error) string {
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		return strings.TrimSpace(upstreamErr.Endpoint)
	}
	return ""
}

func (h *Handler) recordCapabilityObservation(accountID, endpoint, model, feature string, supported bool, reasonValue interface{}, startedAt time.Time) {
	if h == nil || h.pool == nil {
		return
	}
	if strings.TrimSpace(endpoint) == "" {
		if err, ok := reasonValue.(error); ok {
			endpoint = capabilityEndpointFromError(err)
		}
	}
	if strings.TrimSpace(endpoint) == "" {
		return
	}
	status := pool.CapabilitySupported
	reason := ""
	if !supported {
		var err error
		switch value := reasonValue.(type) {
		case error:
			err = value
			reason = value.Error()
		case string:
			reason = value
		}
		status = capabilityStatusForError(err)
		if reason == "" {
			reason = "capability probe did not produce a supported result"
		}
	}
	h.pool.RecordCapability(accountID, normalizeCapabilityEndpoint(endpoint), model, feature, status, reason, time.Since(startedAt))
}

type capabilityQuery struct {
	AccountID string
	Model     string
	Endpoint  string
	Feature   string
	Limit     int
}

func readCapabilityQuery(r *http.Request) capabilityQuery {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(query.Get("limit")))
	if limit <= 0 {
		limit = 10000
	}
	if limit > 50000 {
		limit = 50000
	}
	return capabilityQuery{
		AccountID: strings.TrimSpace(query.Get("accountId")),
		Model:     strings.ToLower(strings.TrimSpace(query.Get("model"))),
		Endpoint:  strings.ToLower(strings.TrimSpace(query.Get("endpoint"))),
		Feature:   strings.ToLower(strings.TrimSpace(query.Get("feature"))),
		Limit:     limit,
	}
}

func filterCapabilityRecords(records []pool.AccountCapability, query capabilityQuery) []pool.AccountCapability {
	result := make([]pool.AccountCapability, 0, minInt(len(records), query.Limit))
	for _, record := range records {
		if query.AccountID != "" && record.AccountID != query.AccountID {
			continue
		}
		if query.Model != "" && record.Model != query.Model {
			continue
		}
		if query.Endpoint != "" && record.Endpoint != query.Endpoint {
			continue
		}
		if query.Feature != "" && record.Feature != query.Feature {
			continue
		}
		result = append(result, record)
		if len(result) >= query.Limit {
			break
		}
	}
	return result
}

func (h *Handler) apiGetCapabilities(w http.ResponseWriter, r *http.Request) {
	query := readCapabilityQuery(r)
	records := filterCapabilityRecords(h.pool.CapabilitySnapshot(), query)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"records": records,
		"count":   len(records),
		"limit":   query.Limit,
	})
}

func (h *Handler) apiGetAccountCapabilities(w http.ResponseWriter, r *http.Request, accountID string) {
	query := readCapabilityQuery(r)
	query.AccountID = strings.TrimSpace(accountID)
	records := filterCapabilityRecords(h.pool.AccountCapabilities(query.AccountID), query)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accountId": query.AccountID,
		"records":   records,
		"count":     len(records),
		"limit":     query.Limit,
	})
}
