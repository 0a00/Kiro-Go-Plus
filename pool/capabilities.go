package pool

import (
	"sort"
	"strings"
	"time"
)

// CapabilityStatus is the result of a capability observation. It is kept
// separate from account availability because one account can support text on
// one endpoint while lacking a profile-bound tool route.
type CapabilityStatus string

const (
	CapabilityUnknown          CapabilityStatus = "unknown"
	CapabilitySupported        CapabilityStatus = "supported"
	CapabilityUnavailable      CapabilityStatus = "unavailable"
	CapabilityProfileRequired  CapabilityStatus = "profile_required"
	CapabilityModelUnavailable CapabilityStatus = "model_unavailable"
	CapabilityRateLimited      CapabilityStatus = "rate_limited"
	CapabilityQuota            CapabilityStatus = "quota"
	CapabilityAuthFailed       CapabilityStatus = "auth_failed"
	CapabilityTransient        CapabilityStatus = "transient"
)

const (
	CapabilityText            = "text"
	CapabilityThinking        = "thinking"
	CapabilityTools           = "tools"
	CapabilityWebSearch       = "websearch"
	CapabilityMCP             = "mcp"
	CapabilityRecordTTL       = 6 * time.Hour
	CapabilityTransientTTL    = 5 * time.Minute
	maxCapabilitiesPerAccount = 64
	maxCapabilitiesTotal      = 250000
)

type capabilityKey struct {
	accountID string
	endpoint  string
	model     string
	feature   string
}

// AccountCapability is an admin-facing, bounded observation of one account
// route. It is not an entitlement claim and does not make a request eligible
// by itself; routing still verifies the live upstream response.
type AccountCapability struct {
	AccountID string           `json:"accountId"`
	Endpoint  string           `json:"endpoint"`
	Model     string           `json:"model"`
	Feature   string           `json:"feature"`
	Status    CapabilityStatus `json:"status"`
	Reason    string           `json:"reason,omitempty"`
	CheckedAt int64            `json:"checkedAt"`
	ExpiresAt int64            `json:"expiresAt"`
	LatencyMs int64            `json:"latencyMs,omitempty"`
}

func normalizeCapabilityPart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeCapabilityEndpoint(value string) string {
	value = normalizeCapabilityPart(value)
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

func capabilityTTL(status CapabilityStatus) time.Duration {
	if status == CapabilityTransient || status == CapabilityRateLimited || status == CapabilityQuota {
		return CapabilityTransientTTL
	}
	return CapabilityRecordTTL
}

func capabilityRecordExpired(record AccountCapability, now time.Time) bool {
	return record.ExpiresAt <= 0 || record.ExpiresAt <= now.Unix()
}

func (p *AccountPool) ensureCapabilityMapLocked() {
	if p.capabilities == nil {
		p.capabilities = make(map[capabilityKey]AccountCapability)
	}
}

func (p *AccountPool) evictCapabilitiesLocked(now time.Time) {
	if len(p.capabilities) == 0 {
		return
	}
	for key, record := range p.capabilities {
		if capabilityRecordExpired(record, now) {
			delete(p.capabilities, key)
		}
	}
	if len(p.capabilities) <= maxCapabilitiesTotal {
		return
	}
	keys := make([]capabilityKey, 0, len(p.capabilities))
	for key := range p.capabilities {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return p.capabilities[keys[i]].CheckedAt < p.capabilities[keys[j]].CheckedAt
	})
	for _, key := range keys[:len(keys)-maxCapabilitiesTotal] {
		delete(p.capabilities, key)
	}
}

func (p *AccountPool) evictAccountCapabilitiesLocked(accountID string) {
	keys := make([]capabilityKey, 0)
	for key := range p.capabilities {
		if key.accountID == accountID {
			keys = append(keys, key)
		}
	}
	if len(keys) <= maxCapabilitiesPerAccount {
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		return p.capabilities[keys[i]].CheckedAt < p.capabilities[keys[j]].CheckedAt
	})
	for _, key := range keys[:len(keys)-maxCapabilitiesPerAccount] {
		delete(p.capabilities, key)
	}
}

