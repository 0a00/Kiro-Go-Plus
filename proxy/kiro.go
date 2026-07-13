// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/internal/clientcache"
	"kiro-go/internal/httpbody"
	"kiro-go/internal/outboundproxy"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	Key                string
	URL                string
	Origin             string
	AmzTarget          string
	Name               string
	ContentType        string
	RequiresProfileArn bool
}

func (ep kiroEndpoint) ResolveURL(account *config.Account, profileArn ...string) string {
	payloadProfileArn := ""
	if len(profileArn) > 0 {
		payloadProfileArn = profileArn[0]
	}
	return regionalizeURLForProfile(ep.URL, account, payloadProfileArn)
}

var kiroEndpoints = []kiroEndpoint{
	{
		Key:                "runtime",
		URL:                "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		Origin:             "AI_EDITOR",
		AmzTarget:          "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:               "Kiro Runtime",
		ContentType:        "application/x-amz-json-1.0",
		RequiresProfileArn: true,
	},
	{
		Key:       "kiro",
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		Key:       "codewhisperer",
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		Key:       "amazonq",
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache = clientcache.New(2048, 30*time.Minute)

func init() {
	if err := InitKiroHttpClient(""); err != nil {
		panic(err)
	}
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || proxyURL == strings.TrimSpace(config.GetProxyURL()) {
		return kiroHttpStore.Load(), nil
	}
	transport, err := buildKiroTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return proxyClientCache.Get(proxyURL, func() *http.Client {
		return &http.Client{Transport: transport}
	}), nil
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || proxyURL == strings.TrimSpace(config.GetProxyURL()) {
		return kiroRestHttpStore.Load(), nil
	}
	transport, err := buildKiroTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	cacheKey := "rest:" + proxyURL
	return proxyClientCache.Get(cacheKey, func() *http.Client {
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}), nil
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil {
		proxyURL := strings.TrimSpace(account.ProxyURL)
		if strings.EqualFold(proxyURL, "direct") {
			return "direct"
		}
		if proxyURL != "" {
			return proxyURL
		}
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) (*http.Transport, error) {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	if err := outboundproxy.Apply(t, proxyURL); err != nil {
		return nil, err
	}
	return t, nil
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) error {
	transport, err := buildKiroTransport(proxyURL)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: transport,
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport.Clone(),
	}
	kiroRestHttpStore.Store(restClient)
	return nil
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	requestContext          context.Context
	attemptBudget           *upstreamAttemptBudget
	contextWindowTokens     int
	requireActionableOutput bool
	requireToolUse          bool
	deferTextUntilComplete  bool
	streamThinkingPrecommit bool
	streamToolUseDeltas     bool
	toolUsePolicy           string
	tokenRefreshMu          sync.Mutex
	tokenRefreshAttempts    map[string]int
	streamMetricsMu         sync.Mutex
	streamMetricsEnabled    bool
	streamMetricsStartedAt  time.Time
	firstUpstreamActivityMs int64
	maxToolAssemblyMs       int64
	runtimeMu               sync.RWMutex
	selectedEndpoint        string
}

func (p *KiroPayload) beginStreamMetrics(startedAt time.Time) {
	if p == nil {
		return
	}
	p.streamMetricsMu.Lock()
	p.streamMetricsEnabled = true
	p.streamMetricsStartedAt = startedAt
	p.firstUpstreamActivityMs = -1
	p.maxToolAssemblyMs = -1
	p.streamMetricsMu.Unlock()
}

func (p *KiroPayload) recordUpstreamActivity() {
	if p == nil {
		return
	}
	p.streamMetricsMu.Lock()
	if p.streamMetricsEnabled && p.firstUpstreamActivityMs < 0 && !p.streamMetricsStartedAt.IsZero() {
		elapsed := time.Since(p.streamMetricsStartedAt).Milliseconds()
		if elapsed < 0 {
			elapsed = 0
		}
		p.firstUpstreamActivityMs = elapsed
	}
	p.streamMetricsMu.Unlock()
}

func (p *KiroPayload) recordToolAssembly(elapsed time.Duration) {
	if p == nil {
		return
	}
	ms := elapsed.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	p.streamMetricsMu.Lock()
	if p.streamMetricsEnabled && ms > p.maxToolAssemblyMs {
		p.maxToolAssemblyMs = ms
	}
	p.streamMetricsMu.Unlock()
}

func (p *KiroPayload) streamMetrics() (firstUpstreamActivityMs, maxToolAssemblyMs *int64) {
	if p == nil {
		return nil, nil
	}
	p.streamMetricsMu.Lock()
	defer p.streamMetricsMu.Unlock()
	if !p.streamMetricsEnabled {
		return nil, nil
	}
	if p.firstUpstreamActivityMs >= 0 {
		value := p.firstUpstreamActivityMs
		firstUpstreamActivityMs = &value
	}
	if p.maxToolAssemblyMs >= 0 {
		value := p.maxToolAssemblyMs
		maxToolAssemblyMs = &value
	}
	return firstUpstreamActivityMs, maxToolAssemblyMs
}

func (p *KiroPayload) setSuccessfulEndpoint(endpoint string) {
	if p == nil {
		return
	}
	p.runtimeMu.Lock()
	p.selectedEndpoint = endpoint
	p.runtimeMu.Unlock()
}

func (p *KiroPayload) successfulEndpoint() string {
	if p == nil {
		return ""
	}
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	return p.selectedEndpoint
}

func (p *KiroPayload) takeTokenRefreshAttempt(account *config.Account) bool {
	if p == nil || account == nil {
		return false
	}
	key := strings.TrimSpace(account.ID)
	if key == "" {
		key = strings.Join([]string{
			strings.TrimSpace(account.AuthMethod),
			strings.TrimSpace(account.Region),
			strings.TrimSpace(account.ClientID),
			strings.TrimSpace(account.Email),
		}, "|")
	}
	p.tokenRefreshMu.Lock()
	defer p.tokenRefreshMu.Unlock()
	if p.tokenRefreshAttempts == nil {
		p.tokenRefreshAttempts = make(map[string]int)
	}
	if p.tokenRefreshAttempts[key] >= 1 {
		return false
	}
	p.tokenRefreshAttempts[key]++
	return true
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText            func(text string, isThinking bool)
	OnToolUse         func(toolUse KiroToolUse)
	OnToolUseActivity func()
	OnToolUseStart    func(toolUseID, name string)
	OnToolUseDelta    func(toolUseID, input string)
	OnToolUseStop     func(toolUseID string)
	OnProgress        func()
	OnComplete        func(inputTokens, outputTokens int)
	OnUsage           func(usage KiroTokenUsage)
	OnTruncated       func(reason string)
	OnError           func(err error)
	OnCredits         func(credits float64)
	OnContextUsage    func(percentage float64)
	detailTrace       *requestDetailTrace
	detailToolNameMap map[string]string
}

// KiroTokenUsage preserves upstream cache accounting when the event stream
// exposes it. HasCacheBreakdown distinguishes real zero values from absence.
type KiroTokenUsage struct {
	InputTokens              int
	OutputTokens             int
	UncachedInputTokens      int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CacheCreation5mTokens    int
	CacheCreation1hTokens    int
	HasCacheBreakdown        bool
	hasUncachedBreakdown     bool
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()
	preferred = strings.ToLower(strings.TrimSpace(preferred))
	if preferred == "" || preferred == "auto" {
		return append([]kiroEndpoint(nil), kiroEndpoints...)
	}

	primary := -1
	for i := range kiroEndpoints {
		if kiroEndpoints[i].Key == preferred {
			primary = i
			break
		}
	}
	if primary < 0 {
		return append([]kiroEndpoint(nil), kiroEndpoints...)
	}
	if !fallback {
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	requestContext := context.Background()
	if payload != nil && payload.requestContext != nil {
		requestContext = payload.requestContext
	}
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)
	if payload != nil && payload.attemptBudget == nil {
		payload.attemptBudget = newUpstreamAttemptBudget()
	}

	if _, err := json.Marshal(payload); err != nil {
		return err
	}
	detailTrace := requestDetailTraceFromContext(requestContext)
	if detailTrace != nil {
		if callback == nil {
			callback = &KiroStreamCallback{}
		}
		observed := *callback
		observed.detailTrace = detailTrace
		observed.detailToolNameMap = payload.ToolNameMap
		callback = &observed
	}

	// Keep request diagnostics useful without writing prompts, tool arguments or
	// image content to logs.
	if payloadJSON, err := json.Marshal(payload); err == nil {
		message := payload.ConversationState.CurrentMessage.UserInputMessage
		toolCount := 0
		if message.UserInputMessageContext != nil {
			toolCount = len(message.UserInputMessageContext.Tools)
		}
		logger.Debugf("[KiroAPI] Request model=%s bytes=%d history=%d tools=%d images=%d",
			message.ModelID, len(payloadJSON), len(payload.ConversationState.History), toolCount, len(message.Images))
	}

	// Restore original tool names before callbacks leave the upstream layer.
	if callback != nil && len(payload.ToolNameMap) > 0 && (callback.OnToolUse != nil || callback.OnToolUseStart != nil) {
		nameMap := payload.ToolNameMap
		wrapped := *callback
		if callback.OnToolUse != nil {
			originalOnToolUse := callback.OnToolUse
			wrapped.OnToolUse = func(tu KiroToolUse) {
				if original, ok := nameMap[tu.Name]; ok {
					tu.Name = original
				}
				originalOnToolUse(tu)
			}
		}
		if callback.OnToolUseStart != nil {
			originalOnToolUseStart := callback.OnToolUseStart
			wrapped.OnToolUseStart = func(toolUseID, name string) {
				if original, ok := nameMap[name]; ok {
					name = original
				}
				originalOnToolUseStart(toolUseID, name)
			}
		}
		callback = &wrapped
	}

	// Build endpoint list before profile lookup. Legacy/custom endpoints that do
	// not require a profile must not pay for a potentially slow profile probe.
	preferredEndpoint := config.GetPreferredEndpoint()
	endpoints := getSortedEndpoints(preferredEndpoint)
	accountID := ""
	accountEmail := ""
	if account != nil {
		accountID = account.ID
		accountEmail = account.Email
	}
	modelKey := endpointRouteModel(payload)
	var routeErr error
	endpoints, routeErr = sharedAccountEndpointRoutes.availableEndpoints(accountID, modelKey, preferredEndpoint, endpoints)
	if routeErr != nil {
		return routeErr
	}
	requiresProfileArn := false
	for _, endpoint := range endpoints {
		if endpoint.RequiresProfileArn {
			requiresProfileArn = true
			break
		}
	}
	if requiresProfileArn && payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArnContext(requestContext, account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	var lastErr error
	var lastCircuitError error
	effectiveProxyURL := ResolveAccountProxyURL(account)
	client, err := GetClientForProxy(effectiveProxyURL)
	if err != nil {
		return classifyTransportError("outbound proxy", fmt.Errorf("configure outbound proxy: %w", err))
	}
	proxyCircuitKey := ""
	proxyCircuitLabel := ""
	proxyStartedAt := time.Now()
	proxyTransportFailed := false
	proxyTransportSucceeded := false
	if strings.TrimSpace(effectiveProxyURL) != "" && !strings.EqualFold(strings.TrimSpace(effectiveProxyURL), "direct") {
		proxyCircuitKey = effectiveProxyURL
		proxyCircuitLabel, _ = sanitizedProxyURL(effectiveProxyURL)
		if !sharedUpstreamHealth.beginProxy(proxyCircuitKey, proxyCircuitLabel) {
			return &UpstreamError{
				Kind:                UpstreamErrorEndpointUnavailable,
				Endpoint:            "outbound proxy",
				Message:             "outbound proxy circuit is open",
				RetryAcrossAccounts: true,
			}
		}
		defer func() {
			latency := time.Since(proxyStartedAt)
			switch {
			case proxyTransportSucceeded:
				sharedUpstreamHealth.proxySuccess(proxyCircuitKey, latency)
			case proxyTransportFailed:
				sharedUpstreamHealth.proxyFailure(proxyCircuitKey, lastCircuitError, latency)
			default:
				sharedUpstreamHealth.releaseProxy(proxyCircuitKey)
			}
		}()
	}

	for _, ep := range endpoints {
		attemptStartedAt := time.Now()
		if err := requestContext.Err(); err != nil {
			return classifyRequestCancellation(ep.Name, err)
		}
		if ep.RequiresProfileArn && strings.TrimSpace(payload.ProfileArn) == "" {
			lastErr = &UpstreamError{
				Kind:                 UpstreamErrorEndpointUnavailable,
				Endpoint:             ep.Name,
				Message:              "profileArn is required by the runtime endpoint",
				RetryAcrossEndpoints: true,
				RetryAcrossAccounts:  true,
			}
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, "", attemptStartedAt, 0, "skipped", lastErr, requestDetailRetryReason(lastErr))
			continue
		}
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		reqBody, _ := json.Marshal(payload)
		endpointURL := ep.ResolveURL(account, payload.ProfileArn)
		endpointHost := endpointURL
		if parsedURL, parseErr := url.Parse(endpointURL); parseErr == nil && parsedURL.Host != "" {
			endpointHost = parsedURL.Host
		}
		endpointCircuitKey := ep.Key + "|" + endpointHost
		endpointCircuitLabel := ep.Name + " (" + endpointHost + ")"
		if !sharedUpstreamHealth.beginEndpoint(endpointCircuitKey, endpointCircuitLabel) {
			lastErr = &UpstreamError{
				Kind:                 UpstreamErrorEndpointUnavailable,
				Endpoint:             ep.Name,
				Message:              "endpoint circuit is open",
				RetryAcrossEndpoints: true,
				RetryAcrossAccounts:  true,
			}
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, 0, "skipped", lastErr, requestDetailRetryReason(lastErr))
			continue
		}
		if payload != nil && !payload.attemptBudget.take() {
			sharedUpstreamHealth.releaseEndpoint(endpointCircuitKey)
			lastErr = newRetryBudgetError(payload.attemptBudget)
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, 0, "rejected", lastErr, requestDetailRetryReason(lastErr))
			return lastErr
		}
		upstreamContext := requestContext
		var cancelRequest context.CancelFunc
		if payload != nil {
			if deadline, ok := payload.attemptBudget.deadline(); ok {
				upstreamContext, cancelRequest = context.WithDeadline(requestContext, deadline)
			}
		}
		if cancelRequest == nil {
			upstreamContext, cancelRequest = context.WithCancel(requestContext)
		}
		req, err := http.NewRequestWithContext(upstreamContext, "POST", endpointURL, bytes.NewReader(reqBody))
		if err != nil {
			cancelRequest()
			sharedUpstreamHealth.releaseEndpoint(endpointCircuitKey)
			lastErr = err
			if payload != nil {
				payload.attemptBudget.recordFailure(ep.Name, lastErr)
			}
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, 0, "error", lastErr, requestDetailRetryReason(lastErr))
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(endpointURL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		contentType := ep.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		if requestID := requestIDFromContext(requestContext); requestID != "" {
			req.Header.Set("X-Request-Id", requestID)
		}
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		req.Header.Set("x-amzn-codewhisperer-optout", "true")
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		var firstTokenTimedOut atomic.Bool
		firstTokenTimeout := time.Duration(config.GetRetryConfig().FirstTokenTimeoutSeconds) * time.Second
		var firstTokenTimer *time.Timer
		wrappedCallback, meaningfulGate := wrapMeaningfulStreamCallback(callback, func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
			if payload != nil {
				payload.recordUpstreamActivity()
			}
		}, payload != nil && payload.requireActionableOutput, payload != nil && payload.requireToolUse, payload != nil && payload.deferTextUntilComplete, payload != nil && payload.streamThinkingPrecommit)
		toolAssemblyTimeout := time.Duration(config.GetRetryConfig().ToolAssemblyTimeoutSeconds) * time.Second
		wrappedCallback, toolMonitor := wrapToolAssemblyMonitor(wrappedCallback, toolAssemblyTimeout, func(toolAssemblySnapshot) {
			cancelRequest()
		})
		if firstTokenTimeout > 0 {
			firstTokenTimer = time.AfterFunc(firstTokenTimeout, func() {
				firstTokenTimedOut.Store(true)
				cancelRequest()
			})
		}

		resp, err := client.Do(req)
		if err != nil {
			stopAndRecordToolAssembly(payload, toolMonitor)
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
			cancelRequest()
			if firstTokenTimedOut.Load() {
				err = context.DeadlineExceeded
			} else if requestContext.Err() != nil {
				lastErr = classifyRequestCancellation(ep.Name, requestContext.Err())
				detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, 0, "canceled", lastErr, requestDetailRetryReason(lastErr))
				return lastErr
			}
			lastErr = classifyTransportError(ep.Name, err)
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, 0, "error", lastErr, requestDetailRetryReason(lastErr))
			if payload != nil {
				payload.attemptBudget.recordFailure(ep.Name, lastErr)
			}
			if cooldown := sharedAccountEndpointRoutes.recordFailure(accountID, modelKey, ep, lastErr); cooldown > 0 {
				logger.Warnf("[EndpointRouting] Account %s model %s endpoint %s cooling for %s after transport error: %v", accountID, modelKey, ep.Name, cooldown, lastErr)
			}
			lastCircuitError = lastErr
			proxyTransportFailed = true
			sharedUpstreamHealth.endpointFailure(endpointCircuitKey, lastErr, time.Since(attemptStartedAt))
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			if payload != nil && payload.attemptBudget.expired() && requestContext.Err() == nil {
				return newRetryBudgetError(payload.attemptBudget)
			}
			if shouldRetryAcrossEndpoints(lastErr) {
				continue
			}
			return lastErr
		}
		proxyTransportSucceeded = true

		if resp.StatusCode != 200 {
			stopAndRecordToolAssembly(payload, toolMonitor)
			errBody := httpbody.ReadAllTruncated(resp.Body, httpbody.DefaultLimit)
			resp.Body.Close()
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
			cancelRequest()
			classifiedErr := classifyUpstreamHTTPError(resp.StatusCode, ep.Name, errBody)
			classifiedErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			lastErr = classifiedErr
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, resp.StatusCode, "http_error", lastErr, requestDetailRetryReason(lastErr))
			if payload != nil {
				payload.attemptBudget.recordFailure(ep.Name, lastErr)
			}
			if cooldown := sharedAccountEndpointRoutes.recordFailure(accountID, modelKey, ep, lastErr); cooldown > 0 {
				logger.Warnf("[EndpointRouting] Account %s model %s endpoint %s cooling for %s after %v", accountID, modelKey, ep.Name, cooldown, lastErr)
			}
			if circuitEligibleFailure(lastErr) {
				lastCircuitError = lastErr
				sharedUpstreamHealth.endpointFailure(endpointCircuitKey, lastErr, time.Since(attemptStartedAt))
			} else {
				sharedUpstreamHealth.endpointSuccess(endpointCircuitKey, time.Since(attemptStartedAt))
			}
			if upstreamErr, ok := asUpstreamError(lastErr); ok && upstreamErr.RefreshToken &&
				payload != nil && payload.takeTokenRefreshAttempt(account) && !isKiroAPIKeyAccount(account) {
				if refreshErr := sharedTokenRefreshCoordinator.RefreshContext(requestContext, account, true); refreshErr == nil {
					return CallKiroAPI(account, payload, callback)
				} else {
					return classifyRefreshFailure(ep.Name, refreshErr)
				}
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			if shouldRetryAcrossEndpoints(lastErr) {
				continue
			}
			return lastErr
		}

		var streamIdleTimedOut atomic.Bool
		idleTimeout := time.Duration(config.GetRetryConfig().StreamIdleTimeoutSeconds) * time.Second
		idleReader := newStreamIdleReader(resp.Body, idleTimeout, func() {
			streamIdleTimedOut.Store(true)
			cancelRequest()
		})
		err = parseEventStream(idleReader, wrappedCallback)
		idleReader.Stop()
		resp.Body.Close()
		stopAndRecordToolAssembly(payload, toolMonitor)
		if firstTokenTimer != nil {
			firstTokenTimer.Stop()
		}
		cancelRequest()
		if err == nil && meaningfulGate.hasInvalidCommittedToolUse() {
			err = &EventStreamError{
				Kind:    EventStreamInvalidPayload,
				Message: "tool use arguments are not valid JSON",
			}
		}
		if err != nil {
			if toolSnapshot, timedOut := toolMonitor.TimedOut(); timedOut {
				err = newToolAssemblyTimeoutError(ep.Name, toolSnapshot.Name, toolSnapshot.ArgumentBytes, toolAssemblyTimeout)
			}
			if requestContext.Err() != nil && !firstTokenTimedOut.Load() {
				sharedUpstreamHealth.releaseEndpoint(endpointCircuitKey)
				lastErr = classifyRequestCancellation(ep.Name, requestContext.Err())
				detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, http.StatusOK, "canceled", lastErr, requestDetailRetryReason(lastErr))
				return lastErr
			}
			if streamIdleTimedOut.Load() {
				err = classifyTransportError(ep.Name, context.DeadlineExceeded)
			} else if firstTokenTimedOut.Load() && !meaningfulGate.hasActivity() {
				err = classifyTransportError(ep.Name, context.DeadlineExceeded)
			} else if _, ok := asUpstreamError(err); !ok {
				err = classifyTransportError(ep.Name, err)
			}
			lastErr = err
			attemptStatus := "stream_error"
			if meaningfulGate.hasActionableOutput() {
				attemptStatus = "partial_stream_error"
			}
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, http.StatusOK, attemptStatus, lastErr, requestDetailRetryReason(lastErr))
			if payload != nil {
				payload.attemptBudget.recordFailure(ep.Name, lastErr)
			}
			if cooldown := sharedAccountEndpointRoutes.recordFailure(accountID, modelKey, ep, lastErr); cooldown > 0 {
				logger.Warnf("[EndpointRouting] Account %s model %s endpoint %s cooling for %s after stream error: %v", accountID, modelKey, ep.Name, cooldown, lastErr)
			}
			if meaningfulGate.hasActionableOutput() || !circuitEligibleFailure(err) {
				sharedUpstreamHealth.endpointSuccess(endpointCircuitKey, time.Since(attemptStartedAt))
			} else {
				lastCircuitError = err
				sharedUpstreamHealth.endpointFailure(endpointCircuitKey, err, time.Since(attemptStartedAt))
			}
			if payload != nil && payload.attemptBudget.expired() && requestContext.Err() == nil {
				return newRetryBudgetError(payload.attemptBudget)
			}
			if meaningfulGate.hasActionableOutput() || !shouldRetryAcrossEndpoints(err) {
				return err
			}
			continue
		}
		if !meaningfulGate.hasActionableOutput() {
			retry := payload != nil && payload.attemptBudget.recordEmpty()
			lastErr = newEmptyResponseError(ep.Name, retry)
			if payload != nil {
				payload.attemptBudget.recordFailure(ep.Name, lastErr)
			}
			detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, http.StatusOK, "empty_response", lastErr, requestDetailRetryReason(lastErr))
			lastCircuitError = lastErr
			sharedUpstreamHealth.endpointFailure(endpointCircuitKey, lastErr, time.Since(attemptStartedAt))
			if retry {
				continue
			}
			return lastErr
		}
		sharedUpstreamHealth.endpointSuccess(endpointCircuitKey, time.Since(attemptStartedAt))
		sharedAccountEndpointRoutes.recordSuccess(accountID, modelKey, ep)
		payload.setSuccessfulEndpoint(ep.Name)
		detailTrace.recordAttempt(accountID, accountEmail, ep.Name, endpointHost, attemptStartedAt, http.StatusOK, "success", nil, "")
		return nil
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var tokenUsage KiroTokenUsage
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string
	var sawOutput bool
	var sawCompletionSignal bool
	var recoveredToolUse bool

	for {
		frame, err := readEventStreamFrame(body)
		if err == io.EOF {
			break
		}
		if err != nil {
			if recoverCompleteToolUseWithoutStop(currentToolUse, callback, err) {
				currentToolUse = nil
				recoveredToolUse = true
				break
			}
			return err
		}
		if callback.OnProgress != nil {
			callback.OnProgress()
		}

		eventType := frame.header(":event-type")
		messageType := strings.ToLower(strings.TrimSpace(frame.header(":message-type")))
		if eventType == "" {
			switch messageType {
			case "exception":
				eventType = frame.header(":exception-type")
			case "error":
				eventType = frame.header(":error-code")
			}
		}

		payloadBytes := frame.payload
		lowerType := strings.ToLower(eventType)
		if messageType == "error" || messageType == "exception" ||
			strings.Contains(lowerType, "exception") || strings.Contains(lowerType, "error") {
			reason, message := upstreamErrorDetails(payloadBytes)
			classified := classifyUpstreamHTTPError(http.StatusBadGateway, eventType, payloadBytes)
			if reason != "" {
				classified.Reason = reason
			}
			if message != "" {
				classified.Message = message
			}
			return classified
		}
		if len(payloadBytes) == 0 {
			return &EventStreamError{Kind: EventStreamInvalidPayload, Message: fmt.Sprintf("%s payload is empty", eventType)}
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			label := eventType
			if label == "" {
				label = "event"
			}
			return &EventStreamError{
				Kind:    EventStreamInvalidPayload,
				Message: fmt.Sprintf("%s payload is not valid JSON", label),
				Cause:   err,
			}
		}
		toolEvent := isToolUseEventPayload(eventType, event, currentToolUse)
		if eventType == "" && !toolEvent {
			return &EventStreamError{Kind: EventStreamInvalidHeaders, Message: "missing :event-type header"}
		}

		previousUsage := tokenUsage
		tokenUsage = updateTokenUsageFromEvent(event, tokenUsage)
		if callback.OnUsage != nil && tokenUsage != previousUsage {
			callback.OnUsage(tokenUsage)
		}

		// Some Kiro data planes use distinct event names for tool start, input,
		// and stop frames. Classify those frames by both header and payload.
		if toolEvent {
			if toolUseEventSignalsStop(eventType) {
				event["stop"] = true
			}
			sawOutput = true
			currentToolUse, err = handleToolUseEvent(event, currentToolUse, callback)
			if err != nil {
				return err
			}
			continue
		}

		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				sawOutput = true
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				sawOutput = true
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
				}
			}
		case "meteringEvent":
			sawCompletionSignal = true
			if recoverCompleteToolUseWithoutStop(currentToolUse, callback, fmt.Errorf("%s completion signal", eventType)) {
				currentToolUse = nil
				recoveredToolUse = true
			}
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			sawCompletionSignal = true
			if recoverCompleteToolUseWithoutStop(currentToolUse, callback, fmt.Errorf("%s completion signal", eventType)) {
				currentToolUse = nil
				recoveredToolUse = true
			}
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		if !recoverCompleteToolUseWithoutStop(currentToolUse, callback, io.EOF) {
			return &EventStreamError{
				Kind:    EventStreamIncompleteToolUse,
				Message: fmt.Sprintf("tool %q ended without a stop marker", currentToolUse.Name),
			}
		}
		currentToolUse = nil
		recoveredToolUse = true
	}

	if sawOutput && !sawCompletionSignal && !recoveredToolUse && callback.OnTruncated != nil {
		callback.OnTruncated("stream ended without metering or context completion event")
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	if callback.OnUsage != nil {
		callback.OnUsage(tokenUsage)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(tokenUsage.InputTokens, tokenUsage.OutputTokens)
	}
	return nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	usage := updateTokenUsageFromEvent(event, KiroTokenUsage{
		InputTokens:  currentInputTokens,
		OutputTokens: currentOutputTokens,
	})
	return usage.InputTokens, usage.OutputTokens
}

