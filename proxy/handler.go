package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"kiro-go/internal/outboundproxy"
	"kiro-go/logger"
	"kiro-go/pool"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120
const maxRequestBodyBytes int64 = 8 << 20
const maxCredentialImportBatch = 5000
const externalIdpImportMaxTrust = 15 * time.Minute

var claudeStreamHeartbeatInterval = 10 * time.Second

func requestBodyErrorStatus(err error) int {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func requestConversationNamespace(r *http.Request, apiKeyID string) string {
	parts := make([]string, 0, 6)
	hasStableIdentity := false
	if apiKeyID = strings.TrimSpace(apiKeyID); apiKeyID != "" {
		parts = append(parts, "api-key:"+apiKeyID)
		hasStableIdentity = true
	}
	if r != nil {
		for _, header := range []string{
			"X-Conversation-ID",
			"X-Client-User",
			"OpenAI-Organization",
			"Anthropic-Organization-Id",
		} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				parts = append(parts, strings.ToLower(header)+":"+value)
				hasStableIdentity = true
			}
		}
		if !hasStableIdentity {
			remote := strings.TrimSpace(r.RemoteAddr)
			if host, _, err := net.SplitHostPort(remote); err == nil {
				remote = host
			}
			if remote != "" {
				parts = append(parts, "remote:"+remote)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// Typed atomics provide the required 64-bit alignment on 32-bit targets.
	totalRequests     atomic.Int64
	successRequests   atomic.Int64
	failedRequests    atomic.Int64
	totalTokens       atomic.Int64
	totalCredits      float64 // float64 需要用锁保护
	creditsMu         sync.RWMutex
	startTime         int64
	stopRefresh       chan struct{}
	stopStatsSaver    chan struct{}
	backgroundCtx     context.Context
	backgroundCancel  context.CancelFunc
	backgroundWG      sync.WaitGroup
	backgroundTaskMu  sync.Mutex
	backgroundClosing bool
	stopOnce          sync.Once
	closeOnce         sync.Once
	// 模型缓存
	cachedModels         []ModelInfo
	modelsByAccount      map[string][]ModelInfo
	modelsCacheMu        sync.RWMutex
	modelsCacheTime      int64
	modelsRefreshing     atomic.Bool
	modelHealthMu        sync.RWMutex
	modelHealth          map[string]modelHealthState
	modelHealthRunning   atomic.Bool
	promptCache          *promptCacheTracker
	requestLog           *requestLog
	requestDetailsMu     sync.Mutex
	requestDetails       *requestDetailStore
	diagnosticLog        *diagnosticLog
	alerts               *healthAlertManager
	autoRefreshMu        sync.Mutex
	autoRefreshFail      map[string]int64
	autoRefreshNext      int
	autoRefreshModelNext int
	autoRefreshStat      autoRefreshStatus
	adminAuthMu          sync.Mutex
	adminSessions        map[[32]byte]adminSession
	adminAttempts        map[string]adminAuthAttempt
}

type autoRefreshStatus struct {
	Running           bool                                `json:"running"`
	LastRunStartedAt  int64                               `json:"lastRunStartedAt,omitempty"`
	LastRunFinishedAt int64                               `json:"lastRunFinishedAt,omitempty"`
	LastRunDurationMs int64                               `json:"lastRunDurationMs,omitempty"`
	LastRunSelected   int                                 `json:"lastRunSelected"`
	LastRunAttempted  int                                 `json:"lastRunAttempted"`
	LastRunSuccess    int                                 `json:"lastRunSuccess"`
	LastRunFailed     int                                 `json:"lastRunFailed"`
	LastRunSkipped    int                                 `json:"lastRunSkipped"`
	Cursor            int                                 `json:"cursor"`
	CooldownCount     int                                 `json:"cooldownCount"`
	Recent            map[string]autoRefreshAccountStatus `json:"recent,omitempty"`
}

type autoRefreshAccountStatus struct {
	AccountID     string `json:"accountId"`
	Email         string `json:"email,omitempty"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Error         string `json:"error,omitempty"`
	LastStartedAt int64  `json:"lastStartedAt,omitempty"`
	LastSuccessAt int64  `json:"lastSuccessAt,omitempty"`
	LastFailureAt int64  `json:"lastFailureAt,omitempty"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
}

type autoRefreshAccountResult struct {
	AccountID     string
	Email         string
	Status        string
	Reason        string
	Err           error
	Attempted     bool
	StartedAt     int64
	FinishedAt    int64
	CooldownUntil int64
	Info          *config.AccountInfo
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	for index := range req.Messages {
		if err := normalizeOpenAIMessageRole(&req.Messages[index]); err != nil {
			return fmt.Sprintf("messages[%d]: %v", index, err)
		}
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	if err := applyProxyConfig(config.GetProxyURL()); err != nil {
		panic(fmt.Sprintf("invalid outbound proxy configuration: %v", err))
	}

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	promptCacheCfg := config.GetPromptCacheConfig()
	promptCache := newPromptCacheTrackerWithEfficiencyRange(time.Duration(promptCacheCfg.KvCacheTTLSecs)*time.Second, promptCacheCfg.CacheReadEfficiencyMin, promptCacheCfg.CacheReadEfficiencyMax)
	promptCache.ConfigurePolicy(promptCacheCfg.Enabled, promptCacheCfg.NamespaceMode)
	promptCache.ConfigureLimits(promptCacheCfg.MaxEntriesPerAccount, promptCacheCfg.MaxEntriesTotal)
	if promptCacheCfg.PersistEnabled {
		if restored, err := promptCache.Load(promptCachePath()); err != nil {
			logger.Warnf("[PromptCache] Failed to restore persisted state: %v", err)
		} else if restored > 0 {
			logger.Infof("[PromptCache] Restored %d cache fingerprints", restored)
		}
	} else if err := promptCache.RemovePersisted(promptCachePath()); err != nil {
		logger.Warnf("[PromptCache] Failed to remove disabled persisted state: %v", err)
	}
	requestLogCfg := config.GetRequestLogConfig()
	requestLog, requestLogErr := newPersistentRequestLog(requestLogCfg.MaxEntries, requestLogPath())
	if requestLogErr != nil {
		logger.Warnf("[RequestLog] Failed to restore persisted request log: %v", requestLogErr)
	}
	requestDetails, requestDetailsErr := newPersistentRequestDetailStore(requestLogCfg.DetailedMaxEntries, requestLogCfg.MaxDetailBytes, requestDetailPath())
	if requestDetailsErr != nil {
		logger.Warnf("[RequestDetail] Failed to restore persisted request details: %v", requestDetailsErr)
	}
	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	h := &Handler{
		pool:             pool.GetPool(),
		totalCredits:     totalCredits,
		startTime:        time.Now().Unix(),
		stopRefresh:      make(chan struct{}),
		stopStatsSaver:   make(chan struct{}),
		backgroundCtx:    backgroundCtx,
		backgroundCancel: backgroundCancel,
		promptCache:      promptCache,
		modelsByAccount:  make(map[string][]ModelInfo),
		modelHealth:      make(map[string]modelHealthState),
		requestLog:       requestLog,
		requestDetails:   requestDetails,
		diagnosticLog:    newDiagnosticLog(config.GetDiagnosticConfig().MaxEntries),
		alerts:           newHealthAlertManager(),
		autoRefreshFail:  pool.GetPool().RefreshFailureCooldowns(),
		autoRefreshNext:  pool.GetPool().RefreshCursor(),
		autoRefreshStat:  autoRefreshStatus{Recent: make(map[string]autoRefreshAccountStatus)},
		adminSessions:    make(map[[32]byte]adminSession),
		adminAttempts:    make(map[string]adminAuthAttempt),
	}
	h.totalRequests.Store(int64(totalReq))
	h.successRequests.Store(int64(successReq))
	h.failedRequests.Store(int64(failedReq))
	h.totalTokens.Store(int64(totalTokens))
	// 启动后台刷新、统计保存和短期状态回收。
	h.startBackgroundTask(h.backgroundRefresh)
	h.startBackgroundTask(h.backgroundStatsSaver)
	h.startBackgroundTask(h.backgroundResponsesGC)
	h.startBackgroundTask(h.backgroundPromptCacheGC)
	return h
}

func (h *Handler) startBackgroundTask(run func()) bool {
	if h == nil || run == nil {
		return false
	}
	h.backgroundTaskMu.Lock()
	if h.backgroundClosing {
		h.backgroundTaskMu.Unlock()
		return false
	}
	h.backgroundWG.Add(1)
	h.backgroundTaskMu.Unlock()
	go func() {
		defer h.backgroundWG.Done()
		run()
	}()
	return true
}

func (h *Handler) backgroundResponsesGC() {
	purgeResponsesStorage()
	for {
		interval := time.Duration(config.GetResponsesStorageConfig().GCIntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = time.Hour
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			purgeResponsesStorage()
		case <-h.stopRefresh:
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
}

func (h *Handler) backgroundPromptCacheGC() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	lastPrune := time.Now()
	for {
		select {
		case now := <-ticker.C:
			if h.promptCache != nil {
				if now.Sub(lastPrune) >= time.Minute {
					h.promptCache.PruneExpired(now)
					lastPrune = now
				}
				if config.GetPromptCacheConfig().PersistEnabled {
					if err := h.promptCache.Flush(promptCachePath()); err != nil {
						logger.Warnf("[PromptCache] Failed to persist state: %v", err)
					}
				}
			}
		case <-h.stopRefresh:
			return
		}
	}
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	startupDelay := time.NewTimer(10 * time.Second)
	select {
	case <-startupDelay.C:
	case <-h.stopRefresh:
		if !startupDelay.Stop() {
			<-startupDelay.C
		}
		return
	}
	var lastAccountRun time.Time
	var lastModelRun time.Time

	for {
		autoRefresh := config.GetAutoRefreshConfig()
		if autoRefresh.Enabled {
			interval := time.Duration(autoRefresh.IntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = 30 * time.Minute
			}
			if lastAccountRun.IsZero() || time.Since(lastAccountRun) >= interval {
				if autoRefresh.RefreshJitterSeconds > 0 {
					maxJitter := time.Duration(autoRefresh.RefreshJitterSeconds) * time.Second
					jitter := time.Duration(time.Now().UnixNano() % int64(maxJitter+1))
					select {
					case <-time.After(jitter):
					case <-h.stopRefresh:
						return
					}
				}
				h.refreshAllAccountsWithConfig(autoRefresh)
				lastAccountRun = time.Now()
			}

			modelInterval := time.Duration(autoRefresh.ModelIntervalMinutes) * time.Minute
			if modelInterval <= 0 {
				modelInterval = 12 * time.Hour
			}
			if autoRefresh.RefreshModels && (lastModelRun.IsZero() || time.Since(lastModelRun) >= modelInterval) {
				h.refreshScheduledModelCaches(autoRefresh)
				lastModelRun = time.Now()
			}
		}

		select {
		case <-ticker.C:
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	h.refreshAllAccountsWithConfig(config.GetAutoRefreshConfig())
}

func (h *Handler) refreshAllAccountsWithConfig(autoRefresh config.AutoRefreshConfig) {
	accounts := config.GetAccounts()
	due := h.dueAutoRefreshAccounts(accounts, autoRefresh)
	selected := h.nextAutoRefreshAccounts(due, autoRefresh.MaxAccountsPerRun)
	startedAt := time.Now()
	h.beginAutoRefreshRun(startedAt.Unix(), len(selected))
	if len(selected) == 0 {
		h.finishAutoRefreshRun(startedAt, nil)
		return
	}

	concurrency := boundedRefreshConcurrency(len(selected))

	jobs := make(chan *config.Account)
	results := make(chan autoRefreshAccountResult, len(selected))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for account := range jobs {
				results <- h.refreshOneAccount(account, autoRefresh)
			}
		}()
	}
	for _, account := range selected {
		select {
		case jobs <- account:
		case <-h.stopRefresh:
			close(jobs)
			wg.Wait()
			close(results)
			h.finishAutoRefreshRun(startedAt, nil)
			return
		}
	}
	close(jobs)
	wg.Wait()
	close(results)

	runResults := make([]autoRefreshAccountResult, 0, len(selected))
	infoUpdates := make(map[string]config.AccountInfo)
	for result := range results {
		if result.Info != nil {
			infoUpdates[result.AccountID] = *result.Info
		}
		runResults = append(runResults, result)
	}
	if err := config.UpdateAccountInfoBatch(infoUpdates); err != nil {
		logger.Warnf("[BackgroundRefresh] Failed to persist account info batch: %v", err)
		for i := range runResults {
			if runResults[i].Info != nil {
				runResults[i].Status = "failed"
				runResults[i].Reason = "account info persistence failed"
				runResults[i].Err = err
			}
		}
	}
	h.finishAutoRefreshRun(startedAt, runResults)
	h.pool.Reload()
}

func boundedRefreshConcurrency(total int) int {
	if total <= 0 {
		return 0
	}
	concurrency := config.GetAutoRefreshConfig().RefreshConcurrency
	if concurrency <= 0 {
		concurrency = 5
	}
	if concurrency > 50 {
		concurrency = 50
	}
	if concurrency > total {
		concurrency = total
	}
	return concurrency
}

func (h *Handler) refreshOneAccount(account *config.Account, autoRefresh config.AutoRefreshConfig) (result autoRefreshAccountResult) {
	startedAt := time.Now().Unix()
	result = autoRefreshAccountResult{
		AccountID:  account.ID,
		Email:      account.Email,
		Status:     "skipped",
		StartedAt:  startedAt,
		FinishedAt: startedAt,
	}
	defer func() {
		result.FinishedAt = time.Now().Unix()
	}()

	if isKiroAPIKeyAccount(account) && account.AccessToken == "" {
		account.AccessToken = account.KiroApiKey
	}
	if !account.Enabled {
		result.Reason = "account disabled"
		return result
	}

	result.Attempted = true
	now := time.Now().Unix()
	tokenRefreshBefore := autoRefresh.TokenRefreshBeforeSeconds
	if tokenRefreshBefore <= 0 {
		tokenRefreshBefore = tokenRefreshSkewSeconds
	}
	needsTokenRefresh := !isKiroAPIKeyAccount(account) &&
		account.RefreshToken != "" &&
		(account.AccessToken == "" || (account.ExpiresAt > 0 && now > account.ExpiresAt-tokenRefreshBefore))
	if needsTokenRefresh {
		if err := h.refreshAccountToken(account); err != nil {
			if auth.IsRefreshUpstreamBlocked(err) {
				logger.Debugf("[BackgroundRefresh] Shared authentication upstream block for %s: %v", account.Email, err)
				result.Reason = "authentication refresh upstream temporarily blocked"
				result.Err = err
				return result
			}
			logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
			result.Status = "failed"
			result.Reason = "token refresh failed"
			result.Err = err
			result.CooldownUntil = h.markAutoRefreshFailure(account.ID, autoRefresh.FailureCooldownSeconds)
			h.handleAccountFailure(account, err)
			return result
		}
		h.clearAutoRefreshFailure(account.ID)
	}

	if account.AccessToken == "" {
		result.Reason = "no access token and no refresh token available"
		logger.Warnf("[BackgroundRefresh] Skipping %s: %s", account.Email, result.Reason)
		return result
	}
	if until := h.autoRefreshCooldownUntil(account.ID, time.Now().Unix()); until > 0 {
		result.Reason = "failure cooldown"
		result.CooldownUntil = until
		return result
	}

	info, err := RefreshAccountInfoContext(h.backgroundCtx, account)
	if err != nil {
		logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
		result.Status = "failed"
		result.Reason = "account info refresh failed"
		result.Err = err
		result.CooldownUntil = h.markAutoRefreshFailure(account.ID, autoRefresh.FailureCooldownSeconds)
		return result
	}

	h.clearAutoRefreshFailure(account.ID)
	logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	result.Status = "success"
	result.Info = info
	return result
}

func (h *Handler) refreshAccountToken(account *config.Account) error {
	ctx := h.backgroundCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return sharedTokenRefreshCoordinator.RefreshContext(ctx, account, true)
}

func (h *Handler) dueAutoRefreshAccounts(accounts []config.Account, autoRefresh config.AutoRefreshConfig) []config.Account {
	if len(accounts) == 0 {
		return nil
	}
	now := time.Now()
	baseInterval := time.Duration(autoRefresh.IntervalMinutes) * time.Minute
	if baseInterval <= 0 {
		baseInterval = 30 * time.Minute
	}
	refreshBefore := autoRefresh.TokenRefreshBeforeSeconds
	if refreshBefore <= 0 {
		refreshBefore = tokenRefreshSkewSeconds
	}

	due := make([]config.Account, 0, len(accounts))
	for _, account := range accounts {
		if !account.Enabled || h.isAutoRefreshCooling(account.ID, now.Unix()) {
			continue
		}
		if account.AccessToken == "" && account.RefreshToken == "" && account.KiroApiKey == "" {
			continue
		}

		tokenDue, _ := tokenRefreshUrgency(account, now.Unix(), refreshBefore)
		if tokenDue {
			due = append(due, account)
			continue
		}

		infoInterval := baseInterval
		if account.LastUsed == 0 || now.Unix()-account.LastUsed > int64((24*time.Hour)/time.Second) {
			infoInterval *= 4
			if infoInterval < 6*time.Hour {
				infoInterval = 6 * time.Hour
			}
		}
		if account.LastRefresh == 0 || now.Sub(time.Unix(account.LastRefresh, 0)) >= infoInterval {
			due = append(due, account)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		iDue, iExpiry := tokenRefreshUrgency(due[i], now.Unix(), refreshBefore)
		jDue, jExpiry := tokenRefreshUrgency(due[j], now.Unix(), refreshBefore)
		if iDue != jDue {
			return iDue
		}
		if iDue && iExpiry != jExpiry {
			return iExpiry < jExpiry
		}
		return false
	})
	return due
}

func tokenRefreshUrgency(account config.Account, now, refreshBefore int64) (bool, int64) {
	if isKiroAPIKeyAccount(&account) || strings.TrimSpace(account.RefreshToken) == "" {
		return false, 0
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return true, 0
	}
	if account.ExpiresAt > 0 && now >= account.ExpiresAt-refreshBefore {
		return true, account.ExpiresAt
	}
	return false, 0
}

func (h *Handler) nextAutoRefreshAccounts(accounts []config.Account, max int) []*config.Account {
	if len(accounts) == 0 {
		return nil
	}
	if max <= 0 || max > len(accounts) {
		max = len(accounts)
	}

	out := make([]*config.Account, 0, max)
	autoRefresh := config.GetAutoRefreshConfig()
	refreshBefore := autoRefresh.TokenRefreshBeforeSeconds
	if refreshBefore <= 0 {
		refreshBefore = tokenRefreshSkewSeconds
	}
	now := time.Now().Unix()
	urgentEnd := 0
	for urgentEnd < len(accounts) {
		urgent, _ := tokenRefreshUrgency(accounts[urgentEnd], now, refreshBefore)
		if !urgent {
			break
		}
		urgentEnd++
	}
	for i := 0; i < urgentEnd && len(out) < max; i++ {
		out = append(out, &accounts[i])
	}
	if len(out) >= max || urgentEnd == len(accounts) {
		return out
	}

	regular := accounts[urgentEnd:]
	remaining := max - len(out)
	if remaining > len(regular) {
		remaining = len(regular)
	}
	h.autoRefreshMu.Lock()
	start := h.autoRefreshNext % len(regular)
	h.autoRefreshNext = (start + remaining) % len(regular)
	if h.pool != nil {
		h.pool.SetRefreshCursor(h.autoRefreshNext)
	}
	h.autoRefreshMu.Unlock()
	for i := 0; i < remaining; i++ {
		idx := (start + i) % len(regular)
		out = append(out, &regular[idx])
	}
	return out
}

func (h *Handler) autoRefreshCooldownUntil(accountID string, now int64) int64 {
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	until := h.autoRefreshFail[accountID]
	if until <= 0 {
		return 0
	}
	if now >= until {
		delete(h.autoRefreshFail, accountID)
		if h.pool != nil {
			h.pool.ClearRefreshFailureCooldown(accountID)
		}
		return 0
	}
	return until
}

func (h *Handler) isAutoRefreshCooling(accountID string, now int64) bool {
	return h.autoRefreshCooldownUntil(accountID, now) > 0
}

func (h *Handler) markAutoRefreshFailure(accountID string, cooldownSeconds int64) int64 {
	if cooldownSeconds <= 0 {
		cooldownSeconds = 300
	}
	until := time.Now().Unix() + cooldownSeconds
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	if h.autoRefreshFail == nil {
		h.autoRefreshFail = make(map[string]int64)
	}
	h.autoRefreshFail[accountID] = until
	if h.pool != nil {
		h.pool.SetRefreshFailureCooldown(accountID, time.Unix(until, 0))
	}
	return until
}

func (h *Handler) clearAutoRefreshFailure(accountID string) {
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	delete(h.autoRefreshFail, accountID)
	if h.pool != nil {
		h.pool.ClearRefreshFailureCooldown(accountID)
	}
}

func (h *Handler) beginAutoRefreshRun(startedAt int64, selected int) {
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	if h.autoRefreshStat.Recent == nil {
		h.autoRefreshStat.Recent = make(map[string]autoRefreshAccountStatus)
	}
	h.autoRefreshStat.Running = true
	h.autoRefreshStat.LastRunStartedAt = startedAt
	h.autoRefreshStat.LastRunFinishedAt = 0
	h.autoRefreshStat.LastRunDurationMs = 0
	h.autoRefreshStat.LastRunSelected = selected
	h.autoRefreshStat.LastRunAttempted = 0
	h.autoRefreshStat.LastRunSuccess = 0
	h.autoRefreshStat.LastRunFailed = 0
	h.autoRefreshStat.LastRunSkipped = 0
	h.autoRefreshStat.Cursor = h.autoRefreshNext
	h.autoRefreshStat.CooldownCount = h.countActiveAutoRefreshCooldownsLocked(time.Now().Unix())
}

func (h *Handler) finishAutoRefreshRun(startedAt time.Time, results []autoRefreshAccountResult) {
	now := time.Now().Unix()
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	if h.autoRefreshStat.Recent == nil {
		h.autoRefreshStat.Recent = make(map[string]autoRefreshAccountStatus)
	}
	h.autoRefreshStat.Running = false
	h.autoRefreshStat.LastRunFinishedAt = now
	h.autoRefreshStat.LastRunDurationMs = time.Since(startedAt).Milliseconds()
	h.autoRefreshStat.LastRunAttempted = 0
	h.autoRefreshStat.LastRunSuccess = 0
	h.autoRefreshStat.LastRunFailed = 0
	h.autoRefreshStat.LastRunSkipped = 0
	for _, result := range results {
		if result.Attempted {
			h.autoRefreshStat.LastRunAttempted++
		}
		switch result.Status {
		case "success":
			h.autoRefreshStat.LastRunSuccess++
		case "failed":
			h.autoRefreshStat.LastRunFailed++
		default:
			h.autoRefreshStat.LastRunSkipped++
		}
		prev := h.autoRefreshStat.Recent[result.AccountID]
		next := autoRefreshAccountStatus{
			AccountID:     result.AccountID,
			Email:         result.Email,
			Status:        result.Status,
			Reason:        result.Reason,
			Error:         truncateAutoRefreshError(result.Err),
			LastStartedAt: result.StartedAt,
			LastSuccessAt: prev.LastSuccessAt,
			LastFailureAt: prev.LastFailureAt,
			CooldownUntil: result.CooldownUntil,
		}
		if next.Email == "" {
			next.Email = prev.Email
		}
		switch result.Status {
		case "success":
			next.LastSuccessAt = result.FinishedAt
			if prev.Status == "failed" && h.alerts != nil {
				h.alerts.Notify("account_recovered", map[string]interface{}{
					"accountId": result.AccountID, "email": result.Email,
				})
			}
		case "failed":
			next.LastFailureAt = result.FinishedAt
		}
		h.autoRefreshStat.Recent[result.AccountID] = next
	}
	h.autoRefreshStat.Cursor = h.autoRefreshNext
	h.autoRefreshStat.CooldownCount = h.countActiveAutoRefreshCooldownsLocked(now)
	h.pruneAutoRefreshStatusLocked()
}

func (h *Handler) countActiveAutoRefreshCooldownsLocked(now int64) int {
	count := 0
	for accountID, until := range h.autoRefreshFail {
		if until <= now {
			delete(h.autoRefreshFail, accountID)
			continue
		}
		count++
	}
	return count
}

func (h *Handler) pruneAutoRefreshStatusLocked() {
	if len(h.autoRefreshStat.Recent) == 0 {
		return
	}
	accounts := config.GetAccounts()
	active := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		active[account.ID] = true
	}
	for accountID := range h.autoRefreshStat.Recent {
		if !active[accountID] {
			delete(h.autoRefreshStat.Recent, accountID)
		}
	}
}

func (h *Handler) getAutoRefreshStatusSnapshot() autoRefreshStatus {
	h.autoRefreshMu.Lock()
	defer h.autoRefreshMu.Unlock()
	now := time.Now().Unix()
	h.autoRefreshStat.Cursor = h.autoRefreshNext
	h.autoRefreshStat.CooldownCount = h.countActiveAutoRefreshCooldownsLocked(now)
	snapshot := h.autoRefreshStat
	if snapshot.Recent != nil {
		snapshot.Recent = make(map[string]autoRefreshAccountStatus, len(h.autoRefreshStat.Recent))
		for k, v := range h.autoRefreshStat.Recent {
			if v.CooldownUntil > 0 && v.CooldownUntil <= now {
				v.CooldownUntil = 0
			}
			snapshot.Recent[k] = v
		}
	}
	return snapshot
}

func truncateAutoRefreshError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 500 {
		return msg[:500]
	}
	return msg
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		applyAuthErrorHeaders(w, ae)
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	r = withApiKeyContext(r, entry)
	lease, admissionErr := sharedAPIKeyAdmission.Acquire(r.Context(), entry)
	if admissionErr != nil {
		applyAuthErrorHeaders(w, admissionErr)
		h.sendClaudeError(w, admissionErr.status, admissionErr.code, admissionErr.message)
		return nil
	}
	return withAPIKeyAdmission(r, lease)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		applyAuthErrorHeaders(w, ae)
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	r = withApiKeyContext(r, entry)
	lease, admissionErr := sharedAPIKeyAdmission.Acquire(r.Context(), entry)
	if admissionErr != nil {
		applyAuthErrorHeaders(w, admissionErr)
		h.sendOpenAIError(w, admissionErr.status, admissionErr.code, admissionErr.message)
		return nil
	}
	return withAPIKeyAdmission(r, lease)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = withRequestID(w, r)
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] request_id=%s %s %s from %s", requestIDFromContext(r.Context()), r.Method, path, r.RemoteAddr)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"
	if path == "/admin/event-viewer.html" {
		csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
	}
	w.Header().Set("Content-Security-Policy", csp)
	if strings.HasPrefix(path, "/admin") {
		w.Header().Set("Cache-Control", "no-store")
	}

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch) {
		if r.ContentLength > maxRequestBodyBytes {
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		defer releaseAPIKeyAdmission(ar.Context())
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		defer releaseAPIKeyAdmission(ar.Context())
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		defer releaseAPIKeyAdmission(ar.Context())
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		defer releaseAPIKeyAdmission(ar.Context())
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		defer releaseAPIKeyAdmission(ar.Context())
		h.handleModels(w, ar)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

		// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)
	case path == "/ready":
		h.handleReady(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) readinessSnapshot() (bool, int, int, float64, string) {
	total := h.pool.Count()
	available := h.pool.AvailableCount()
	ratio := 0.0
	if total > 0 {
		ratio = float64(available) / float64(total)
	}
	health := config.GetHealthConfig()
	ready := total > 0 && available >= health.MinReadyAccounts && ratio >= health.MinReadyRatio
	reason := ""
	if total == 0 {
		reason = "no enabled accounts"
	} else if available < health.MinReadyAccounts {
		reason = fmt.Sprintf("available accounts %d below minimum %d", available, health.MinReadyAccounts)
	} else if ratio < health.MinReadyRatio {
		reason = fmt.Sprintf("available ratio %.3f below minimum %.3f", ratio, health.MinReadyRatio)
	}
	return ready, total, available, ratio, reason
}

func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request) {
	ready, total, available, ratio, reason := h.readinessSnapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		if h.alerts != nil {
			h.alerts.Notify("available_pool_low", map[string]interface{}{
				"accounts": total, "available": available, "availableRatio": ratio, "reason": reason,
			})
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         map[bool]string{true: "ready", false: "not_ready"}[ready],
		"ready":          ready,
		"accounts":       total,
		"available":      available,
		"availableRatio": ratio,
		"reason":         reason,
		"version":        config.Version,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests.Load(),
		"successRequests": h.successRequests.Load(),
		"failedRequests":  h.failedRequests.Load(),
		"totalTokens":     h.totalTokens.Load(),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.refreshModelsCacheAsyncOnce()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}
	models = mergeConfiguredModels(models, config.GetModelRegistryConfig().Models, thinkingSuffix)

	// 添加别名模型
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)
	models = dedupeModelResponse(models)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			maxInputTokens, maxOutputTokens := 0, 0
			if m.TokenLimits != nil {
				maxInputTokens = m.TokenLimits.MaxInputTokens
				maxOutputTokens = m.TokenLimits.MaxOutputTokens
			}
			models = append(models, buildModelInfoWithLimits(m.ModelId, "anthropic", supportsImage, maxInputTokens, maxOutputTokens))
			// 自动生成 thinking 变体
			models = append(models, buildModelInfoWithLimits(m.ModelId+thinkingSuffix, "anthropic", supportsImage, maxInputTokens, maxOutputTokens))
		}
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-5", "anthropic", true),
		buildModelInfo("claude-sonnet-5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.8", "anthropic", true),
		buildModelInfo("claude-opus-4.8"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	return buildModelInfoWithLimits(id, ownedBy, supportsImage, 0, 0)
}

func buildModelInfoWithLimits(id, ownedBy string, supportsImage bool, maxInputTokens, maxOutputTokens int) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	if entry, ok := config.GetConfiguredModelMetadata(id); ok {
		maxInputTokens = entry.ContextWindow
		maxOutputTokens = entry.MaxTokens
	} else {
		defaults := config.GetThinkingConfig()
		if defaults.DefaultContextWindowTokens > 0 {
			maxInputTokens = defaults.DefaultContextWindowTokens
		}
		if defaults.DefaultMaxOutputTokens > 0 {
			maxOutputTokens = defaults.DefaultMaxOutputTokens
		}
	}
	if maxInputTokens <= 0 {
		maxInputTokens = getContextWindowSize(id)
	}
	tokenLimits := map[string]int{"max_input_tokens": maxInputTokens}
	model := map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"context_window":   maxInputTokens,
		"max_input_tokens": maxInputTokens,
		"token_limits":     tokenLimits,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
	if maxOutputTokens > 0 {
		model["max_output_tokens"] = maxOutputTokens
		model["max_tokens"] = maxOutputTokens
		tokenLimits["max_output_tokens"] = maxOutputTokens
	}
	return model
}