// RecordCapability stores one bounded observation and schedules the existing
// runtime-state writer. Empty account, endpoint, model, or feature values are
// ignored so malformed diagnostics cannot create a large unbounded key set.
func (p *AccountPool) RecordCapability(accountID, endpoint, model, feature string, status CapabilityStatus, reason string, latency time.Duration) {
	if p == nil {
		return
	}
	accountID = strings.TrimSpace(accountID)
	endpoint = normalizeCapabilityEndpoint(endpoint)
	model = normalizeCapabilityPart(model)
	feature = normalizeCapabilityPart(feature)
	if accountID == "" || endpoint == "" || model == "" || feature == "" {
		return
	}
	if status == "" {
		status = CapabilityUnknown
	}
	if len(reason) > 800 {
		reason = reason[:800]
	}
	if latency < 0 {
		latency = 0
	}
	now := time.Now()
	record := AccountCapability{
		AccountID: accountID,
		Endpoint:  endpoint,
		Model:     model,
		Feature:   feature,
		Status:    status,
		Reason:    strings.TrimSpace(reason),
		CheckedAt: now.Unix(),
		ExpiresAt: now.Add(capabilityTTL(status)).Unix(),
		LatencyMs: latency.Milliseconds(),
	}
	persist := false
	p.mu.Lock()
	p.ensureCapabilityMapLocked()
	key := capabilityKey{accountID: accountID, endpoint: endpoint, model: model, feature: feature}
	if previous, ok := p.capabilities[key]; !ok || previous.Status != record.Status {
		persist = true
	}
	p.capabilities[key] = record
	p.evictAccountCapabilitiesLocked(accountID)
	p.evictCapabilitiesLocked(now)
	p.mu.Unlock()
	if persist {
		p.scheduleRuntimeStateSave()
	}
}

func (p *AccountPool) ClearCapabilities(accountID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for key := range p.capabilities {
		if accountID == "" || key.accountID == accountID {
			delete(p.capabilities, key)
		}
	}
	p.mu.Unlock()
	p.scheduleRuntimeStateSave()
}

func (p *AccountPool) AccountCapabilities(accountID string) []AccountCapability {
	if p == nil {
		return []AccountCapability{}
	}
	now := time.Now()
	p.mu.Lock()
	p.ensureCapabilityMapLocked()
	result := make([]AccountCapability, 0)
	for key, record := range p.capabilities {
		if key.accountID != strings.TrimSpace(accountID) {
			continue
		}
		if capabilityRecordExpired(record, now) {
			delete(p.capabilities, key)
			continue
		}
		result = append(result, record)
	}
	p.mu.Unlock()
	sortCapabilityRecords(result)
	return result
}

func (p *AccountPool) CapabilitySnapshot() []AccountCapability {
	if p == nil {
		return []AccountCapability{}
	}
	now := time.Now()
	p.mu.Lock()
	p.ensureCapabilityMapLocked()
	result := make([]AccountCapability, 0, len(p.capabilities))
	for key, record := range p.capabilities {
		if capabilityRecordExpired(record, now) {
			delete(p.capabilities, key)
			continue
		}
		result = append(result, record)
	}
	p.mu.Unlock()
	sortCapabilityRecords(result)
	return result
}

func sortCapabilityRecords(records []AccountCapability) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].AccountID != records[j].AccountID {
			return records[i].AccountID < records[j].AccountID
		}
		if records[i].Model != records[j].Model {
			return records[i].Model < records[j].Model
		}
		if records[i].Endpoint != records[j].Endpoint {
			return records[i].Endpoint < records[j].Endpoint
		}
		return records[i].Feature < records[j].Feature
	})
}
