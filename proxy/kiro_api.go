package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/internal/awsregion"
	"kiro-go/internal/httpbody"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	kiroRestAPIBase               = "https://codewhisperer.us-east-1.amazonaws.com"
	kiroManagementAPIBase         = "https://management.us-east-1.kiro.dev"
	profileArnUnsupportedCooldown = 24 * time.Hour
	kiroAPIKeyProbeTimeout        = 20 * time.Second
)

var profileArnResolutionCooldowns sync.Map
var discoveredModelTokenLimits sync.Map

const (
	maxModelListPages   = 20
	maxProfileListPages = 20
	profileListPageSize = 50
)

type ModelTokenLimits struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

func rememberDiscoveredModelTokenLimits(models []ModelInfo) {
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" || model.TokenLimits == nil {
			continue
		}
		limits := *model.TokenLimits
		if existingValue, ok := discoveredModelTokenLimits.Load(key); ok {
			existing := existingValue.(ModelTokenLimits)
			if existing.MaxInputTokens > limits.MaxInputTokens {
				limits.MaxInputTokens = existing.MaxInputTokens
			}
			if existing.MaxOutputTokens > limits.MaxOutputTokens {
				limits.MaxOutputTokens = existing.MaxOutputTokens
			}
		}
		discoveredModelTokenLimits.Store(key, limits)
	}
}

func getDiscoveredModelTokenLimits(model string) (ModelTokenLimits, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if value, ok := discoveredModelTokenLimits.Load(key); ok {
		return value.(ModelTokenLimits), true
	}
	if strings.HasSuffix(key, "-thinking") {
		if value, ok := discoveredModelTokenLimits.Load(strings.TrimSuffix(key, "-thinking")); ok {
			return value.(ModelTokenLimits), true
		}
	}
	return ModelTokenLimits{}, false
}

