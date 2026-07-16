package pool

import (
	"kiro-go/config"
	"strings"
	"time"
)

// AccountInventory partitions configured Kiro identities into mutually
// exclusive operational states. Total counts identities; ConfiguredRows also
// exposes the raw number of stored account records for migration diagnostics.
type AccountInventory struct {
	Total           int `json:"total"`
	ConfiguredRows  int `json:"configuredRows"`
	Routable        int `json:"routable"`
	Cooling         int `json:"cooling"`
	QuotaBlocked    int `json:"quotaBlocked"`
	Banned          int `json:"banned"`
	Disabled        int `json:"disabled"`
	CredentialIssue int `json:"credentialIssue"`
	ProfileIssue    int `json:"profileIssue"`
}

type accountInventoryState int

const (
	inventoryRoutable accountInventoryState = iota
	inventoryCooling
	inventoryQuotaBlocked
	inventoryCredentialIssue
	inventoryProfileIssue
	inventoryDisabled
	inventoryBanned
)

// InventorySnapshot is a pure read. It never refreshes credentials or probes
// Kiro, so the admin status endpoint remains fast even with thousands of rows.
func (p *AccountPool) InventorySnapshot() AccountInventory {
	accounts := config.GetAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	resident := make(map[string]bool)
	cooling := make(map[string]bool)
	if p != nil {
		p.mu.RLock()
		for i := range p.accounts {
			resident[p.accounts[i].ID] = true
		}
		for id, until := range p.cooldowns {
			if until.After(now) {
				cooling[id] = true
			}
		}
		for id, until := range p.refreshFailures {
			if until.After(now) {
				cooling[id] = true
			}
		}
		p.mu.RUnlock()
	}

	best := make(map[string]accountInventoryState, len(accounts))
	for _, account := range accounts {
		state := classifyInventoryAccount(account, resident[account.ID], cooling[account.ID], allowOverUsage, now)
		identity := inventoryIdentity(account)
		if current, exists := best[identity]; !exists || state < current {
			best[identity] = state
		}
	}

	result := AccountInventory{Total: len(best), ConfiguredRows: len(accounts)}
	for _, state := range best {
		switch state {
		case inventoryRoutable:
			result.Routable++
		case inventoryCooling:
			result.Cooling++
		case inventoryQuotaBlocked:
			result.QuotaBlocked++
		case inventoryCredentialIssue:
			result.CredentialIssue++
		case inventoryProfileIssue:
			result.ProfileIssue++
		case inventoryDisabled:
			result.Disabled++
		case inventoryBanned:
			result.Banned++
		}
	}
	return result
}

func classifyInventoryAccount(account config.Account, resident, cooling, allowOverUsage bool, now time.Time) accountInventoryState {
	status := strings.ToUpper(strings.TrimSpace(account.BanStatus))
	if status == "BANNED" || status == "SUSPENDED" {
		return inventoryBanned
	}
	if hasPersistedProfileIssue(account.BanReason) {
		return inventoryProfileIssue
	}
	if !account.Enabled {
		return inventoryDisabled
	}
	if hasCredentialIssue(account, now) {
		return inventoryCredentialIssue
	}
	if isQuotaBlocked(account, allowOverUsage) {
		return inventoryQuotaBlocked
	}
	if cooling || !resident {
		return inventoryCooling
	}
	return inventoryRoutable
}

func inventoryIdentity(account config.Account) string {
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	if userID := strings.TrimSpace(account.UserId); userID != "" {
		return "user\x00" + userID
	}
	if fingerprint := strings.TrimSpace(account.CredentialFingerprint); fingerprint != "" {
		return "credential\x00" + fingerprint
	}
	if email := strings.ToLower(strings.TrimSpace(account.Email)); email != "" {
		return "email\x00" + provider + "\x00" + email
	}
	return "id\x00" + account.ID
}

func hasCredentialIssue(account config.Account, now time.Time) bool {
	apiKey := strings.TrimSpace(account.KiroApiKey)
	if apiKey != "" || strings.EqualFold(strings.TrimSpace(account.AuthMethod), "api_key") || strings.EqualFold(strings.TrimSpace(account.AuthMethod), "apikey") {
		return apiKey == "" && strings.TrimSpace(account.AccessToken) == ""
	}
	refreshToken := strings.TrimSpace(account.RefreshToken)
	if strings.TrimSpace(account.AccessToken) == "" && refreshToken == "" {
		return true
	}
	return account.ExpiresAt > 0 && now.Unix() > account.ExpiresAt-tokenRefreshSkewSeconds && refreshToken == ""
}

func hasPersistedProfileIssue(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(reason, "no available kiro profile") ||
		strings.Contains(reason, "no available profile") ||
		strings.Contains(reason, "profile arn unsupported") ||
		strings.Contains(reason, "profile unavailable")
}
