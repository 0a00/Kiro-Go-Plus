// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kiro-go/internal/outboundproxy"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken           string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken          string `json:"refreshToken"`           // OAuth refresh token for token renewal
	KiroApiKey            string `json:"kiroApiKey,omitempty"`   // Direct upstream Kiro API key credential
	ClientID              string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret          string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	CredentialFingerprint string `json:"credentialFingerprint,omitempty"`
	AuthMethod            string `json:"authMethod"`           // Authentication method: "idc", "social", "external_idp", or "api_key"
	Provider              string `json:"provider,omitempty"`   // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region                string `json:"region"`               // AWS region for OIDC endpoints
	StartUrl              string `json:"startUrl,omitempty"`   // AWS SSO start URL
	ExpiresAt             int64  `json:"expiresAt,omitempty"`  // Token expiration timestamp (Unix seconds)
	MachineId             string `json:"machineId,omitempty"`  // UUID machine identifier for request tracking
	ProfileArn            string `json:"profileArn,omitempty"` // CodeWhisperer/Kiro profile ARN for generation requests

	// External IdP refresh material. These fields are used when AuthMethod is
	// "external_idp" (Microsoft 365 / Entra ID) and no client secret is involved.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
	IssuerURL     string `json:"issuerUrl,omitempty"`
	Scopes        string `json:"scopes,omitempty"`

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Priority order for priority/balanced routing (lower = preferred).
	Priority int `json:"priority,omitempty"`

	// MaxConcurrency overrides the global total concurrency for this account.
	// 0 means inherit UpstreamProtection.MaxPerAccountConcurrency.
	MaxConcurrency int `json:"maxConcurrency,omitempty"`

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation). RequestCount and ErrorCount
	// are retained as JSON field names for backward compatibility.
	RequestCount int     `json:"requestCount,omitempty"` // Successful requests completed by this account
	ErrorCount   int     `json:"errorCount,omitempty"`   // Failed account-level upstream attempts
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit        int64   `json:"tokenLimit,omitempty"`
	CreditLimit       float64 `json:"creditLimit,omitempty"`
	RequestsPerMinute int     `json:"requestsPerMinute,omitempty"`
	TokensPerMinute   int64   `json:"tokensPerMinute,omitempty"`
	MaxConcurrency    int     `json:"maxConcurrency,omitempty"`
	QueueCapacity     int     `json:"queueCapacity,omitempty"`
	QueueTimeoutMs    int     `json:"queueTimeoutMs,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
}

// UpstreamProtectionConfig controls local per-upstream concurrency and 429 backoff.
type UpstreamProtectionConfig struct {
	Enabled                       bool                      `json:"enabled"`
	MaxPerAccountConcurrency      int                       `json:"maxPerAccountConcurrency,omitempty"`
	MaxPerAccountModelConcurrency int                       `json:"maxPerAccountModelConcurrency,omitempty"`
	PerModelConcurrency           map[string]int            `json:"perModelConcurrency,omitempty"`
	PerProfileModelConcurrency    map[string]map[string]int `json:"perProfileModelConcurrency,omitempty"`
	RateLimitCooldownMs           int                       `json:"rateLimitCooldownMs,omitempty"`
	MaxRateLimitCooldownMs        int                       `json:"maxRateLimitCooldownMs,omitempty"`
	RouteAffinityTTLSeconds       int                       `json:"routeAffinityTtlSeconds,omitempty"`
	RouteAffinityMaxEntries       int                       `json:"routeAffinityMaxEntries,omitempty"`
}

// PromptCacheConfig controls local prompt/KV cache simulation for usage reporting.
type PromptCacheConfig struct {
	Enabled                bool    `json:"enabled"`
	NamespaceMode          string  `json:"namespaceMode"`
	CacheReadEfficiency    float64 `json:"cacheReadEfficiency"`
	CacheReadEfficiencyMin float64 `json:"cacheReadEfficiencyMin"`
	CacheReadEfficiencyMax float64 `json:"cacheReadEfficiencyMax"`
	KvCacheTTLSecs         int64   `json:"kvCacheTtlSecs"`
	MaxEntriesPerAccount   int     `json:"maxEntriesPerAccount"`
	MaxEntriesTotal        int     `json:"maxEntriesTotal"`
}

const (
	PromptCacheNamespaceAccount       = "account"
	PromptCacheNamespaceAccountAPIKey = "account_api_key"
)

// RuntimeConfig contains process-level and client header settings surfaced in Admin.
type RuntimeConfig struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	LogLevel      string `json:"logLevel"`
	KiroVersion   string `json:"kiroVersion"`
	SystemVersion string `json:"systemVersion"`
	NodeVersion   string `json:"nodeVersion"`
}

// RoutingConfig controls how accounts are selected from the pool.
type RoutingConfig struct {
	LoadBalancingMode string `json:"loadBalancingMode"`
}

// AutoRefreshConfig controls background account/token refresh behavior.
type AutoRefreshConfig struct {
	Enabled                   bool  `json:"enabled"`
	IntervalMinutes           int   `json:"intervalMinutes"`
	TokenRefreshBeforeSeconds int64 `json:"tokenRefreshBeforeSeconds"`
	MaxAccountsPerRun         int   `json:"maxAccountsPerRun"`
	FailureCooldownSeconds    int64 `json:"failureCooldownSeconds"`
	RefreshConcurrency        int   `json:"refreshConcurrency"`
	RefreshQueueCapacity      int   `json:"refreshQueueCapacity"`
	RefreshTaskTimeoutSeconds int   `json:"refreshTaskTimeoutSeconds"`
	RefreshJitterSeconds      int   `json:"refreshJitterSeconds"`
	RefreshModels             bool  `json:"refreshModels"`
	ModelIntervalMinutes      int   `json:"modelIntervalMinutes"`
	MaxModelsPerRun           int   `json:"maxModelsPerRun"`
	ModelRefreshConcurrency   int   `json:"modelRefreshConcurrency"`
}

// RetryConfig bounds retries across accounts and upstream endpoint fallbacks.
type RetryConfig struct {
	MaxAccountAttempts             int `json:"maxAccountAttempts"`
	MaxUpstreamAttempts            int `json:"maxUpstreamAttempts"`
	MaxRetryDurationSeconds        int `json:"maxRetryDurationSeconds"`
	FirstTokenTimeoutSeconds       int `json:"firstTokenTimeoutSeconds"`
	StreamIdleTimeoutSeconds       int `json:"streamIdleTimeoutSeconds"`
	ToolAssemblyTimeoutSeconds     int `json:"toolAssemblyTimeoutSeconds"`
	EmptyResponseRetries           int `json:"emptyResponseRetries"`
	EndpointFailureThreshold       int `json:"endpointFailureThreshold"`
	EndpointCircuitCooldownSeconds int `json:"endpointCircuitCooldownSeconds"`
	ProxyFailureThreshold          int `json:"proxyFailureThreshold"`
	ProxyCircuitCooldownSeconds    int `json:"proxyCircuitCooldownSeconds"`
}

// LongToolConfig protects large file/command tool calls from upstream
// truncation and bounds recovery work before any partial tool JSON is exposed.
type LongToolConfig struct {
	Enabled              bool   `json:"enabled"`
	DefaultMaxToolTokens int    `json:"defaultMaxToolTokens"`
	TruncationRetries    int    `json:"truncationRetries"`
	FallbackEnabled      bool   `json:"fallbackEnabled"`
	FallbackModel        string `json:"fallbackModel,omitempty"`
}

// ResponsesStorageConfig bounds persisted OpenAI Responses API state.
type ResponsesStorageConfig struct {
	DefaultStore      bool  `json:"defaultStore"`
	TTLHours          int   `json:"ttlHours"`
	MaxFiles          int   `json:"maxFiles"`
	MaxBytes          int64 `json:"maxBytes"`
	MaxHistoryBytes   int   `json:"maxHistoryBytes"`
	GCIntervalMinutes int   `json:"gcIntervalMinutes"`
}

// ModelEntry maps a client-facing model ID to the Kiro upstream model ID.
type ModelEntry struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"displayName"`
	KiroModelID   string   `json:"kiroModelId"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
	MaxToolTokens int      `json:"maxToolTokens,omitempty"`
	MatchKeywords []string `json:"matchKeywords,omitempty"`
	Created       int64    `json:"created,omitempty"`
}

// ModelRegistryConfig controls dynamic model mappings and per-account negative caching.
type ModelRegistryConfig struct {
	NegativeCacheTTLSeconds int          `json:"negativeCacheTtlSeconds"`
	Models                  []ModelEntry `json:"models,omitempty"`
}

// HealthConfig controls readiness thresholds and optional webhook notifications.
type HealthConfig struct {
	MinReadyAccounts       int     `json:"minReadyAccounts"`
	MinReadyRatio          float64 `json:"minReadyRatio"`
	WebhookEnabled         bool    `json:"webhookEnabled"`
	WebhookURL             string  `json:"webhookUrl,omitempty"`
	WebhookCooldownSeconds int     `json:"webhookCooldownSeconds"`
}

// DiagnosticConfig controls optional failure/request diagnostic logging.
type DiagnosticConfig struct {
	Enabled               bool `json:"enabled"`
	IncludeRequestSummary bool `json:"includeRequestSummary"`
	MaxEntries            int  `json:"maxEntries"`
}

const (
	DefaultRequestLogMaxEntries = 1000
	MinRequestLogMaxEntries     = 100
	MaxRequestLogMaxEntries     = 20000

	DefaultRequestDetailMaxEntries = 100
	MinRequestDetailMaxEntries     = 1
	MaxRequestDetailMaxEntries     = 1000
	DefaultRequestDetailMaxBytes   = 256 << 10
	MinRequestDetailMaxBytes       = 16 << 10
	MaxRequestDetailMaxBytes       = 1 << 20
)

// RequestLogConfig controls the bounded, persisted recent-request history.
type RequestLogConfig struct {
	MaxEntries         int  `json:"maxEntries"`
	DetailedLogEnabled bool `json:"detailedLogEnabled"`
	DetailedMaxEntries int  `json:"detailedMaxEntries"`
	MaxDetailBytes     int  `json:"maxDetailBytes"`
}

// WebSearchConfig controls the Anthropic web_search compatibility shim.
type WebSearchConfig struct {
	Enabled bool `json:"enabled"`
}

// CountTokensProviderConfig controls the optional remote count_tokens provider.
type CountTokensProviderConfig struct {
	Enabled  bool   `json:"enabled"`
	ApiURL   string `json:"apiUrl"`
	ApiKey   string `json:"apiKey,omitempty"`
	AuthType string `json:"authType"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix              string `json:"thinkingSuffix,omitempty"`              // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat        string `json:"openaiThinkingFormat,omitempty"`        // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat        string `json:"claudeThinkingFormat,omitempty"`        // Claude output format: "reasoning_content", "thinking", or "think"
	ThinkingDefaultBudgetTokens int    `json:"thinkingDefaultBudgetTokens,omitempty"` // Default fake-reasoning budget when the client does not provide one
	ThinkingBudgetCapTokens     *int   `json:"thinkingBudgetCapTokens,omitempty"`     // Maximum proxy-derived fake-reasoning budget; 0 disables the cap
	DefaultMaxOutputTokens      int    `json:"defaultMaxOutputTokens,omitempty"`      // Default max output tokens when the client omits a limit; 0 leaves it unset
	DefaultContextWindowTokens  int    `json:"defaultContextWindowTokens,omitempty"`  // Default context window when the client/model omits one; 0 auto-detects
	ToolStreamMode              string `json:"toolStreamMode,omitempty"`              // Claude tool stream mode: safe, adaptive, balanced, or live
	BufferToolStreams           *bool  `json:"bufferToolStreams,omitempty"`           // Deprecated compatibility field: true maps to buffered/safe, false maps to live
	EnforceAgentToolUse         *bool  `json:"enforceAgentToolUse,omitempty"`         // Require tools for detected workspace mutation/execution requests

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// LoadBalancingMode controls account routing: "weighted" (default),
	// "priority", or "balanced".
	LoadBalancingMode string `json:"loadBalancingMode,omitempty"`

	// AutoRefresh controls background account info/token refresh.
	AutoRefresh AutoRefreshConfig `json:"autoRefresh,omitempty"`

	// Retry controls the total work a single client request may trigger upstream.
	Retry RetryConfig `json:"retry,omitempty"`

	// LongTool controls protection and recovery for large tool-call arguments.
	LongTool LongToolConfig `json:"longTool,omitempty"`

	// ResponsesStorage controls local /v1/responses persistence and history expansion.
	ResponsesStorage ResponsesStorageConfig `json:"responsesStorage,omitempty"`

	// ModelRegistry provides hot-reloadable model aliases and metadata.
	ModelRegistry ModelRegistryConfig `json:"modelRegistry,omitempty"`

	// Health controls readiness and production notifications.
	Health HealthConfig `json:"health,omitempty"`

	// Diagnostics controls optional failure diagnostics.
	Diagnostics DiagnosticConfig `json:"diagnostics,omitempty"`

	// RequestLog controls persisted recent-request metadata.
	RequestLog RequestLogConfig `json:"requestLog,omitempty"`

	// WebSearch controls the optional Anthropic web_search shim.
	WebSearch WebSearchConfig `json:"webSearch,omitempty"`

	// CountTokensProvider controls optional remote count_tokens calls.
	CountTokensProvider CountTokensProviderConfig `json:"countTokensProvider,omitempty"`

	// Legacy Rust-compatible count_tokens provider fields. They are migrated
	// into CountTokensProvider on load and kept for JSON compatibility only.
	CountTokensApiURL   string `json:"countTokensApiUrl,omitempty"`
	CountTokensApiKey   string `json:"countTokensApiKey,omitempty"`
	CountTokensAuthType string `json:"countTokensAuthType,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Use "direct" to explicitly connect without a proxy. Empty inherits the
	// process HTTP_PROXY/HTTPS_PROXY environment.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// UpstreamProtection prevents local traffic from overloading a single upstream account/model.
	UpstreamProtection UpstreamProtectionConfig `json:"upstreamProtection,omitempty"`

	// Prompt/KV cache simulation settings.
	PromptCacheEnabled        bool    `json:"promptCacheEnabled"`
	PromptCacheNamespaceMode  string  `json:"promptCacheNamespaceMode,omitempty"`
	CacheReadEfficiency       float64 `json:"cacheReadEfficiency"`
	CacheReadEfficiencyMin    float64 `json:"cacheReadEfficiencyMin"`
	CacheReadEfficiencyMax    float64 `json:"cacheReadEfficiencyMax"`
	KvCacheTTLSecs            int64   `json:"kvCacheTtlSecs"`
	CacheMaxEntriesPerAccount int     `json:"cacheMaxEntriesPerAccount,omitempty"`
	CacheMaxEntriesTotal      int     `json:"cacheMaxEntriesTotal,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