func regionFromProfileArn(profileArn string) string {
	parts := strings.SplitN(strings.TrimSpace(profileArn), ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "codewhisperer" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// kiroRegion returns the AWS data-plane region. The profile ARN wins because
// account.Region is the OIDC region and may differ from the Kiro profile region.
func kiroRegion(account *config.Account) string {
	return kiroRegionForProfile(account, "")
}

func kiroRegionForProfile(account *config.Account, profileArn string) string {
	if region := regionFromProfileArn(profileArn); region != "" {
		return region
	}
	if account != nil {
		if region := regionFromProfileArn(account.ProfileArn); region != "" {
			return region
		}
		// API key credentials use Region for the CLI runtime data plane.
		// OAuth credentials use Region for authentication only.
		if isKiroAPIKeyAccount(account) {
			if region := strings.TrimSpace(account.Region); region != "" {
				return region
			}
		}
	}
	return "us-east-1"
}

func regionalizeURL(rawURL string, account *config.Account) string {
	return regionalizeURLForProfile(rawURL, account, "")
}

func regionalizeURLForProfile(rawURL string, account *config.Account, profileArn string) string {
	return regionalizeURLForRegion(rawURL, kiroRegionForProfile(account, profileArn))
}

func regionalizeURLForRegion(rawURL, region string) string {
	var err error
	region, err = awsregion.Normalize(region)
	if err != nil {
		return rawURL
	}
	if region == "us-east-1" {
		return rawURL
	}
	regionalHost := "q." + region + ".amazonaws.com"
	return strings.NewReplacer(
		"q.us-east-1.amazonaws.com", regionalHost,
		"codewhisperer.us-east-1.amazonaws.com", regionalHost,
		"runtime.us-east-1.kiro.dev", "runtime."+region+".kiro.dev",
		"management.us-east-1.kiro.dev", "management."+region+".kiro.dev",
	).Replace(rawURL)
}

var defaultKiroProfileRegions = []string{"us-east-1", "eu-central-1"}

func kiroAPIKeyCandidateRegions() []string {
	configured := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS"))
	if configured == "" {
		return append([]string(nil), defaultKiroProfileRegions...)
	}
	seen := make(map[string]bool)
	regions := make([]string, 0)
	for _, region := range strings.Split(configured, ",") {
		region = strings.TrimSpace(region)
		if !validKiroRegion(region) || seen[region] {
			continue
		}
		seen[region] = true
		regions = append(regions, region)
	}
	if len(regions) == 0 {
		return append([]string(nil), defaultKiroProfileRegions...)
	}
	return regions
}

func validKiroRegion(region string) bool {
	return awsregion.Valid(region)
}

var probeKiroAPIKeyRegion = func(ctx context.Context, key, region, proxyURL string) (*config.AccountInfo, error) {
	account := &config.Account{
		AccessToken: key,
		KiroApiKey:  key,
		AuthMethod:  "api_key",
		Region:      region,
		MachineId:   config.GenerateMachineId(),
		ProxyURL:    proxyURL,
	}
	return RefreshAccountInfoContext(ctx, account)
}

type apiKeyRegionProbeCall struct {
	done   chan struct{}
	region string
	info   *config.AccountInfo
	ok     bool
}

var apiKeyRegionProbes = struct {
	sync.Mutex
	inFlight map[string]*apiKeyRegionProbeCall
}{inFlight: make(map[string]*apiKeyRegionProbeCall)}

func apiKeyRegionProbeKey(account *config.Account) string {
	if account == nil {
		return ""
	}
	if id := strings.TrimSpace(account.ID); id != "" {
		return id
	}
	if fingerprint := strings.TrimSpace(account.CredentialFingerprint); fingerprint != "" {
		return fingerprint
	}
	return strings.Join([]string{strings.TrimSpace(account.Email), strings.TrimSpace(account.MachineId)}, "|")
}

// reprobeKiroAPIKeyRegion finds a management-plane candidate without persisting
// it. A successful model request is required before account routing is updated.
func reprobeKiroAPIKeyRegion(ctx context.Context, account *config.Account) (string, *config.AccountInfo, bool) {
	if account == nil || !isKiroAPIKeyAccount(account) {
		return "", nil, false
	}
	key := strings.TrimSpace(account.KiroApiKey)
	if key == "" {
		key = strings.TrimSpace(account.AccessToken)
	}
	if key == "" {
		return "", nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	current := strings.ToLower(strings.TrimSpace(account.Region))
	for _, persisted := range config.GetAccounts() {
		region := strings.ToLower(strings.TrimSpace(persisted.Region))
		if persisted.ID == account.ID && region != current && validKiroRegion(region) {
			account.Region = region
			return region, nil, true
		}
	}

	probeKey := apiKeyRegionProbeKey(account)
	apiKeyRegionProbes.Lock()
	if call := apiKeyRegionProbes.inFlight[probeKey]; call != nil {
		apiKeyRegionProbes.Unlock()
		select {
		case <-ctx.Done():
			return "", nil, false
		case <-call.done:
			if call.ok {
				account.Region = call.region
			}
			return call.region, call.info, call.ok
		}
	}
	call := &apiKeyRegionProbeCall{done: make(chan struct{})}
	apiKeyRegionProbes.inFlight[probeKey] = call
	apiKeyRegionProbes.Unlock()

	call.region, call.info, call.ok = probeAlternateKiroAPIKeyRegions(ctx, account, key, current)
	apiKeyRegionProbes.Lock()
	delete(apiKeyRegionProbes.inFlight, probeKey)
	close(call.done)
	apiKeyRegionProbes.Unlock()
	if call.ok {
		account.Region = call.region
	}
	return call.region, call.info, call.ok
}

func probeAlternateKiroAPIKeyRegions(ctx context.Context, account *config.Account, key, current string) (string, *config.AccountInfo, bool) {
	for _, candidate := range kiroAPIKeyCandidateRegions() {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == current || !validKiroRegion(candidate) {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, kiroAPIKeyProbeTimeout)
		info, err := probeKiroAPIKeyRegion(probeCtx, key, candidate, ResolveAccountProxyURL(account))
		cancel()
		if err == nil {
			logger.Infof("[KiroAPI] API-key account %s found candidate region %s; awaiting model-request confirmation", accountEmailForLog(account), candidate)
			return candidate, info, true
		}
		if ctx.Err() != nil {
			return "", nil, false
		}
		logger.Debugf("[KiroAPI] API-key region probe failed for %s in %s: %v", accountEmailForLog(account), candidate, err)
	}
	return "", nil, false
}

func isAPIKeyRegionAuthFailure(err error) bool {
	upstreamErr, ok := asUpstreamError(err)
	if !ok {
		return false
	}
	switch upstreamErr.Kind {
	case UpstreamErrorTokenExpired, UpstreamErrorAuthRevoked, UpstreamErrorForbidden:
		return true
	default:
		return false
	}
}

func retryAfterAPIKeyRegionRecovery(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback, upstreamErr error, emitted bool) (bool, error) {
	if emitted || account == nil || payload == nil || !isKiroAPIKeyAccount(account) ||
		!isAPIKeyRegionAuthFailure(upstreamErr) || !payload.takeAPIKeyRegionAttempt(account) {
		return false, nil
	}
	previousRegion := account.Region
	region, info, ok := reprobeKiroAPIKeyRegion(ctx, account)
	if !ok {
		return false, nil
	}

	retryErr := CallKiroAPI(account, payload, callback)
	if retryErr != nil {
		account.Region = previousRegion
		return true, retryErr
	}
	if err := config.UpdateAccountRegion(account.ID, region); err != nil {
		logger.Warnf("[KiroAPI] Failed to persist confirmed API-key region %s for %s: %v", region, accountEmailForLog(account), err)
	} else {
		accountpool.GetPool().UpdateAccountRegion(account.ID, region)
	}
	if info != nil {
		if err := config.UpdateAccountInfo(account.ID, *info); err != nil {
			logger.Debugf("[KiroAPI] Failed to persist account info after region recovery for %s: %v", accountEmailForLog(account), err)
		}
	}
	logger.Infof("[KiroAPI] API-key account %s confirmed data-plane region %s", accountEmailForLog(account), region)
	return true, nil
}

// resolveKiroAPIKeyRegion validates the data-plane region for a ksk_ key. An
// explicit region is checked alone; otherwise the configured candidates are
// tried in order. retryable is true when any failure was transport/upstream
// related rather than an authentication rejection.
func resolveKiroAPIKeyRegion(ctx context.Context, key, explicitRegion string, proxyURLs ...string) (region string, info *config.AccountInfo, retryable bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil, false, fmt.Errorf("kiroApiKey is required")
	}
	proxyURL := ""
	if len(proxyURLs) > 0 {
		proxyURL = strings.TrimSpace(proxyURLs[0])
	}

	regions := kiroAPIKeyCandidateRegions()
	if explicitRegion = strings.TrimSpace(explicitRegion); explicitRegion != "" {
		if !validKiroRegion(explicitRegion) {
			return "", nil, false, fmt.Errorf("invalid Kiro data-plane region")
		}
		regions = []string{explicitRegion}
	}

	errorsByRegion := make([]string, 0, len(regions))
	for _, candidate := range regions {
		probeCtx, cancel := context.WithTimeout(ctx, kiroAPIKeyProbeTimeout)
		probedInfo, probeErr := probeKiroAPIKeyRegion(probeCtx, key, candidate, proxyURL)
		cancel()
		if probeErr == nil {
			return candidate, probedInfo, false, nil
		}
		if !accountpool.IsAuthFailure(probeErr) {
			retryable = true
		}
		errorsByRegion = append(errorsByRegion, candidate+": "+probeErr.Error())
	}
	return "", nil, retryable, fmt.Errorf("kiroApiKey not usable in any probed region (%s)", strings.Join(errorsByRegion, "; "))
}

func kiroProfileRegionCandidates(account *config.Account) []string {
	seen := make(map[string]bool)
	regions := make([]string, 0, len(defaultKiroProfileRegions)+1)
	add := func(region string) {
		region = strings.TrimSpace(region)
		if region == "" || seen[region] {
			return
		}
		seen[region] = true
		regions = append(regions, region)
	}
	if account != nil {
		// The profile ARN is authoritative. account.Region is the authentication
		// region and may not host an Amazon Q Developer profile.
		add(regionFromProfileArn(account.ProfileArn))
	}
	if configured := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); configured != "" {
		for _, region := range strings.Split(configured, ",") {
			add(region)
		}
		return regions
	}
	for _, region := range defaultKiroProfileRegions {
		add(region)
	}
	return regions
}

func isEnterpriseIDCAccount(account *config.Account) bool {
	if account == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(account.Provider), "Enterprise") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(account.AuthMethod), "idc") &&
		strings.TrimSpace(account.StartUrl) != "" &&
		!auth.IsBuilderIDStartURL(account.StartUrl)
}