func mergeConfiguredModels(models []map[string]interface{}, configured []config.ModelEntry, thinkingSuffix string) []map[string]interface{} {
	index := make(map[string]int, len(models))
	for i, model := range models {
		if id, ok := model["id"].(string); ok {
			index[strings.ToLower(strings.TrimSpace(id))] = i
		}
	}
	for _, entry := range configured {
		for _, id := range []string{entry.ID, entry.ID + thinkingSuffix} {
			info := buildModelInfoWithLimits(id, "configured", true, entry.ContextWindow, entry.MaxTokens)
			info["display_name"] = entry.DisplayName
			info["kiro_model_id"] = entry.KiroModelID
			info["created"] = entry.Created
			key := strings.ToLower(strings.TrimSpace(id))
			if existing, ok := index[key]; ok {
				models[existing] = info
				continue
			}
			index[key] = len(models)
			models = append(models, info)
		}
	}
	return models
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	if strings.EqualFold(config.GetPreferredEndpoint(), "runtime") && !config.GetEndpointFallback() {
		logger.Debugf("[ModelsCache] Runtime endpoint does not support ListAvailableModels; keeping static/configured cache")
		return
	}
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}
	h.pruneModelsByAccount(accounts)
	_, failed := h.refreshModelCaches(accounts)
	h.modelsCacheMu.RLock()
	cached := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	logger.Infof("[ModelsCache] Cached %d models (%d account failures)", cached, failed)
}

func (h *Handler) refreshModelsCacheAsyncOnce() {
	if !h.modelsRefreshing.CompareAndSwap(false, true) {
		return
	}
	if !h.startBackgroundTask(func() {
		defer h.modelsRefreshing.Store(false)
		h.refreshModelsCache()
	}) {
		h.modelsRefreshing.Store(false)
	}
}

func (h *Handler) refreshScheduledModelCaches(autoRefresh config.AutoRefreshConfig) {
	if strings.EqualFold(config.GetPreferredEndpoint(), "runtime") && !config.GetEndpointFallback() {
		return
	}
	if !h.modelsRefreshing.CompareAndSwap(false, true) {
		return
	}
	defer h.modelsRefreshing.Store(false)
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}
	h.pruneModelsByAccount(accounts)

	now := time.Now().Unix()
	eligible := make([]config.Account, 0, len(accounts))
	for _, account := range accounts {
		if h.isAutoRefreshCooling(account.ID, now) {
			continue
		}
		if account.AccessToken == "" && account.RefreshToken == "" && account.KiroApiKey == "" {
			continue
		}
		eligible = append(eligible, account)
	}
	selected := h.nextModelRefreshAccounts(eligible, autoRefresh.MaxModelsPerRun)
	if len(selected) == 0 {
		return
	}
	succeeded, failed := h.refreshModelCachesWithConcurrency(selected, autoRefresh.ModelRefreshConcurrency)
	logger.Infof("[ModelsCache] Scheduled refresh completed: %d succeeded, %d failed", succeeded, failed)
}

func (h *Handler) nextModelRefreshAccounts(accounts []config.Account, max int) []config.Account {
	if len(accounts) == 0 {
		return nil
	}
	if max <= 0 || max > len(accounts) {
		max = len(accounts)
	}
	h.autoRefreshMu.Lock()
	start := h.autoRefreshModelNext % len(accounts)
	h.autoRefreshModelNext = (start + max) % len(accounts)
	h.autoRefreshMu.Unlock()

	selected := make([]config.Account, 0, max)
	for i := 0; i < max; i++ {
		selected = append(selected, accounts[(start+i)%len(accounts)])
	}
	return selected
}

func (h *Handler) refreshModelCaches(accounts []config.Account) (int, int) {
	return h.refreshModelCachesWithConcurrency(accounts, boundedRefreshConcurrency(len(accounts)))
}