const (
	ToolStreamModeSafe     = "safe"
	ToolStreamModeAdaptive = "adaptive"
	ToolStreamModeBalanced = "balanced"
	ToolStreamModeLive     = "live"
)

// Version current version
const Version = "1.2.24"

var (
	cfg           *Config
	cfgLock       sync.RWMutex
	cfgPath       string
	cfgGeneration uint64
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfgPath = path
	cfgGeneration++
	return loadLocked()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return loadLocked()
}

func loadLocked() error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			defaultPassword, hashErr := hashAdminPassword("changeme")
			if hashErr != nil {
				return hashErr
			}
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:                  defaultPassword,
				Port:                      8080,
				Host:                      "0.0.0.0",
				ProxyURL:                  "direct",
				RequireApiKey:             false,
				Accounts:                  []Account{},
				UpstreamProtection:        defaultUpstreamProtectionConfig(),
				PromptCacheEnabled:        defaultPromptCacheConfig().Enabled,
				PromptCacheNamespaceMode:  defaultPromptCacheConfig().NamespaceMode,
				CacheReadEfficiency:       defaultPromptCacheConfig().CacheReadEfficiency,
				CacheReadEfficiencyMin:    defaultPromptCacheConfig().CacheReadEfficiencyMin,
				CacheReadEfficiencyMax:    defaultPromptCacheConfig().CacheReadEfficiencyMax,
				KvCacheTTLSecs:            defaultPromptCacheConfig().KvCacheTTLSecs,
				CacheMaxEntriesPerAccount: defaultPromptCacheConfig().MaxEntriesPerAccount,
				CacheMaxEntriesTotal:      defaultPromptCacheConfig().MaxEntriesTotal,
				LoadBalancingMode:         defaultRoutingConfig().LoadBalancingMode,
				AutoRefresh:               defaultAutoRefreshConfig(),
				Retry:                     defaultRetryConfig(),
				LongTool:                  defaultLongToolConfig(),
				ResponsesStorage:          defaultResponsesStorageConfig(),
				ModelRegistry:             defaultModelRegistryConfig(),
				Health:                    defaultHealthConfig(),
				Diagnostics:               defaultDiagnosticConfig(),
				RequestLog:                defaultRequestLogConfig(),
				WebSearch:                 defaultWebSearchConfig(),
				CountTokensProvider:       defaultCountTokensProviderConfig(),
				ToolStreamMode:            ToolStreamModeSafe,
			}
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	secretsMigrated, err := prepareLoadedConfigSecrets(&c)
	if err != nil {
		return err
	}
	passwordMigrated := false
	if strings.TrimSpace(c.Password) == "" {
		c.Password = "changeme"
	}
	if !isAdminPasswordHash(c.Password) {
		hashed, hashErr := hashAdminPassword(c.Password)
		if hashErr != nil {
			return fmt.Errorf("hash admin password: %w", hashErr)
		}
		c.Password = hashed
		passwordMigrated = true
	}
	if !rawConfigHasKey(data, "upstreamProtection") {
		c.UpstreamProtection.Enabled = true
	}
	if !rawConfigHasKey(data, "promptCacheEnabled") {
		c.PromptCacheEnabled = defaultPromptCacheConfig().Enabled
	}
	if !rawConfigHasKey(data, "promptCacheNamespaceMode") {
		c.PromptCacheNamespaceMode = defaultPromptCacheConfig().NamespaceMode
	}
	if !rawConfigHasKey(data, "cacheReadEfficiency") {
		c.CacheReadEfficiency = defaultPromptCacheConfig().CacheReadEfficiency
	}
	if !rawConfigHasKey(data, "cacheReadEfficiencyMin") && !rawConfigHasKey(data, "cacheReadEfficiencyMax") {
		c.CacheReadEfficiencyMin = c.CacheReadEfficiency
		c.CacheReadEfficiencyMax = c.CacheReadEfficiency
	} else {
		if !rawConfigHasKey(data, "cacheReadEfficiencyMin") {
			c.CacheReadEfficiencyMin = c.CacheReadEfficiencyMax
		}
		if !rawConfigHasKey(data, "cacheReadEfficiencyMax") {
			c.CacheReadEfficiencyMax = c.CacheReadEfficiencyMin
		}
	}
	if !rawConfigHasKey(data, "kvCacheTtlSecs") {
		c.KvCacheTTLSecs = defaultPromptCacheConfig().KvCacheTTLSecs
	}
	if !rawConfigHasKey(data, "cacheMaxEntriesPerAccount") {
		c.CacheMaxEntriesPerAccount = defaultPromptCacheConfig().MaxEntriesPerAccount
	}
	if !rawConfigHasKey(data, "cacheMaxEntriesTotal") {
		c.CacheMaxEntriesTotal = defaultPromptCacheConfig().MaxEntriesTotal
	}
	if !rawConfigHasKey(data, "loadBalancingMode") {
		c.LoadBalancingMode = defaultRoutingConfig().LoadBalancingMode
	}
	if !rawConfigHasKey(data, "autoRefresh") {
		c.AutoRefresh = defaultAutoRefreshConfig()
	} else if !rawConfigHasNestedKey(data, "autoRefresh", "refreshModels") {
		c.AutoRefresh.RefreshModels = defaultAutoRefreshConfig().RefreshModels
	}
	if !rawConfigHasKey(data, "retry") {
		c.Retry = defaultRetryConfig()
	} else {
		defaults := defaultRetryConfig()
		if !rawConfigHasNestedKey(data, "retry", "maxAccountAttempts") {
			c.Retry.MaxAccountAttempts = defaults.MaxAccountAttempts
		}
		if !rawConfigHasNestedKey(data, "retry", "maxRetryDurationSeconds") {
			c.Retry.MaxRetryDurationSeconds = defaults.MaxRetryDurationSeconds
		}
		if !rawConfigHasNestedKey(data, "retry", "toolAssemblyTimeoutSeconds") {
			c.Retry.ToolAssemblyTimeoutSeconds = defaults.ToolAssemblyTimeoutSeconds
		}
	}
	if !rawConfigHasKey(data, "longTool") {
		c.LongTool = defaultLongToolConfig()
	}
	if !rawConfigHasKey(data, "responsesStorage") {
		c.ResponsesStorage = defaultResponsesStorageConfig()
	}
	if !rawConfigHasKey(data, "modelRegistry") {
		c.ModelRegistry = defaultModelRegistryConfig()
	}
	if !rawConfigHasKey(data, "health") {
		c.Health = defaultHealthConfig()
	}
	if !rawConfigHasKey(data, "diagnostics") {
		c.Diagnostics = defaultDiagnosticConfig()
	}
	if !rawConfigHasKey(data, "requestLog") {
		c.RequestLog = defaultRequestLogConfig()
	}
	if !rawConfigHasKey(data, "webSearch") {
		c.WebSearch = defaultWebSearchConfig()
	}
	if !rawConfigHasKey(data, "countTokensProvider") {
		c.CountTokensProvider = defaultCountTokensProviderConfig()
	}
	if !rawConfigHasKey(data, "proxyURL") {
		c.ProxyURL = "direct"
	}
	if err := validateOutboundProxyConfig(&c); err != nil {
		return err
	}
	cfg = &c
	migrateCountTokensProviderLocked()
	normalizeUpstreamProtectionLocked()
	normalizePromptCacheLocked()
	normalizeRoutingLocked()
	normalizeAutoRefreshLocked()
	normalizeRetryLocked()
	normalizeLongToolLocked()
	normalizeResponsesStorageLocked()
	normalizeModelRegistryLocked()
	normalizeHealthLocked()
	normalizeDiagnosticLocked()
	normalizeRequestLogLocked()
	normalizeCountTokensProviderLocked()

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	fingerprintMigrated := false
	for i := range cfg.Accounts {
		hadFingerprint := cfg.Accounts[i].CredentialFingerprint != ""
		normalizeKiroAPIKeyAccount(&cfg.Accounts[i])
		if !hadFingerprint && cfg.Accounts[i].CredentialFingerprint != "" {
			fingerprintMigrated = true
		}
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated || fingerprintMigrated || passwordMigrated || secretsMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

func adminPasswordInput(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	encoded := base64.RawStdEncoding.EncodeToString(sum[:])
	return []byte(encoded)
}

func hashAdminPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword(adminPasswordInput(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func isAdminPasswordHash(password string) bool {
	return strings.HasPrefix(password, "$2a$") || strings.HasPrefix(password, "$2b$") || strings.HasPrefix(password, "$2y$")
}

// GetGeneration identifies the currently initialized configuration instance.
// It changes only when Init switches the process to another config path.
func GetGeneration() uint64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfgGeneration
}

func normalizeKiroAPIKeyAccount(account *Account) {
	if account == nil {
		return
	}
	if account.Weight < 0 {
		account.Weight = 0
	}
	if account.Weight > 100 {
		account.Weight = 100
	}
	if account.MaxConcurrency < 0 {
		account.MaxConcurrency = 0
	}
	if account.MaxConcurrency > 1000 {
		account.MaxConcurrency = 1000
	}
	if strings.EqualFold(account.AuthMethod, "api_key") || strings.EqualFold(account.AuthMethod, "apikey") || account.KiroApiKey != "" {
		account.AuthMethod = "api_key"
		if account.KiroApiKey == "" {
			account.KiroApiKey = account.AccessToken
		}
		if account.AccessToken == "" {
			account.AccessToken = account.KiroApiKey
		}
		account.RefreshToken = ""
		account.ExpiresAt = 0
	}
	normalizeAccountCredentialFingerprint(account)
}

func normalizeAccountCredentialFingerprint(account *Account) {
	if account == nil || strings.TrimSpace(account.CredentialFingerprint) != "" {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	var material string
	switch {
	case strings.TrimSpace(account.KiroApiKey) != "":
		material = "api_key\x00" + account.KiroApiKey
	case strings.TrimSpace(account.UserId) != "":
		material = "user\x00" + provider + "\x00" + account.UserId
	case strings.TrimSpace(account.Email) != "":
		material = "email\x00" + provider + "\x00" + strings.ToLower(strings.TrimSpace(account.Email))
	case strings.TrimSpace(account.RefreshToken) != "":
		material = "refresh\x00" + account.RefreshToken
	}
	if material == "" {
		return
	}
	sum := sha256.Sum256([]byte(material))
	account.CredentialFingerprint = base64.RawURLEncoding.EncodeToString(sum[:])
}

func rawConfigHasKey(data []byte, key string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, ok := raw[key]
	return ok
}

func rawConfigHasNestedKey(data []byte, parent, key string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	child, ok := raw[parent]
	if !ok {
		return false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(child, &nested); err != nil {
		return false
	}
	_, ok = nested[key]
	return ok
}

func defaultUpstreamProtectionConfig() UpstreamProtectionConfig {
	return UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountConcurrency:      10,
		MaxPerAccountModelConcurrency: 5,
		PerModelConcurrency:           map[string]int{},
		PerProfileModelConcurrency:    map[string]map[string]int{},
		RateLimitCooldownMs:           2000,
		MaxRateLimitCooldownMs:        60000,
		RouteAffinityTTLSeconds:       3600,
		RouteAffinityMaxEntries:       20000,
	}
}

func normalizeUpstreamProtectionLocked() {
	defaults := defaultUpstreamProtectionConfig()
	up := cfg.UpstreamProtection
	if up.MaxPerAccountConcurrency <= 0 {
		up.MaxPerAccountConcurrency = defaults.MaxPerAccountConcurrency
	}
	if up.MaxPerAccountConcurrency > 10000 {
		up.MaxPerAccountConcurrency = 10000
	}
	if up.MaxPerAccountModelConcurrency <= 0 {
		up.MaxPerAccountModelConcurrency = defaults.MaxPerAccountModelConcurrency
	}
	if up.MaxPerAccountModelConcurrency > 10000 {
		up.MaxPerAccountModelConcurrency = 10000
	}
	if up.PerModelConcurrency == nil {
		up.PerModelConcurrency = map[string]int{}
	}
	normalizedPerModel := make(map[string]int, len(up.PerModelConcurrency))
	for model, limit := range up.PerModelConcurrency {
		modelKey := strings.ToLower(strings.TrimSpace(model))
		if modelKey != "" && limit > 0 {
			if limit > 10000 {
				limit = 10000
			}
			normalizedPerModel[modelKey] = limit
		}
	}
	up.PerModelConcurrency = normalizedPerModel
	if up.PerProfileModelConcurrency == nil {
		up.PerProfileModelConcurrency = map[string]map[string]int{}
	}
	normalizedProfiles := make(map[string]map[string]int, len(up.PerProfileModelConcurrency))
	for profile, limits := range up.PerProfileModelConcurrency {
		profileKey := strings.TrimSpace(profile)
		if profileKey == "" {
			continue
		}
		normalized := make(map[string]int)
		for model, limit := range limits {
			modelKey := strings.ToLower(strings.TrimSpace(model))
			if modelKey == "" || limit <= 0 {
				continue
			}
			if limit > 10000 {
				limit = 10000
			}
			normalized[modelKey] = limit
		}
		if len(normalized) > 0 {
			normalizedProfiles[profileKey] = normalized
		}
	}
	up.PerProfileModelConcurrency = normalizedProfiles
	if up.RateLimitCooldownMs <= 0 {
		up.RateLimitCooldownMs = defaults.RateLimitCooldownMs
	}
	if up.RateLimitCooldownMs > 3600000 {
		up.RateLimitCooldownMs = 3600000
	}
	if up.MaxRateLimitCooldownMs <= 0 {
		up.MaxRateLimitCooldownMs = defaults.MaxRateLimitCooldownMs
	}
	if up.MaxRateLimitCooldownMs < up.RateLimitCooldownMs {
		up.MaxRateLimitCooldownMs = up.RateLimitCooldownMs
	}
	if up.MaxRateLimitCooldownMs > 86400000 {
		up.MaxRateLimitCooldownMs = 86400000
	}
	if up.RouteAffinityTTLSeconds <= 0 {
		up.RouteAffinityTTLSeconds = defaults.RouteAffinityTTLSeconds
	}
	if up.RouteAffinityTTLSeconds > 604800 {
		up.RouteAffinityTTLSeconds = 604800
	}
	if up.RouteAffinityMaxEntries <= 0 {
		up.RouteAffinityMaxEntries = defaults.RouteAffinityMaxEntries
	}
	if up.RouteAffinityMaxEntries > 1000000 {
		up.RouteAffinityMaxEntries = 1000000
	}
	cfg.UpstreamProtection = up
}

func defaultPromptCacheConfig() PromptCacheConfig {
	return PromptCacheConfig{
		Enabled:                true,
		NamespaceMode:          PromptCacheNamespaceAccount,
		CacheReadEfficiency:    0.87,
		CacheReadEfficiencyMin: 0.87,
		CacheReadEfficiencyMax: 0.87,
		KvCacheTTLSecs:         3600,
		MaxEntriesPerAccount:   2048,
		MaxEntriesTotal:        50000,
	}
}

func defaultRoutingConfig() RoutingConfig {
	return RoutingConfig{LoadBalancingMode: "weighted"}
}

func normalizeRoutingLocked() {
	mode := strings.ToLower(strings.TrimSpace(cfg.LoadBalancingMode))
	switch mode {
	case "priority", "balanced", "weighted":
		cfg.LoadBalancingMode = mode
	default:
		cfg.LoadBalancingMode = defaultRoutingConfig().LoadBalancingMode
	}
}

func defaultAutoRefreshConfig() AutoRefreshConfig {
	return AutoRefreshConfig{
		Enabled:                   true,
		IntervalMinutes:           30,
		TokenRefreshBeforeSeconds: 120,
		MaxAccountsPerRun:         0,
		FailureCooldownSeconds:    300,
		RefreshConcurrency:        5,
		RefreshQueueCapacity:      1000,
		RefreshTaskTimeoutSeconds: 60,
		RefreshJitterSeconds:      30,
		RefreshModels:             true,
		ModelIntervalMinutes:      60,
		MaxModelsPerRun:           25,
		ModelRefreshConcurrency:   3,
	}
}

func normalizeAutoRefreshLocked() {
	defaults := defaultAutoRefreshConfig()
	if cfg.AutoRefresh.IntervalMinutes <= 0 {
		cfg.AutoRefresh.IntervalMinutes = defaults.IntervalMinutes
	}
	if cfg.AutoRefresh.TokenRefreshBeforeSeconds <= 0 {
		cfg.AutoRefresh.TokenRefreshBeforeSeconds = defaults.TokenRefreshBeforeSeconds
	}
	if cfg.AutoRefresh.FailureCooldownSeconds <= 0 {
		cfg.AutoRefresh.FailureCooldownSeconds = defaults.FailureCooldownSeconds
	}
	if cfg.AutoRefresh.MaxAccountsPerRun < 0 {
		cfg.AutoRefresh.MaxAccountsPerRun = 0
	}
	if cfg.AutoRefresh.RefreshConcurrency <= 0 {
		cfg.AutoRefresh.RefreshConcurrency = defaults.RefreshConcurrency
	}
	if cfg.AutoRefresh.RefreshConcurrency > 50 {
		cfg.AutoRefresh.RefreshConcurrency = 50
	}
	if cfg.AutoRefresh.RefreshQueueCapacity <= 0 {
		cfg.AutoRefresh.RefreshQueueCapacity = defaults.RefreshQueueCapacity
	}
	if cfg.AutoRefresh.RefreshQueueCapacity > 100000 {
		cfg.AutoRefresh.RefreshQueueCapacity = 100000
	}
	if cfg.AutoRefresh.RefreshTaskTimeoutSeconds < 10 {
		cfg.AutoRefresh.RefreshTaskTimeoutSeconds = defaults.RefreshTaskTimeoutSeconds
	}
	if cfg.AutoRefresh.RefreshTaskTimeoutSeconds > 600 {
		cfg.AutoRefresh.RefreshTaskTimeoutSeconds = 600
	}
	if cfg.AutoRefresh.RefreshJitterSeconds < 0 {
		cfg.AutoRefresh.RefreshJitterSeconds = 0
	}
	if cfg.AutoRefresh.RefreshJitterSeconds > 3600 {
		cfg.AutoRefresh.RefreshJitterSeconds = 3600
	}
	if cfg.AutoRefresh.ModelIntervalMinutes < 30 {
		cfg.AutoRefresh.ModelIntervalMinutes = defaults.ModelIntervalMinutes
	}
	if cfg.AutoRefresh.ModelIntervalMinutes > 10080 {
		cfg.AutoRefresh.ModelIntervalMinutes = 10080
	}
	if cfg.AutoRefresh.MaxModelsPerRun < 0 {
		cfg.AutoRefresh.MaxModelsPerRun = 0
	}
	if cfg.AutoRefresh.ModelRefreshConcurrency < 1 {
		cfg.AutoRefresh.ModelRefreshConcurrency = defaults.ModelRefreshConcurrency
	}
	if cfg.AutoRefresh.ModelRefreshConcurrency > 20 {
		cfg.AutoRefresh.ModelRefreshConcurrency = 20
	}
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAccountAttempts:             8,
		MaxUpstreamAttempts:            12,
		MaxRetryDurationSeconds:        900,
		FirstTokenTimeoutSeconds:       45,
		StreamIdleTimeoutSeconds:       120,
		ToolAssemblyTimeoutSeconds:     180,
		EmptyResponseRetries:           2,
		EndpointFailureThreshold:       3,
		EndpointCircuitCooldownSeconds: 30,
		ProxyFailureThreshold:          3,
		ProxyCircuitCooldownSeconds:    60,
	}
}

func normalizeRetryLocked() {
	defaults := defaultRetryConfig()
	if cfg.Retry.MaxAccountAttempts < 0 {
		cfg.Retry.MaxAccountAttempts = defaults.MaxAccountAttempts
	}
	if cfg.Retry.MaxAccountAttempts > 100 {
		cfg.Retry.MaxAccountAttempts = 100
	}
	if cfg.Retry.MaxUpstreamAttempts <= 0 {
		cfg.Retry.MaxUpstreamAttempts = defaults.MaxUpstreamAttempts
	}
	if cfg.Retry.MaxUpstreamAttempts > 200 {
		cfg.Retry.MaxUpstreamAttempts = 200
	}
	if cfg.Retry.MaxRetryDurationSeconds < 0 {
		cfg.Retry.MaxRetryDurationSeconds = defaults.MaxRetryDurationSeconds
	}
	if cfg.Retry.MaxRetryDurationSeconds > 86400 {
		cfg.Retry.MaxRetryDurationSeconds = 86400
	}
	if cfg.Retry.FirstTokenTimeoutSeconds < 5 {
		cfg.Retry.FirstTokenTimeoutSeconds = defaults.FirstTokenTimeoutSeconds
	}
	if cfg.Retry.FirstTokenTimeoutSeconds > 600 {
		cfg.Retry.FirstTokenTimeoutSeconds = 600
	}
	if cfg.Retry.StreamIdleTimeoutSeconds < 15 {
		cfg.Retry.StreamIdleTimeoutSeconds = defaults.StreamIdleTimeoutSeconds
	}
	if cfg.Retry.StreamIdleTimeoutSeconds > 3600 {
		cfg.Retry.StreamIdleTimeoutSeconds = 3600
	}
	if cfg.Retry.ToolAssemblyTimeoutSeconds < 0 {
		cfg.Retry.ToolAssemblyTimeoutSeconds = defaults.ToolAssemblyTimeoutSeconds
	}
	if cfg.Retry.ToolAssemblyTimeoutSeconds > 3600 {
		cfg.Retry.ToolAssemblyTimeoutSeconds = 3600
	}
	if cfg.Retry.EmptyResponseRetries < 0 {
		cfg.Retry.EmptyResponseRetries = 0
	}
	if cfg.Retry.EmptyResponseRetries > 20 {
		cfg.Retry.EmptyResponseRetries = 20
	}
	if cfg.Retry.EndpointFailureThreshold < 1 {
		cfg.Retry.EndpointFailureThreshold = defaults.EndpointFailureThreshold
	}
	if cfg.Retry.EndpointFailureThreshold > 20 {
		cfg.Retry.EndpointFailureThreshold = 20
	}
	if cfg.Retry.EndpointCircuitCooldownSeconds < 5 {
		cfg.Retry.EndpointCircuitCooldownSeconds = defaults.EndpointCircuitCooldownSeconds
	}
	if cfg.Retry.EndpointCircuitCooldownSeconds > 900 {
		cfg.Retry.EndpointCircuitCooldownSeconds = 900
	}
	if cfg.Retry.ProxyFailureThreshold < 1 {
		cfg.Retry.ProxyFailureThreshold = defaults.ProxyFailureThreshold
	}
	if cfg.Retry.ProxyFailureThreshold > 20 {
		cfg.Retry.ProxyFailureThreshold = 20
	}
	if cfg.Retry.ProxyCircuitCooldownSeconds < 5 {
		cfg.Retry.ProxyCircuitCooldownSeconds = defaults.ProxyCircuitCooldownSeconds
	}
	if cfg.Retry.ProxyCircuitCooldownSeconds > 900 {
		cfg.Retry.ProxyCircuitCooldownSeconds = 900
	}
}

func defaultResponsesStorageConfig() ResponsesStorageConfig {
	return ResponsesStorageConfig{
		DefaultStore:      false,
		TTLHours:          30 * 24,
		MaxFiles:          10000,
		MaxBytes:          1 << 30,
		MaxHistoryBytes:   4 << 20,
		GCIntervalMinutes: 60,
	}
}

func defaultLongToolConfig() LongToolConfig {
	return LongToolConfig{
		Enabled:              true,
		DefaultMaxToolTokens: 8192,
		TruncationRetries:    1,
		FallbackEnabled:      false,
		FallbackModel:        "claude-sonnet-5",
	}
}

func normalizeLongToolLocked() {
	defaults := defaultLongToolConfig()
	value := cfg.LongTool
	if value.DefaultMaxToolTokens < 1024 {
		value.DefaultMaxToolTokens = defaults.DefaultMaxToolTokens
	}
	if value.DefaultMaxToolTokens > 128000 {
		value.DefaultMaxToolTokens = 128000
	}
	if value.TruncationRetries < 0 {
		value.TruncationRetries = defaults.TruncationRetries
	}
	if value.TruncationRetries > 5 {
		value.TruncationRetries = 5
	}
	value.FallbackModel = strings.TrimSpace(value.FallbackModel)
	if value.FallbackModel == "" {
		value.FallbackModel = defaults.FallbackModel
	}
	cfg.LongTool = value
}

func normalizeResponsesStorageLocked() {
	defaults := defaultResponsesStorageConfig()
	storage := cfg.ResponsesStorage
	if storage.TTLHours < 1 {
		storage.TTLHours = defaults.TTLHours
	}
	if storage.TTLHours > 8760 {
		storage.TTLHours = 8760
	}
	if storage.MaxFiles < 1 {
		storage.MaxFiles = defaults.MaxFiles
	}
	if storage.MaxFiles > 1000000 {
		storage.MaxFiles = 1000000
	}
	if storage.MaxBytes < 1<<20 {
		storage.MaxBytes = defaults.MaxBytes
	}
	if storage.MaxBytes > 1<<40 {
		storage.MaxBytes = 1 << 40
	}
	if storage.MaxHistoryBytes < 64<<10 {
		storage.MaxHistoryBytes = defaults.MaxHistoryBytes
	}
	if storage.MaxHistoryBytes > 64<<20 {
		storage.MaxHistoryBytes = 64 << 20
	}
	if storage.GCIntervalMinutes < 1 {
		storage.GCIntervalMinutes = defaults.GCIntervalMinutes
	}
	if storage.GCIntervalMinutes > 1440 {
		storage.GCIntervalMinutes = 1440
	}
	cfg.ResponsesStorage = storage
}

func defaultModelRegistryConfig() ModelRegistryConfig {
	return ModelRegistryConfig{NegativeCacheTTLSeconds: 3600, Models: []ModelEntry{}}
}

func normalizeModelRegistryLocked() {
	defaults := defaultModelRegistryConfig()
	if cfg.ModelRegistry.NegativeCacheTTLSeconds < 60 {
		cfg.ModelRegistry.NegativeCacheTTLSeconds = defaults.NegativeCacheTTLSeconds
	}
	if cfg.ModelRegistry.NegativeCacheTTLSeconds > 7*24*3600 {
		cfg.ModelRegistry.NegativeCacheTTLSeconds = 7 * 24 * 3600
	}
	if cfg.ModelRegistry.Models == nil {
		cfg.ModelRegistry.Models = []ModelEntry{}
	}
	for i := range cfg.ModelRegistry.Models {
		normalizeModelEntry(&cfg.ModelRegistry.Models[i])
	}
}

func normalizeModelEntry(entry *ModelEntry) {
	if entry == nil {
		return
	}
	entry.ID = strings.TrimSpace(entry.ID)
	entry.DisplayName = strings.TrimSpace(entry.DisplayName)
	entry.KiroModelID = strings.TrimSpace(entry.KiroModelID)
	if entry.DisplayName == "" {
		entry.DisplayName = entry.ID
	}
	if entry.ContextWindow <= 0 {
		entry.ContextWindow = 200000
	}
	if entry.MaxTokens <= 0 {
		entry.MaxTokens = 64000
	}
	if entry.MaxToolTokens < 0 {
		entry.MaxToolTokens = 0
	}
	if entry.MaxToolTokens > 128000 {
		entry.MaxToolTokens = 128000
	}
	if entry.Created <= 0 {
		entry.Created = time.Now().Unix()
	}
	seen := make(map[string]bool)
	keywords := make([]string, 0, len(entry.MatchKeywords))
	for _, keyword := range entry.MatchKeywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword == "" || seen[keyword] {
			continue
		}
		seen[keyword] = true
		keywords = append(keywords, keyword)
	}
	entry.MatchKeywords = keywords
}

func defaultHealthConfig() HealthConfig {
	return HealthConfig{
		MinReadyAccounts:       1,
		MinReadyRatio:          0,
		WebhookCooldownSeconds: 300,
	}
}

func normalizeHealthLocked() {
	defaults := defaultHealthConfig()
	if cfg.Health.MinReadyAccounts < 0 {
		cfg.Health.MinReadyAccounts = 0
	}
	cfg.Health.MinReadyRatio = clampFloatConfig(cfg.Health.MinReadyRatio, 0, 1)
	cfg.Health.WebhookURL = strings.TrimSpace(cfg.Health.WebhookURL)
	if cfg.Health.WebhookCooldownSeconds < 10 {
		cfg.Health.WebhookCooldownSeconds = defaults.WebhookCooldownSeconds
	}
	if cfg.Health.WebhookCooldownSeconds > 86400 {
		cfg.Health.WebhookCooldownSeconds = 86400
	}
	if cfg.Health.WebhookURL == "" {
		cfg.Health.WebhookEnabled = false
	}
}

func defaultDiagnosticConfig() DiagnosticConfig {
	return DiagnosticConfig{
		Enabled:               false,
		IncludeRequestSummary: false,
		MaxEntries:            200,
	}
}

func normalizeDiagnosticLocked() {
	defaults := defaultDiagnosticConfig()
	if cfg.Diagnostics.MaxEntries <= 0 {
		cfg.Diagnostics.MaxEntries = defaults.MaxEntries
	}
	if cfg.Diagnostics.MaxEntries > 2000 {
		cfg.Diagnostics.MaxEntries = 2000
	}
}

func defaultRequestLogConfig() RequestLogConfig {
	return RequestLogConfig{
		MaxEntries:         DefaultRequestLogMaxEntries,
		DetailedLogEnabled: false,
		DetailedMaxEntries: DefaultRequestDetailMaxEntries,
		MaxDetailBytes:     DefaultRequestDetailMaxBytes,
	}
}

func normalizeRequestLogLocked() {
	if cfg.RequestLog.MaxEntries <= 0 {
		cfg.RequestLog.MaxEntries = DefaultRequestLogMaxEntries
	} else if cfg.RequestLog.MaxEntries < MinRequestLogMaxEntries {
		cfg.RequestLog.MaxEntries = MinRequestLogMaxEntries
	}
	if cfg.RequestLog.MaxEntries > MaxRequestLogMaxEntries {
		cfg.RequestLog.MaxEntries = MaxRequestLogMaxEntries
	}
	if cfg.RequestLog.DetailedMaxEntries <= 0 {
		cfg.RequestLog.DetailedMaxEntries = DefaultRequestDetailMaxEntries
	} else if cfg.RequestLog.DetailedMaxEntries < MinRequestDetailMaxEntries {
		cfg.RequestLog.DetailedMaxEntries = MinRequestDetailMaxEntries
	}
	if cfg.RequestLog.DetailedMaxEntries > MaxRequestDetailMaxEntries {
		cfg.RequestLog.DetailedMaxEntries = MaxRequestDetailMaxEntries
	}
	if cfg.RequestLog.MaxDetailBytes <= 0 {
		cfg.RequestLog.MaxDetailBytes = DefaultRequestDetailMaxBytes
	} else if cfg.RequestLog.MaxDetailBytes < MinRequestDetailMaxBytes {
		cfg.RequestLog.MaxDetailBytes = MinRequestDetailMaxBytes
	}
	if cfg.RequestLog.MaxDetailBytes > MaxRequestDetailMaxBytes {
		cfg.RequestLog.MaxDetailBytes = MaxRequestDetailMaxBytes
	}
}

func defaultWebSearchConfig() WebSearchConfig {
	return WebSearchConfig{Enabled: false}
}

func defaultCountTokensProviderConfig() CountTokensProviderConfig {
	return CountTokensProviderConfig{
		Enabled:  false,
		AuthType: "x-api-key",
	}
}

func migrateCountTokensProviderLocked() {
	if cfg.CountTokensProvider.ApiURL == "" && cfg.CountTokensApiURL != "" {
		cfg.CountTokensProvider.ApiURL = cfg.CountTokensApiURL
		cfg.CountTokensProvider.Enabled = true
	}
	if cfg.CountTokensProvider.ApiKey == "" && cfg.CountTokensApiKey != "" {
		cfg.CountTokensProvider.ApiKey = cfg.CountTokensApiKey
	}
	if cfg.CountTokensProvider.AuthType == "" && cfg.CountTokensAuthType != "" {
		cfg.CountTokensProvider.AuthType = cfg.CountTokensAuthType
	}
}

func normalizeCountTokensProviderLocked() {
	cfg.CountTokensProvider.ApiURL = strings.TrimSpace(cfg.CountTokensProvider.ApiURL)
	cfg.CountTokensProvider.ApiKey = strings.TrimSpace(cfg.CountTokensProvider.ApiKey)
	authType := strings.ToLower(strings.TrimSpace(cfg.CountTokensProvider.AuthType))
	switch authType {
	case "bearer", "x-api-key":
		cfg.CountTokensProvider.AuthType = authType
	default:
		cfg.CountTokensProvider.AuthType = defaultCountTokensProviderConfig().AuthType
	}
	if cfg.CountTokensProvider.ApiURL == "" {
		cfg.CountTokensProvider.Enabled = false
	}
}

func normalizePromptCacheLocked() {
	switch strings.ToLower(strings.TrimSpace(cfg.PromptCacheNamespaceMode)) {
	case PromptCacheNamespaceAccountAPIKey:
		cfg.PromptCacheNamespaceMode = PromptCacheNamespaceAccountAPIKey
	default:
		cfg.PromptCacheNamespaceMode = PromptCacheNamespaceAccount
	}
	cfg.CacheReadEfficiency = clampFloatConfig(cfg.CacheReadEfficiency, 0, 1)
	cfg.CacheReadEfficiencyMin = clampFloatConfig(cfg.CacheReadEfficiencyMin, 0, 1)
	cfg.CacheReadEfficiencyMax = clampFloatConfig(cfg.CacheReadEfficiencyMax, 0, 1)
	if cfg.CacheReadEfficiencyMin > cfg.CacheReadEfficiencyMax {
		cfg.CacheReadEfficiencyMin, cfg.CacheReadEfficiencyMax = cfg.CacheReadEfficiencyMax, cfg.CacheReadEfficiencyMin
	}
	if cfg.CacheReadEfficiency == 0 && (cfg.CacheReadEfficiencyMin > 0 || cfg.CacheReadEfficiencyMax > 0) {
		cfg.CacheReadEfficiency = (cfg.CacheReadEfficiencyMin + cfg.CacheReadEfficiencyMax) / 2
	} else {
		cfg.CacheReadEfficiency = clampFloatConfig((cfg.CacheReadEfficiencyMin+cfg.CacheReadEfficiencyMax)/2, 0, 1)
	}
	if cfg.KvCacheTTLSecs < 60 {
		cfg.KvCacheTTLSecs = 60
	}
	defaults := defaultPromptCacheConfig()
	if cfg.CacheMaxEntriesPerAccount <= 0 {
		cfg.CacheMaxEntriesPerAccount = defaults.MaxEntriesPerAccount
	}
	if cfg.CacheMaxEntriesPerAccount > 100000 {
		cfg.CacheMaxEntriesPerAccount = 100000
	}
	if cfg.CacheMaxEntriesTotal <= 0 {
		cfg.CacheMaxEntriesTotal = defaults.MaxEntriesTotal
	}
	if cfg.CacheMaxEntriesTotal < cfg.CacheMaxEntriesPerAccount {
		cfg.CacheMaxEntriesTotal = cfg.CacheMaxEntriesPerAccount
	}
	if cfg.CacheMaxEntriesTotal > 1000000 {
		cfg.CacheMaxEntriesTotal = 1000000
	}
}

func clampFloatConfig(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
func Save() error {
	persisted, err := configForPersistence(cfg)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// SetPassword updates the in-memory admin password using a slow password hash.
// It is primarily used for environment variable overrides in containers.
func SetPassword(password string) error {
	hashed, err := hashAdminPassword(password)
	if err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = hashed
	return nil
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetUpstreamProtectionConfig() UpstreamProtectionConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	up := cfg.UpstreamProtection
	if up.MaxPerAccountConcurrency <= 0 ||
		up.MaxPerAccountModelConcurrency <= 0 ||
		up.PerModelConcurrency == nil ||
		up.PerProfileModelConcurrency == nil ||
		up.RateLimitCooldownMs <= 0 ||
		up.MaxRateLimitCooldownMs <= 0 ||
		up.RouteAffinityTTLSeconds <= 0 ||
		up.RouteAffinityMaxEntries <= 0 {
		defaults := defaultUpstreamProtectionConfig()
		if up.MaxPerAccountConcurrency <= 0 {
			up.MaxPerAccountConcurrency = defaults.MaxPerAccountConcurrency
		}
		if up.MaxPerAccountModelConcurrency <= 0 {
			up.MaxPerAccountModelConcurrency = defaults.MaxPerAccountModelConcurrency
		}
		if up.PerModelConcurrency == nil {
			up.PerModelConcurrency = map[string]int{}
		}
		if up.PerProfileModelConcurrency == nil {
			up.PerProfileModelConcurrency = map[string]map[string]int{}
		}
		if up.RateLimitCooldownMs <= 0 {
			up.RateLimitCooldownMs = defaults.RateLimitCooldownMs
		}
		if up.MaxRateLimitCooldownMs <= 0 {
			up.MaxRateLimitCooldownMs = defaults.MaxRateLimitCooldownMs
		}
		if up.RouteAffinityTTLSeconds <= 0 {
			up.RouteAffinityTTLSeconds = defaults.RouteAffinityTTLSeconds
		}
		if up.RouteAffinityMaxEntries <= 0 {
			up.RouteAffinityMaxEntries = defaults.RouteAffinityMaxEntries
		}
	}
	return up
}

func UpdateUpstreamProtectionConfig(up UpstreamProtectionConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.UpstreamProtection = up
	normalizeUpstreamProtectionLocked()
	return Save()
}

func GetPromptCacheConfig() PromptCacheConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultPromptCacheConfig()
	}
	out := PromptCacheConfig{
		Enabled:                cfg.PromptCacheEnabled,
		NamespaceMode:          cfg.PromptCacheNamespaceMode,
		CacheReadEfficiency:    cfg.CacheReadEfficiency,
		CacheReadEfficiencyMin: cfg.CacheReadEfficiencyMin,
		CacheReadEfficiencyMax: cfg.CacheReadEfficiencyMax,
		KvCacheTTLSecs:         cfg.KvCacheTTLSecs,
		MaxEntriesPerAccount:   cfg.CacheMaxEntriesPerAccount,
		MaxEntriesTotal:        cfg.CacheMaxEntriesTotal,
	}
	out.CacheReadEfficiency = clampFloatConfig(out.CacheReadEfficiency, 0, 1)
	out.CacheReadEfficiencyMin = clampFloatConfig(out.CacheReadEfficiencyMin, 0, 1)
	out.CacheReadEfficiencyMax = clampFloatConfig(out.CacheReadEfficiencyMax, 0, 1)
	if out.CacheReadEfficiencyMin > out.CacheReadEfficiencyMax {
		out.CacheReadEfficiencyMin, out.CacheReadEfficiencyMax = out.CacheReadEfficiencyMax, out.CacheReadEfficiencyMin
	}
	switch strings.ToLower(strings.TrimSpace(out.NamespaceMode)) {
	case PromptCacheNamespaceAccountAPIKey:
		out.NamespaceMode = PromptCacheNamespaceAccountAPIKey
	default:
		out.NamespaceMode = PromptCacheNamespaceAccount
	}
	if out.CacheReadEfficiencyMin == 0 && out.CacheReadEfficiencyMax == 0 && out.CacheReadEfficiency > 0 {
		out.CacheReadEfficiencyMin = out.CacheReadEfficiency
		out.CacheReadEfficiencyMax = out.CacheReadEfficiency
	} else {
		out.CacheReadEfficiency = (out.CacheReadEfficiencyMin + out.CacheReadEfficiencyMax) / 2
	}
	if out.KvCacheTTLSecs < 60 {
		out.KvCacheTTLSecs = defaultPromptCacheConfig().KvCacheTTLSecs
	}
	if out.MaxEntriesPerAccount <= 0 {
		out.MaxEntriesPerAccount = defaultPromptCacheConfig().MaxEntriesPerAccount
	}
	if out.MaxEntriesTotal < out.MaxEntriesPerAccount {
		out.MaxEntriesTotal = maxIntConfig(defaultPromptCacheConfig().MaxEntriesTotal, out.MaxEntriesPerAccount)
	}
	return out
}

func UpdatePromptCacheConfig(cacheReadEfficiencyMin, cacheReadEfficiencyMax float64, kvCacheTTLSecs int64) error {
	current := GetPromptCacheConfig()
	current.CacheReadEfficiencyMin = cacheReadEfficiencyMin
	current.CacheReadEfficiencyMax = cacheReadEfficiencyMax
	current.KvCacheTTLSecs = kvCacheTTLSecs
	return UpdatePromptCacheSettings(current)
}

func UpdatePromptCacheSettings(settings PromptCacheConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PromptCacheEnabled = settings.Enabled
	cfg.PromptCacheNamespaceMode = settings.NamespaceMode
	cfg.CacheReadEfficiencyMin = settings.CacheReadEfficiencyMin
	cfg.CacheReadEfficiencyMax = settings.CacheReadEfficiencyMax
	cfg.CacheReadEfficiency = (settings.CacheReadEfficiencyMin + settings.CacheReadEfficiencyMax) / 2
	cfg.KvCacheTTLSecs = settings.KvCacheTTLSecs
	cfg.CacheMaxEntriesPerAccount = settings.MaxEntriesPerAccount
	cfg.CacheMaxEntriesTotal = settings.MaxEntriesTotal
	normalizePromptCacheLocked()
	return Save()
}

func maxIntConfig(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func GetRoutingConfig() RoutingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultRoutingConfig()
	}
	out := RoutingConfig{LoadBalancingMode: cfg.LoadBalancingMode}
	mode := strings.ToLower(strings.TrimSpace(out.LoadBalancingMode))
	if mode != "priority" && mode != "balanced" && mode != "weighted" {
		out.LoadBalancingMode = defaultRoutingConfig().LoadBalancingMode
	}
	return out
}