func kiroManagementRegionCandidates(account *config.Account) []string {
	seen := make(map[string]bool)
	regions := make([]string, 0, len(defaultKiroProfileRegions)+2)
	add := func(region string) {
		region = strings.TrimSpace(region)
		if !validKiroRegion(region) || seen[region] {
			return
		}
		seen[region] = true
		regions = append(regions, region)
	}
	if account != nil {
		// Management APIs follow the authentication region, unlike Kiro data-plane
		// calls which follow the profile ARN region.
		add(account.Region)
	}
	if len(regions) == 0 {
		add(kiroRegion(account))
	}
	if !isEnterpriseIDCAccount(account) {
		return regions
	}
	if configured := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); configured != "" {
		for _, region := range strings.Split(configured, ",") {
			add(region)
		}
	} else {
		for _, region := range defaultKiroProfileRegions {
			add(region)
		}
	}
	return regions
}

func kiroControlPlaneRegionCandidates(account *config.Account) []string {
	seen := make(map[string]bool)
	regions := make([]string, 0, len(defaultKiroProfileRegions)+2)
	add := func(region string) {
		region = strings.TrimSpace(region)
		if !validKiroRegion(region) || seen[region] {
			return
		}
		seen[region] = true
		regions = append(regions, region)
	}
	if account != nil {
		add(regionFromProfileArn(account.ProfileArn))
		add(account.Region)
	}
	if len(regions) == 0 {
		add("us-east-1")
	}
	if isEnterpriseIDCAccount(account) {
		for _, region := range defaultKiroProfileRegions {
			add(region)
		}
	}
	return regions
}

func shouldTryNextManagementRegion(ctx context.Context, account *config.Account, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	// Builder ID/IDC identities can legitimately lack the optional usage
	// permission. Probing every fallback region only repeats the same 403 and
	// makes an otherwise healthy account look like a slow or failed refresh.
	if isUsageLimitsUnavailable(account, err) {
		return false
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		switch upstreamErr.Kind {
		case UpstreamErrorAuthRevoked, UpstreamErrorTokenExpired, UpstreamErrorSuspended,
			UpstreamErrorClientRequest, UpstreamErrorCanceled:
			return false
		default:
			return true
		}
	}
	return true
}

// GetUsageLimits 获取账户使用量和订阅信息
func GetUsageLimits(account *config.Account) (*UsageLimitsResponse, error) {
	return GetUsageLimitsContext(context.Background(), account)
}