func (h *Handler) refreshModelCachesWithConcurrency(accounts []config.Account, concurrency int) (int, int) {
	if len(accounts) == 0 {
		return 0, 0
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(accounts) {
		concurrency = len(accounts)
	}
	jobs := make(chan config.Account)
	results := make(chan error, len(accounts))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for account := range jobs {
				err := h.fetchAndCacheAccountModels(&account)
				if err != nil {
					logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
				}
				results <- err
			}
		}()
	}
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-h.stopRefresh:
			close(jobs)
			wg.Wait()
			close(results)
			return 0, 0
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	succeeded, failed := 0, 0
	for err := range results {
		if err != nil {
			failed++
		} else {
			succeeded++
		}
	}
	return succeeded, failed
}

func (h *Handler) refreshModelCachesAsync(accounts []config.Account) {
	if len(accounts) == 0 {
		return
	}
	copies := append([]config.Account(nil), accounts...)
	h.startBackgroundTask(func() { h.refreshModelCaches(copies) })
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	return h.fetchAndCacheAccountModelsContext(h.backgroundCtx, account)
}

func (h *Handler) fetchAndCacheAccountModelsContext(ctx context.Context, account *config.Account) error {
	if strings.EqualFold(config.GetPreferredEndpoint(), "runtime") && !config.GetEndpointFallback() {
		return nil
	}
	if err := h.ensureValidTokenContext(ctx, account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModelsContext(ctx, account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// Replace this account's snapshot, then rebuild the aggregate so models
	// removed upstream do not remain forever in the global list.
	h.modelsCacheMu.Lock()
	if h.modelsByAccount == nil {
		h.modelsByAccount = make(map[string][]ModelInfo)
	}
	h.modelsByAccount[account.ID] = append([]ModelInfo(nil), models...)
	h.rebuildCachedModelsLocked()
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

func (h *Handler) pruneModelsByAccount(accounts []config.Account) {
	enabled := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if account.Enabled {
			enabled[account.ID] = struct{}{}
		}
	}
	h.modelsCacheMu.Lock()
	changed := false
	for accountID := range h.modelsByAccount {
		if _, ok := enabled[accountID]; !ok {
			delete(h.modelsByAccount, accountID)
			changed = true
		}
	}
	if changed {
		h.rebuildCachedModelsLocked()
		h.modelsCacheTime = time.Now().Unix()
	}
	h.modelsCacheMu.Unlock()
}

func (h *Handler) rebuildCachedModelsLocked() {
	var aggregate []ModelInfo
	accountIDs := make([]string, 0, len(h.modelsByAccount))
	for accountID := range h.modelsByAccount {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		aggregate = mergeUniqueModels(aggregate, h.modelsByAccount[accountID])
	}
	h.cachedModels = aggregate
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	failed := 0
	accounts := config.GetEnabledAccounts()
	h.pruneModelsByAccount(accounts)
	if !(strings.EqualFold(config.GetPreferredEndpoint(), "runtime") && !config.GetEndpointFallback()) {
		_, failed = h.refreshModelCaches(accounts)
	}
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    failed,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	} else if extra.TokenLimits != nil {
		if extra.TokenLimits.MaxInputTokens > base.TokenLimits.MaxInputTokens {
			base.TokenLimits.MaxInputTokens = extra.TokenLimits.MaxInputTokens
		}
		if extra.TokenLimits.MaxOutputTokens > base.TokenLimits.MaxOutputTokens {
			base.TokenLimits.MaxOutputTokens = extra.TokenLimits.MaxOutputTokens
		}
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	startedAt := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, requestBodyErrorStatus(err), "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	r = h.attachRequestDetailTrace(r, "claude.count_tokens", body)
	w, detailStatus := wrapRequestDetailResponseWriter(w, r.Context())
	defer h.finalizeUnrecordedRequestDetail(r.Context(), detailStatus, startedAt, "claude.count_tokens", req.Model)
	thinkingCfg := config.GetThinkingConfig()
	_ = applyClaudeTokenBudgetDefaults(&req)
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if externalTokens, externalErr := callExternalCountTokensContext(r.Context(), effectiveReq); externalErr == nil && externalTokens > 0 {
		estimatedTokens = externalTokens
	} else if externalErr != nil {
		logger.Warnf("[CountTokens] remote provider failed, using local estimate: %v", externalErr)
	}
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens}); err != nil {
		return
	}
	if trace := requestDetailTraceFromContext(r.Context()); trace != nil {
		trace.recordComplete(estimatedTokens, 0)
	}
	h.recordRequestLogForContext(r.Context(), requestLogEntry{
		Timestamp:    time.Now().Unix(),
		Protocol:     "claude.count_tokens",
		Model:        req.Model,
		Status:       "success",
		StatusCode:   http.StatusOK,
		DurationMs:   requestDurationMs(startedAt),
		InputTokens:  estimatedTokens,
		OutputTokens: 0,
	})
}

func configureClaudeToolStreaming(payload *KiroPayload, req *ClaudeRequest, thinking bool, thinkingOpts claudeThinkingResponseOptions, thinkingCfg config.ThinkingConfig) {
	if payload == nil || req == nil {
		return
	}
	safeMode := thinkingCfg.ToolStreamMode == config.ToolStreamModeSafe
	liveMode := thinkingCfg.ToolStreamMode == config.ToolStreamModeLive
	adaptiveMode := thinkingCfg.ToolStreamMode == config.ToolStreamModeAdaptive
	highRiskTools := hasHighRiskToolNames(claudeToolNames(req.Tools))
	useSafeBehavior := safeMode || (adaptiveMode && highRiskTools)
	useLiveBehavior := liveMode || (adaptiveMode && !highRiskTools)
	strictToolUse := requiresStrictClaudeToolUse(req)
	guardToolStream := len(req.Tools) > 0 && (useSafeBehavior || strictToolUse)
	guardActionableStream := req.Stream && guardToolStream

	payload.requireActionableOutput = (len(req.Tools) > 0 || thinking) && (!req.Stream || guardActionableStream)
	payload.toolUsePolicy = req.ToolUsePolicy
	payload.deferTextUntilComplete = guardActionableStream && useSafeBehavior && req.ToolUsePolicy == toolUsePolicyInferred
	payload.streamThinkingPrecommit = guardActionableStream && thinking && !thinkingOpts.OmitDisplay
	payload.streamToolUseDeltas = req.Stream && len(req.Tools) > 0 && useLiveBehavior
	// Inferred workspace intent adds strong tool guidance, but only an explicit
	// client tool_choice may reject an otherwise valid text response.
	payload.requireToolUse = strictToolUse
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	startedAt := time.Now()
	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, requestBodyErrorStatus(err), "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	r = h.attachRequestDetailTrace(r, "claude.messages", body)
	w, detailStatus := wrapRequestDetailResponseWriter(w, r.Context())
	defer func() {
		protocol := "claude.messages"
		if req.Stream {
			protocol += ".stream"
		}
		h.finalizeUnrecordedRequestDetail(r.Context(), detailStatus, startedAt, protocol, req.Model)
	}()
	thinkingCfg := config.GetThinkingConfig()
	contextWindowTokens := applyClaudeTokenBudgetDefaults(&req)
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	if err := prepareClaudeToolPolicy(&req, thinkingCfg.EnforceAgentToolUse); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	if !h.requestedModelAvailable(req.Model, actualModel) {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "The requested model is not available")
		return
	}
	if fallbackModel, changed := maybeLongToolFallback(actualModel, req.MaxTokens, claudeToolNames(req.Tools)); changed {
		actualModel = fallbackModel
		contextWindowTokens = resolveContextWindowTokens(actualModel, req.ContextWindow, req.MaxInputTokens)
	}
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if admissionErr := reserveAPIKeyTokens(r.Context(), estimatedInputTokens); admissionErr != nil {
		applyAuthErrorHeaders(w, admissionErr)
		h.sendClaudeError(w, admissionErr.status, admissionErr.code, admissionErr.message)
		return
	}
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	if config.GetWebSearchConfig().Enabled && hasPureWebSearchTool(&req) {
		h.handleClaudeWebSearch(r.Context(), w, &req, estimatedInputTokens, apiKeyIDFromContext(r.Context()))
		return
	}

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)
	kiroPayload.requestContext = r.Context()
	kiroPayload.contextWindowTokens = contextWindowTokens
	truncatePayloadToLimit(kiroPayload, kiroPayload.hasSystemPriming)

	// Stream or non-stream
	apiKeyID := apiKeyIDFromContext(r.Context())
	namespaceConversationID(kiroPayload, requestConversationNamespace(r, apiKeyID))
	routeKey := kiroPayload.ConversationState.ConversationID
	configureClaudeToolStreaming(kiroPayload, &req, thinking, thinkingResponseOpts, thinkingCfg)
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, routeKey)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, routeKey)
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID, routeKey string) {
	startedAt := time.Now()
	firstContent := payload.beginRequestTiming(startedAt)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error
	var sseMu sync.Mutex
	messageStarted := false
	streamFinished := false
	var messageStartUsage promptCacheUsage
	lastThinkingHeartbeatAt := startedAt
	nextContentIndex := 0
	var rawThinkingBuilder strings.Builder
	emitSSE := func(event string, data interface{}) {
		firstContent.MarkSSEEvent(event == "ping")
		h.sendSSE(w, flusher, event, data)
	}
	sendSSE := func(event string, data interface{}) {
		sseMu.Lock()
		emitSSE(event, data)
		sseMu.Unlock()
	}
	isMessageStarted := func() bool {
		sseMu.Lock()
		defer sseMu.Unlock()
		return messageStarted
	}

	ensureMessageStart := func() {
		sseMu.Lock()
		defer sseMu.Unlock()
		if messageStarted {
			return
		}
		emitSSE("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}
	sendStreamError := func(status int, errorType, message string) {
		sseMu.Lock()
		if messageStarted {
			streamFinished = true
			emitSSE("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": errorType, "message": message},
			})
			sseMu.Unlock()
			return
		}
		sseMu.Unlock()
		h.sendClaudeError(w, status, errorType, message)
	}
	if payload.requireActionableOutput {
		// A guarded tool stream may spend a long time generating a complete tool
		// payload. Start the standard Anthropic stream immediately while keeping
		// account and endpoint retries safe until actual content is committed.
		ensureMessageStart()
	}
	heartbeatDone := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(claudeStreamHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sseMu.Lock()
				if messageStarted && !streamFinished {
					emitSSE("ping", map[string]string{"type": "ping"})
				}
				sseMu.Unlock()
			case <-heartbeatDone:
				return
			}
		}
	}()
	defer func() {
		close(heartbeatDone)
		heartbeatWG.Wait()
	}()

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, routeKey, payload)
		if busy != nil {
			busyErr = busy
			break
		}
		if account == nil {
			break
		}
		release := func() {
			if guard != nil {
				guard.Release()
				guard = nil
			}
		}
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		cacheScope := h.promptCache.ScopeKey(account.ID, apiKeyID)
		cacheUsage, cacheDiagnostic := h.promptCache.ComputeDetailed(cacheScope, cacheProfile)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage
		var truncated bool
		var toolUses []KiroToolUse
		var rawContentBuilder strings.Builder
		actionableCommitted := false
		activeBlockIndex := -1
		activeBlockType := ""
		type streamedClaudeTool struct {
			index   int
			stopped bool
		}
		streamedTools := make(map[string]*streamedClaudeTool)

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			sendSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				sendSSE("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				sendSSE("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				actionableCommitted = true
				startContentBlock("text")
				sendSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}
			if strings.TrimSpace(text) != "" && (!payload.streamThinkingPrecommit || payload.streamToolUseDeltas) {
				actionableCommitted = true
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				sendSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				sendSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					sendSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		startToolUse := func(toolUseID, name string) {
			if toolUseID == "" || streamedTools[toolUseID] != nil {
				return
			}
			firstContent.MarkToolOutput()
			processClaudeText("", false, true)
			ensureMessageStart()
			actionableCommitted = true
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++
			streamedTools[toolUseID] = &streamedClaudeTool{index: idx}
			sendSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    toolUseID,
					"name":  name,
					"input": map[string]interface{}{},
				},
			})
		}

		sendToolUseDelta := func(toolUseID, input string) {
			if input == "" {
				return
			}
			tool := streamedTools[toolUseID]
			if tool == nil || tool.stopped {
				return
			}
			firstContent.MarkToolOutput()
			sendSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": tool.index,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": input,
				},
			})
		}

		stopToolUse := func(toolUseID string) {
			tool := streamedTools[toolUseID]
			if tool == nil || tool.stopped {
				return
			}
			tool.stopped = true
			sendSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": tool.index,
			})
		}

		callback := &KiroStreamCallback{
			OnResponseStart: ensureMessageStart,
			OnText: func(text string, isThinking bool) {
				firstContent.MarkOutput(text, isThinking)
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				firstContent.MarkToolOutput()
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				if streamedTools[tu.ToolUseID] != nil {
					stopToolUse(tu.ToolUseID)
					delete(streamedTools, tu.ToolUseID)
					return
				}

				startToolUse(tu.ToolUseID, tu.Name)
				inputJSON, _ := json.Marshal(tu.Input)
				sendToolUseDelta(tu.ToolUseID, string(inputJSON))
				stopToolUse(tu.ToolUseID)
				delete(streamedTools, tu.ToolUseID)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnProgress: func() {
				if !isMessageStarted() || time.Since(lastThinkingHeartbeatAt) < claudeStreamHeartbeatInterval {
					return
				}
				lastThinkingHeartbeatAt = time.Now()
				if payload.streamThinkingPrecommit && !actionableCommitted && thinkingFormat == "thinking" {
					startContentBlock("thinking")
					sendSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": " "},
					})
					thinkingStarted = true
					eventThinkingOpen = true
				}
			},
			OnUsage: func(usage KiroTokenUsage) {
				upstreamUsage = usage
				if !isMessageStarted() && usage.HasCacheBreakdown {
					messageStartUsage, startInputTokens = resolvePromptCacheUsage(cacheUsage, usage, startInputTokens, cacheProfile)
				}
			},
			OnTruncated: func(string) {
				truncated = true
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}
		if payload.streamToolUseDeltas {
			callback.OnToolUseStart = startToolUse
			callback.OnToolUseDelta = sendToolUseDelta
			callback.OnToolUseStop = stopToolUse
		}

		err := h.callKiroAPIWithHealth(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, model, err)
			if !actionableCommitted {
				if eventThinkingOpen {
					sendText("", 3)
					eventThinkingOpen = false
					thinkingStarted = false
				}
				closeActiveBlock()
				if !shouldRetryAcrossAccounts(err) {
					break
				}
				continue
			}
			mapped := mapDownstreamError(err)
			h.recordFailure()
			entry := requestLogEntry{
				Timestamp:           time.Now().Unix(),
				Protocol:            "claude.messages.stream",
				Model:               model,
				AccountID:           account.ID,
				AccountEmail:        account.Email,
				Endpoint:            upstreamErrorEndpoint(err),
				Status:              "failed",
				StatusCode:          mapped.Status,
				FirstContentMs:      firstContent.Value(),
				DurationMs:          requestDurationMs(startedAt),
				VisibleOutputChars:  outputCharCount(rawContentBuilder.String()),
				ThinkingOutputChars: outputCharCount(rawThinkingBuilder.String()),
				ToolUseCount:        len(toolUses),
				Error:               err.Error(),
			}
			h.recordDiagnosticFailureForPayload("claude.messages.stream", model, account, mapped.Status, err, payload)
			sendStreamError(mapped.Status, mapped.ClaudeType, err.Error())
			entry.DurationMs = requestDurationMs(startedAt)
			h.recordRequestLogForPayload(payload, entry)
			return
		}

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens = resolvePromptCacheUsage(cacheUsage, upstreamUsage, inputTokens, cacheProfile)
		cacheDiagnostic = finalizePromptCacheDiagnostic(cacheDiagnostic, upstreamUsage, cacheUsage, inputTokens)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		thinkingTokens := upstreamUsage.ThinkingTokens
		if thinkingTokens <= 0 {
			thinkingTokens = estimateApproxTokens(thinkingOutput)
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)
		stopReason := "end_turn"
		if truncated {
			stopReason = "max_tokens"
		} else if len(toolUses) > 0 {
			stopReason = "tool_use"
		}

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(cacheScope, cacheProfile)
		h.promptCache.RecordDecision(cacheUsage, cacheDiagnostic)
		entry := requestLogEntry{
			Timestamp:                time.Now().Unix(),
			Protocol:                 "claude.messages.stream",
			Model:                    model,
			AccountID:                account.ID,
			AccountEmail:             account.Email,
			Status:                   "success",
			StatusCode:               200,
			FirstContentMs:           firstContent.Value(),
			DurationMs:               requestDurationMs(startedAt),
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			ThinkingTokens:           thinkingTokens,
			CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
			CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
			VisibleOutputChars:       outputCharCount(outputContent),
			ThinkingOutputChars:      outputCharCount(thinkingOutput),
			ToolUseCount:             len(toolUses),
			StopReason:               stopReason,
			Credits:                  credits,
		}

		ensureMessageStart()
		sseMu.Lock()
		streamFinished = true
		emitSSE("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, thinkingTokens, cacheUsage, cacheProfile != nil || upstreamUsage.HasCacheBreakdown),
		})

		emitSSE("message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		sseMu.Unlock()
		entry.DurationMs = requestDurationMs(startedAt)
		h.recordRequestLogForPayload(payload, entry)
		return
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		h.recordCanceledRequestForPayload(payload, "claude.messages.stream", model, startedAt, firstContent.Value(), stopErr)
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			entry := requestLogEntry{
				Timestamp:      time.Now().Unix(),
				Protocol:       "claude.messages.stream",
				Model:          model,
				Status:         "failed",
				StatusCode:     429,
				FirstContentMs: firstContent.Value(),
				DurationMs:     requestDurationMs(startedAt),
				Error:          busyErr.Error(),
			}
			h.recordDiagnosticFailureForPayload("claude.messages.stream", model, nil, 429, busyErr, payload)
			w.Header().Set("Retry-After", "1")
			sendStreamError(429, "rate_limit_error", busyErr.Error())
			entry.DurationMs = requestDurationMs(startedAt)
			h.recordRequestLogForPayload(payload, entry)
			return
		}
		sendStreamError(503, "api_error", "No available accounts")
		return
	}

	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	entry := requestLogEntry{
		Timestamp:      time.Now().Unix(),
		Protocol:       "claude.messages.stream",
		Model:          model,
		Endpoint:       upstreamErrorEndpoint(lastErr),
		Status:         "failed",
		StatusCode:     mapped.Status,
		FirstContentMs: firstContent.Value(),
		DurationMs:     requestDurationMs(startedAt),
		Error:          lastErr.Error(),
	}
	h.recordDiagnosticFailureForPayload("claude.messages.stream", model, nil, mapped.Status, lastErr, payload)
	applyDownstreamErrorHeaders(w, mapped)
	sendStreamError(mapped.Status, mapped.ClaudeType, lastErr.Error())
	entry.DurationMs = requestDurationMs(startedAt)
	h.recordRequestLogForPayload(payload, entry)
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	if err := config.UpdateStats(
		int(h.totalRequests.Load()),
		int(h.successRequests.Load()),
		int(h.failedRequests.Load()),
		int(h.totalTokens.Load()),
		h.getCredits(),
	); err != nil {
		logger.Warnf("[Stats] Failed to persist global statistics: %v", err)
	}
}