func UpdateRoutingConfig(routing RoutingConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LoadBalancingMode = routing.LoadBalancingMode
	normalizeRoutingLocked()
	return Save()
}

func GetAutoRefreshConfig() AutoRefreshConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultAutoRefreshConfig()
	}
	out := cfg.AutoRefresh
	defaults := defaultAutoRefreshConfig()
	if out.IntervalMinutes <= 0 {
		out.IntervalMinutes = defaults.IntervalMinutes
	}
	if out.TokenRefreshBeforeSeconds <= 0 {
		out.TokenRefreshBeforeSeconds = defaults.TokenRefreshBeforeSeconds
	}
	if out.FailureCooldownSeconds <= 0 {
		out.FailureCooldownSeconds = defaults.FailureCooldownSeconds
	}
	if out.MaxAccountsPerRun < 0 {
		out.MaxAccountsPerRun = 0
	}
	if out.RefreshConcurrency <= 0 {
		out.RefreshConcurrency = defaults.RefreshConcurrency
	}
	if out.RefreshConcurrency > 50 {
		out.RefreshConcurrency = 50
	}
	if out.RefreshQueueCapacity <= 0 {
		out.RefreshQueueCapacity = defaults.RefreshQueueCapacity
	}
	if out.RefreshTaskTimeoutSeconds < 10 {
		out.RefreshTaskTimeoutSeconds = defaults.RefreshTaskTimeoutSeconds
	}
	if out.RefreshJitterSeconds < 0 {
		out.RefreshJitterSeconds = 0
	}
	if out.ModelIntervalMinutes < 30 {
		out.ModelIntervalMinutes = defaults.ModelIntervalMinutes
	}
	if out.MaxModelsPerRun < 0 {
		out.MaxModelsPerRun = 0
	}
	if out.ModelRefreshConcurrency < 1 {
		out.ModelRefreshConcurrency = defaults.ModelRefreshConcurrency
	}
	return out
}