func GetUsageLimitsContext(ctx context.Context, account *config.Account) (*UsageLimitsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	regions := kiroManagementRegionCandidates(account)
	var lastErr error
	for index, region := range regions {
		result, err := getUsageLimitsForRegion(ctx, account, region)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if index+1 >= len(regions) || !shouldTryNextManagementRegion(ctx, account, err) {
			break
		}
		logger.Warnf("[GetUsageLimits] region %s failed; trying fallback: %v", region, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no valid Kiro management region")
	}
	return nil, lastErr
}

func getUsageLimitsForRegion(ctx context.Context, account *config.Account, region string) (*UsageLimitsResponse, error) {

	url := fmt.Sprintf("%s/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true", kiroRestAPIBase)
	url = regionalizeURLForRegion(url, region)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	setKiroHeaders(req, account)

	client, err := GetRestClientForAccount(account)
	if err != nil {
		return nil, classifyTransportError("GetUsageLimits", fmt.Errorf("configure outbound proxy: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyTransportError("GetUsageLimits", err)
	}
	defer resp.Body.Close()

	body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if resp.StatusCode != 200 {
		return nil, classifyUpstreamHTTPError(resp.StatusCode, "GetUsageLimits", body)
	}
	if readErr != nil {
		return nil, readErr
	}

	var result UsageLimitsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetUserInfo 获取用户信息
func GetUserInfo(account *config.Account) (*UserInfoResponse, error) {
	return GetUserInfoContext(context.Background(), account)
}

func GetUserInfoContext(ctx context.Context, account *config.Account) (*UserInfoResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	regions := kiroManagementRegionCandidates(account)
	var lastErr error
	for index, region := range regions {
		result, err := getUserInfoForRegion(ctx, account, region)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if index+1 >= len(regions) || !shouldTryNextManagementRegion(ctx, account, err) {
			break
		}
		logger.Warnf("[GetUserInfo] region %s failed; trying fallback: %v", region, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no valid Kiro management region")
	}
	return nil, lastErr
}

func getUserInfoForRegion(ctx context.Context, account *config.Account, region string) (*UserInfoResponse, error) {
	url := regionalizeURLForRegion(fmt.Sprintf("%s/GetUserInfo", kiroRestAPIBase), region)

	payload := `{"origin":"KIRO_IDE"}`
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}

	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	client, err := GetRestClientForAccount(account)
	if err != nil {
		return nil, classifyTransportError("GetUserInfo", fmt.Errorf("configure outbound proxy: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyTransportError("GetUserInfo", err)
	}
	defer resp.Body.Close()

	body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if resp.StatusCode != 200 {
		return nil, classifyUpstreamHTTPError(resp.StatusCode, "GetUserInfo", body)
	}
	if readErr != nil {
		return nil, readErr
	}

	var result UserInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAvailableModels 获取可用模型列表
func ListAvailableModels(account *config.Account) ([]ModelInfo, error) {
	return ListAvailableModelsContext(context.Background(), account)
}

const modelListEndpointRouteModel = "__models__"

var modelListRouteEndpoints = []kiroEndpoint{
	{Key: "management-models", Name: "Kiro Management Models"},
	{Key: "legacy-models", Name: "Legacy Kiro Models"},
}

func ListAvailableModelsContext(ctx context.Context, account *config.Account) ([]ModelInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ensureRestProfileArnContext(ctx, account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}
	endpoints := append([]kiroEndpoint(nil), modelListRouteEndpoints...)
	preferred := preferredEndpointForAccount(account)
	if preferred == "kiro" || preferred == "codewhisperer" || preferred == "amazonq" ||
		(preferred == "auto" && !accountPrefersRuntime(account)) {
		endpoints = moveEndpointFirst(endpoints, "legacy-models")
	}
	if preferred != "" && preferred != "auto" && !config.GetEndpointFallback() {
		if preferred == "runtime" {
			endpoints = endpoints[:1]
		} else {
			endpoints = []kiroEndpoint{modelListRouteEndpoints[1]}
		}
	}
	accountID := ""
	if account != nil {
		accountID = account.ID
	}
	var err error
	endpoints, err = sharedAccountEndpointRoutes.availableEndpoints(accountID, modelListEndpointRouteModel, "auto", endpoints)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, endpoint := range endpoints {
		var models []ModelInfo
		switch endpoint.Key {
		case "management-models":
			models, lastErr = listAvailableModelsManagementContext(ctx, account)
		default:
			models, lastErr = listAvailableModelsLegacyContext(ctx, account)
		}
		if lastErr == nil {
			sharedAccountEndpointRoutes.recordSuccess(accountID, modelListEndpointRouteModel, endpoint)
			return models, nil
		}
		sharedAccountEndpointRoutes.recordFailure(accountID, modelListEndpointRouteModel, endpoint, lastErr)
		if !shouldRetryControlPlaneEndpoint(lastErr) {
			return nil, lastErr
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no model-list endpoint is available")
}

func shouldRetryControlPlaneEndpoint(err error) bool {
	if err == nil {
		return false
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		switch upstreamErr.Kind {
		case UpstreamErrorCanceled, UpstreamErrorSuspended, UpstreamErrorAuthRevoked, UpstreamErrorTokenExpired:
			return false
		default:
			return true
		}
	}
	return true
}

func listAvailableModelsManagementContext(ctx context.Context, account *config.Account) ([]ModelInfo, error) {
	client, err := GetRestClientForAccount(account)
	if err != nil {
		return nil, classifyTransportError("Kiro Management Models", fmt.Errorf("configure outbound proxy: %w", err))
	}
	var lastErr error
	for _, region := range kiroControlPlaneRegionCandidates(account) {
		models, err := listAvailableModelsManagementRegionContext(ctx, client, account, region)
		if err == nil {
			return models, nil
		}
		lastErr = err
		if !shouldTryNextManagementRegion(ctx, account, err) {
			return nil, err
		}
		logger.Debugf("[ListAvailableModels] Management region %s failed; trying fallback: %v", region, err)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no Kiro Management region is available")
}

func listAvailableModelsManagementRegionContext(ctx context.Context, client *http.Client, account *config.Account, region string) ([]ModelInfo, error) {
	rawURL := regionalizeURLForRegion(kiroManagementAPIBase, region)
	models := make([]ModelInfo, 0, 16)
	nextToken := ""
	seenTokens := make(map[string]struct{})
	for page := 0; page < maxModelListPages; page++ {
		requestBody := map[string]interface{}{"origin": "AI_EDITOR"}
		if account != nil {
			if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
				requestBody["profileArn"] = profileArn
			}
		}
		if nextToken != "" {
			requestBody["nextToken"] = nextToken
		}
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "KiroControlPlaneBearerService.ListAvailableModels")
		applyKiroControlPlaneHeaders(req, account)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, classifyRequestCancellation("Kiro Management Models", ctx.Err())
			}
			return nil, classifyTransportError("Kiro Management Models", err)
		}
		body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, classifyUpstreamHTTPError(resp.StatusCode, "Kiro Management Models", body)
		}
		if readErr != nil {
			return nil, readErr
		}
		pageModels, next, err := decodeAvailableModelsPage(body)
		if err != nil {
			return nil, err
		}
		models = append(models, pageModels...)
		nextToken = strings.TrimSpace(next)
		if nextToken == "" {
			rememberDiscoveredModelMetadata(models)
			return models, nil
		}
		if _, duplicate := seenTokens[nextToken]; duplicate {
			return nil, fmt.Errorf("Kiro Management Models returned repeated nextToken")
		}
		seenTokens[nextToken] = struct{}{}
	}
	return nil, fmt.Errorf("Kiro Management Models exceeded %d pages", maxModelListPages)
}

func listAvailableModelsLegacyContext(ctx context.Context, account *config.Account) ([]ModelInfo, error) {

	models := make([]ModelInfo, 0, 16)
	nextToken := ""
	seenTokens := make(map[string]struct{})
	client, err := GetRestClientForAccount(account)
	if err != nil {
		return nil, classifyTransportError("ListAvailableModels", fmt.Errorf("configure outbound proxy: %w", err))
	}
	for page := 0; page < maxModelListPages; page++ {
		rawURL := regionalizeURL(fmt.Sprintf("%s/ListAvailableModels", kiroRestAPIBase), account)
		parsedURL, err := neturl.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		query := parsedURL.Query()
		query.Set("origin", "AI_EDITOR")
		query.Set("maxResults", "50")
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		parsedURL.RawQuery = query.Encode()
		rawURL = withProfileArnQuery(parsedURL.String(), account)

		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)

		resp, err := client.Do(req)
		if err != nil {
			return nil, classifyTransportError("ListAvailableModels", err)
		}
		body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, classifyUpstreamHTTPError(resp.StatusCode, "ListAvailableModels", body)
		}
		if readErr != nil {
			return nil, readErr
		}

		pageModels, next, err := decodeAvailableModelsPage(body)
		if err != nil {
			return nil, err
		}
		models = append(models, pageModels...)
		nextToken = strings.TrimSpace(next)
		if nextToken == "" {
			rememberDiscoveredModelMetadata(models)
			return models, nil
		}
		if _, duplicate := seenTokens[nextToken]; duplicate {
			return nil, fmt.Errorf("ListAvailableModels returned repeated nextToken")
		}
		seenTokens[nextToken] = struct{}{}
	}
	return nil, fmt.Errorf("ListAvailableModels exceeded %d pages", maxModelListPages)
}

func decodeAvailableModelsPage(body []byte) ([]ModelInfo, string, error) {
	var result struct {
		Models          []ModelInfo `json:"models"`
		AvailableModels []ModelInfo `json:"availableModels"`
		NextToken       string      `json:"nextToken"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, "", err
	}
	models := result.Models
	if len(result.AvailableModels) > 0 {
		models = append(models, result.AvailableModels...)
	}
	normalizeModelInfos(models)
	return models, result.NextToken, nil
}

// ResolveProfileArn returns the account profile ARN, fetching and caching it
// when it is missing. First tries ListAvailableProfiles; if that returns empty,
// falls back to refreshing the token (which returns profileArn in the response).
func ResolveProfileArn(account *config.Account) (string, error) {
	return ResolveProfileArnContext(context.Background(), account)
}

func ResolveProfileArnContext(ctx context.Context, account *config.Account) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return "", fmt.Errorf("account is nil")
	}
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		return profileArn, nil
	}
	if isKiroAPIKeyAccount(account) {
		return "", fmt.Errorf("profile ARN resolution skipped: api_key account uses key-bound profile")
	}

	profileLookupSuppressed := isProfileArnResolutionSuppressed(account)
	var profileUnsupportedErr error
	var profileUnsupported bool

	if !profileLookupSuppressed {
		profileArn, err := resolveProfileArnAcrossRegionsContext(ctx, account)
		if err == nil && profileArn != "" {
			if updateErr := config.UpdateAccountProfileArn(account.ID, profileArn); updateErr != nil {
				logger.Warnf("[ProfileArn] Failed to cache profile ARN for %s: %v", account.Email, updateErr)
			}
			accountpool.GetPool().UpdateProfileArn(account.ID, profileArn)
			account.ProfileArn = profileArn
			return profileArn, nil
		}
		profileUnsupportedErr = err
		profileUnsupported = isBuilderIDProfileUnsupportedError(account, err)
	}

	// Fallback: refresh token to get profileArn from auth response
	if account.RefreshToken != "" {
		refreshErr := sharedTokenRefreshCoordinator.RefreshContext(ctx, account, true)
		if refreshErr == nil && strings.TrimSpace(account.ProfileArn) != "" {
			return strings.TrimSpace(account.ProfileArn), nil
		}
	}
	if profileLookupSuppressed {
		return "", fmt.Errorf("profile ARN resolution skipped: previous Builder ID profile lookup was unsupported")
	}
	if profileUnsupported {
		suppressProfileArnResolution(account)
		logger.Debugf("[ProfileArn] Builder ID profile lookup unsupported for %s: %v", accountEmailForLog(account), profileUnsupportedErr)
		return "", fmt.Errorf("profile ARN unsupported for Builder ID account")
	}

	return "", fmt.Errorf("no available Kiro profile")
}

func isBuilderIDProfileUnsupportedError(account *config.Account, err error) bool {
	if account == nil || err == nil || !strings.EqualFold(strings.TrimSpace(account.Provider), "BuilderId") {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "HTTP 403") && strings.Contains(msg, "AWS Builder ID is not supported for this operation")
}

func profileArnCooldownKey(account *config.Account) string {
	if account == nil {
		return ""
	}
	provider := strings.TrimSpace(account.Provider)
	if id := strings.TrimSpace(account.ID); id != "" {
		return provider + "\x00" + id
	}
	if userID := strings.TrimSpace(account.UserId); userID != "" {
		return provider + "\x00" + userID
	}
	return provider + "\x00" + strings.TrimSpace(account.Email)
}

func suppressProfileArnResolution(account *config.Account) {
	if key := profileArnCooldownKey(account); key != "" {
		profileArnResolutionCooldowns.Store(key, time.Now().Add(profileArnUnsupportedCooldown))
	}
}

func isProfileArnResolutionSuppressed(account *config.Account) bool {
	key := profileArnCooldownKey(account)
	if key == "" {
		return false
	}
	value, ok := profileArnResolutionCooldowns.Load(key)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok || time.Now().After(until) {
		profileArnResolutionCooldowns.Delete(key)
		return false
	}
	return true
}

func isProfileArnResolutionSkippedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN resolution skipped")
}

func isProfileArnResolutionUnsupportedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN unsupported for Builder ID account")
}

func isProfileArnResolutionSoftError(err error) bool {
	return isProfileArnResolutionSkippedError(err) || isProfileArnResolutionUnsupportedError(err)
}

func ensureRestProfileArn(account *config.Account) error {
	return ensureRestProfileArnContext(context.Background(), account)
}

func ensureRestProfileArnContext(ctx context.Context, account *config.Account) error {
	if account == nil || strings.TrimSpace(account.ProfileArn) != "" {
		return nil
	}
	profileArn, err := ResolveProfileArnContext(ctx, account)
	if err != nil {
		if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Continuing REST request without profile ARN for %s: %v", accountEmailForLog(account), err)
			return nil
		}
		return err
	}
	account.ProfileArn = profileArn
	return nil
}

func listAvailableProfilesWithRetry(account *config.Account) (string, error) {
	return listAvailableProfilesWithRetryContext(context.Background(), account)
}

func listAvailableProfilesWithRetryContext(ctx context.Context, account *config.Account) (string, error) {
	return listAvailableProfilesWithRetryInRegionContext(ctx, account, kiroRegion(account))
}

func resolveProfileArnAcrossRegionsContext(ctx context.Context, account *config.Account) (string, error) {
	var lastErr error
	for _, region := range kiroProfileRegionCandidates(account) {
		profileArn, err := listAvailableProfilesWithRetryInRegionContext(ctx, account, region)
		if err == nil && strings.TrimSpace(profileArn) != "" {
			return profileArn, nil
		}
		if err != nil {
			lastErr = err
			if isBuilderIDProfileUnsupportedError(account, err) {
				return "", err
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("empty profile list")
	}
	return "", lastErr
}

func listAvailableProfilesWithRetryInRegionContext(ctx context.Context, account *config.Account, region string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const maxAttempts = 3
	backoff := 200 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		profileArn, err := listAvailableProfilesInRegionContext(ctx, account, region)
		if err == nil {
			return profileArn, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return "", err
		}
		logger.Debugf("[ProfileArn] ListAvailableProfiles transient failure for %s in %s (attempt %d/%d): %v",
			accountEmailForLog(account), region, attempt, maxAttempts, err)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", ctx.Err()
		}
		backoff *= 2
	}
	return "", lastErr
}

// isTransientProfileFetchError reports whether a ListAvailableProfiles error
// is worth retrying. Network errors and upstream 5xx/429 are transient; other
// HTTP errors and an empty profile list are not.
func isTransientProfileFetchError(err error) bool {
	if err == nil {
		return false
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		switch upstreamErr.Kind {
		case UpstreamErrorTransient, UpstreamErrorRateLimit, UpstreamErrorEndpointUnavailable, UpstreamErrorFirstTokenTimeout:
			return true
		default:
			return false
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "empty profile list") {
		return false
	}
	if strings.HasPrefix(msg, "HTTP ") {
		return strings.HasPrefix(msg, "HTTP 5") || strings.HasPrefix(msg, "HTTP 429")
	}
	// Non-HTTP errors are network/transport level — retry.
	return true
}

func listAvailableProfiles(account *config.Account) (string, error) {
	return listAvailableProfilesContext(context.Background(), account)
}

func listAvailableProfilesContext(ctx context.Context, account *config.Account) (string, error) {
	return listAvailableProfilesInRegionContext(ctx, account, kiroRegion(account))
}

func listAvailableProfilesInRegionContext(ctx context.Context, account *config.Account, region string) (string, error) {
	profiles, err := listProfileArnsInRegionContext(ctx, account, region)
	if err != nil {
		return "", err
	}
	if len(profiles) == 0 {
		return "", fmt.Errorf("empty profile list")
	}
	return profiles[0], nil
}

func listProfileArnsInRegionContext(ctx context.Context, account *config.Account, region string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := regionalizeURLForRegion(fmt.Sprintf("%s/ListAvailableProfiles", kiroRestAPIBase), region)
	client, err := GetRestClientForAccount(account)
	if err != nil {
		return nil, classifyTransportError("ListAvailableProfiles", fmt.Errorf("configure outbound proxy: %w", err))
	}

	profiles := make([]string, 0)
	seenProfiles := make(map[string]bool)
	seenTokens := make(map[string]bool)
	nextToken := ""
	for page := 0; page < maxProfileListPages; page++ {
		requestBody := map[string]interface{}{"maxResults": profileListPageSize}
		if nextToken != "" {
			requestBody["nextToken"] = nextToken
		}
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, classifyTransportError("ListAvailableProfiles", err)
		}
		body, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, classifyUpstreamHTTPError(resp.StatusCode, "ListAvailableProfiles", body)
		}
		if readErr != nil {
			return nil, readErr
		}

		var result struct {
			Profiles []struct {
				Arn string `json:"arn"`
			} `json:"profiles"`
			NextToken string `json:"nextToken"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, err
		}
		for _, profile := range result.Profiles {
			profileArn := strings.TrimSpace(profile.Arn)
			if profileArn == "" || seenProfiles[profileArn] {
				continue
			}
			seenProfiles[profileArn] = true
			profiles = append(profiles, profileArn)
		}

		next := strings.TrimSpace(result.NextToken)
		if next == "" {
			return profiles, nil
		}
		if seenTokens[next] {
			return nil, fmt.Errorf("ListAvailableProfiles returned repeated nextToken")
		}
		seenTokens[next] = true
		nextToken = next
	}
	return nil, fmt.Errorf("ListAvailableProfiles exceeded %d pages", maxProfileListPages)
}

func listProfileArnsWithRetryInRegionContext(ctx context.Context, account *config.Account, region string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const maxAttempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		profiles, err := listProfileArnsInRegionContext(ctx, account, region)
		if err == nil {
			return profiles, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return nil, err
		}
		logger.Debugf("[ProfileArn] Profile discovery transient failure for %s in %s (attempt %d/%d): %v",
			accountEmailForLog(account), region, attempt, maxAttempts, err)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		}
		backoff *= 2
	}
	return nil, lastErr
}