func updateTokenUsageFromEvent(event map[string]interface{}, current KiroTokenUsage) KiroTokenUsage {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			current.OutputTokens = v
		}

		inputValue, hasExplicitInput := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		)
		if hasExplicitInput {
			current.InputTokens = inputValue
		}

		uncached, hasUncached := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, hasCacheRead := readTokenNumber(usage, "cacheReadInputTokens", "cacheReadTokens", "cache_read_input_tokens", "cache_read_tokens")
		cacheWrite, hasCacheWrite := readTokenNumber(usage, "cacheWriteInputTokens", "cacheWriteTokens", "cache_write_input_tokens", "cache_write_tokens", "cacheCreationInputTokens", "cacheCreationTokens", "cache_creation_input_tokens", "cache_creation_tokens")
		cache5m, hasCache5m := readTokenNumber(usage, "ephemeral5mInputTokens", "ephemeral_5m_input_tokens")
		cache1h, hasCache1h := readTokenNumber(usage, "ephemeral1hInputTokens", "ephemeral_1h_input_tokens")
		for _, key := range []string{"cache_creation", "cacheCreation"} {
			creation, ok := usage[key].(map[string]interface{})
			if !ok {
				continue
			}
			if v, found := readTokenNumber(creation, "ephemeral5mInputTokens", "ephemeral_5m_input_tokens"); found {
				cache5m, hasCache5m = v, true
			}
			if v, found := readTokenNumber(creation, "ephemeral1hInputTokens", "ephemeral_1h_input_tokens"); found {
				cache1h, hasCache1h = v, true
			}
		}
		if hasUncached || hasCacheRead || hasCacheWrite || hasCache5m || hasCache1h {
			current.HasCacheBreakdown = true
		}
		if hasUncached {
			current.UncachedInputTokens = uncached
			current.hasUncachedBreakdown = true
		}
		if hasCacheRead {
			current.CacheReadInputTokens = cacheRead
		}
		if hasCacheWrite {
			current.CacheCreationInputTokens = cacheWrite
		}
		if hasCache5m {
			current.CacheCreation5mTokens = cache5m
		}
		if hasCache1h {
			current.CacheCreation1hTokens = cache1h
		}
		breakdownTotal := current.UncachedInputTokens + current.CacheReadInputTokens + current.CacheCreationInputTokens
		if current.InputTokens <= 0 && breakdownTotal > 0 {
			current.InputTokens = breakdownTotal
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if !hasExplicitInput && ok && total > 0 {
			candidateOutput := current.OutputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				current.InputTokens = total - candidateOutput
			}
		}
	}

	if current.HasCacheBreakdown && !current.hasUncachedBreakdown && current.InputTokens > 0 {
		current.UncachedInputTokens = maxInt(current.InputTokens-current.CacheReadInputTokens-current.CacheCreationInputTokens, 0)
	}
	return current
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if entry, ok := config.GetConfiguredModelMetadata(model); ok && entry.ContextWindow > 0 {
		return entry.ContextWindow
	}
	if configured := config.GetThinkingConfig().DefaultContextWindowTokens; configured > 0 {
		return configured
	}
	if limits, ok := getDiscoveredModelTokenLimits(model); ok && limits.MaxInputTokens > 0 {
		return limits.MaxInputTokens
	}
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