func UpdateAutoRefreshConfig(autoRefresh AutoRefreshConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AutoRefresh = autoRefresh
	normalizeAutoRefreshLocked()
	return Save()
}

func GetRetryConfig() RetryConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultRetryConfig()
	}
	out := cfg.Retry
	defaults := defaultRetryConfig()
	if out.MaxAccountAttempts < 0 {
		out.MaxAccountAttempts = defaults.MaxAccountAttempts
	}
	if out.MaxUpstreamAttempts <= 0 {
		out.MaxUpstreamAttempts = defaults.MaxUpstreamAttempts
	}
	if out.MaxRetryDurationSeconds < 0 {
		out.MaxRetryDurationSeconds = defaults.MaxRetryDurationSeconds
	}
	if out.FirstTokenTimeoutSeconds < 5 {
		out.FirstTokenTimeoutSeconds = defaults.FirstTokenTimeoutSeconds
	}
	if out.StreamIdleTimeoutSeconds < 15 {
		out.StreamIdleTimeoutSeconds = defaults.StreamIdleTimeoutSeconds
	}
	if out.ToolAssemblyTimeoutSeconds < 0 {
		out.ToolAssemblyTimeoutSeconds = defaults.ToolAssemblyTimeoutSeconds
	}
	if out.EmptyResponseRetries < 0 {
		out.EmptyResponseRetries = 0
	}
	if out.EndpointFailureThreshold < 1 {
		out.EndpointFailureThreshold = defaults.EndpointFailureThreshold
	}
	if out.EndpointCircuitCooldownSeconds < 5 {
		out.EndpointCircuitCooldownSeconds = defaults.EndpointCircuitCooldownSeconds
	}
	if out.ProxyFailureThreshold < 1 {
		out.ProxyFailureThreshold = defaults.ProxyFailureThreshold
	}
	if out.ProxyCircuitCooldownSeconds < 5 {
		out.ProxyCircuitCooldownSeconds = defaults.ProxyCircuitCooldownSeconds
	}
	return out
}