type KiroProfile struct {
	Arn    string `json:"arn"`
	Region string `json:"region"`
}

func DiscoverKiroProfiles(account *config.Account) ([]KiroProfile, error) {
	return DiscoverKiroProfilesContext(context.Background(), account)
}

func DiscoverKiroProfilesContext(ctx context.Context, account *config.Account) ([]KiroProfile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	profiles := make([]KiroProfile, 0)
	seen := make(map[string]bool)
	var lastErr error
	for _, region := range kiroProfileRegionCandidates(account) {
		arns, err := listProfileArnsWithRetryInRegionContext(ctx, account, region)
		if err != nil {
			if isBuilderIDProfileUnsupportedError(account, err) {
				return nil, err
			}
			lastErr = err
			logger.Warnf("[ProfileArn] Profile discovery failed in %s for %s: %v", region, accountEmailForLog(account), err)
			continue
		}
		for _, arn := range arns {
			if seen[arn] {
				continue
			}
			seen[arn] = true
			profileRegion := regionFromProfileArn(arn)
			if profileRegion == "" {
				profileRegion = region
			}
			profiles = append(profiles, KiroProfile{Arn: arn, Region: profileRegion})
		}
	}
	if len(profiles) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return profiles, nil
}

func withProfileArnQuery(rawURL string, account *config.Account) string {
	if account == nil {
		return rawURL
	}
	profileArn := strings.TrimSpace(account.ProfileArn)
	if profileArn == "" {
		return rawURL
	}
	return rawURL + "&profileArn=" + neturl.QueryEscape(profileArn)
}