// FlushStats persists global and per-account counters immediately.
func (h *Handler) FlushStats() {
	if h == nil {
		return
	}
	h.pool.FlushStats()
	h.pool.FlushRuntimeState()
	if err := config.FlushPendingWrites(); err != nil {
		logger.Warnf("[ApiKey] Failed to flush pending usage: %v", err)
	}
	h.saveStats()
}

// StopBackground signals background loops without waiting for active HTTP requests.
func (h *Handler) StopBackground() {
	if h == nil {
		return
	}
	h.stopOnce.Do(func() {
		h.backgroundTaskMu.Lock()
		h.backgroundClosing = true
		h.backgroundTaskMu.Unlock()
		if h.backgroundCancel != nil {
			h.backgroundCancel()
		}
		if h.stopRefresh != nil {
			close(h.stopRefresh)
		}
		if h.stopStatsSaver != nil {
			close(h.stopStatsSaver)
		}
	})
}

// Close stops and joins background loops. It is safe to call more than once.
func (h *Handler) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		h.StopBackground()
		h.backgroundWG.Wait()
		if h.requestLog != nil {
			if err := h.requestLog.Flush(); err != nil {
				logger.Warnf("[RequestLog] Failed to flush request log: %v", err)
			}
		}
		if h.requestDetails != nil {
			if err := h.requestDetails.Flush(); err != nil {
				logger.Warnf("[RequestDetail] Failed to flush request details: %v", err)
			}
		}
		if h.promptCache != nil {
			cacheCfg := config.GetPromptCacheConfig()
			if cacheCfg.PersistEnabled {
				if err := h.promptCache.Flush(promptCachePath()); err != nil {
					logger.Warnf("[PromptCache] Failed to flush persisted state: %v", err)
				}
			} else if err := h.promptCache.RemovePersisted(promptCachePath()); err != nil {
				logger.Warnf("[PromptCache] Failed to remove persisted state: %v", err)
			}
		}
		if h.alerts != nil {
			h.alerts.Close()
		}
		h.FlushStats()
	})
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	h.totalRequests.Add(1)
	h.successRequests.Add(1)
	h.totalTokens.Add(int64(inputTokens + outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(ctx context.Context, apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	reconcileAPIKeyTokens(ctx, inputTokens+outputTokens)
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

func (h *Handler) recordFailure() {
	h.totalRequests.Add(1)
	h.failedRequests.Add(1)
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID, routeKey string) {
	startedAt := time.Now()
	firstContent := payload.beginRequestTiming(startedAt)
	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, routeKey, payload)
		if busy != nil {
			busyErr = busy
			break
		}
		if account == nil {
			break
		}
		release := func() {
			if guard != nil {
				guard.Release()
				guard = nil
			}
		}
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		cacheScope := h.promptCache.ScopeKey(account.ID, apiKeyID)
		cacheUsage, cacheDiagnostic := h.promptCache.ComputeDetailed(cacheScope, cacheProfile)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				firstContent.MarkOutput(text, isThinking)
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				firstContent.MarkToolOutput()
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnUsage: func(usage KiroTokenUsage) {
				upstreamUsage = usage
			},
			OnTruncated: func(string) {
				truncated = true
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}

		err := h.callKiroAPIWithHealth(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, model, err)
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens = resolvePromptCacheUsage(cacheUsage, upstreamUsage, inputTokens, cacheProfile)
		cacheDiagnostic = finalizePromptCacheDiagnostic(cacheDiagnostic, upstreamUsage, cacheUsage, inputTokens)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		thinkingTokens := upstreamUsage.ThinkingTokens
		if thinkingTokens <= 0 {
			thinkingTokens = estimateApproxTokens(rawThinkingContent)
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)
		stopReason := "end_turn"
		if truncated {
			stopReason = "max_tokens"
		} else if len(toolUses) > 0 {
			stopReason = "tool_use"
		}

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(cacheScope, cacheProfile)
		h.promptCache.RecordDecision(cacheUsage, cacheDiagnostic)
		h.recordRequestLogForPayload(payload, requestLogEntry{
			Timestamp:                time.Now().Unix(),
			Protocol:                 "claude.messages",
			Model:                    model,
			AccountID:                account.ID,
			AccountEmail:             account.Email,
			Status:                   "success",
			StatusCode:               200,
			FirstContentMs:           firstContent.Value(),
			DurationMs:               requestDurationMs(startedAt),
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			ThinkingTokens:           thinkingTokens,
			CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
			CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
			VisibleOutputChars:       outputCharCount(finalContent),
			ThinkingOutputChars:      outputCharCount(rawThinkingContent),
			ToolUseCount:             len(toolUses),
			StopReason:               stopReason,
			Credits:                  credits,
		})

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		if truncated {
			resp.StopReason = "max_tokens"
		}
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.ThinkingTokens = thinkingTokens
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil || upstreamUsage.HasCacheBreakdown {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		h.recordCanceledRequestForPayload(payload, "claude.messages", model, startedAt, firstContent.Value(), stopErr)
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:      time.Now().Unix(),
				Protocol:       "claude.messages",
				Model:          model,
				Status:         "failed",
				StatusCode:     429,
				FirstContentMs: firstContent.Value(),
				DurationMs:     requestDurationMs(startedAt),
				Error:          busyErr.Error(),
			})
			h.recordDiagnosticFailure(diagnosticLogEntry{
				Protocol:       "claude.messages",
				Model:          model,
				StatusCode:     429,
				Error:          busyErr.Error(),
				RequestSummary: summarizeKiroPayload(payload),
			})
			w.Header().Set("Retry-After", "1")
			h.sendClaudeError(w, 429, "rate_limit_error", busyErr.Error())
			return
		}
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:      time.Now().Unix(),
		Protocol:       "claude.messages",
		Model:          model,
		Status:         "failed",
		StatusCode:     mapped.Status,
		FirstContentMs: firstContent.Value(),
		DurationMs:     requestDurationMs(startedAt),
		Error:          lastErr.Error(),
	})
	h.recordDiagnosticFailure(diagnosticLogEntry{
		Protocol:       "claude.messages",
		Model:          model,
		StatusCode:     mapped.Status,
		Error:          lastErr.Error(),
		RequestSummary: summarizeKiroPayload(payload),
	})
	applyDownstreamErrorHeaders(w, mapped)
	h.sendClaudeError(w, mapped.Status, mapped.ClaudeType, lastErr.Error())
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	startedAt := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, requestBodyErrorStatus(err), "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	r = h.attachRequestDetailTrace(r, "openai.chat", body)
	w, detailStatus := wrapRequestDetailResponseWriter(w, r.Context())
	defer func() {
		protocol := "openai.chat"
		if req.Stream {
			protocol += ".stream"
		}
		h.finalizeUnrecordedRequestDetail(r.Context(), detailStatus, startedAt, protocol, req.Model)
	}()
	contextWindowTokens := applyOpenAITokenBudgetDefaults(&req)
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}
	if err := applyOpenAIToolChoice(&req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	if !h.requestedModelAvailable(req.Model, actualModel) {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "The requested model is not available")
		return
	}
	if fallbackModel, changed := maybeLongToolFallback(actualModel, req.MaxTokens, openAIToolNames(req.Tools)); changed {
		actualModel = fallbackModel
		contextWindowTokens = resolveContextWindowTokens(actualModel, req.ContextWindow, req.MaxInputTokens)
	}
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)
	if admissionErr := reserveAPIKeyTokens(r.Context(), estimatedInputTokens); admissionErr != nil {
		applyAuthErrorHeaders(w, admissionErr)
		h.sendOpenAIError(w, admissionErr.status, admissionErr.code, admissionErr.message)
		return
	}

	kiroPayload := OpenAIToKiro(&req, thinking)
	kiroPayload.requestContext = r.Context()
	kiroPayload.contextWindowTokens = contextWindowTokens
	truncatePayloadToLimit(kiroPayload, kiroPayload.hasSystemPriming)

	apiKeyID := apiKeyIDFromContext(r.Context())
	namespaceConversationID(kiroPayload, requestConversationNamespace(r, apiKeyID))
	if req.Stream {
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	} else {
		h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	startedAt := time.Now()
	firstContent := payload.beginRequestTiming(startedAt)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, payload.ConversationState.ConversationID, payload)
		if busy != nil {
			busyErr = busy
			break
		}
		if account == nil {
			break
		}
		release := func() {
			if guard != nil {
				guard.Release()
				guard = nil
			}
		}
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage
		var truncated bool
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			firstContent.MarkSSEEvent(false)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				firstContent.MarkOutput(text, isThinking)
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				firstContent.MarkToolOutput()
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				firstContent.MarkSSEEvent(false)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnUsage: func(usage KiroTokenUsage) {
				upstreamUsage = usage
			},
			OnTruncated: func(string) {
				truncated = true
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}

		err := h.callKiroAPIWithHealth(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, model, err)
			if !responseStarted {
				if !shouldRetryAcrossAccounts(err) {
					break
				}
				continue
			}
			mapped := mapDownstreamError(err)
			h.recordFailure()
			entry := requestLogEntry{
				Timestamp:      time.Now().Unix(),
				Protocol:       "openai.chat.stream",
				Model:          model,
				AccountID:      account.ID,
				AccountEmail:   account.Email,
				Status:         "failed",
				StatusCode:     mapped.Status,
				FirstContentMs: firstContent.Value(),
				DurationMs:     requestDurationMs(startedAt),
				Error:          err.Error(),
			}
			h.recordDiagnosticFailureForPayload("openai.chat.stream", model, account, mapped.Status, err, payload)
			chunk, _ := json.Marshal(map[string]interface{}{
				"error": map[string]string{"type": mapped.OpenAIType, "message": err.Error()},
			})
			firstContent.MarkSSEEvent(false)
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			firstContent.MarkSSEEvent(false)
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			entry.DurationMs = requestDurationMs(startedAt)
			h.recordRequestLogForPayload(payload, entry)
			return
		}

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens := resolvePromptCacheUsage(promptCacheUsage{}, upstreamUsage, inputTokens, nil)
		cacheDiagnostic := finalizePromptCacheDiagnostic(promptCacheDiagnostic{Status: "skipped", Reason: "no_cache_breakpoint", Source: "local"}, upstreamUsage, cacheUsage, inputTokens)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		thinkingTokens := upstreamUsage.ThinkingTokens
		if thinkingTokens <= 0 {
			thinkingTokens = estimateApproxTokens(reasoningOutput)
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		entry := requestLogEntry{
			Timestamp:                time.Now().Unix(),
			Protocol:                 "openai.chat.stream",
			Model:                    model,
			AccountID:                account.ID,
			AccountEmail:             account.Email,
			Status:                   "success",
			StatusCode:               200,
			FirstContentMs:           firstContent.Value(),
			DurationMs:               requestDurationMs(startedAt),
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			ThinkingTokens:           thinkingTokens,
			CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
			CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
			VisibleOutputChars:       outputCharCount(outputContent),
			ThinkingOutputChars:      outputCharCount(reasoningOutput),
			ToolUseCount:             len(toolCalls),
			Credits:                  credits,
		}

		finishReason := "stop"
		if truncated {
			finishReason = "length"
		} else if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": buildOpenAIUsageMap(inputTokens, outputTokens, thinkingTokens, cacheUsage),
		}
		data, _ := json.Marshal(chunk)
		firstContent.MarkSSEEvent(false)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		firstContent.MarkSSEEvent(false)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		entry.DurationMs = requestDurationMs(startedAt)
		h.recordRequestLogForPayload(payload, entry)
		return
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		h.recordCanceledRequestForPayload(payload, "openai.chat.stream", model, startedAt, firstContent.Value(), stopErr)
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:      time.Now().Unix(),
				Protocol:       "openai.chat.stream",
				Model:          model,
				Status:         "failed",
				StatusCode:     429,
				FirstContentMs: firstContent.Value(),
				DurationMs:     requestDurationMs(startedAt),
				Error:          busyErr.Error(),
			})
			h.recordDiagnosticFailureForPayload("openai.chat.stream", model, nil, 429, busyErr, payload)
			w.Header().Set("Retry-After", "1")
			h.sendOpenAIError(w, 429, "rate_limit_error", busyErr.Error())
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:      time.Now().Unix(),
		Protocol:       "openai.chat.stream",
		Model:          model,
		Status:         "failed",
		StatusCode:     mapped.Status,
		FirstContentMs: firstContent.Value(),
		DurationMs:     requestDurationMs(startedAt),
		Error:          lastErr.Error(),
	})
	h.recordDiagnosticFailureForPayload("openai.chat.stream", model, nil, mapped.Status, lastErr, payload)
	applyDownstreamErrorHeaders(w, mapped)
	h.sendOpenAIError(w, mapped.Status, mapped.OpenAIType, lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	startedAt := time.Now()
	firstContent := payload.beginRequestTiming(startedAt)
	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, payload.ConversationState.ConversationID, payload)
		if busy != nil {
			busyErr = busy
			break
		}
		if account == nil {
			break
		}
		release := func() {
			if guard != nil {
				guard.Release()
				guard = nil
			}
		}
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				firstContent.MarkOutput(text, isThinking)
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				firstContent.MarkToolOutput()
				toolUses = append(toolUses, tu)
			},
			OnComplete:  func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnUsage:     func(usage KiroTokenUsage) { upstreamUsage = usage },
			OnTruncated: func(string) { truncated = true },
			OnCredits:   func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}

		err := h.callKiroAPIWithHealth(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, model, err)
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens := resolvePromptCacheUsage(promptCacheUsage{}, upstreamUsage, inputTokens, nil)
		cacheDiagnostic := finalizePromptCacheDiagnostic(promptCacheDiagnostic{Status: "skipped", Reason: "no_cache_breakpoint", Source: "local"}, upstreamUsage, cacheUsage, inputTokens)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		thinkingTokens := upstreamUsage.ThinkingTokens
		if thinkingTokens <= 0 {
			thinkingTokens = estimateApproxTokens(reasoningContent)
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordRequestLogForPayload(payload, requestLogEntry{
			Timestamp:                time.Now().Unix(),
			Protocol:                 "openai.chat",
			Model:                    model,
			AccountID:                account.ID,
			AccountEmail:             account.Email,
			Status:                   "success",
			StatusCode:               200,
			FirstContentMs:           firstContent.Value(),
			DurationMs:               requestDurationMs(startedAt),
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			ThinkingTokens:           thinkingTokens,
			CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
			CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
			VisibleOutputChars:       outputCharCount(finalContent),
			ThinkingOutputChars:      outputCharCount(reasoningContent),
			ToolUseCount:             len(toolUses),
			Credits:                  credits,
		})

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		resp["usage"] = buildOpenAIUsageMap(inputTokens, outputTokens, thinkingTokens, cacheUsage)
		if truncated {
			setOpenAIResponseFinishReason(resp, "length")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		h.recordCanceledRequestForPayload(payload, "openai.chat", model, startedAt, firstContent.Value(), stopErr)
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:      time.Now().Unix(),
				Protocol:       "openai.chat",
				Model:          model,
				Status:         "failed",
				StatusCode:     429,
				FirstContentMs: firstContent.Value(),
				DurationMs:     requestDurationMs(startedAt),
				Error:          busyErr.Error(),
			})
			h.recordDiagnosticFailureForPayload("openai.chat", model, nil, 429, busyErr, payload)
			w.Header().Set("Retry-After", "1")
			h.sendOpenAIError(w, 429, "rate_limit_error", busyErr.Error())
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:      time.Now().Unix(),
		Protocol:       "openai.chat",
		Model:          model,
		Status:         "failed",
		StatusCode:     mapped.Status,
		FirstContentMs: firstContent.Value(),
		DurationMs:     requestDurationMs(startedAt),
		Error:          lastErr.Error(),
	})
	h.recordDiagnosticFailureForPayload("openai.chat", model, nil, mapped.Status, lastErr, payload)
	applyDownstreamErrorHeaders(w, mapped)
	h.sendOpenAIError(w, mapped.Status, mapped.OpenAIType, lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

func isKiroAPIKeyAccount(account *config.Account) bool {
	if account == nil {
		return false
	}
	return strings.EqualFold(account.AuthMethod, "api_key") ||
		strings.EqualFold(account.AuthMethod, "apikey") ||
		account.KiroApiKey != ""
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	return h.ensureValidTokenContext(context.Background(), account)
}

func (h *Handler) ensureValidTokenContext(ctx context.Context, account *config.Account) error {
	if isKiroAPIKeyAccount(account) {
		account.AuthMethod = "api_key"
		if account.KiroApiKey == "" {
			account.KiroApiKey = account.AccessToken
		}
		if account.AccessToken == "" {
			account.AccessToken = account.KiroApiKey
		}
		account.RefreshToken = ""
		account.ExpiresAt = 0
		return nil
	}

	if tokenStillValid(account, tokenRefreshSkewSeconds) {
		return nil
	}
	startedAt := time.Now()
	err := sharedTokenRefreshCoordinator.RefreshContext(ctx, account, false)
	status := "success"
	if err != nil {
		status = "error"
	}
	accountID := ""
	accountEmail := ""
	if account != nil {
		accountID = account.ID
		accountEmail = account.Email
	}
	requestDetailTraceFromContext(ctx).recordAttempt(accountID, accountEmail, "token_refresh", "", startedAt, 0, status, err, requestDetailRetryReason(err))
	return err
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if path == "/login" && r.Method == http.MethodPost {
		h.handleAdminLogin(w, r)
		return
	}
	if path == "/logout" && r.Method == http.MethodPost {
		h.handleAdminLogout(w, r)
		return
	}

	authenticated, sessionAuth, throttledFor := h.authenticateAdminRequest(r)
	if !authenticated {
		if throttledFor > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(throttledFor))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many login attempts"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	if sessionAuth && !adminRequestOriginAllowed(r) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Cross-origin admin request rejected"})
		return
	}

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiListAccountKiroProfiles(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiSwitchAccountKiroProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/credentials") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/credentials")
		h.apiExportAccountCredentials(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/auth/kiro-sso/select-profile" && r.Method == "POST":
		h.apiSelectKiroSsoProfile(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/runtime-config" && r.Method == "GET":
		h.apiGetRuntimeConfig(w, r)
	case path == "/runtime-config" && r.Method == "POST":
		h.apiUpdateRuntimeConfig(w, r)
	case path == "/routing-config" && r.Method == "GET":
		h.apiGetRoutingConfig(w, r)
	case path == "/routing-config" && r.Method == "POST":
		h.apiUpdateRoutingConfig(w, r)
	case path == "/auto-refresh" && r.Method == "GET":
		h.apiGetAutoRefresh(w, r)
	case path == "/auto-refresh" && r.Method == "POST":
		h.apiUpdateAutoRefresh(w, r)
	case path == "/auto-refresh/status" && r.Method == "GET":
		h.apiGetAutoRefreshStatus(w, r)
	case path == "/retry" && r.Method == "GET":
		h.apiGetRetryConfig(w, r)
	case path == "/retry" && r.Method == "POST":
		h.apiUpdateRetryConfig(w, r)
	case path == "/long-tool" && r.Method == "GET":
		h.apiGetLongToolConfig(w, r)
	case path == "/long-tool" && r.Method == "POST":
		h.apiUpdateLongToolConfig(w, r)
	case path == "/responses-storage" && r.Method == "GET":
		h.apiGetResponsesStorageConfig(w, r)
	case path == "/responses-storage" && r.Method == "POST":
		h.apiUpdateResponsesStorageConfig(w, r)
	case path == "/responses-storage" && r.Method == "DELETE":
		h.apiPurgeResponsesStorage(w, r)
	case path == "/model-registry" && r.Method == "GET":
		h.apiGetModelRegistry(w, r)
	case path == "/model-registry" && r.Method == "POST":
		h.apiUpdateModelRegistry(w, r)
	case path == "/model-health" && r.Method == "GET":
		h.apiGetModelHealth(w, r)
	case path == "/model-health/test" && r.Method == "POST":
		h.apiTestModelHealth(w, r)
	case path == "/health-config" && r.Method == "GET":
		h.apiGetHealthConfig(w, r)
	case path == "/health-config" && r.Method == "POST":
		h.apiUpdateHealthConfig(w, r)
	case path == "/diagnostics" && r.Method == "GET":
		h.apiGetDiagnostics(w, r)
	case path == "/diagnostics" && r.Method == "POST":
		h.apiUpdateDiagnostics(w, r)
	case path == "/diagnostics/events" && r.Method == "GET":
		h.apiGetDiagnosticEvents(w, r)
	case path == "/request-log" && r.Method == "GET":
		h.apiGetRequestLogConfig(w, r)
	case path == "/request-log" && r.Method == "POST":
		h.apiUpdateRequestLogConfig(w, r)
	case path == "/request-details" && r.Method == "GET":
		h.apiGetRequestDetail(w, r, false)
	case path == "/request-details/download" && r.Method == "GET":
		h.apiGetRequestDetail(w, r, true)
	case path == "/request-details" && r.Method == "DELETE":
		h.apiClearRequestDetails(w, r)
	case path == "/web-search" && r.Method == "GET":
		h.apiGetWebSearch(w, r)
	case path == "/web-search" && r.Method == "POST":
		h.apiUpdateWebSearch(w, r)
	case path == "/count-tokens-provider" && r.Method == "GET":
		h.apiGetCountTokensProvider(w, r)
	case path == "/count-tokens-provider" && r.Method == "POST":
		h.apiUpdateCountTokensProvider(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/requests" && r.Method == "GET":
		h.apiGetRequests(w, r)
	case path == "/upstream-protection" && r.Method == "GET":
		h.apiGetUpstreamProtection(w, r)
	case path == "/upstream-protection" && r.Method == "POST":
		h.apiUpdateUpstreamProtection(w, r)
	case path == "/upstream-protection/status" && r.Method == "GET":
		h.apiGetUpstreamProtectionStatus(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/prompt-cache" && r.Method == "GET":
		h.apiGetPromptCache(w, r)
	case path == "/prompt-cache" && r.Method == "POST":
		h.apiUpdatePromptCache(w, r)
	case path == "/prompt-cache" && r.Method == "DELETE":
		h.apiClearPromptCache(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()
	healthMap := h.pool.AccountHealthSnapshots()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]
		health := healthMap[a.ID]
		proxyURL, proxyPasswordSet := sanitizedProxyURL(a.ProxyURL)

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"profileArn":        a.ProfileArn,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"priority":          a.Priority,
			"maxConcurrency":    a.MaxConcurrency,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          proxyURL,
			"proxyPasswordSet":  proxyPasswordSet,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"latencyMsEwma":     health.LatencyMsEWMA,
			"errorRateEwma":     health.ErrorRateEWMA,
			"healthSamples":     health.Samples,
			"dispatchCount":     health.Dispatches,
			"affinityHitCount":  health.AffinityHits,
			"affinityHitRate":   health.AffinityHitRate,
			"lastOutcomeAt":     health.LastOutcomeAt,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"successCount":      stats.RequestCount,
			"failureCount":      stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	account.KiroApiKey = strings.TrimSpace(account.KiroApiKey)
	account.ProxyURL = strings.TrimSpace(account.ProxyURL)
	if err := validateAccountProxyURL(account.ProxyURL); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	isAPIKey := isKiroAPIKeyAccount(&account)
	if isAPIKey {
		if method := strings.TrimSpace(account.AuthMethod); account.KiroApiKey != "" && method != "" && !strings.EqualFold(method, "api_key") && !strings.EqualFold(method, "apikey") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey cannot be combined with authMethod " + method})
			return
		}
		if account.KiroApiKey == "" {
			account.KiroApiKey = strings.TrimSpace(account.AccessToken)
		}
		if account.KiroApiKey == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		account.AuthMethod = "api_key"
		if strings.TrimSpace(account.Provider) == "" {
			account.Provider = "API Key"
		}
		account.AccessToken = account.KiroApiKey
		account.RefreshToken = ""
		account.ExpiresAt = 0
		if strings.HasPrefix(account.KiroApiKey, "ksk_") {
			region, info, retryable, err := resolveKiroAPIKeyRegion(r.Context(), account.KiroApiKey, account.Region, account.ProxyURL)
			if err != nil {
				status := http.StatusBadRequest
				if retryable {
					status = http.StatusBadGateway
				}
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			account.Region = region
			applyProbedAccountInfo(&account, info)
		}
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	action := "created"
	var err error
	if isAPIKey {
		var updated bool
		account, updated, err = config.UpsertAccountByIdentity(account)
		if updated {
			action = "updated"
		}
	} else {
		err = config.AddAccount(account)
	}
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
		h.refreshModelCachesAsync([]config.Account{account})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID, "action": action})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	h.pruneModelsByAccount(config.GetEnabledAccounts())
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
		if v && !oldEnabled && existing.BanStatus != "" && !strings.EqualFold(existing.BanStatus, "ACTIVE") {
			existing.BanStatus = "ACTIVE"
			existing.BanReason = ""
			existing.BanTime = 0
		}
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		weight := int(v)
		if weight < 0 || weight > 100 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "weight must be between 0 and 100"})
			return
		}
		existing.Weight = weight
	}
	if v, ok := updates["priority"].(float64); ok {
		existing.Priority = int(v)
	}
	if v, ok := updates["maxConcurrency"].(float64); ok {
		maxConcurrency := int(v)
		if maxConcurrency < 0 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "maxConcurrency must be >= 0"})
			return
		}
		if maxConcurrency > 1000 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "maxConcurrency must be <= 1000"})
			return
		}
		existing.MaxConcurrency = maxConcurrency
	}
	if v, ok := updates["proxyURL"].(string); ok {
		v = preserveProxyPassword(existing.ProxyURL, v)
		if err := validateAccountProxyURL(v); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled {
		h.pool.ClearAccountCooldowns(map[string]bool{id: true})
		if existing.AccessToken != "" {
			h.refreshModelCachesAsync([]config.Account{*existing})
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		matchedIDs := make(map[string]bool)
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				matchedIDs[a.ID] = true
			}
		}
		if err := config.SetAccountsEnabled(matchedIDs, enabled); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if enabled {
			h.pool.ClearAccountCooldowns(matchedIDs)
		}
		h.pool.Reload()
		h.pruneModelsByAccount(config.GetEnabledAccounts())
		for i := range toRefreshModels {
			toRefreshModels[i].Enabled = true
		}
		h.refreshModelCachesAsync(toRefreshModels)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(matchedIDs)})

	case "refresh":
		accounts := config.GetAccounts()
		byID := make(map[string]config.Account, len(accounts))
		for _, account := range accounts {
			byID[account.ID] = account
		}
		selected := make([]config.Account, 0, len(req.IDs))
		seen := make(map[string]bool, len(req.IDs))
		failCount := 0
		for _, id := range req.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			account, ok := byID[id]
			if !ok {
				failCount++
				continue
			}
			selected = append(selected, account)
		}

		type batchRefreshResult struct {
			accountID string
			info      *config.AccountInfo
			err       error
		}
		jobs := make(chan config.Account)
		results := make(chan batchRefreshResult, len(selected))
		var wg sync.WaitGroup
		for i := 0; i < boundedRefreshConcurrency(len(selected)); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for account := range jobs {
					if account.RefreshToken != "" {
						if err := sharedTokenRefreshCoordinator.RefreshContext(r.Context(), &account, true); err != nil {
							results <- batchRefreshResult{accountID: account.ID, err: err}
							continue
						}
					}
					info, err := RefreshAccountInfoContext(r.Context(), &account)
					results <- batchRefreshResult{accountID: account.ID, info: info, err: err}
				}
			}()
		}
		for _, account := range selected {
			jobs <- account
		}
		close(jobs)
		wg.Wait()
		close(results)

		infoUpdates := make(map[string]config.AccountInfo)
		for result := range results {
			if result.err != nil || result.info == nil {
				failCount++
				logger.Warnf("[BatchRefresh] Failed to refresh %s: %v", result.accountID, result.err)
				continue
			}
			infoUpdates[result.accountID] = *result.info
		}
		successCount := len(infoUpdates)
		if err := config.UpdateAccountInfoBatch(infoUpdates); err != nil {
			failCount += successCount
			successCount = 0
			logger.Warnf("[BatchRefresh] Failed to persist account info batch: %v", err)
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	login, err := auth.CompleteIamSsoLoginWithMetadata(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(login.AccessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  login.AccessToken,
		RefreshToken: login.RefreshToken,
		ClientID:     login.ClientID,
		ClientSecret: login.ClientSecret,
		AuthMethod:   "idc",
		Provider:     "Enterprise",
		Region:       login.Region,
		StartUrl:     login.StartURL,
		ExpiresAt:    time.Now().Unix() + int64(login.ExpiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

type importCredentialsRequest struct {
	ID            string       `json:"id"`
	Email         string       `json:"email"`
	UserID        string       `json:"userId"`
	AccessToken   string       `json:"accessToken"`
	RefreshToken  string       `json:"refreshToken"`
	KiroApiKey    string       `json:"kiroApiKey"`
	ClientID      string       `json:"clientId"`
	ClientSecret  string       `json:"clientSecret"`
	AuthMethod    string       `json:"authMethod"`
	Provider      string       `json:"provider"`
	IDP           string       `json:"idp"`
	Region        string       `json:"region"`
	AuthRegion    string       `json:"authRegion"`
	AuthRegionAlt string       `json:"auth_region"`
	StartUrl      string       `json:"startUrl"`
	StartUrlAlt   string       `json:"start_url"`
	ExpiresAt     int64        `json:"expiresAt"`
	ProfileArn    string       `json:"profileArn"`
	TokenEndpoint string       `json:"tokenEndpoint"`
	IssuerURL     string       `json:"issuerUrl"`
	Scopes        importScopes `json:"scopes"`
	MachineId     string       `json:"machineId"`
	Status        string       `json:"status"`

	Credentials *struct {
		AccessToken   string       `json:"accessToken"`
		RefreshToken  string       `json:"refreshToken"`
		KiroApiKey    string       `json:"kiroApiKey"`
		ClientID      string       `json:"clientId"`
		ClientSecret  string       `json:"clientSecret"`
		AuthMethod    string       `json:"authMethod"`
		Region        string       `json:"region"`
		AuthRegion    string       `json:"authRegion"`
		AuthRegionAlt string       `json:"auth_region"`
		StartUrl      string       `json:"startUrl"`
		StartUrlAlt   string       `json:"start_url"`
		ExpiresAt     int64        `json:"expiresAt"`
		ProfileArn    string       `json:"profileArn"`
		TokenEndpoint string       `json:"tokenEndpoint"`
		IssuerURL     string       `json:"issuerUrl"`
		Scopes        importScopes `json:"scopes"`
	} `json:"credentials"`
	Subscription *struct {
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"subscription"`
	Usage *struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	} `json:"usage"`
}

type importScopes string

func (s *importScopes) UnmarshalJSON(raw []byte) error {
	if s == nil {
		return fmt.Errorf("scopes target is nil")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*s = ""
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		*s = importScopes(normalizeImportedScopes([]string{single}))
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("scopes must be a string or string array")
	}
	*s = importScopes(normalizeImportedScopes(list))
	return nil
}

func normalizeImportedScopes(values []string) string {
	seen := make(map[string]bool)
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		for _, scope := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
			if scope == "" || seen[scope] {
				continue
			}
			seen[scope] = true
			normalized = append(normalized, scope)
		}
	}
	return strings.Join(normalized, " ")
}

type importCredentialsBatchRequest struct {
	Accounts []importCredentialsRequest `json:"accounts"`
}

type credentialImportError struct {
	status int
	err    error
}

type credentialImportResult struct {
	account config.Account
	action  string
}

func (e *credentialImportError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func newCredentialImportError(status int, message string) *credentialImportError {
	return &credentialImportError{status: status, err: fmt.Errorf("%s", message)}
}

func decodeImportCredentialsRequests(raw []byte) ([]importCredentialsRequest, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, fmt.Errorf("empty request body")
	}

	if trimmed[0] == '[' {
		var reqs []importCredentialsRequest
		if err := json.Unmarshal(trimmed, &reqs); err != nil {
			return nil, false, err
		}
		return reqs, true, nil
	}

	var probe struct {
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(trimmed, &probe); err == nil && probe.Accounts != nil {
		var batch importCredentialsBatchRequest
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, false, err
		}
		return batch.Accounts, true, nil
	}

	var req importCredentialsRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return nil, false, err
	}
	return []importCredentialsRequest{req}, false, nil
}

func normalizeNestedCredentialImport(req importCredentialsRequest) importCredentialsRequest {
	if req.Provider == "" {
		req.Provider = req.IDP
	}
	if req.Credentials == nil {
		return req
	}
	creds := req.Credentials
	if req.AccessToken == "" {
		req.AccessToken = creds.AccessToken
	}
	if req.RefreshToken == "" {
		req.RefreshToken = creds.RefreshToken
	}
	if req.KiroApiKey == "" {
		req.KiroApiKey = creds.KiroApiKey
	}
	if req.ClientID == "" {
		req.ClientID = creds.ClientID
	}
	if req.ClientSecret == "" {
		req.ClientSecret = creds.ClientSecret
	}
	if req.AuthMethod == "" {
		req.AuthMethod = creds.AuthMethod
	}
	if req.Region == "" {
		req.Region = creds.Region
	}
	if req.AuthRegion == "" {
		req.AuthRegion = creds.AuthRegion
	}
	if req.AuthRegionAlt == "" {
		req.AuthRegionAlt = creds.AuthRegionAlt
	}
	if req.StartUrl == "" {
		req.StartUrl = creds.StartUrl
	}
	if req.StartUrlAlt == "" {
		req.StartUrlAlt = creds.StartUrlAlt
	}
	if req.ExpiresAt == 0 {
		req.ExpiresAt = creds.ExpiresAt
	}
	if req.ProfileArn == "" {
		req.ProfileArn = creds.ProfileArn
	}
	if req.TokenEndpoint == "" {
		req.TokenEndpoint = creds.TokenEndpoint
	}
	if req.IssuerURL == "" {
		req.IssuerURL = creds.IssuerURL
	}
	if req.Scopes == "" {
		req.Scopes = creds.Scopes
	}
	return req
}

func normalizeImportedIDCMetadata(req *importCredentialsRequest) {
	if req == nil || !strings.EqualFold(strings.TrimSpace(req.AuthMethod), "idc") {
		return
	}
	if region := strings.TrimSpace(req.AuthRegion); region != "" {
		req.Region = region
	} else if region := strings.TrimSpace(req.AuthRegionAlt); region != "" {
		req.Region = region
	}
	if strings.TrimSpace(req.StartUrl) == "" {
		req.StartUrl = strings.TrimSpace(req.StartUrlAlt)
	}
	if strings.TrimSpace(req.StartUrl) == "" {
		req.StartUrl = auth.ExtractStartURLFromClientSecret(req.ClientSecret)
	}
	req.StartUrl = auth.NormalizeStartURL(req.StartUrl)
	if req.StartUrl != "" && !auth.IsBuilderIDStartURL(req.StartUrl) {
		req.Provider = "Enterprise"
	}
}

func applyImportedAccountMetadata(account *config.Account, req importCredentialsRequest) {
	if account == nil {
		return
	}
	if req.Email != "" {
		account.Email = req.Email
	}
	if req.UserID != "" {
		account.UserId = req.UserID
	}
	if req.MachineId != "" {
		account.MachineId = req.MachineId
	}
	if req.Subscription != nil {
		account.SubscriptionType = strings.ToUpper(strings.TrimSpace(req.Subscription.Type))
		account.SubscriptionTitle = req.Subscription.Title
	}
	if req.Usage != nil {
		account.UsageCurrent = req.Usage.Current
		account.UsageLimit = req.Usage.Limit
		account.UsagePercent = req.Usage.PercentUsed
		account.LastRefresh = req.Usage.LastUpdated
	}
	switch strings.ToLower(strings.TrimSpace(req.Status)) {
	case "disabled", "banned", "suspended":
		account.Enabled = false
		account.BanStatus = strings.ToUpper(req.Status)
	case "active", "enabled":
		account.Enabled = true
		account.BanStatus = "ACTIVE"
	}
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(requestBodyErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}

	reqs, batch, err := decodeImportCredentialsRequests(raw)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if batch && (len(reqs) == 0 || len(reqs) > maxCredentialImportBatch) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("batch must contain between 1 and %d accounts", maxCredentialImportBatch)})
		return
	}

	if batch {
		imported := make([]map[string]interface{}, 0, len(reqs))
		errors := make([]string, 0)
		status := http.StatusBadRequest
		type preparedImport struct {
			account config.Account
			err     *credentialImportError
		}
		prepared := make([]preparedImport, len(reqs))
		concurrency := config.GetAutoRefreshConfig().RefreshConcurrency
		if concurrency < 1 {
			concurrency = 1
		}
		if concurrency > 20 {
			concurrency = 20
		}
		if concurrency > len(reqs) {
			concurrency = len(reqs)
		}
		jobs := make(chan int)
		var workers sync.WaitGroup
		for worker := 0; worker < concurrency; worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for index := range jobs {
					prepared[index].account, prepared[index].err = h.prepareCredentialsAccountContext(r.Context(), reqs[index])
				}
			}()
		}
		for i := range reqs {
			jobs <- i
		}
		close(jobs)
		workers.Wait()

		accountsToPersist := make([]config.Account, 0, len(reqs))
		preparedIndexes := make([]int, 0, len(reqs))
		for i, result := range prepared {
			if result.err != nil {
				if result.err.status >= 500 {
					status = result.err.status
				}
				errors = append(errors, fmt.Sprintf("account %d: %s", i+1, result.err.Error()))
				continue
			}
			accountsToPersist = append(accountsToPersist, result.account)
			preparedIndexes = append(preparedIndexes, i)
		}

		if len(accountsToPersist) > 0 {
			persisted, persistErr := config.UpsertAccountsByIdentity(accountsToPersist)
			if persistErr != nil {
				status = http.StatusInternalServerError
				errors = append(errors, "batch persist: "+persistErr.Error())
			} else {
				for i, result := range persisted {
					imported = append(imported, map[string]interface{}{
						"index":  preparedIndexes[i] + 1,
						"id":     result.Account.ID,
						"email":  result.Account.Email,
						"action": importAction(result.Updated),
					})
				}
			}
		}

		if len(imported) > 0 {
			h.pool.Reload()
		}
		if len(imported) == 0 && len(errors) > 0 {
			w.WriteHeader(status)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  len(errors) == 0,
			"accounts": imported,
			"errors":   errors,
		})
		return
	}

	result, importErr := h.importCredentialsAccountContext(r.Context(), reqs[0])
	if importErr != nil {
		w.WriteHeader(importErr.status)
		json.NewEncoder(w).Encode(map[string]string{"error": importErr.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":     result.account.ID,
			"email":  result.account.Email,
			"action": result.action,
		},
		"action": result.action,
	})
}

func (h *Handler) prepareCredentialsAccount(req importCredentialsRequest) (config.Account, *credentialImportError) {
	return h.prepareCredentialsAccountContext(context.Background(), req)
}

func (h *Handler) prepareCredentialsAccountContext(ctx context.Context, req importCredentialsRequest) (config.Account, *credentialImportError) {
	req = normalizeNestedCredentialImport(req)
	req.AuthMethod = normalizeImportAuthMethod(req.AuthMethod, req.ClientID, req.ClientSecret, req.KiroApiKey, req.TokenEndpoint)
	normalizeImportedIDCMetadata(&req)
	if req.AuthMethod == "api_key" {
		if req.KiroApiKey == "" {
			req.KiroApiKey = strings.TrimSpace(req.AccessToken)
		}
		req.KiroApiKey = strings.TrimSpace(req.KiroApiKey)
		if req.KiroApiKey == "" {
			return config.Account{}, newCredentialImportError(http.StatusBadRequest, "kiroApiKey is required")
		}
		var probedInfo *config.AccountInfo
		if strings.HasPrefix(req.KiroApiKey, "ksk_") {
			region, info, retryable, err := resolveKiroAPIKeyRegion(ctx, req.KiroApiKey, req.Region)
			if err != nil {
				status := http.StatusBadRequest
				if retryable {
					status = http.StatusBadGateway
				}
				return config.Account{}, &credentialImportError{status: status, err: err}
			}
			req.Region = region
			probedInfo = info
		}
		if req.Region == "" {
			req.Region = "us-east-1"
		}
		if req.Provider == "" {
			req.Provider = "API Key"
		}
		account := config.Account{
			ID:          auth.GenerateAccountID(),
			Email:       maskedKiroAPIKeyLabel(req.KiroApiKey),
			AccessToken: req.KiroApiKey,
			KiroApiKey:  req.KiroApiKey,
			AuthMethod:  "api_key",
			Provider:    req.Provider,
			Region:      req.Region,
			Enabled:     true,
			MachineId:   config.GenerateMachineId(),
		}
		applyImportedAccountMetadata(&account, req)
		applyProbedAccountInfo(&account, probedInfo)
		return account, nil
	}

	// OAuth credentials default to the historical OIDC/data-plane region.
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	derivedTokenEndpoint, derivedIssuer, derivedScopes := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTokenEndpoint != "" && auth.ValidateExternalIdpEndpoint(derivedTokenEndpoint) == nil && req.AuthMethod != "external_idp" {
		req.AuthMethod = "external_idp"
	}
	if req.AuthMethod == "external_idp" {
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTokenEndpoint
		}
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIssuer
		}
		if req.Scopes == "" {
			req.Scopes = importScopes(derivedScopes)
		}
		if strings.TrimSpace(req.ClientID) == "" || strings.TrimSpace(req.TokenEndpoint) == "" {
			return config.Account{}, newCredentialImportError(http.StatusBadRequest, "external_idp requires clientId and tokenEndpoint (or userId/accessToken to derive it)")
		}
		if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
			return config.Account{}, newCredentialImportError(http.StatusBadRequest, "external IdP endpoint rejected: "+err.Error())
		}
		if req.IssuerURL != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				return config.Account{}, newCredentialImportError(http.StatusBadRequest, "external IdP issuer rejected: "+err.Error())
			}
		}
	}

	if req.RefreshToken == "" {
		return config.Account{}, newCredentialImportError(http.StatusBadRequest, "refreshToken is required")
	}

	var accessToken string
	var expiresAt int64
	profileArn := req.ProfileArn
	if req.AuthMethod == "external_idp" && req.AccessToken != "" {
		if exp := auth.ExpFromAccessTokenJWT(req.AccessToken); exp > time.Now().Unix() {
			accessToken = req.AccessToken
			expiresAt = exp
			maxTrustedExpiry := time.Now().Add(externalIdpImportMaxTrust).Unix()
			if expiresAt > maxTrustedExpiry {
				expiresAt = maxTrustedExpiry
			}
		}
	}
	if accessToken == "" {
		tempAccount := &config.Account{
			RefreshToken: req.RefreshToken, ClientID: req.ClientID,
			ClientSecret: req.ClientSecret, AuthMethod: req.AuthMethod,
			Region: req.Region, StartUrl: req.StartUrl, TokenEndpoint: req.TokenEndpoint,
			IssuerURL: req.IssuerURL, Scopes: string(req.Scopes),
		}
		var newRefreshToken string
		var refreshedProfileArn string
		var err error
		accessToken, newRefreshToken, expiresAt, refreshedProfileArn, err = auth.RefreshToken(tempAccount)
		if err != nil {
			return config.Account{}, newCredentialImportError(http.StatusBadRequest, "Token refresh failed: "+err.Error())
		}
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
		if refreshedProfileArn != "" {
			profileArn = refreshedProfileArn
		}
	}

	email := auth.ExtractEmailFromJWT(accessToken)
	if email == "" && req.AuthMethod != "external_idp" {
		email, _, _ = auth.GetUserInfo(accessToken)
	}
	if req.Provider == "" && req.AuthMethod == "external_idp" {
		req.Provider = "AzureAD"
	} else if req.Provider == "" && req.AuthMethod == "idc" {
		req.Provider = "BuilderId"
	}

	// 创建账号
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         email,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Provider:      req.Provider,
		Region:        req.Region,
		StartUrl:      req.StartUrl,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		ProfileArn:    profileArn,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        string(req.Scopes),
	}
	applyImportedAccountMetadata(&account, req)
	if account.Email == "" {
		account.Email = email
	}

	return account, nil
}

func (h *Handler) importCredentialsAccount(req importCredentialsRequest) (*credentialImportResult, *credentialImportError) {
	return h.importCredentialsAccountContext(context.Background(), req)
}

func (h *Handler) importCredentialsAccountContext(ctx context.Context, req importCredentialsRequest) (*credentialImportResult, *credentialImportError) {
	account, prepareErr := h.prepareCredentialsAccountContext(ctx, req)
	if prepareErr != nil {
		return nil, prepareErr
	}
	account, updated, err := config.UpsertAccountByIdentity(account)
	if err != nil {
		return nil, &credentialImportError{status: http.StatusInternalServerError, err: err}
	}
	return &credentialImportResult{account: account, action: importAction(updated)}, nil
}

func applyProbedAccountInfo(account *config.Account, info *config.AccountInfo) {
	if account == nil || info == nil {
		return
	}
	if info.Email != "" {
		account.Email = info.Email
	}
	if info.UserId != "" {
		account.UserId = info.UserId
	}
	if info.SubscriptionType != "" {
		account.SubscriptionType = info.SubscriptionType
	}
	if info.SubscriptionTitle != "" {
		account.SubscriptionTitle = info.SubscriptionTitle
	}
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

func importAction(updated bool) string {
	if updated {
		return "updated"
	}
	return "created"
}

var externalIdpAuthMethodAliases = map[string]bool{
	"external_idp": true,
	"azuread":      true,
	"azure":        true,
	"entra":        true,
	"entra_id":     true,
	"microsoft":    true,
	"m365":         true,
	"office365":    true,
	"external":     true,
}

func normalizeImportAuthMethod(authMethod, clientID, clientSecret, kiroAPIKey, tokenEndpoint string) string {
	if kiroAPIKey != "" {
		return "api_key"
	}
	normalized := strings.ToLower(strings.TrimSpace(authMethod))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	if externalIdpAuthMethodAliases[normalized] || strings.TrimSpace(tokenEndpoint) != "" {
		return "external_idp"
	}
	if normalized == "" {
		if clientID != "" {
			return "idc"
		}
		return "social"
	}
	// 标准化 authMethod
	switch normalized {
	case "api_key", "apikey", "api-key", "kiro_api_key":
		return "api_key"
	case "idc", "builderid", "enterprise":
		return "idc"
	case "social", "google", "github":
		return "social"
	default:
		if clientID != "" && clientSecret != "" {
			return "idc"
		}
		return "social"
	}
}

func maskedKiroAPIKeyLabel(key string) string {
	if len(key) <= 4 {
		return "api-key-" + strings.Repeat("*", len(key))
	}
	return "api-key-" + key[len(key)-4:]
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	inventory := h.pool.InventorySnapshot()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":         h.pool.Count(),
		"available":        h.pool.AvailableCount(),
		"totalRequests":    h.totalRequests.Load(),
		"successRequests":  h.successRequests.Load(),
		"failedRequests":   h.failedRequests.Load(),
		"totalTokens":      h.totalTokens.Load(),
		"totalCredits":     h.getCredits(),
		"uptime":           time.Now().Unix() - h.startTime,
		"accountInventory": inventory,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":               maskedCredentialValue(config.GetApiKey()),
		"apiKeyConfigured":     config.GetApiKey() != "",
		"credentialEncryption": config.CredentialEncryptionEnabled(),
		"requireApiKey":        config.IsApiKeyRequired(),
		"port":                 config.GetPort(),
		"host":                 config.GetHost(),
		"allowOverUsage":       config.GetAllowOverUsage(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetPromptCache(w http.ResponseWriter, r *http.Request) {
	response := struct {
		config.PromptCacheConfig
		Stats promptCacheStats `json:"stats"`
	}{PromptCacheConfig: config.GetPromptCacheConfig()}
	if h.promptCache != nil {
		response.Stats = h.promptCache.Stats()
	}
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) apiUpdatePromptCache(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled                *bool    `json:"enabled,omitempty"`
		PersistEnabled         *bool    `json:"persistEnabled,omitempty"`
		NamespaceMode          *string  `json:"namespaceMode,omitempty"`
		CacheReadEfficiency    *float64 `json:"cacheReadEfficiency,omitempty"`
		CacheReadEfficiencyMin *float64 `json:"cacheReadEfficiencyMin,omitempty"`
		CacheReadEfficiencyMax *float64 `json:"cacheReadEfficiencyMax,omitempty"`
		KvCacheTTLSecs         *int64   `json:"kvCacheTtlSecs,omitempty"`
		MaxEntriesPerAccount   *int     `json:"maxEntriesPerAccount,omitempty"`
		MaxEntriesTotal        *int     `json:"maxEntriesTotal,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	current := config.GetPromptCacheConfig()
	wasPersistEnabled := current.PersistEnabled
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.PersistEnabled != nil {
		current.PersistEnabled = *req.PersistEnabled
	}
	if req.NamespaceMode != nil {
		current.NamespaceMode = *req.NamespaceMode
	}
	if req.CacheReadEfficiency != nil {
		current.CacheReadEfficiency = *req.CacheReadEfficiency
		current.CacheReadEfficiencyMin = *req.CacheReadEfficiency
		current.CacheReadEfficiencyMax = *req.CacheReadEfficiency
	}
	if req.CacheReadEfficiencyMin != nil {
		current.CacheReadEfficiencyMin = *req.CacheReadEfficiencyMin
	}
	if req.CacheReadEfficiencyMax != nil {
		current.CacheReadEfficiencyMax = *req.CacheReadEfficiencyMax
	}
	if req.KvCacheTTLSecs != nil {
		current.KvCacheTTLSecs = *req.KvCacheTTLSecs
	}
	if req.MaxEntriesPerAccount != nil {
		current.MaxEntriesPerAccount = *req.MaxEntriesPerAccount
	}
	if req.MaxEntriesTotal != nil {
		current.MaxEntriesTotal = *req.MaxEntriesTotal
	}
	if current.MaxEntriesPerAccount < 1 || current.MaxEntriesPerAccount > 100000 ||
		current.MaxEntriesTotal < current.MaxEntriesPerAccount || current.MaxEntriesTotal > 1000000 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid prompt cache entry limits"})
		return
	}
	if err := config.UpdatePromptCacheSettings(current); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	current = config.GetPromptCacheConfig()
	if h.promptCache == nil {
		h.promptCache = newPromptCacheTrackerWithEfficiencyRange(time.Duration(current.KvCacheTTLSecs)*time.Second, current.CacheReadEfficiencyMin, current.CacheReadEfficiencyMax)
	} else {
		h.promptCache.ConfigureEfficiencyRange(time.Duration(current.KvCacheTTLSecs)*time.Second, current.CacheReadEfficiencyMin, current.CacheReadEfficiencyMax)
	}
	h.promptCache.ConfigurePolicy(current.Enabled, current.NamespaceMode)
	h.promptCache.ConfigureLimits(current.MaxEntriesPerAccount, current.MaxEntriesTotal)
	if current.PersistEnabled {
		if !wasPersistEnabled {
			h.promptCache.markStateChanged()
		}
		if err := h.promptCache.Flush(promptCachePath()); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	} else if err := h.promptCache.RemovePersisted(promptCachePath()); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  current,
	})
}

func (h *Handler) apiClearPromptCache(w http.ResponseWriter, r *http.Request) {
	if h.promptCache != nil {
		h.promptCache.Clear()
		if err := h.promptCache.RemovePersisted(promptCachePath()); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey         *string `json:"apiKey,omitempty"`
		RequireApiKey  *bool   `json:"requireApiKey,omitempty"`
		Password       string  `json:"password,omitempty"`
		AllowOverUsage *bool   `json:"allowOverUsage,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatchWithOverUsage(req.ApiKey, req.RequireApiKey, req.Password, req.AllowOverUsage); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.AllowOverUsage != nil {
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}
	if req.Password != "" {
		h.clearAdminSessions()
		if err := h.issueAdminSession(w, r, adminSessionDuration, false); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Password changed, but session rotation failed"})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	runtimeCfg := config.GetRuntimeConfig()
	activeHost, activePort, externallyManaged, err := config.ResolveListenAddress()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"host":              runtimeCfg.Host,
		"port":              runtimeCfg.Port,
		"logLevel":          runtimeCfg.LogLevel,
		"activeLogLevel":    logger.LevelName(logger.GetLevel()),
		"activeHost":        activeHost,
		"activePort":        activePort,
		"externallyManaged": externallyManaged,
		"kiroVersion":       runtimeCfg.KiroVersion,
		"systemVersion":     runtimeCfg.SystemVersion,
		"nodeVersion":       runtimeCfg.NodeVersion,
		"restartRequired":   runtimeCfg.Host != activeHost || runtimeCfg.Port != activePort,
	})
}

func (h *Handler) apiUpdateRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	var req config.RuntimeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	req.LogLevel = strings.TrimSpace(strings.ToLower(req.LogLevel))
	req.KiroVersion = strings.TrimSpace(req.KiroVersion)
	req.SystemVersion = strings.TrimSpace(req.SystemVersion)
	req.NodeVersion = strings.TrimSpace(req.NodeVersion)
	if req.Host == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "host is required"})
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "port must be between 1 and 65535"})
		return
	}
	if _, ok := logger.ParseLevel(req.LogLevel); !ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "logLevel must be debug, info, warn, or error"})
		return
	}

	before := config.GetRuntimeConfig()
	if err := config.UpdateRuntimeConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if level, ok := logger.ParseLevel(req.LogLevel); ok {
		logger.SetLevel(level)
	}
	after := config.GetRuntimeConfig()
	activeHost, activePort, externallyManaged, resolveErr := config.ResolveListenAddress()
	if resolveErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": resolveErr.Error()})
		return
	}
	restartRequired := before.Host != after.Host || before.Port != after.Port ||
		after.Host != activeHost || after.Port != activePort
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"config":            after,
		"activeLogLevel":    logger.LevelName(logger.GetLevel()),
		"activeHost":        activeHost,
		"activePort":        activePort,
		"externallyManaged": externallyManaged,
		"restartRequired":   restartRequired,
	})
}

func (h *Handler) apiGetRoutingConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetRoutingConfig())
}

func (h *Handler) apiUpdateRoutingConfig(w http.ResponseWriter, r *http.Request) {
	var req config.RoutingConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.LoadBalancingMode = strings.ToLower(strings.TrimSpace(req.LoadBalancingMode))
	if req.LoadBalancingMode != "weighted" && req.LoadBalancingMode != "priority" && req.LoadBalancingMode != "balanced" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "loadBalancingMode must be weighted, priority, or balanced"})
		return
	}
	if err := config.UpdateRoutingConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetRoutingConfig()})
}

func (h *Handler) apiGetAutoRefresh(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetAutoRefreshConfig())
}

func (h *Handler) apiGetAutoRefreshStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config": config.GetAutoRefreshConfig(),
		"status": h.getAutoRefreshStatusSnapshot(),
	})
}

func (h *Handler) apiUpdateAutoRefresh(w http.ResponseWriter, r *http.Request) {
	var req config.AutoRefreshConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.IntervalMinutes < 1 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "intervalMinutes must be at least 1"})
		return
	}
	if req.TokenRefreshBeforeSeconds < 60 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "tokenRefreshBeforeSeconds must be at least 60"})
		return
	}
	if req.FailureCooldownSeconds < 60 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "failureCooldownSeconds must be at least 60"})
		return
	}
	if req.MaxAccountsPerRun < 0 {
		req.MaxAccountsPerRun = 0
	}
	if req.RefreshConcurrency < 1 || req.RefreshConcurrency > 50 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshConcurrency must be between 1 and 50"})
		return
	}
	current := config.GetAutoRefreshConfig()
	if req.RefreshQueueCapacity <= 0 {
		req.RefreshQueueCapacity = current.RefreshQueueCapacity
	}
	if req.RefreshTaskTimeoutSeconds <= 0 {
		req.RefreshTaskTimeoutSeconds = current.RefreshTaskTimeoutSeconds
	}
	if req.ModelIntervalMinutes <= 0 {
		req.ModelIntervalMinutes = current.ModelIntervalMinutes
	}
	if req.ModelRefreshConcurrency <= 0 {
		req.ModelRefreshConcurrency = current.ModelRefreshConcurrency
	}
	if req.RefreshJitterSeconds < 0 {
		req.RefreshJitterSeconds = 0
	}
	if req.RefreshQueueCapacity < 1 || req.RefreshQueueCapacity > 100000 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshQueueCapacity must be between 1 and 100000"})
		return
	}
	if req.RefreshTaskTimeoutSeconds < 10 || req.RefreshTaskTimeoutSeconds > 600 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshTaskTimeoutSeconds must be between 10 and 600"})
		return
	}
	if req.RefreshJitterSeconds > 3600 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshJitterSeconds must be between 0 and 3600"})
		return
	}
	if req.ModelIntervalMinutes < 30 || req.ModelIntervalMinutes > 10080 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "modelIntervalMinutes must be between 30 and 10080"})
		return
	}
	if req.MaxModelsPerRun < 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "maxModelsPerRun must be >= 0"})
		return
	}
	if req.ModelRefreshConcurrency < 1 || req.ModelRefreshConcurrency > 20 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "modelRefreshConcurrency must be between 1 and 20"})
		return
	}
	if err := config.UpdateAutoRefreshConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetAutoRefreshConfig()})
}

func (h *Handler) apiGetRetryConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetRetryConfig())
}

func (h *Handler) apiUpdateRetryConfig(w http.ResponseWriter, r *http.Request) {
	var update struct {
		config.RetryConfig
		MaxAccountAttempts         *int `json:"maxAccountAttempts"`
		MaxRetryDurationSeconds    *int `json:"maxRetryDurationSeconds"`
		ToolAssemblyTimeoutSeconds *int `json:"toolAssemblyTimeoutSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if update.MaxAccountAttempts == nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid retry configuration"})
		return
	}
	req := update.RetryConfig
	req.MaxAccountAttempts = *update.MaxAccountAttempts
	current := config.GetRetryConfig()
	if update.MaxRetryDurationSeconds == nil {
		req.MaxRetryDurationSeconds = current.MaxRetryDurationSeconds
	} else {
		req.MaxRetryDurationSeconds = *update.MaxRetryDurationSeconds
	}
	if update.ToolAssemblyTimeoutSeconds == nil {
		req.ToolAssemblyTimeoutSeconds = current.ToolAssemblyTimeoutSeconds
	} else {
		req.ToolAssemblyTimeoutSeconds = *update.ToolAssemblyTimeoutSeconds
	}
	if req.StreamIdleTimeoutSeconds <= 0 {
		req.StreamIdleTimeoutSeconds = current.StreamIdleTimeoutSeconds
	}
	if req.MaxAccountAttempts < 0 || req.MaxAccountAttempts > 100 ||
		req.MaxUpstreamAttempts < 1 || req.MaxUpstreamAttempts > 200 ||
		req.MaxRetryDurationSeconds < 0 || req.MaxRetryDurationSeconds > 86400 ||
		req.FirstTokenTimeoutSeconds < 5 || req.FirstTokenTimeoutSeconds > 600 ||
		req.StreamIdleTimeoutSeconds < 15 || req.StreamIdleTimeoutSeconds > 3600 ||
		(req.ToolAssemblyTimeoutSeconds != 0 && (req.ToolAssemblyTimeoutSeconds < 30 || req.ToolAssemblyTimeoutSeconds > 3600)) ||
		req.EmptyResponseRetries < 0 || req.EmptyResponseRetries > 20 ||
		req.EndpointFailureThreshold < 1 || req.EndpointFailureThreshold > 20 ||
		req.EndpointCircuitCooldownSeconds < 5 || req.EndpointCircuitCooldownSeconds > 900 ||
		req.ProxyFailureThreshold < 1 || req.ProxyFailureThreshold > 20 ||
		req.ProxyCircuitCooldownSeconds < 5 || req.ProxyCircuitCooldownSeconds > 900 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid retry configuration"})
		return
	}
	if err := config.UpdateRetryConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetRetryConfig()})
}