func getPayloadContextWindowSize(payload *KiroPayload, model string) int {
	if payload != nil && payload.contextWindowTokens > 0 {
		return payload.contextWindowTokens
	}
	return getContextWindowSize(model)
}

// claudeVersionExtractor matches both bare major versions and major/minor
// versions in dot or dash form.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		minor := 0
		var errMin error
		if match[2] != "" {
			minor, errMin = strconv.Atoi(match[2])
		}
		if errMaj == nil && errMin == nil {
			// 1M window for Claude >= 4.6 (4.6, 4.7, 4.8, ...) and any major >= 5.
			if major > 4 {
				return true
			}
			if major == 4 && minor >= 6 {
				return true
			}
			return false
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}

	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID     string
	Name          string
	InputBuffer   strings.Builder
	GeneratedID   bool
	StreamStarted bool
}

var toolUseEventTypeNormalizer = strings.NewReplacer("_", "", "-", "", ".", "")

func isToolUseEventPayload(eventType string, event map[string]interface{}, current *toolUseState) bool {
	normalizedType := toolUseEventTypeNormalizer.Replace(strings.ToLower(strings.TrimSpace(eventType)))
	if strings.Contains(normalizedType, "tooluse") {
		return true
	}
	_, hasName := firstPresentField(event, "name", "toolName", "tool_name")
	_, hasID := firstPresentField(event, "toolUseId", "toolUseID", "tool_use_id")
	_, hasInput := event["input"]
	_, hasStop := firstPresentField(event, "stop", "isStop", "done")
	if hasName && (hasID || hasInput || hasStop) {
		return true
	}
	return current != nil && (hasInput || hasStop)
}