func UpdateRetryConfig(retry RetryConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Retry = retry
	normalizeRetryLocked()
	return Save()
}

func GetLongToolConfig() LongToolConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultLongToolConfig()
	}
	out := cfg.LongTool
	defaults := defaultLongToolConfig()
	if out.DefaultMaxToolTokens < 1024 {
		out.DefaultMaxToolTokens = defaults.DefaultMaxToolTokens
	}
	if out.TruncationRetries < 0 {
		out.TruncationRetries = defaults.TruncationRetries
	}
	if strings.TrimSpace(out.FallbackModel) == "" {
		out.FallbackModel = defaults.FallbackModel
	}
	return out
}

func UpdateLongToolConfig(value LongToolConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LongTool = value
	normalizeLongToolLocked()
	return Save()
}

func GetResponsesStorageConfig() ResponsesStorageConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultResponsesStorageConfig()
	}
	out := cfg.ResponsesStorage
	defaults := defaultResponsesStorageConfig()
	if out.TTLHours < 1 {
		out.TTLHours = defaults.TTLHours
	}
	if out.MaxFiles < 1 {
		out.MaxFiles = defaults.MaxFiles
	}
	if out.MaxBytes < 1<<20 {
		out.MaxBytes = defaults.MaxBytes
	}
	if out.MaxHistoryBytes < 64<<10 {
		out.MaxHistoryBytes = defaults.MaxHistoryBytes
	}
	if out.GCIntervalMinutes < 1 {
		out.GCIntervalMinutes = defaults.GCIntervalMinutes
	}
	return out
}