func (h *Handler) apiGetLongToolConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetLongToolConfig())
}

func (h *Handler) apiUpdateLongToolConfig(w http.ResponseWriter, r *http.Request) {
	var req config.LongToolConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.FallbackModel = strings.TrimSpace(req.FallbackModel)
	if req.ActionableOutputTimeoutSeconds == 0 {
		req.ActionableOutputTimeoutSeconds = config.GetLongToolConfig().ActionableOutputTimeoutSeconds
	}
	if req.DefaultMaxToolTokens < 1024 || req.DefaultMaxToolTokens > 128000 ||
		req.TruncationRetries < 0 || req.TruncationRetries > 5 ||
		req.ActionableOutputTimeoutSeconds < 30 || req.ActionableOutputTimeoutSeconds > 600 ||
		req.FallbackModel == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid long-tool configuration"})
		return
	}
	if err := config.UpdateLongToolConfig(req); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetLongToolConfig()})
}

func (h *Handler) apiGetResponsesStorageConfig(w http.ResponseWriter, r *http.Request) {
	files, bytes := responsesStorageStats()
	json.NewEncoder(w).Encode(struct {
		config.ResponsesStorageConfig
		EncryptionEnabled bool  `json:"encryptionEnabled"`
		FileCount         int   `json:"fileCount"`
		StoredBytes       int64 `json:"storedBytes"`
	}{
		ResponsesStorageConfig: config.GetResponsesStorageConfig(),
		EncryptionEnabled:      responsesStorageEncryptionEnabled(),
		FileCount:              files,
		StoredBytes:            bytes,
	})
}