func setKiroHeaders(req *http.Request, account *config.Account) {
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	headerValues := buildRuntimeHeaderValues(account, host)

	req.Header.Set("Accept", "application/json")
	applyKiroBaseHeaders(req, account, headerValues)
}

// RefreshAccountInfo 刷新账户信息（使用量、订阅等）
func RefreshAccountInfo(account *config.Account) (*config.AccountInfo, error) {
	return RefreshAccountInfoContext(context.Background(), account)
}

func copyAccountStatus(dst, src *config.Account) {
	if dst == nil || src == nil {
		return
	}
	dst.Enabled = src.Enabled
	dst.BanStatus = src.BanStatus
	dst.BanReason = src.BanReason
	dst.BanTime = src.BanTime
}

func RefreshAccountInfoContext(ctx context.Context, account *config.Account) (*config.AccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	info := &config.AccountInfo{
		LastRefresh: time.Now().Unix(),
	}

	// 获取使用量和订阅信息
	usage, err := GetUsageLimitsContext(ctx, account)
	if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.RefreshToken && account.RefreshToken != "" {
		if refreshErr := sharedTokenRefreshCoordinator.RefreshContext(ctx, account, true); refreshErr == nil {
			usage, err = GetUsageLimitsContext(ctx, account)
		} else {
			err = classifyRefreshFailure("GetUsageLimits", refreshErr)
		}
	}
	if err != nil {
		if isUsageLimitsUnavailable(account, err) {
			// Builder ID accounts can still serve generation requests while the
			// optional usage endpoint rejects them. Preserve the token and mark
			// only metadata as unavailable; callers must not cool or disable the
			// account because of this control-plane limitation.
			info.MetadataUnavailable = true
			logger.Debugf("[RefreshAccountInfo] Usage metadata unavailable for %s: %v", accountEmailForLog(account), err)
			return info, &accountInfoUnavailableError{cause: err}
		}
		if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.Kind == UpstreamErrorSuspended {
			// 账户被暂时封禁，自动禁用并标记封禁状态
			logger.Warnf("[RefreshAccountInfo] Account %s is temporarily suspended: %v", account.Email, err)

			enabled := false
			banStatus := "BANNED"
			banReason := "AWS temporarily suspended - unusual user activity detected"
			banTime := time.Now().Unix()
			updatedAccount, updateErr := config.PatchAccountStatus(account.ID, config.AccountStatusPatch{
				Enabled: &enabled, BanStatus: &banStatus, BanReason: &banReason, BanTime: &banTime,
			})
			if updateErr != nil {
				logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", updateErr)
			} else {
				copyAccountStatus(account, &updatedAccount)
			}

			return nil, fmt.Errorf("Account suspended: %w", err)
		}
		if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.Kind == UpstreamErrorAuthRevoked {
			logger.Warnf("[RefreshAccountInfo] Revoked credentials for %s: %v", account.Email, err)
			enabled := false
			banStatus := "BANNED"
			banReason := "Authentication credentials were revoked"
			banTime := time.Now().Unix()
			updatedAccount, updateErr := config.PatchAccountStatus(account.ID, config.AccountStatusPatch{
				Enabled: &enabled, BanStatus: &banStatus, BanReason: &banReason, BanTime: &banTime,
			})
			if updateErr != nil {
				logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", updateErr)
			} else {
				copyAccountStatus(account, &updatedAccount)
			}
		}

		return nil, fmt.Errorf("GetUsageLimits: %w", err)
	}

	// 如果成功获取信息，清除封禁状态（如果之前被标记）
	if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
		logger.Infof("[RefreshAccountInfo] Account %s is now active, clearing ban status", account.Email)

		updatedAccount, updateErr := config.ClearAccountBan(account.ID,
			"temporarily suspended", "credentials were revoked", "authentication failed")
		if updateErr != nil {
			logger.Errorf("[RefreshAccountInfo] Failed to clear account ban status: %v", updateErr)
		} else {
			copyAccountStatus(account, &updatedAccount)
		}
	}

	// 解析用户信息
	if usage.UserInfo != nil {
		info.Email = usage.UserInfo.Email
		info.UserId = usage.UserInfo.UserId
	}
	if isEnterpriseIDCAccount(account) && (strings.TrimSpace(info.Email) == "" || strings.TrimSpace(info.UserId) == "") {
		if userInfo, userErr := GetUserInfoContext(ctx, account); userErr == nil {
			if info.Email == "" {
				info.Email = userInfo.Email
			}
			if info.UserId == "" {
				info.UserId = userInfo.UserId
			}
		} else {
			logger.Debugf("[RefreshAccountInfo] GetUserInfo fallback failed for %s: %v", account.Email, userErr)
		}
	}

	// 解析订阅信息
	if usage.SubscriptionInfo != nil {
		// 优先从 SubscriptionTitle 或 SubscriptionName 解析类型
		titleOrName := usage.SubscriptionInfo.SubscriptionTitle
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionName
		}
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionType
		}
		if strings.TrimSpace(titleOrName) != "" {
			info.SubscriptionType = parseSubscriptionType(titleOrName)
			info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionTitle
			if info.SubscriptionTitle == "" {
				info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionName
			}
		}
		logger.Debugf("[RefreshAccountInfo] Subscription: type=%s, title=%s, name=%s, parsed=%s",
			usage.SubscriptionInfo.SubscriptionType,
			usage.SubscriptionInfo.SubscriptionTitle,
			usage.SubscriptionInfo.SubscriptionName,
			info.SubscriptionType)
	}

	// 解析使用量
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		info.UsageCurrent = breakdown.CurrentUsage
		info.UsageLimit = breakdown.UsageLimit
		if info.UsageLimit > 0 {
			info.UsagePercent = info.UsageCurrent / info.UsageLimit
		}
	}

	// 解析重置日期
	if usage.NextDateReset != "" {
		if ts, err := usage.NextDateReset.Int64(); err == nil && ts > 0 {
			info.NextResetDate = time.Unix(ts, 0).Format("2006-01-02")
		} else if f, err := usage.NextDateReset.Float64(); err == nil && f > 0 {
			info.NextResetDate = time.Unix(int64(f), 0).Format("2006-01-02")
		}
	}

	// 解析试用配额信息
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		if breakdown.FreeTrialInfo != nil {
			info.TrialUsageCurrent = breakdown.FreeTrialInfo.CurrentUsage
			info.TrialUsageLimit = breakdown.FreeTrialInfo.UsageLimit
			if info.TrialUsageLimit > 0 {
				info.TrialUsagePercent = info.TrialUsageCurrent / info.TrialUsageLimit
			}
			info.TrialStatus = breakdown.FreeTrialInfo.FreeTrialStatus

			// 解析试用到期时间
			if breakdown.FreeTrialInfo.FreeTrialExpiry != "" {
				if ts, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Int64(); err == nil && ts > 0 {
					info.TrialExpiresAt = ts
				} else if f, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Float64(); err == nil && f > 0 {
					info.TrialExpiresAt = int64(f)
				}
			}
		}
	}

	return info, nil
}