func UpdateResponsesStorageConfig(storage ResponsesStorageConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ResponsesStorage = storage
	normalizeResponsesStorageLocked()
	return Save()
}

func GetModelRegistryConfig() ModelRegistryConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultModelRegistryConfig()
	}
	out := cfg.ModelRegistry
	out.Models = append([]ModelEntry(nil), cfg.ModelRegistry.Models...)
	for i := range out.Models {
		out.Models[i].MatchKeywords = append([]string(nil), out.Models[i].MatchKeywords...)
	}
	return out
}

func UpdateModelRegistryConfig(registry ModelRegistryConfig) error {
	for i := range registry.Models {
		normalizeModelEntry(&registry.Models[i])
	}
	if err := validateModelEntries(registry.Models); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ModelRegistry = registry
	normalizeModelRegistryLocked()
	return Save()
}

func validateModelEntries(models []ModelEntry) error {
	ids := make(map[string]bool)
	keywords := make(map[string]string)
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		if id == "" || strings.TrimSpace(model.KiroModelID) == "" {
			return fmt.Errorf("model id and kiroModelId are required")
		}
		if ids[id] {
			return fmt.Errorf("duplicate model id: %s", model.ID)
		}
		ids[id] = true
		if model.ContextWindow < 1024 || model.MaxTokens < 1 ||
			(model.MaxToolTokens != 0 && (model.MaxToolTokens < 1024 || model.MaxToolTokens > 128000)) {
			return fmt.Errorf("invalid token limits for model %s", model.ID)
		}
		for _, keyword := range model.MatchKeywords {
			key := strings.ToLower(strings.TrimSpace(keyword))
			if owner, ok := keywords[key]; ok && owner != id {
				return fmt.Errorf("model keyword %q conflicts between %s and %s", key, owner, id)
			}
			keywords[key] = id
		}
	}
	return nil
}

// ResolveConfiguredModel applies exact-ID matching first, then the longest keyword match.
func ResolveConfiguredModel(model string) (ModelEntry, bool) {
	registry := GetModelRegistryConfig()
	needle := strings.ToLower(strings.TrimSpace(model))
	for _, entry := range registry.Models {
		if strings.EqualFold(entry.ID, needle) {
			return entry, true
		}
	}
	bestIndex := -1
	bestLength := -1
	for i, entry := range registry.Models {
		for _, keyword := range entry.MatchKeywords {
			if strings.Contains(needle, keyword) && len(keyword) > bestLength {
				bestIndex = i
				bestLength = len(keyword)
			}
		}
	}
	if bestIndex >= 0 {
		return registry.Models[bestIndex], true
	}
	return ModelEntry{}, false
}

func GetConfiguredModelMetadata(model string) (ModelEntry, bool) {
	registry := GetModelRegistryConfig()
	for _, entry := range registry.Models {
		if strings.EqualFold(entry.ID, model) || strings.EqualFold(entry.KiroModelID, model) {
			return entry, true
		}
	}
	return ModelEntry{}, false
}

func GetHealthConfig() HealthConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultHealthConfig()
	}
	return cfg.Health
}

func UpdateHealthConfig(health HealthConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Health = health
	normalizeHealthLocked()
	return Save()
}

func GetDiagnosticConfig() DiagnosticConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultDiagnosticConfig()
	}
	out := cfg.Diagnostics
	if out.MaxEntries <= 0 || out.MaxEntries > 2000 {
		defaults := defaultDiagnosticConfig()
		if out.MaxEntries <= 0 {
			out.MaxEntries = defaults.MaxEntries
		} else {
			out.MaxEntries = 2000
		}
	}
	return out
}

func UpdateDiagnosticConfig(diagnostics DiagnosticConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Diagnostics = diagnostics
	normalizeDiagnosticLocked()
	return Save()
}