func (h *Handler) apiUpdateResponsesStorageConfig(w http.ResponseWriter, r *http.Request) {
	var req config.ResponsesStorageConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.TTLHours < 1 || req.TTLHours > 8760 ||
		req.MaxFiles < 1 || req.MaxFiles > 1000000 ||
		req.MaxBytes < 1<<20 || req.MaxBytes > 1<<40 ||
		req.MaxHistoryBytes < 64<<10 || req.MaxHistoryBytes > 64<<20 ||
		req.GCIntervalMinutes < 1 || req.GCIntervalMinutes > 1440 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid Responses storage configuration"})
		return
	}
	if req.DefaultStore && !responsesStorageEncryptionEnabled() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "default Responses storage requires KIRO_MASTER_KEY or KIRO_MASTER_KEY_FILE"})
		return
	}
	if err := config.UpdateResponsesStorageConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	purgeResponsesStorage()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetResponsesStorageConfig()})
}

func (h *Handler) apiPurgeResponsesStorage(w http.ResponseWriter, r *http.Request) {
	files, bytes, err := purgeAllResponsesStorage()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"deletedFiles": files,
		"deletedBytes": bytes,
	})
}

func (h *Handler) apiGetModelRegistry(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetModelRegistryConfig())
}

func (h *Handler) apiUpdateModelRegistry(w http.ResponseWriter, r *http.Request) {
	var req config.ModelRegistryConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.NegativeCacheTTLSeconds < 60 || req.NegativeCacheTTLSeconds > 7*24*3600 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "negativeCacheTtlSeconds must be between 60 and 604800"})
		return
	}
	if err := config.UpdateModelRegistryConfig(req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetModelRegistryConfig()})
}