func toolUseEventSignalsStop(eventType string) bool {
	normalizedType := toolUseEventTypeNormalizer.Replace(strings.ToLower(strings.TrimSpace(eventType)))
	return strings.Contains(normalizedType, "toolusestop") ||
		strings.Contains(normalizedType, "tooluseend") ||
		strings.Contains(normalizedType, "toolusecomplete")
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) (*toolUseState, error) {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")
	created := false

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
			created = true
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				return nil, &EventStreamError{
					Kind:    EventStreamIncompleteToolUse,
					Message: fmt.Sprintf("tool %q was replaced before its stop marker", current.Name),
				}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
		created = true
	} else if name != "" && current != nil && current.Name != name {
		return nil, &EventStreamError{
			Kind:    EventStreamIncompleteToolUse,
			Message: fmt.Sprintf("tool %q was replaced by %q before its stop marker", current.Name, name),
		}
	}
	if current != nil && toolUseID != "" && current.GeneratedID {
		current.ToolUseID = toolUseID
		current.GeneratedID = false
	}
	if current != nil && toolUseID != "" && !current.GeneratedID && current.ToolUseID != "" && current.ToolUseID != toolUseID {
		return nil, &EventStreamError{
			Kind:    EventStreamIncompleteToolUse,
			Message: fmt.Sprintf("tool %q received input for unexpected id %q", current.Name, toolUseID),
		}
	}
	if current == nil && isStop {
		return nil, nil
	}
	if current == nil {
		return nil, &EventStreamError{
			Kind:    EventStreamInvalidPayload,
			Message: "toolUseEvent is missing a tool name",
		}
	}
	if created && current.GeneratedID && callback != nil && callback.OnToolUseActivity != nil {
		callback.OnToolUseActivity()
	}
	if (created || !current.StreamStarted) && !current.GeneratedID {
		startToolUseStream(current, callback)
	}

	if input, ok := event["input"].(string); ok {
		current.InputBuffer.WriteString(input)
		if input != "" && callback != nil && callback.OnToolUseDelta != nil {
			if current.StreamStarted {
				callback.OnToolUseDelta(current.ToolUseID, input)
			}
		}
		if input != "" && !current.StreamStarted && callback != nil && callback.OnToolUseActivity != nil {
			callback.OnToolUseActivity()
		}
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok && len(inputObj) > 0 {
		data, _ := json.Marshal(inputObj)
		current.InputBuffer.Reset()
		current.InputBuffer.Write(data)
		if callback != nil && callback.OnToolUseDelta != nil {
			if current.StreamStarted {
				callback.OnToolUseDelta(current.ToolUseID, string(data))
			}
		}
		if !current.StreamStarted && callback != nil && callback.OnToolUseActivity != nil {
			callback.OnToolUseActivity()
		}
	}

	if isStop && current != nil {
		if err := finishToolUse(current, callback); err != nil {
			return nil, err
		}
		return nil, nil
	}

	return current, nil
}