func GetRequestLogConfig() RequestLogConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultRequestLogConfig()
	}
	out := cfg.RequestLog
	if out.MaxEntries <= 0 {
		out.MaxEntries = DefaultRequestLogMaxEntries
	} else if out.MaxEntries < MinRequestLogMaxEntries {
		out.MaxEntries = MinRequestLogMaxEntries
	}
	if out.MaxEntries > MaxRequestLogMaxEntries {
		out.MaxEntries = MaxRequestLogMaxEntries
	}
	if out.DetailedMaxEntries <= 0 {
		out.DetailedMaxEntries = DefaultRequestDetailMaxEntries
	} else if out.DetailedMaxEntries < MinRequestDetailMaxEntries {
		out.DetailedMaxEntries = MinRequestDetailMaxEntries
	}
	if out.DetailedMaxEntries > MaxRequestDetailMaxEntries {
		out.DetailedMaxEntries = MaxRequestDetailMaxEntries
	}
	if out.MaxDetailBytes <= 0 {
		out.MaxDetailBytes = DefaultRequestDetailMaxBytes
	} else if out.MaxDetailBytes < MinRequestDetailMaxBytes {
		out.MaxDetailBytes = MinRequestDetailMaxBytes
	}
	if out.MaxDetailBytes > MaxRequestDetailMaxBytes {
		out.MaxDetailBytes = MaxRequestDetailMaxBytes
	}
	return out
}

func UpdateRequestLogConfig(requestLog RequestLogConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.RequestLog = requestLog
	normalizeRequestLogLocked()
	return Save()
}

func GetWebSearchConfig() WebSearchConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultWebSearchConfig()
	}
	return cfg.WebSearch
}

func UpdateWebSearchConfig(webSearch WebSearchConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.WebSearch = webSearch
	return Save()
}

func GetCountTokensProviderConfig() CountTokensProviderConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultCountTokensProviderConfig()
	}
	out := cfg.CountTokensProvider
	if out.AuthType == "" {
		out.AuthType = defaultCountTokensProviderConfig().AuthType
	}
	return out
}