func (h *Handler) apiGetHealthConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetHealthConfig())
}

func (h *Handler) apiUpdateHealthConfig(w http.ResponseWriter, r *http.Request) {
	var req config.HealthConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.WebhookURL = strings.TrimSpace(req.WebhookURL)
	if req.MinReadyAccounts < 0 || req.MinReadyRatio < 0 || req.MinReadyRatio > 1 ||
		req.WebhookCooldownSeconds < 10 || req.WebhookCooldownSeconds > 86400 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid health configuration"})
		return
	}
	if req.WebhookEnabled {
		parsed, err := url.Parse(req.WebhookURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "webhookUrl must be a valid http or https URL"})
			return
		}
	}
	if err := config.UpdateHealthConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetHealthConfig()})
}

func (h *Handler) apiGetDiagnostics(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetDiagnosticConfig())
}

func (h *Handler) apiUpdateDiagnostics(w http.ResponseWriter, r *http.Request) {
	var req config.DiagnosticConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.MaxEntries < 1 {
		req.MaxEntries = defaultDiagnosticLogLimit
	}
	if req.MaxEntries > 2000 {
		req.MaxEntries = 2000
	}
	if err := config.UpdateDiagnosticConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.diagnosticLog == nil {
		h.diagnosticLog = newDiagnosticLog(req.MaxEntries)
	}
	h.diagnosticLog.configure(req.MaxEntries)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetDiagnosticConfig()})
}

func (h *Handler) apiGetDiagnosticEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	cfg := config.GetDiagnosticConfig()
	if limit > cfg.MaxEntries {
		limit = cfg.MaxEntries
	}
	if h.diagnosticLog == nil {
		h.diagnosticLog = newDiagnosticLog(cfg.MaxEntries)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config": cfg,
		"events": h.diagnosticLog.list(limit),
		"limit":  limit,
	})
}

func (h *Handler) apiGetRequestLogConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetRequestLogConfig()
	count, bytes := h.ensureRequestDetailStore().stats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"maxEntries":         cfg.MaxEntries,
		"detailedLogEnabled": cfg.DetailedLogEnabled,
		"detailedMaxEntries": cfg.DetailedMaxEntries,
		"maxDetailBytes":     cfg.MaxDetailBytes,
		"detailCount":        count,
		"detailBytes":        bytes,
	})
}

func (h *Handler) apiUpdateRequestLogConfig(w http.ResponseWriter, r *http.Request) {
	var req config.RequestLogConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	existing := config.GetRequestLogConfig()
	if req.DetailedMaxEntries == 0 {
		req.DetailedMaxEntries = existing.DetailedMaxEntries
	}
	if req.MaxDetailBytes == 0 {
		req.MaxDetailBytes = existing.MaxDetailBytes
	}
	if req.MaxEntries < config.MinRequestLogMaxEntries || req.MaxEntries > config.MaxRequestLogMaxEntries {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "maxEntries must be between 100 and 20000"})
		return
	}
	if req.DetailedMaxEntries < config.MinRequestDetailMaxEntries || req.DetailedMaxEntries > config.MaxRequestDetailMaxEntries {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "detailedMaxEntries must be between 1 and 1000"})
		return
	}
	if req.MaxDetailBytes < config.MinRequestDetailMaxBytes || req.MaxDetailBytes > config.MaxRequestDetailMaxBytes {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "maxDetailBytes must be between 16384 and 1048576"})
		return
	}
	if err := config.UpdateRequestLogConfig(req); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.requestLog == nil {
		h.requestLog = newRequestLog(req.MaxEntries)
	} else {
		h.requestLog.configure(req.MaxEntries)
	}
	h.ensureRequestDetailStore().configure(req.DetailedMaxEntries, req.MaxDetailBytes)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetRequestLogConfig()})
}