func parseSubscriptionType(raw string) string {
	upper := strings.ToUpper(raw)
	if strings.Contains(upper, "PRO_PLUS") || strings.Contains(upper, "PROPLUS") {
		return "PRO_PLUS"
	}
	if strings.Contains(upper, "POWER") {
		return "POWER"
	}
	if strings.Contains(upper, "PRO") {
		return "PRO"
	}
	return "FREE"
}

// 响应结构体
type UsageLimitsResponse struct {
	UsageBreakdownList []UsageBreakdown  `json:"usageBreakdownList"`
	NextDateReset      json.Number       `json:"nextDateReset"`
	SubscriptionInfo   *SubscriptionInfo `json:"subscriptionInfo"`
	UserInfo           *UserInfo         `json:"userInfo"`
}

type UsageBreakdown struct {
	ResourceType  string         `json:"resourceType"`
	CurrentUsage  float64        `json:"currentUsage"`
	UsageLimit    float64        `json:"usageLimit"`
	Currency      string         `json:"currency"`
	Unit          string         `json:"unit"`
	OverageRate   float64        `json:"overageRate"`
	FreeTrialInfo *FreeTrialInfo `json:"freeTrialInfo"`
	Bonuses       []BonusInfo    `json:"bonuses"`
}

type FreeTrialInfo struct {
	CurrentUsage    float64     `json:"currentUsage"`
	UsageLimit      float64     `json:"usageLimit"`
	FreeTrialStatus string      `json:"freeTrialStatus"`
	FreeTrialExpiry json.Number `json:"freeTrialExpiry"`
}