func startToolUseStream(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.StreamStarted {
		return
	}
	state.StreamStarted = true
	if callback != nil && callback.OnToolUseStart != nil {
		callback.OnToolUseStart(state.ToolUseID, state.Name)
	}
	if state.InputBuffer.Len() > 0 && callback != nil && callback.OnToolUseDelta != nil {
		callback.OnToolUseDelta(state.ToolUseID, state.InputBuffer.String())
	}
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) error {
	if state == nil || state.Name == "" {
		return nil
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	startToolUseStream(state, callback)
	if callback != nil && callback.OnToolUseStop != nil {
		callback.OnToolUseStop(state.ToolUseID)
	}
	input := make(map[string]interface{})
	if state.InputBuffer.Len() > 0 {
		rawArguments := state.InputBuffer.String()
		if err := json.Unmarshal([]byte(rawArguments), &input); err != nil {
			input = map[string]interface{}{"_raw_arguments": rawArguments}
		}
	}
	if callback != nil && callback.OnToolUse != nil {
		callback.OnToolUse(KiroToolUse{
			ToolUseID: state.ToolUseID,
			Name:      state.Name,
			Input:     input,
		})
	}
	return nil
}

func recoverCompleteToolUseWithoutStop(state *toolUseState, callback *KiroStreamCallback, streamErr error) bool {
	if state == nil || strings.TrimSpace(state.Name) == "" {
		return false
	}
	rawArguments := strings.TrimSpace(state.InputBuffer.String())
	if rawArguments == "" {
		return false
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(rawArguments), &input); err != nil || input == nil {
		return false
	}
	if err := finishToolUse(state, callback); err != nil {
		return false
	}
	reason := "clean EOF"
	if streamErr != nil && streamErr != io.EOF {
		reason = streamErr.Error()
	}
	logger.Warnf("[KiroAPI] Recovered complete tool use %q without stop marker (%d argument bytes, %s)", state.Name, len(rawArguments), reason)
	return true
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		switch value := m[key].(type) {
		case bool:
			return value
		case string:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err == nil {
				return parsed
			}
			return strings.TrimSpace(value) == "1"
		case float64:
			return value != 0
		case json.Number:
			parsed, err := value.Int64()
			return err == nil && parsed != 0
		}
	}
	return false
}

func firstPresentField(m map[string]interface{}, keys ...string) (interface{}, bool) {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value, true
		}
	}
	return nil, false
}