func (h *Handler) apiGetRequestDetail(w http.ResponseWriter, r *http.Request, download bool) {
	requestID := strings.TrimSpace(r.URL.Query().Get("id"))
	if requestID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "id is required"})
		return
	}
	raw, ok := h.ensureRequestDetailStore().get(requestID)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "request detail not found"})
		return
	}
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="request-detail.json"`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func (h *Handler) apiClearRequestDetails(w http.ResponseWriter, r *http.Request) {
	store := h.ensureRequestDetailStore()
	deleted := store.clear()
	if err := store.Flush(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "deleted": deleted})
}

func (h *Handler) apiGetWebSearch(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetWebSearchConfig())
}

func (h *Handler) apiUpdateWebSearch(w http.ResponseWriter, r *http.Request) {
	var req config.WebSearchConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.UpdateWebSearchConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": config.GetWebSearchConfig()})
}

func (h *Handler) apiGetCountTokensProvider(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(countTokensProviderView(config.GetCountTokensProviderConfig()))
}

func (h *Handler) apiUpdateCountTokensProvider(w http.ResponseWriter, r *http.Request) {
	var req config.CountTokensProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ApiURL = strings.TrimSpace(req.ApiURL)
	req.ApiKey = strings.TrimSpace(req.ApiKey)
	if req.ApiKey == "" {
		req.ApiKey = config.GetCountTokensProviderConfig().ApiKey
	}
	req.AuthType = strings.ToLower(strings.TrimSpace(req.AuthType))
	if req.Enabled && req.ApiURL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiUrl is required when enabled"})
		return
	}
	if req.AuthType != "" && req.AuthType != "bearer" && req.AuthType != "x-api-key" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "authType must be bearer or x-api-key"})
		return
	}
	if err := config.UpdateCountTokensProviderConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "config": countTokensProviderView(config.GetCountTokensProviderConfig())})
}

func countTokensProviderView(value config.CountTokensProviderConfig) map[string]interface{} {
	return map[string]interface{}{
		"enabled":          value.Enabled,
		"apiUrl":           value.ApiURL,
		"apiKey":           maskedCredentialValue(value.ApiKey),
		"apiKeyConfigured": value.ApiKey != "",
		"authType":         value.AuthType,
	}
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   h.totalRequests.Load(),
		"successRequests": h.successRequests.Load(),
		"failedRequests":  h.failedRequests.Load(),
		"totalTokens":     h.totalTokens.Load(),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetRequests(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	requestLogCfg := config.GetRequestLogConfig()
	if limit > requestLogCfg.MaxEntries {
		limit = requestLogCfg.MaxEntries
	}
	if h.requestLog == nil {
		h.requestLog = newRequestLog(requestLogCfg.MaxEntries)
	}
	requests := h.requestLog.list(limit)
	details := h.ensureRequestDetailStore()
	for i := range requests {
		requests[i].DetailAvailable = details.has(requests[i].RequestID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"requests":   requests,
		"limit":      limit,
		"maxEntries": requestLogCfg.MaxEntries,
	})
}

func (h *Handler) apiGetUpstreamProtection(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetUpstreamProtectionConfig())
}

func (h *Handler) apiUpdateUpstreamProtection(w http.ResponseWriter, r *http.Request) {
	var req config.UpstreamProtectionConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.UpdateUpstreamProtectionConfig(req); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetUpstreamProtectionStatus(w http.ResponseWriter, r *http.Request) {
	snapshot := h.pool.ProtectionSnapshot()
	snapshot["networkCircuits"] = sharedUpstreamHealth.Snapshot()
	snapshot["accountEndpointRoutes"] = sharedAccountEndpointRoutes.snapshot()
	json.NewEncoder(w).Encode(snapshot)
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	h.totalRequests.Store(0)
	h.successRequests.Store(0)
	h.failedRequests.Store(0)
	h.totalTokens.Store(0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	if h.pool != nil {
		h.pool.ResetStats()
	}
	if err := config.ResetStatistics(); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	checks := []accountTestCheck{}
	if err := h.ensureValidTokenContext(r.Context(), account); err != nil {
		checks = append(checks, failedAccountTestCheck("token", "Token refresh failed: "+err.Error()))
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Token refresh failed: " + err.Error(), "checks": checks})
		return
	}
	checks = append(checks, accountTestCheck{Name: "token", Success: true})

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	if info, err := RefreshAccountInfoContext(r.Context(), account); err != nil {
		checks = append(checks, failedAccountTestCheck("usage", err.Error()))
	} else {
		checks = append(checks, accountTestCheck{
			Name:               "usage",
			Success:            true,
			SubscriptionType:   info.SubscriptionType,
			SubscriptionTitle:  info.SubscriptionTitle,
			UsageCurrent:       info.UsageCurrent,
			UsageLimit:         info.UsageLimit,
			UsagePercent:       info.UsagePercent,
			OverageStatus:      account.OverageStatus,
			OverageCapability:  account.OverageCapability,
			OverageCheckedAt:   account.OverageCheckedAt,
			AvailableModelHint: req.Model,
		})
	}

	if err := h.fetchAndCacheAccountModels(account); err != nil {
		checks = append(checks, failedAccountTestCheck("models", err.Error()))
	} else {
		checks = append(checks, accountTestCheck{Name: "models", Success: true, Count: len(h.pool.GetModelList(account.ID))})
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	var generationErr error
	for _, endpoint := range getRequestEndpointsForAccount(config.GetPreferredEndpoint(), kiroPayload, account) {
		check := h.testKiroGenerationEndpoint(account, kiroPayload, endpoint)
		checks = append(checks, check)
		if check.Success && content == "" {
			content = check.Reply
		}
		if !check.Success {
			generationErr = fmt.Errorf("%s", check.Error)
		}
	}

	if config.GetWebSearchConfig().Enabled {
		if results, err := callMCPWebSearch(account, "Kiro IDE release notes"); err != nil {
			checks = append(checks, failedAccountTestCheck("websearch", err.Error()))
		} else {
			checks = append(checks, accountTestCheck{Name: "websearch", Success: true, Count: len(results.Results)})
		}
	}

	if content == "" {
		errText := "generation failed"
		if generationErr != nil {
			errText = generationErr.Error()
		}
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": errText, "checks": checks, "model": req.Model})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
		"checks":  checks,
	})
}

type accountTestCheck struct {
	Name               string  `json:"name"`
	Endpoint           string  `json:"endpoint,omitempty"`
	Success            bool    `json:"success"`
	StatusCode         int     `json:"statusCode,omitempty"`
	Error              string  `json:"error,omitempty"`
	Reply              string  `json:"reply,omitempty"`
	Count              int     `json:"count,omitempty"`
	SubscriptionType   string  `json:"subscriptionType,omitempty"`
	SubscriptionTitle  string  `json:"subscriptionTitle,omitempty"`
	UsageCurrent       float64 `json:"usageCurrent,omitempty"`
	UsageLimit         float64 `json:"usageLimit,omitempty"`
	UsagePercent       float64 `json:"usagePercent,omitempty"`
	OverageStatus      string  `json:"overageStatus,omitempty"`
	OverageCapability  string  `json:"overageCapability,omitempty"`
	OverageCheckedAt   int64   `json:"overageCheckedAt,omitempty"`
	AvailableModelHint string  `json:"availableModelHint,omitempty"`
}

func failedAccountTestCheck(name, err string) accountTestCheck {
	return accountTestCheck{Name: name, Success: false, Error: truncateDiagnosticText(err, 800)}
}

func (h *Handler) testKiroGenerationEndpoint(account *config.Account, payload *KiroPayload, endpoint kiroEndpoint) accountTestCheck {
	check := accountTestCheck{Name: "generation", Endpoint: endpoint.Name}
	payloadCopy := cloneKiroPayloadForTest(payload)
	if payloadCopy == nil {
		check.Error = "invalid Kiro payload"
		return check
	}
	setPayloadProfileArnForAccount(payloadCopy, account)
	if endpoint.RequiresProfileArn && strings.TrimSpace(payloadCopy.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payloadCopy.ProfileArn = profileArn
		}
	}
	payloadCopy.ConversationState.CurrentMessage.UserInputMessage.Origin = endpoint.Origin

	reqBody, err := json.Marshal(payloadCopy)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	endpointURL := endpoint.ResolveURL(account, payloadCopy.ProfileArn)
	req, err := http.NewRequest("POST", endpointURL, bytes.NewReader(reqBody))
	if err != nil {
		check.Error = err.Error()
		return check
	}
	host := ""
	if parsedURL, parseErr := url.Parse(endpointURL); parseErr == nil {
		host = parsedURL.Host
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if endpoint.AmzTarget != "" {
		req.Header.Set("X-Amz-Target", endpoint.AmzTarget)
	}
	applyKiroBaseHeaders(req, account, buildStreamingHeaderValues(account, host))
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

	client, err := GetClientForProxy(ResolveAccountProxyURL(account))
	if err != nil {
		check.Error = "configure outbound proxy: " + err.Error()
		return check
	}
	resp, err := client.Do(req)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	defer resp.Body.Close()
	check.StatusCode = resp.StatusCode
	if resp.StatusCode != 200 {
		body := httpbody.ReadAllTruncated(resp.Body, httpbody.DefaultLimit)
		if resp.StatusCode == 429 {
			check.Error = "quota exhausted"
		} else {
			check.Error = truncateDiagnosticText(string(body), 800)
		}
		return check
	}

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}
	if err := parseEventStream(resp.Body, callback); err != nil {
		check.Error = err.Error()
		return check
	}
	check.Success = true
	check.Reply = content
	return check
}

func cloneKiroPayloadForTest(payload *KiroPayload) *KiroPayload {
	if payload == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var cloned KiroPayload
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil
	}
	return &cloned
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		return sharedTokenRefreshCoordinator.RefreshContext(r.Context(), account, true)
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfoContext(r.Context(), account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfoContext(r.Context(), account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull returns account details with credentials masked. Raw
// credentials are only available through the re-authenticated export endpoint.
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	proxyURL, proxyPasswordSet := sanitizedProxyURL(account.ProxyURL)
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       maskedCredentialValue(account.AccessToken),
		"refreshToken":      maskedCredentialValue(account.RefreshToken),
		"kiroApiKey":        maskedCredentialValue(account.KiroApiKey),
		"clientId":          maskedCredentialValue(account.ClientID),
		"clientSecret":      maskedCredentialValue(account.ClientSecret),
		"hasAccessToken":    account.AccessToken != "",
		"hasRefreshToken":   account.RefreshToken != "",
		"hasKiroApiKey":     account.KiroApiKey != "",
		"hasClientSecret":   account.ClientSecret != "",
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
		"profileArn":        account.ProfileArn,
		"tokenEndpoint":     account.TokenEndpoint,
		"issuerUrl":         account.IssuerURL,
		"scopes":            account.Scopes,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"priority":          account.Priority,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          proxyURL,
		"proxyPasswordSet":  proxyPasswordSet,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"successCount":      stats.RequestCount,
		"failureCount":      stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiExportAccountCredentials(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if !h.verifyAdminReauthentication(w, r, req.Password) {
		return
	}

	for _, account := range config.GetAccounts() {
		if account.ID != id {
			continue
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"clientId":      account.ClientID,
			"clientSecret":  account.ClientSecret,
			"accessToken":   account.AccessToken,
			"refreshToken":  account.RefreshToken,
			"kiroApiKey":    account.KiroApiKey,
			"authMethod":    account.AuthMethod,
			"provider":      account.Provider,
			"region":        account.Region,
			"profileArn":    account.ProfileArn,
			"tokenEndpoint": account.TokenEndpoint,
			"issuerUrl":     account.IssuerURL,
			"scopes":        account.Scopes,
		})
		return
	}

	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models, err := ListAvailableModelsContext(r.Context(), account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":                     cfg.Suffix,
		"openaiFormat":               cfg.OpenAIFormat,
		"claudeFormat":               cfg.ClaudeFormat,
		"defaultBudgetTokens":        cfg.DefaultBudgetTokens,
		"budgetCapTokens":            cfg.BudgetCapTokens,
		"defaultMaxOutputTokens":     cfg.DefaultMaxOutputTokens,
		"defaultContextWindowTokens": cfg.DefaultContextWindowTokens,
		"toolStreamMode":             cfg.ToolStreamMode,
		"bufferToolStreams":          cfg.BufferToolStreams,
		"enforceAgentToolUse":        cfg.EnforceAgentToolUse,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix                     string  `json:"suffix"`
		OpenAIFormat               string  `json:"openaiFormat"`
		ClaudeFormat               string  `json:"claudeFormat"`
		DefaultBudgetTokens        *int    `json:"defaultBudgetTokens"`
		BudgetCapTokens            *int    `json:"budgetCapTokens"`
		DefaultMaxOutputTokens     *int    `json:"defaultMaxOutputTokens"`
		DefaultContextWindowTokens *int    `json:"defaultContextWindowTokens"`
		ToolStreamMode             *string `json:"toolStreamMode"`
		BufferToolStreams          *bool   `json:"bufferToolStreams"`
		EnforceAgentToolUse        *bool   `json:"enforceAgentToolUse"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	current := config.GetThinkingConfig()
	defaultBudgetTokens := current.DefaultBudgetTokens
	if req.DefaultBudgetTokens != nil {
		defaultBudgetTokens = *req.DefaultBudgetTokens
	}
	budgetCapTokens := current.BudgetCapTokens
	if req.BudgetCapTokens != nil {
		budgetCapTokens = *req.BudgetCapTokens
	}
	defaultMaxOutputTokens := current.DefaultMaxOutputTokens
	if req.DefaultMaxOutputTokens != nil {
		defaultMaxOutputTokens = *req.DefaultMaxOutputTokens
	}
	defaultContextWindowTokens := current.DefaultContextWindowTokens
	if req.DefaultContextWindowTokens != nil {
		defaultContextWindowTokens = *req.DefaultContextWindowTokens
	}
	toolStreamMode := current.ToolStreamMode
	if req.ToolStreamMode != nil {
		normalized, ok := config.NormalizeToolStreamMode(*req.ToolStreamMode)
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "toolStreamMode must be: safe, adaptive, balanced, or live"})
			return
		}
		toolStreamMode = normalized
	} else if req.BufferToolStreams != nil {
		toolStreamMode = config.ToolStreamModeLive
		if *req.BufferToolStreams {
			toolStreamMode = config.ToolStreamModeSafe
		}
	}
	enforceAgentToolUse := current.EnforceAgentToolUse
	if req.EnforceAgentToolUse != nil {
		enforceAgentToolUse = *req.EnforceAgentToolUse
	}
	if defaultBudgetTokens < 1024 || defaultBudgetTokens > 200000 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "defaultBudgetTokens must be between 1024 and 200000"})
		return
	}
	if budgetCapTokens < 0 || budgetCapTokens > 200000 || (budgetCapTokens > 0 && budgetCapTokens < 1024) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "budgetCapTokens must be 0 or between 1024 and 200000"})
		return
	}
	if budgetCapTokens > 0 && defaultBudgetTokens > budgetCapTokens {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "defaultBudgetTokens cannot exceed budgetCapTokens"})
		return
	}
	if defaultMaxOutputTokens < 0 || defaultMaxOutputTokens > 1000000 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "defaultMaxOutputTokens must be between 0 and 1000000"})
		return
	}
	if defaultContextWindowTokens < 0 || defaultContextWindowTokens > 10000000 || (defaultContextWindowTokens > 0 && defaultContextWindowTokens < 1024) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "defaultContextWindowTokens must be 0 or between 1024 and 10000000"})
		return
	}

	if err := config.UpdateThinkingConfigWithToolStreamMode(req.Suffix, req.OpenAIFormat, req.ClaudeFormat, defaultBudgetTokens, budgetCapTokens, defaultMaxOutputTokens, defaultContextWindowTokens, toolStreamMode, enforceAgentToolUse); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "runtime": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, runtime, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdateEndpointConfig(req.PreferredEndpoint, req.EndpointFallback); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) error {
	if err := InitKiroHttpClient(proxyURL); err != nil {
		return err
	}
	if err := auth.InitHttpClient(proxyURL); err != nil {
		return err
	}
	return nil
}

func validateAccountProxyURL(proxyURL string) error {
	if err := outboundproxy.Validate(proxyURL); err != nil {
		return fmt.Errorf("invalid proxyURL: %w", err)
	}
	return nil
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	proxyURL, passwordSet := sanitizedProxyURL(config.GetProxyURL())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"proxyURL":         proxyURL,
		"proxyPasswordSet": passwordSet,
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProxyURL = preserveProxyPassword(config.GetProxyURL(), req.ProxyURL)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)
	if err := validateAccountProxyURL(req.ProxyURL); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	previousProxyURL := config.GetProxyURL()
	if err := applyProxyConfig(req.ProxyURL); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		_ = applyProxyConfig(previousProxyURL)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs      []string `json:"ids"` // 为空则导出全部
		Password string   `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if !h.verifyAdminReauthentication(w, r, req.Password) {
		return
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken   string `json:"accessToken"`
		CsrfToken     string `json:"csrfToken"`
		RefreshToken  string `json:"refreshToken"`
		KiroApiKey    string `json:"kiroApiKey,omitempty"`
		ClientID      string `json:"clientId,omitempty"`
		ClientSecret  string `json:"clientSecret,omitempty"`
		Region        string `json:"region,omitempty"`
		ExpiresAt     int64  `json:"expiresAt"`
		AuthMethod    string `json:"authMethod,omitempty"`
		Provider      string `json:"provider,omitempty"`
		ProfileArn    string `json:"profileArn,omitempty"`
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "api_key" {
				idp = "API Key"
			} else if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:   a.AccessToken,
				CsrfToken:     "",
				RefreshToken:  a.RefreshToken,
				KiroApiKey:    a.KiroApiKey,
				ClientID:      a.ClientID,
				ClientSecret:  a.ClientSecret,
				Region:        a.Region,
				ExpiresAt:     a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:    authMethod,
				Provider:      a.Provider,
				ProfileArn:    a.ProfileArn,
				TokenEndpoint: a.TokenEndpoint,
				IssuerURL:     a.IssuerURL,
				Scopes:        a.Scopes,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