func UpdateCountTokensProviderConfig(provider CountTokensProviderConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.CountTokensProvider = provider
	normalizeCountTokensProviderLocked()
	return Save()
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func VerifyPassword(password string) bool {
	cfgLock.RLock()
	stored := cfg.Password
	cfgLock.RUnlock()
	if !isAdminPasswordHash(stored) {
		return stored == password
	}
	return bcrypt.CompareHashAndPassword([]byte(stored), adminPasswordInput(password)) == nil
}

func IsDefaultPassword() bool {
	return VerifyPassword("changeme")
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	if err := validateAccountProxy(account); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	normalizeKiroAPIKeyAccount(&account)
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

func UpdateAccount(id string, account Account) error {
	if err := validateAccountProxy(account); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			normalizeKiroAPIKeyAccount(&account)
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// SetAccountsEnabled updates only account status fields with one config write,
// preserving credentials and usage data that may be changing concurrently.
func SetAccountsEnabled(ids map[string]bool, enabled bool) error {
	if len(ids) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	changed := false
	for i := range cfg.Accounts {
		if !ids[cfg.Accounts[i].ID] {
			continue
		}
		cfg.Accounts[i].Enabled = enabled
		if enabled && cfg.Accounts[i].BanStatus != "" && cfg.Accounts[i].BanStatus != "ACTIVE" {
			cfg.Accounts[i].BanStatus = "ACTIVE"
			cfg.Accounts[i].BanReason = ""
			cfg.Accounts[i].BanTime = 0
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return Save()
}

type AccountUpsertResult struct {
	Account Account
	Updated bool
}

func upsertAccountByIdentityLocked(account Account) (Account, bool) {
	normalizeKiroAPIKeyAccount(&account)
	for i, existing := range cfg.Accounts {
		if !sameAccountIdentity(existing, account) {
			continue
		}
		if account.ID == "" {
			account.ID = existing.ID
		}
		if account.ID != existing.ID {
			account.ID = existing.ID
		}
		mergeAccountDefaults(&account, existing)
		cfg.Accounts[i] = account
		return account, true
	}
	cfg.Accounts = append(cfg.Accounts, account)
	return account, false
}

func UpsertAccountByIdentity(account Account) (Account, bool, error) {
	if err := validateAccountProxy(account); err != nil {
		return Account{}, false, err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := append([]Account(nil), cfg.Accounts...)
	upserted, updated := upsertAccountByIdentityLocked(account)
	if err := Save(); err != nil {
		cfg.Accounts = previous
		return Account{}, false, err
	}
	return upserted, updated, nil
}

// UpsertAccountsByIdentity persists a batch with one atomic config write.
func UpsertAccountsByIdentity(accounts []Account) ([]AccountUpsertResult, error) {
	if len(accounts) == 0 {
		return []AccountUpsertResult{}, nil
	}
	for _, account := range accounts {
		if err := validateAccountProxy(account); err != nil {
			return nil, err
		}
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := append([]Account(nil), cfg.Accounts...)
	results := make([]AccountUpsertResult, 0, len(accounts))
	for _, account := range accounts {
		upserted, updated := upsertAccountByIdentityLocked(account)
		results = append(results, AccountUpsertResult{Account: upserted, Updated: updated})
	}
	if err := Save(); err != nil {
		cfg.Accounts = previous
		return nil, err
	}
	return results, nil
}

func sameAccountIdentity(a, b Account) bool {
	if a.ID != "" && b.ID != "" && a.ID == b.ID {
		return true
	}
	if a.CredentialFingerprint != "" && b.CredentialFingerprint != "" && a.CredentialFingerprint == b.CredentialFingerprint {
		return true
	}
	if a.UserId != "" && b.UserId != "" && a.UserId == b.UserId && sameProvider(a.Provider, b.Provider) {
		return true
	}
	apiKeyFingerprintMismatch := a.AuthMethod == "api_key" && b.AuthMethod == "api_key" &&
		a.CredentialFingerprint != "" && b.CredentialFingerprint != "" && a.CredentialFingerprint != b.CredentialFingerprint
	if !apiKeyFingerprintMismatch && a.Email != "" && b.Email != "" && strings.EqualFold(a.Email, b.Email) && sameProvider(a.Provider, b.Provider) {
		return true
	}
	if a.MachineId != "" && b.MachineId != "" && a.MachineId == b.MachineId {
		return true
	}
	return false
}

func sameProvider(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	return a == b || a == "" || b == ""
}

func mergeAccountDefaults(account *Account, existing Account) {
	if account == nil {
		return
	}
	if account.Nickname == "" {
		account.Nickname = existing.Nickname
	}
	if account.Weight == 0 {
		account.Weight = existing.Weight
	}
	if account.Priority == 0 {
		account.Priority = existing.Priority
	}
	if account.ProxyURL == "" {
		account.ProxyURL = existing.ProxyURL
	}
	if account.ProfileArn == "" {
		account.ProfileArn = existing.ProfileArn
	}
	if account.TokenEndpoint == "" {
		account.TokenEndpoint = existing.TokenEndpoint
	}
	if account.IssuerURL == "" {
		account.IssuerURL = existing.IssuerURL
	}
	if account.Scopes == "" {
		account.Scopes = existing.Scopes
	}
	if account.OverageStatus == "" {
		account.OverageStatus = existing.OverageStatus
		account.OverageCapability = existing.OverageCapability
		account.OverageCap = existing.OverageCap
		account.OverageRate = existing.OverageRate
		account.CurrentOverages = existing.CurrentOverages
		account.OverageCheckedAt = existing.OverageCheckedAt
	}
	if account.RequestCount == 0 {
		account.RequestCount = existing.RequestCount
		account.ErrorCount = existing.ErrorCount
		account.TotalTokens = existing.TotalTokens
		account.TotalCredits = existing.TotalCredits
		account.LastUsed = existing.LastUsed
	}
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return Save()
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return fmt.Errorf("account not found: %s", id)
}

type AccountCredentialUpdate struct {
	ID           string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	ProfileArn   string
}

// UpdateAccountCredentialsBatch persists refreshed credential sets with one
// atomic config write and rolls the in-memory snapshot back on failure.
func UpdateAccountCredentialsBatch(updates []AccountCredentialUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := append([]Account(nil), cfg.Accounts...)
	indexes := make(map[string]int, len(cfg.Accounts))
	for i := range cfg.Accounts {
		indexes[cfg.Accounts[i].ID] = i
	}
	for _, update := range updates {
		i, ok := indexes[update.ID]
		if !ok {
			continue
		}
		cfg.Accounts[i].AccessToken = update.AccessToken
		if update.RefreshToken != "" {
			cfg.Accounts[i].RefreshToken = update.RefreshToken
		}
		cfg.Accounts[i].ExpiresAt = update.ExpiresAt
		if update.ProfileArn != "" {
			cfg.Accounts[i].ProfileArn = update.ProfileArn
		}
	}
	if err := Save(); err != nil {
		cfg.Accounts = previous
		return err
	}
	return nil
}

// UpdateAccountCredentials persists one refreshed token set.
func UpdateAccountCredentials(id, accessToken, refreshToken string, expiresAt int64, profileArn string) error {
	return UpdateAccountCredentialsBatch([]AccountCredentialUpdate{{
		ID: id, AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt, ProfileArn: profileArn,
	}})
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	return UpdateAccountCredentials(id, accessToken, refreshToken, expiresAt, "")
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	var hashedPassword string
	if password != "" {
		var err error
		hashedPassword, err = hashAdminPassword(password)
		if err != nil {
			return err
		}
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = hashedPassword
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	return UpdateSettingsPatchWithOverUsage(apiKey, requireApiKey, password, nil)
}

// UpdateSettingsPatchWithOverUsage persists the general settings form in one
// atomic write.
func UpdateSettingsPatchWithOverUsage(apiKey *string, requireApiKey *bool, password string, allowOverUsage *bool) error {
	var hashedPassword string
	if password != "" {
		var err error
		hashedPassword, err = hashAdminPassword(password)
		if err != nil {
			return err
		}
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = hashedPassword
	}
	if allowOverUsage != nil {
		cfg.AllowOverUsage = *allowOverUsage
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

// ResetStatistics clears global and per-account cumulative runtime counters.
func ResetStatistics() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = 0
	cfg.SuccessRequests = 0
	cfg.FailedRequests = 0
	cfg.TotalTokens = 0
	cfg.TotalCredits = 0
	for i := range cfg.Accounts {
		cfg.Accounts[i].RequestCount = 0
		cfg.Accounts[i].ErrorCount = 0
		cfg.Accounts[i].TotalTokens = 0
		cfg.Accounts[i].TotalCredits = 0
		cfg.Accounts[i].LastUsed = 0
	}
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

// AccountStatsSnapshot is the persisted runtime counter set for one account.
type AccountStatsSnapshot struct {
	RequestCount int
	ErrorCount   int
	TotalTokens  int
	TotalCredits float64
	LastUsed     int64
}

// UpdateAccountStatsBatch merges multiple account counter snapshots and writes
// the config once. A stale generation is ignored so delayed work from an old
// config instance cannot modify a newly initialized config file.
func UpdateAccountStatsBatch(expectedGeneration uint64, updates map[string]AccountStatsSnapshot) error {
	if len(updates) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if expectedGeneration != cfgGeneration {
		return nil
	}
	changed := false
	for i := range cfg.Accounts {
		stats, ok := updates[cfg.Accounts[i].ID]
		if !ok {
			continue
		}
		cfg.Accounts[i].RequestCount = stats.RequestCount
		cfg.Accounts[i].ErrorCount = stats.ErrorCount
		cfg.Accounts[i].TotalTokens = stats.TotalTokens
		cfg.Accounts[i].TotalCredits = stats.TotalCredits
		cfg.Accounts[i].LastUsed = stats.LastUsed
		changed = true
	}
	if !changed {
		return nil
	}
	return Save()
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID != id {
			continue
		}
		applyAccountInfo(&cfg.Accounts[i], info)
		return Save()
	}
	return nil
}

// UpdateAccountInfoBatch applies multiple refresh results with one config write.
func UpdateAccountInfoBatch(updates map[string]AccountInfo) error {
	if len(updates) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	changed := false
	for i := range cfg.Accounts {
		info, ok := updates[cfg.Accounts[i].ID]
		if !ok {
			continue
		}
		applyAccountInfo(&cfg.Accounts[i], info)
		changed = true
	}
	if !changed {
		return nil
	}
	return Save()
}

func applyAccountInfo(account *Account, info AccountInfo) {
	if account == nil {
		return
	}
	if info.Email != "" {
		account.Email = info.Email
	}
	if info.UserId != "" {
		account.UserId = info.UserId
	}
	account.SubscriptionType = info.SubscriptionType
	account.SubscriptionTitle = info.SubscriptionTitle
	account.DaysRemaining = info.DaysRemaining
	account.UsageCurrent = info.UsageCurrent
	account.UsageLimit = info.UsageLimit
	account.UsagePercent = info.UsagePercent
	account.NextResetDate = info.NextResetDate
	account.LastRefresh = info.LastRefresh
	account.TrialUsageCurrent = info.TrialUsageCurrent
	account.TrialUsageLimit = info.TrialUsageLimit
	account.TrialUsagePercent = info.TrialUsagePercent
	account.TrialStatus = info.TrialStatus
	account.TrialExpiresAt = info.TrialExpiresAt
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix                     string `json:"suffix"`                     // Model name suffix that triggers thinking mode
	OpenAIFormat               string `json:"openaiFormat"`               // Output format for OpenAI-compatible responses
	ClaudeFormat               string `json:"claudeFormat"`               // Output format for Claude-compatible responses
	DefaultBudgetTokens        int    `json:"defaultBudgetTokens"`        // Default fake-reasoning budget
	BudgetCapTokens            int    `json:"budgetCapTokens"`            // Maximum proxy-derived fake-reasoning budget; 0 disables the cap
	DefaultMaxOutputTokens     int    `json:"defaultMaxOutputTokens"`     // Default max output tokens; 0 leaves it unset
	DefaultContextWindowTokens int    `json:"defaultContextWindowTokens"` // Default context window; 0 auto-detects
	ToolStreamMode             string `json:"toolStreamMode"`             // Claude tool stream mode: safe, adaptive, balanced, or live
	BufferToolStreams          bool   `json:"bufferToolStreams"`          // Deprecated compatibility mirror; false only for live mode
	EnforceAgentToolUse        bool   `json:"enforceAgentToolUse"`        // Require tools for workspace actions
}

// NormalizeToolStreamMode validates and canonicalizes a Claude tool stream mode.
func NormalizeToolStreamMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ToolStreamModeSafe:
		return ToolStreamModeSafe, true
	case ToolStreamModeAdaptive:
		return ToolStreamModeAdaptive, true
	case ToolStreamModeBalanced:
		return ToolStreamModeBalanced, true
	case ToolStreamModeLive:
		return ToolStreamModeLive, true
	default:
		return "", false
	}
}

func resolveToolStreamMode(mode string, legacyBuffer *bool) string {
	if normalized, ok := NormalizeToolStreamMode(mode); ok {
		return normalized
	}
	if legacyBuffer != nil && !*legacyBuffer {
		return ToolStreamModeLive
	}
	return ToolStreamModeSafe
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	if cfg == nil {
		return ThinkingConfig{
			Suffix:                     "-thinking",
			OpenAIFormat:               "reasoning_content",
			ClaudeFormat:               "thinking",
			DefaultBudgetTokens:        4000,
			BudgetCapTokens:            10000,
			DefaultMaxOutputTokens:     0,
			DefaultContextWindowTokens: 0,
			ToolStreamMode:             ToolStreamModeSafe,
			BufferToolStreams:          true,
			EnforceAgentToolUse:        true,
		}
	}

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}
	defaultBudgetTokens := cfg.ThinkingDefaultBudgetTokens
	if defaultBudgetTokens <= 0 {
		defaultBudgetTokens = 4000
	}
	budgetCapTokens := 10000
	if cfg.ThinkingBudgetCapTokens != nil {
		budgetCapTokens = max(0, *cfg.ThinkingBudgetCapTokens)
	}
	defaultMaxOutputTokens := max(0, cfg.DefaultMaxOutputTokens)
	defaultContextWindowTokens := max(0, cfg.DefaultContextWindowTokens)
	toolStreamMode := resolveToolStreamMode(cfg.ToolStreamMode, cfg.BufferToolStreams)
	enforceAgentToolUse := true
	if cfg.EnforceAgentToolUse != nil {
		enforceAgentToolUse = *cfg.EnforceAgentToolUse
	}

	return ThinkingConfig{
		Suffix:                     suffix,
		OpenAIFormat:               openaiFormat,
		ClaudeFormat:               claudeFormat,
		DefaultBudgetTokens:        defaultBudgetTokens,
		BudgetCapTokens:            budgetCapTokens,
		DefaultMaxOutputTokens:     defaultMaxOutputTokens,
		DefaultContextWindowTokens: defaultContextWindowTokens,
		ToolStreamMode:             toolStreamMode,
		BufferToolStreams:          toolStreamMode != ToolStreamModeLive,
		EnforceAgentToolUse:        enforceAgentToolUse,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string, defaultBudgetTokens, budgetCapTokens, defaultMaxOutputTokens, defaultContextWindowTokens int, bufferToolStreams, enforceAgentToolUse bool) error {
	toolStreamMode := ToolStreamModeLive
	if bufferToolStreams {
		toolStreamMode = ToolStreamModeSafe
	}
	return UpdateThinkingConfigWithToolStreamMode(suffix, openaiFormat, claudeFormat, defaultBudgetTokens, budgetCapTokens, defaultMaxOutputTokens, defaultContextWindowTokens, toolStreamMode, enforceAgentToolUse)
}

// UpdateThinkingConfigWithToolStreamMode persists thinking settings using the
// four-state Claude tool stream policy. The legacy boolean is also written so
// rolling back to an older binary preserves buffered/live behavior.
func UpdateThinkingConfigWithToolStreamMode(suffix, openaiFormat, claudeFormat string, defaultBudgetTokens, budgetCapTokens, defaultMaxOutputTokens, defaultContextWindowTokens int, toolStreamMode string, enforceAgentToolUse bool) error {
	normalizedMode, ok := NormalizeToolStreamMode(toolStreamMode)
	if !ok {
		return fmt.Errorf("invalid tool stream mode %q", toolStreamMode)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	cfg.ThinkingDefaultBudgetTokens = defaultBudgetTokens
	budgetCap := max(0, budgetCapTokens)
	cfg.ThinkingBudgetCapTokens = &budgetCap
	cfg.DefaultMaxOutputTokens = max(0, defaultMaxOutputTokens)
	cfg.DefaultContextWindowTokens = max(0, defaultContextWindowTokens)
	cfg.ToolStreamMode = normalizedMode
	bufferEnabled := normalizedMode != ToolStreamModeLive
	cfg.BufferToolStreams = &bufferEnabled
	enforceTools := enforceAgentToolUse
	cfg.EnforceAgentToolUse = &enforceTools
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// UpdateEndpointConfig persists the preferred endpoint and optional fallback
// switch atomically.
func UpdateEndpointConfig(endpoint string, fallback *bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	if fallback != nil {
		enabled := *fallback
		cfg.EndpointFallback = &enabled
	}
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if err := outboundproxy.Validate(proxyURL); err != nil {
		return fmt.Errorf("proxyURL: %w", err)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

func validateAccountProxy(account Account) error {
	if err := outboundproxy.Validate(account.ProxyURL); err != nil {
		return fmt.Errorf("account %q proxyURL: %w", account.ID, err)
	}
	return nil
}

func validateOutboundProxyConfig(value *Config) error {
	if value == nil {
		return nil
	}
	value.ProxyURL = strings.TrimSpace(value.ProxyURL)
	if err := outboundproxy.Validate(value.ProxyURL); err != nil {
		return fmt.Errorf("proxyURL: %w", err)
	}
	for i := range value.Accounts {
		value.Accounts[i].ProxyURL = strings.TrimSpace(value.Accounts[i].ProxyURL)
		if err := validateAccountProxy(value.Accounts[i]); err != nil {
			return err
		}
	}
	return nil
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

func GetRuntimeConfig() RuntimeConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	clientCfg := getKiroClientConfigLocked()
	host := "127.0.0.1"
	port := 8080
	logLevel := "info"
	if cfg != nil {
		if cfg.Host != "" {
			host = cfg.Host
		}
		if cfg.Port != 0 {
			port = cfg.Port
		}
		if cfg.LogLevel != "" {
			logLevel = cfg.LogLevel
		}
	}
	return RuntimeConfig{
		Host:          host,
		Port:          port,
		LogLevel:      logLevel,
		KiroVersion:   clientCfg.KiroVersion,
		SystemVersion: clientCfg.SystemVersion,
		NodeVersion:   clientCfg.NodeVersion,
	}
}

// ResolveListenAddress returns the process listen address after optional
// deployment-level overrides. Docker Compose uses these overrides to keep the
// container port stable even when standalone runtime settings differ.
func ResolveListenAddress() (host string, port int, externallyManaged bool, err error) {
	host = GetHost()
	port = GetPort()
	if value, ok := os.LookupEnv("KIRO_LISTEN_HOST"); ok {
		externallyManaged = true
		host = strings.TrimSpace(value)
		if host == "" {
			return "", 0, true, fmt.Errorf("KIRO_LISTEN_HOST must not be empty")
		}
	}
	if value, ok := os.LookupEnv("KIRO_LISTEN_PORT"); ok {
		externallyManaged = true
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
		if parseErr != nil || parsed < 1 || parsed > 65535 {
			return "", 0, true, fmt.Errorf("KIRO_LISTEN_PORT must be between 1 and 65535")
		}
		port = parsed
	}
	return host, port, externallyManaged, nil
}

func UpdateRuntimeConfig(runtime RuntimeConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if runtime.Host != "" {
		cfg.Host = runtime.Host
	}
	if runtime.Port > 0 {
		cfg.Port = runtime.Port
	}
	if runtime.LogLevel != "" {
		cfg.LogLevel = runtime.LogLevel
	}
	cfg.KiroVersion = runtime.KiroVersion
	cfg.SystemVersion = runtime.SystemVersion
	cfg.NodeVersion = runtime.NodeVersion
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return getKiroClientConfigLocked()
}

func getKiroClientConfigLocked() KiroClientConfig {
	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