type BonusInfo struct {
	BonusCode    string      `json:"bonusCode"`
	DisplayName  string      `json:"displayName"`
	CurrentUsage float64     `json:"currentUsage"`
	UsageLimit   float64     `json:"usageLimit"`
	ExpiresAt    json.Number `json:"expiresAt"`
	Status       string      `json:"status"`
}

type SubscriptionInfo struct {
	SubscriptionName  string `json:"subscriptionName"`
	SubscriptionTitle string `json:"subscriptionTitle"`
	SubscriptionType  string `json:"subscriptionType"`
	Status            string `json:"status"`
	UpgradeCapability string `json:"upgradeCapability"`
}

type UserInfo struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
}

type UserInfoResponse struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
	Idp    string `json:"idp"`
	Status string `json:"status"`
}

type ModelInfo struct {
	ModelId                            string                 `json:"modelId"`
	ModelName                          string                 `json:"modelName"`
	Description                        string                 `json:"description"`
	Provider                           string                 `json:"provider,omitempty"`
	Capabilities                       []string               `json:"capabilities,omitempty"`
	ContextWindow                      int                    `json:"contextWindow,omitempty"`
	IsDefault                          bool                   `json:"isDefault,omitempty"`
	InputTypes                         []string               `json:"supportedInputTypes"`
	RateMultiplier                     float64                `json:"rateMultiplier"`
	RateUnit                           string                 `json:"rateUnit,omitempty"`
	PromptCaching                      *ModelPromptCaching    `json:"promptCaching,omitempty"`
	TokenLimits                        *ModelTokenLimits      `json:"tokenLimits"`
	AdditionalModelRequestFieldsSchema map[string]interface{} `json:"additionalModelRequestFieldsSchema,omitempty"`
	EffortLevels                       []string               `json:"effortLevels,omitempty"`
	EffortSchemaPath                   string                 `json:"effortSchemaPath,omitempty"`
}

type ModelPromptCaching struct {
	MaximumCacheCheckpointsPerRequest int   `json:"maximumCacheCheckpointsPerRequest,omitempty"`
	MinimumTokensPerCacheCheckpoint   int   `json:"minimumTokensPerCacheCheckpoint,omitempty"`
	SupportsPromptCaching             *bool `json:"supportsPromptCaching,omitempty"`
}
