package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	statusPass = "PASS"
	statusWarn = "WARN"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

type options struct {
	baseURL                  string
	model                    string
	thinkingModel            string
	modelsCSV                string
	models                   []string
	allModels                bool
	suite                    string
	timeout                  time.Duration
	concurrency              int
	requests                 int
	concurrencyCSV           string
	concurrencySteps         []int
	allowHighLoad            bool
	loadProfile              string
	loadPattern              string
	loadMaxTokens            int
	warmupRequests           int
	targetRPS                float64
	rampDuration             time.Duration
	postLoadRecovery         bool
	staircaseHold            time.Duration
	staircaseCooldown        time.Duration
	staircaseMaxRequests     int
	resourceSampleInterval   time.Duration
	collectServerStats       bool
	requireServerStats       bool
	soakDuration             time.Duration
	soakMaxRequests          int
	soakTokenBudget          int
	minSuccessRate           float64
	maxP95Millis             int64
	maxTTFTP95Millis         int64
	maxStreamGapMillis       int64
	maxClientOverloadRate    float64
	maxClientGoroutineGrowth int
	maxClientHeapGrowthMB    int64
	baselinePath             string
	baselineTolerancePercent float64
	webSearch                bool
	cancellation             bool
	scenariosCSV             string
	scenarioFilter           map[string]struct{}
	listScenarios            bool
	reportPath               string
	failOnWarning            bool
	allowRemote              bool
}

type scenarioResult struct {
	Name                      string         `json:"name"`
	Status                    string         `json:"status"`
	Protocol                  string         `json:"protocol,omitempty"`
	Model                     string         `json:"model,omitempty"`
	Stream                    bool           `json:"stream,omitempty"`
	HTTPStatus                int            `json:"httpStatus,omitempty"`
	RequestID                 string         `json:"requestId,omitempty"`
	RequestIDs                []string       `json:"requestIds,omitempty"`
	StopReason                string         `json:"stopReason,omitempty"`
	ResponseHeaderMS          int64          `json:"responseHeaderMillis,omitempty"`
	FirstEventMillis          int64          `json:"firstEventMillis,omitempty"`
	TTFTMillis                int64          `json:"ttftMillis,omitempty"`
	FirstTextMillis           int64          `json:"firstTextMillis,omitempty"`
	FirstThinkMillis          int64          `json:"firstThinkingMillis,omitempty"`
	FirstToolMillis           int64          `json:"firstToolMillis,omitempty"`
	MaxStreamGapMS            int64          `json:"maxStreamGapMillis,omitempty"`
	MaxWireGapMS              int64          `json:"maxWireGapMillis,omitempty"`
	TotalMillis               int64          `json:"totalMillis,omitempty"`
	HealthCheckMillis         int64          `json:"healthCheckMillis,omitempty"`
	P50Millis                 int64          `json:"p50Millis,omitempty"`
	P95Millis                 int64          `json:"p95Millis,omitempty"`
	P99Millis                 int64          `json:"p99Millis,omitempty"`
	SuccessP50Millis          int64          `json:"successP50Millis,omitempty"`
	SuccessP95Millis          int64          `json:"successP95Millis,omitempty"`
	SuccessP99Millis          int64          `json:"successP99Millis,omitempty"`
	FailureP50Millis          int64          `json:"failureP50Millis,omitempty"`
	FailureP95Millis          int64          `json:"failureP95Millis,omitempty"`
	FailureP99Millis          int64          `json:"failureP99Millis,omitempty"`
	HeaderP95Millis           int64          `json:"responseHeaderP95Millis,omitempty"`
	TTFTP50Millis             int64          `json:"ttftP50Millis,omitempty"`
	TTFTP95Millis             int64          `json:"ttftP95Millis,omitempty"`
	TTFTP99Millis             int64          `json:"ttftP99Millis,omitempty"`
	QueueP95Millis            int64          `json:"queueP95Millis,omitempty"`
	StreamGapP95MS            int64          `json:"streamGapP95Millis,omitempty"`
	WireGapP95MS              int64          `json:"wireGapP95Millis,omitempty"`
	ScheduleMillis            int64          `json:"scheduleMillis,omitempty"`
	WallMillis                int64          `json:"wallMillis,omitempty"`
	SampleCount               int            `json:"sampleCount,omitempty"`
	CompletedRequests         int            `json:"completedRequests,omitempty"`
	SuccessRate               float64        `json:"successRate,omitempty"`
	CompletionRate            float64        `json:"completionRate,omitempty"`
	ClientOverloadRate        float64        `json:"clientOverloadRate,omitempty"`
	ThresholdFailures         []string       `json:"thresholdFailures,omitempty"`
	TargetRPS                 float64        `json:"targetRps,omitempty"`
	ArrivalRPS                float64        `json:"arrivalRps,omitempty"`
	AchievedRPS               float64        `json:"achievedRps,omitempty"`
	SuccessRPS                float64        `json:"successRps,omitempty"`
	ClientGoroutineDelta      int            `json:"clientGoroutineDelta,omitempty"`
	ClientHeapDeltaBytes      int64          `json:"clientHeapAllocDeltaBytes,omitempty"`
	ClientResourceSamples     int            `json:"clientResourceSamples,omitempty"`
	ClientPeakGoroutines      int            `json:"clientPeakGoroutines,omitempty"`
	ClientPeakHeapAllocBytes  int64          `json:"clientPeakHeapAllocBytes,omitempty"`
	ClientPeakGoroutineGrowth int            `json:"clientPeakGoroutineGrowth,omitempty"`
	ClientPeakHeapGrowthBytes int64          `json:"clientPeakHeapGrowthBytes,omitempty"`
	ServerStatsSamples        int            `json:"serverStatsSamples,omitempty"`
	ServerStatsRequestsDelta  int64          `json:"serverStatsRequestsDelta,omitempty"`
	ServerStatsTokensDelta    int64          `json:"serverStatsTokensDelta,omitempty"`
	ServerStatsErrors         int            `json:"serverStatsErrors,omitempty"`
	ServerStatsCounterReset   bool           `json:"serverStatsCounterReset,omitempty"`
	Events                    int            `json:"events,omitempty"`
	Heartbeats                int            `json:"heartbeats,omitempty"`
	ContentDeltas             int            `json:"contentDeltas,omitempty"`
	ThinkingDeltas            int            `json:"thinkingDeltas,omitempty"`
	ToolCalls                 int            `json:"toolCalls,omitempty"`
	InputTokens               int            `json:"inputTokens,omitempty"`
	OutputTokens              int            `json:"outputTokens,omitempty"`
	ReasoningTokens           int            `json:"reasoningTokens,omitempty"`
	CacheReadTokens           int            `json:"cacheReadTokens,omitempty"`
	CacheCreateTokens         int            `json:"cacheCreationTokens,omitempty"`
	DistinctRequestIDs        int            `json:"distinctRequestIds,omitempty"`
	Requests                  int            `json:"requests,omitempty"`
	ScheduledRequests         int            `json:"scheduledRequests,omitempty"`
	DroppedRequests           int            `json:"droppedRequests,omitempty"`
	WarmupRequests            int            `json:"warmupRequests,omitempty"`
	Successes                 int            `json:"successes,omitempty"`
	FailureCategories         map[string]int `json:"failureCategories,omitempty"`
	WorkloadSuccesses         map[string]int `json:"workloadSuccesses,omitempty"`
	WorkloadFailures          map[string]int `json:"workloadFailures,omitempty"`
	EndpointCounts            map[string]int `json:"endpointCounts,omitempty"`
	CorrelatedRequests        int            `json:"correlatedRequests,omitempty"`
	AccountAttempts           int            `json:"accountAttempts,omitempty"`
	AffinityHits              int            `json:"affinityHits,omitempty"`
	CacheHits                 int            `json:"cacheHits,omitempty"`
	ToolUses                  int            `json:"toolUses,omitempty"`
	SelectionP95MS            int64          `json:"accountSelectionP95Millis,omitempty"`
	Detail                    string         `json:"detail,omitempty"`
}

type devReport struct {
	GeneratedAt              string           `json:"generatedAt"`
	BaseURL                  string           `json:"baseUrl"`
	ServerVersion            string           `json:"serverVersion,omitempty"`
	ConfigurationFingerprint string           `json:"configurationFingerprint"`
	Suite                    string           `json:"suite"`
	LoadProfile              string           `json:"loadProfile,omitempty"`
	LoadPattern              string           `json:"loadPattern,omitempty"`
	LoadMaxTokens            int              `json:"loadMaxTokens,omitempty"`
	Concurrency              int              `json:"concurrency,omitempty"`
	Requests                 int              `json:"requests,omitempty"`
	TargetRPS                float64          `json:"targetRps,omitempty"`
	RampMillis               int64            `json:"rampMillis,omitempty"`
	WarmupRequests           int              `json:"warmupRequests,omitempty"`
	ConcurrencyLevels        []int            `json:"concurrencyLevels,omitempty"`
	StaircaseHoldMillis      int64            `json:"staircaseHoldMillis,omitempty"`
	StaircaseCooldownMillis  int64            `json:"staircaseCooldownMillis,omitempty"`
	StaircaseMaxRequests     int              `json:"staircaseMaxRequests,omitempty"`
	SoakMillis               int64            `json:"soakMillis,omitempty"`
	SoakMaxRequests          int              `json:"soakMaxRequests,omitempty"`
	SoakTokenBudget          int              `json:"soakTokenBudget,omitempty"`
	Model                    string           `json:"model,omitempty"`
	Models                   []string         `json:"models,omitempty"`
	Results                  []scenarioResult `json:"results"`
	Summary                  map[string]int   `json:"summary"`
	BaselineCompared         bool             `json:"baselineCompared,omitempty"`
	BaselineRegressions      int              `json:"baselineRegressions,omitempty"`
	BaselineMissing          int              `json:"baselineMissing,omitempty"`
}

type runner struct {
	opts                options
	apiKey              string
	client              *http.Client
	results             []scenarioResult
	models              []string
	selected            []string
	model               string
	thinking            string
	serverVersion       string
	startedAt           time.Time
	userAgent           string
	baselineCompared    bool
	baselineRegressions int
	baselineMissing     int
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, err := parseOptions(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if opts.listScenarios {
		printScenarioCatalog()
		return 0
	}
	apiKey := strings.TrimSpace(os.Getenv("KIRO_DEV_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "KIRO_DEV_API_KEY is required; credentials are accepted only through the environment")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &runner{
		opts:      opts,
		apiKey:    apiKey,
		client:    newHTTPClient(),
		model:     opts.model,
		thinking:  opts.thinkingModel,
		startedAt: time.Now(),
		userAgent: "kiro-go-plus-devcheck/1",
	}
	r.runSuite(ctx)
	if opts.baselinePath != "" {
		if err := r.compareLoadBaseline(opts.baselinePath); err != nil {
			fmt.Fprintf(os.Stderr, "compare baseline: %v\n", err)
			return 1
		}
	}
	r.printResults()

	if opts.reportPath != "" {
		if err := r.writeReport(opts.reportPath); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
			return 1
		}
	}
	if r.baselineMissing > 0 {
		return 1
	}
	for _, result := range r.results {
		if result.Status == statusFail || (opts.failOnWarning && result.Status == statusWarn) {
			return 1
		}
	}
	return 0
}

func parseOptions(args []string) (options, error) {
	opts := options{}
	set := flag.NewFlagSet("devcheck", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.StringVar(&opts.baseURL, "base-url", envOr("KIRO_DEV_BASE_URL", "http://127.0.0.1:8080"), "local Kiro-Go-Plus base URL")
	set.StringVar(&opts.model, "model", strings.TrimSpace(os.Getenv("KIRO_DEV_MODEL")), "model ID; auto-discovered when omitted")
	set.StringVar(&opts.thinkingModel, "thinking-model", strings.TrimSpace(os.Getenv("KIRO_DEV_THINKING_MODEL")), "thinking model ID; defaults to <model>-thinking")
	set.StringVar(&opts.modelsCSV, "models", strings.TrimSpace(os.Getenv("KIRO_DEV_MODELS")), "comma-separated model IDs for the matrix suite")
	set.BoolVar(&opts.allModels, "all-models", false, "test every discovered Claude model in the matrix suite")
	set.StringVar(&opts.suite, "suite", "smoke", "suite: smoke, full, matrix, load, staircase, or soak")
	set.DurationVar(&opts.timeout, "timeout", 150*time.Second, "timeout for each live scenario")
	set.IntVar(&opts.concurrency, "concurrency", 5, "parallel requests for the load suite")
	set.IntVar(&opts.requests, "requests", 10, "total requests for the load suite")
	set.StringVar(&opts.concurrencyCSV, "concurrency-levels", "1,5,10,20,50,100", "comma-separated worker counts for the staircase suite")
	set.BoolVar(&opts.allowHighLoad, "allow-high-load", false, "allow explicitly configured concurrency above 100 (maximum 1000)")
	set.StringVar(&opts.loadProfile, "load-profile", "marker", "load workload profile: marker or realistic")
	set.StringVar(&opts.loadPattern, "load-pattern", "closed", "load arrival pattern: closed, fixed, or ramp")
	set.IntVar(&opts.loadMaxTokens, "load-max-tokens", 32, "maximum output tokens reserved by each load request")
	set.IntVar(&opts.warmupRequests, "warmup-requests", 0, "unmeasured requests sent before a load, staircase, or soak suite")
	set.Float64Var(&opts.targetRPS, "target-rps", 0, "target arrivals per second for fixed or ramp load patterns")
	set.DurationVar(&opts.rampDuration, "ramp-duration", 30*time.Second, "time for a ramp load to rise from 10 percent to target RPS")
	set.BoolVar(&opts.postLoadRecovery, "post-load-recovery", true, "verify health and one deterministic request after load execution")
	set.DurationVar(&opts.staircaseHold, "staircase-hold", 0, "hold each staircase level for this duration (0 keeps request-count mode)")
	set.DurationVar(&opts.staircaseCooldown, "staircase-cooldown", 0, "cool down between staircase levels")
	set.IntVar(&opts.staircaseMaxRequests, "staircase-max-requests", 10000, "request cap per staircase level when a hold duration is used")
	set.DurationVar(&opts.resourceSampleInterval, "resource-sample-interval", 5*time.Second, "client resource sampling interval; 0 disables time-series sampling")
	set.BoolVar(&opts.collectServerStats, "server-stats", true, "sample authenticated customer counters before and after load")
	set.BoolVar(&opts.requireServerStats, "require-server-stats", false, "fail load results when authenticated customer counters cannot be sampled or reset")
	set.DurationVar(&opts.soakDuration, "soak-duration", 5*time.Minute, "maximum duration of the soak suite")
	set.IntVar(&opts.soakMaxRequests, "soak-max-requests", 100, "maximum requests in the soak suite")
	set.IntVar(&opts.soakTokenBudget, "soak-token-budget", 3200, "maximum requested output tokens in the soak suite")
	set.Float64Var(&opts.minSuccessRate, "min-success-rate", 0, "minimum load success rate in percent; 0 keeps the default all-request gate")
	set.Int64Var(&opts.maxP95Millis, "max-p95-ms", 0, "maximum load total-latency P95 in milliseconds; 0 disables the threshold")
	set.Int64Var(&opts.maxTTFTP95Millis, "max-ttft-p95-ms", 0, "maximum load semantic TTFT P95 in milliseconds; 0 disables the threshold")
	set.Int64Var(&opts.maxStreamGapMillis, "max-stream-gap-p95-ms", 0, "maximum load protocol-event gap P95 in milliseconds; 0 disables the threshold")
	set.Float64Var(&opts.maxClientOverloadRate, "max-client-overload-rate", -1, "maximum dropped-arrival rate in percent; -1 disables the threshold")
	set.IntVar(&opts.maxClientGoroutineGrowth, "max-client-goroutine-growth", -1, "maximum post-load client goroutine delta; -1 disables the threshold")
	set.Int64Var(&opts.maxClientHeapGrowthMB, "max-client-heap-growth-mb", -1, "maximum post-load client heap growth in MiB; -1 disables the threshold")
	set.StringVar(&opts.baselinePath, "baseline", "", "load JSON report used for regression comparison")
	set.Float64Var(&opts.baselineTolerancePercent, "baseline-tolerance-percent", 10, "allowed relative latency regression against --baseline")
	set.BoolVar(&opts.webSearch, "web-search", false, "run the network-dependent native WebSearch scenario")
	set.BoolVar(&opts.cancellation, "cancellation", true, "run client cancellation and recovery in the full suite")
	set.StringVar(&opts.scenariosCSV, "scenarios", "", "comma-separated full-suite scenario IDs to run")
	set.BoolVar(&opts.listScenarios, "list-scenarios", false, "list selectable scenario IDs without sending requests")
	set.StringVar(&opts.reportPath, "json-report", "", "write a machine-readable report with mode 0600")
	set.BoolVar(&opts.failOnWarning, "fail-on-warning", false, "return non-zero when a scenario warns")
	set.BoolVar(&opts.allowRemote, "allow-remote", false, "allow credentials to be sent to a non-loopback base URL")
	if err := set.Parse(args); err != nil {
		return options{}, err
	}

	opts.baseURL = strings.TrimRight(strings.TrimSpace(opts.baseURL), "/")
	parsedURL, err := url.Parse(opts.baseURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return options{}, fmt.Errorf("invalid --base-url %q", opts.baseURL)
	}
	if !opts.allowRemote && !isLoopbackHost(parsedURL.Hostname()) {
		return options{}, fmt.Errorf("refusing to send credentials to non-loopback host %q without --allow-remote", parsedURL.Hostname())
	}
	opts.suite = strings.ToLower(strings.TrimSpace(opts.suite))
	validSuites := map[string]bool{"smoke": true, "full": true, "matrix": true, "load": true, "staircase": true, "soak": true}
	if !validSuites[opts.suite] {
		return options{}, fmt.Errorf("invalid --suite %q: expected smoke, full, matrix, load, staircase, or soak", opts.suite)
	}
	if opts.timeout < time.Second {
		return options{}, errors.New("--timeout must be at least 1s")
	}
	maxConcurrency := 100
	if opts.allowHighLoad {
		maxConcurrency = 1000
	}
	if opts.concurrency < 1 || opts.concurrency > maxConcurrency {
		if opts.allowHighLoad {
			return options{}, errors.New("--concurrency must be between 1 and 1000")
		}
		return options{}, errors.New("--concurrency above 100 requires --allow-high-load")
	}
	if opts.requests < 1 || opts.requests > 10000 {
		return options{}, errors.New("--requests must be between 1 and 10000")
	}
	opts.loadProfile = strings.ToLower(strings.TrimSpace(opts.loadProfile))
	if opts.loadProfile != "marker" && opts.loadProfile != "realistic" {
		return options{}, errors.New("--load-profile must be marker or realistic")
	}
	opts.loadPattern = strings.ToLower(strings.TrimSpace(opts.loadPattern))
	if opts.loadPattern != "closed" && opts.loadPattern != "fixed" && opts.loadPattern != "ramp" {
		return options{}, errors.New("--load-pattern must be closed, fixed, or ramp")
	}
	if opts.loadMaxTokens < 1 || opts.loadMaxTokens > 128000 {
		return options{}, errors.New("--load-max-tokens must be between 1 and 128000")
	}
	if opts.loadProfile == "realistic" && opts.loadMaxTokens < 128 {
		return options{}, errors.New("--load-profile realistic requires --load-max-tokens of at least 128")
	}
	if opts.warmupRequests < 0 || opts.warmupRequests > 1000 {
		return options{}, errors.New("--warmup-requests must be between 0 and 1000")
	}
	if opts.loadPattern == "closed" && opts.targetRPS != 0 {
		return options{}, errors.New("--target-rps requires --load-pattern fixed or ramp")
	}
	if opts.loadPattern != "closed" && (opts.targetRPS < 0.1 || opts.targetRPS > 10000) {
		return options{}, errors.New("fixed and ramp loads require --target-rps between 0.1 and 10000")
	}
	if math.IsNaN(opts.targetRPS) || math.IsInf(opts.targetRPS, 0) {
		return options{}, errors.New("--target-rps must be a finite number")
	}
	if opts.suite != "load" && opts.loadPattern != "closed" {
		return options{}, errors.New("fixed and ramp patterns are currently supported only by --suite load")
	}
	if opts.rampDuration < time.Second || opts.rampDuration > time.Hour {
		return options{}, errors.New("--ramp-duration must be between 1s and 1h")
	}
	if opts.staircaseHold < 0 || opts.staircaseHold > 24*time.Hour {
		return options{}, errors.New("--staircase-hold must be between 0 and 24h")
	}
	if opts.staircaseCooldown < 0 || opts.staircaseCooldown > time.Hour {
		return options{}, errors.New("--staircase-cooldown must be between 0 and 1h")
	}
	if opts.staircaseMaxRequests < 1 || opts.staircaseMaxRequests > 10000 {
		return options{}, errors.New("--staircase-max-requests must be between 1 and 10000")
	}
	if opts.resourceSampleInterval != 0 && (opts.resourceSampleInterval < 100*time.Millisecond || opts.resourceSampleInterval > time.Minute) {
		return options{}, errors.New("--resource-sample-interval must be 0 or between 100ms and 1m")
	}
	if opts.soakDuration < time.Second || opts.soakDuration > 24*time.Hour {
		return options{}, errors.New("--soak-duration must be between 1s and 24h")
	}
	if opts.soakMaxRequests < 1 || opts.soakMaxRequests > 10000 {
		return options{}, errors.New("--soak-max-requests must be between 1 and 10000")
	}
	if opts.soakTokenBudget < 32 || opts.soakTokenBudget > 1000000 {
		return options{}, errors.New("--soak-token-budget must be between 32 and 1000000")
	}
	if math.IsNaN(opts.minSuccessRate) || math.IsInf(opts.minSuccessRate, 0) || opts.minSuccessRate < 0 || opts.minSuccessRate > 100 {
		return options{}, errors.New("--min-success-rate must be between 0 and 100")
	}
	if opts.maxP95Millis < 0 || opts.maxTTFTP95Millis < 0 || opts.maxStreamGapMillis < 0 {
		return options{}, errors.New("latency thresholds must not be negative")
	}
	if math.IsNaN(opts.maxClientOverloadRate) || math.IsInf(opts.maxClientOverloadRate, 0) || opts.maxClientOverloadRate < -1 || opts.maxClientOverloadRate > 100 {
		return options{}, errors.New("--max-client-overload-rate must be -1 or between 0 and 100")
	}
	if opts.maxClientGoroutineGrowth < -1 || opts.maxClientGoroutineGrowth > 1000000 {
		return options{}, errors.New("--max-client-goroutine-growth must be -1 or between 0 and 1000000")
	}
	if opts.maxClientHeapGrowthMB < -1 || opts.maxClientHeapGrowthMB > 16384 {
		return options{}, errors.New("--max-client-heap-growth-mb must be -1 or between 0 and 16384")
	}
	if math.IsNaN(opts.baselineTolerancePercent) || math.IsInf(opts.baselineTolerancePercent, 0) || opts.baselineTolerancePercent < 0 || opts.baselineTolerancePercent > 1000 {
		return options{}, errors.New("--baseline-tolerance-percent must be between 0 and 1000")
	}
	if opts.suite == "soak" && opts.soakTokenBudget < opts.loadMaxTokens {
		return options{}, errors.New("--soak-token-budget must reserve at least one --load-max-tokens request")
	}
	if opts.requireServerStats && !opts.collectServerStats {
		return options{}, errors.New("--require-server-stats cannot be combined with --server-stats=false")
	}
	if opts.requireServerStats && opts.suite != "load" && opts.suite != "staircase" && opts.suite != "soak" {
		return options{}, errors.New("--require-server-stats is supported only by load, staircase, and soak suites")
	}
	if opts.model != "" && opts.modelsCSV != "" {
		return options{}, errors.New("--model and --models cannot be used together")
	}
	if opts.allModels && (opts.model != "" || opts.modelsCSV != "") {
		return options{}, errors.New("--all-models cannot be combined with --model or --models")
	}
	var parseErr error
	opts.models, parseErr = parseCSV(opts.modelsCSV, 50)
	if parseErr != nil {
		return options{}, fmt.Errorf("invalid --models: %w", parseErr)
	}
	opts.concurrencySteps, parseErr = parseConcurrencySteps(opts.concurrencyCSV, maxConcurrency)
	if parseErr != nil {
		return options{}, fmt.Errorf("invalid --concurrency-levels: %w", parseErr)
	}
	scenarios, parseErr := parseCSV(opts.scenariosCSV, len(scenarioCatalog))
	if parseErr != nil {
		return options{}, fmt.Errorf("invalid --scenarios: %w", parseErr)
	}
	if len(scenarios) > 0 {
		opts.scenarioFilter = make(map[string]struct{}, len(scenarios))
		for _, scenario := range scenarios {
			if _, known := scenarioCatalog[scenario]; !known {
				return options{}, fmt.Errorf("unknown scenario %q; use --list-scenarios", scenario)
			}
			opts.scenarioFilter[scenario] = struct{}{}
		}
	}
	return opts, nil
}

var scenarioCatalog = map[string]string{
	"anthropic-non-stream":     "Anthropic Messages JSON response",
	"anthropic-stream":         "Anthropic Messages SSE and timing",
	"thinking-stream":          "thinking/reasoning SSE visibility",
	"thinking-protocols":       "Chat and Responses reasoning SSE visibility",
	"skill-context":            "client-side Skill system instruction transport",
	"anthropic-tool-roundtrip": "Anthropic forced tool call and tool_result continuation",
	"mcp-roundtrip":            "MCP-shaped zero-argument call and tool_result continuation",
	"chat-tool-roundtrip":      "Chat Completions function call and tool result continuation",
	"responses-tool-roundtrip": "Responses function call and function output continuation",
	"responses-custom-tool":    "Responses custom tool input and output continuation",
	"chat-stream":              "Chat Completions SSE and timing",
	"responses-non-stream":     "Responses API JSON response",
	"responses-stream":         "Responses API SSE and timing",
	"cache-reuse":              "cold/create/read prompt-cache usage over repeated requests",
	"multimodal-accounting":    "same-dimension image accounting independent of base64 size",
	"output-limit":             "three-protocol output-limit terminal semantics",
	"long-stream":              "long Anthropic stream timing, gaps, and burst buffering",
	"websearch-non-stream":     "native WebSearch JSON response",
	"websearch-stream":         "native WebSearch SSE response",
	"websearch-multi":          "bounded multi-search request",
	"websearch-mixed-tools":    "native WebSearch alongside a client tool schema",
	"cancellation":             "established stream cancellation and recovery",
	"protocol-matrix":          "selected models across three protocols and stream modes",
}

func printScenarioCatalog() {
	names := make([]string, 0, len(scenarioCatalog))
	for name := range scenarioCatalog {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-28s %s\n", name, scenarioCatalog[name])
	}
}

func parseCSV(raw string, limit int) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, rawValue := range strings.Split(raw, ",") {
		value := strings.TrimSpace(rawValue)
		if value == "" {
			return nil, errors.New("contains an empty value")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
		if len(values) > limit {
			return nil, fmt.Errorf("contains more than %d values", limit)
		}
	}
	return values, nil
}

func parseConcurrencySteps(raw string, maximum int) ([]int, error) {
	values, err := parseCSV(raw, 20)
	if err != nil {
		return nil, err
	}
	steps := make([]int, 0, len(values))
	for _, value := range values {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || fmt.Sprint(parsed) != value || parsed < 1 || parsed > maximum {
			return nil, fmt.Errorf("%q is not an integer between 1 and %d", value, maximum)
		}
		steps = append(steps, parsed)
	}
	if len(steps) == 0 {
		return nil, errors.New("at least one level is required")
	}
	return steps, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (r *runner) add(result scenarioResult) {
	result.RequestIDs = uniqueNonEmpty(result.RequestIDs)
	result.Detail = compactDetail(result.Detail)
	r.results = append(r.results, result)
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (r *runner) scenarioContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := r.opts.timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func (r *runner) printResults() {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tSCENARIO\tMODEL\tHTTP\tHEAD\tEVENT\tSEMANTIC\tTEXT\tTHINK\tTOOL\tGAP\tWIRE\tHEART\tTOTAL\tDETAIL")
	for _, result := range r.results {
		httpStatus := "-"
		if result.HTTPStatus > 0 {
			httpStatus = fmt.Sprint(result.HTTPStatus)
		}
		model := result.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			result.Status, result.Name, model, httpStatus,
			formatMillis(result.ResponseHeaderMS), formatMillis(result.FirstEventMillis), formatMillis(result.TTFTMillis),
			formatMillis(result.FirstTextMillis), formatMillis(result.FirstThinkMillis), formatMillis(result.FirstToolMillis),
			formatMillis(result.MaxStreamGapMS), formatMillis(result.MaxWireGapMS), result.Heartbeats,
			formatMillis(result.TotalMillis), result.Detail)
	}
	_ = w.Flush()

	summary := r.summary()
	fmt.Printf("\nSummary: pass=%d warn=%d fail=%d skip=%d model=%s elapsed=%s\n",
		summary[statusPass], summary[statusWarn], summary[statusFail], summary[statusSkip], r.model, time.Since(r.startedAt).Round(time.Millisecond))
}

func formatMillis(value int64) string {
	if value <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", value)
}

func (r *runner) summary() map[string]int {
	summary := map[string]int{statusPass: 0, statusWarn: 0, statusFail: 0, statusSkip: 0}
	for _, result := range r.results {
		summary[result.Status]++
	}
	return summary
}

func (r *runner) writeReport(path string) error {
	report := devReport{
		GeneratedAt:              time.Now().UTC().Format(time.RFC3339),
		BaseURL:                  r.opts.baseURL,
		ServerVersion:            r.serverVersion,
		ConfigurationFingerprint: configurationFingerprint(r.opts),
		Suite:                    r.opts.suite,
		LoadProfile:              r.opts.loadProfile,
		LoadPattern:              r.opts.loadPattern,
		LoadMaxTokens:            r.opts.loadMaxTokens,
		Concurrency:              r.opts.concurrency,
		Requests:                 r.opts.requests,
		TargetRPS:                r.opts.targetRPS,
		RampMillis:               r.opts.rampDuration.Milliseconds(),
		WarmupRequests:           r.opts.warmupRequests,
		ConcurrencyLevels:        append([]int(nil), r.opts.concurrencySteps...),
		StaircaseHoldMillis:      r.opts.staircaseHold.Milliseconds(),
		StaircaseCooldownMillis:  r.opts.staircaseCooldown.Milliseconds(),
		StaircaseMaxRequests:     r.opts.staircaseMaxRequests,
		SoakMillis:               r.opts.soakDuration.Milliseconds(),
		SoakMaxRequests:          r.opts.soakMaxRequests,
		SoakTokenBudget:          r.opts.soakTokenBudget,
		Model:                    r.model,
		Models:                   append([]string(nil), r.selected...),
		Results:                  r.results,
		Summary:                  r.summary(),
		BaselineCompared:         r.baselineCompared,
		BaselineRegressions:      r.baselineRegressions,
		BaselineMissing:          r.baselineMissing,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateFileAtomically(path, data)
}

// writePrivateFileAtomically prevents readers from observing a half-written
// report and keeps the temporary file beside the destination for an atomic
// same-filesystem rename.
func writePrivateFileAtomically(path string, data []byte) error {
	directory := filepath.Dir(path)
	pattern := "." + filepath.Base(path) + ".tmp-*"
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return err
	}
	temporary := file.Name()
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = os.Remove(temporary)
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	return os.Rename(temporary, path)
}

func configurationFingerprint(opts options) string {
	scenarios := make([]string, 0, len(opts.scenarioFilter))
	for scenario := range opts.scenarioFilter {
		scenarios = append(scenarios, scenario)
	}
	sort.Strings(scenarios)
	snapshot := struct {
		BaseURL                  string   `json:"baseUrl"`
		Suite                    string   `json:"suite"`
		TimeoutMillis            int64    `json:"timeoutMillis"`
		Concurrency              int      `json:"concurrency"`
		Requests                 int      `json:"requests"`
		LoadProfile              string   `json:"loadProfile"`
		LoadPattern              string   `json:"loadPattern"`
		LoadMaxTokens            int      `json:"loadMaxTokens"`
		WarmupRequests           int      `json:"warmupRequests"`
		TargetRPS                float64  `json:"targetRps"`
		RampMillis               int64    `json:"rampMillis"`
		AllowHighLoad            bool     `json:"allowHighLoad"`
		PostRecovery             bool     `json:"postLoadRecovery"`
		SoakMillis               int64    `json:"soakMillis"`
		SoakMaxRequests          int      `json:"soakMaxRequests"`
		SoakTokenBudget          int      `json:"soakTokenBudget"`
		StaircaseHoldMillis      int64    `json:"staircaseHoldMillis"`
		StaircaseCooldownMillis  int64    `json:"staircaseCooldownMillis"`
		StaircaseMaxRequests     int      `json:"staircaseMaxRequests"`
		ResourceSampleMillis     int64    `json:"resourceSampleMillis"`
		CollectServerStats       bool     `json:"collectServerStats"`
		RequireServerStats       bool     `json:"requireServerStats"`
		MinSuccessRate           float64  `json:"minSuccessRate"`
		MaxP95Millis             int64    `json:"maxP95Millis"`
		MaxTTFTP95Millis         int64    `json:"maxTTFTP95Millis"`
		MaxStreamGapMillis       int64    `json:"maxStreamGapMillis"`
		MaxClientOverloadRate    float64  `json:"maxClientOverloadRate"`
		MaxClientGoroutineGrowth int      `json:"maxClientGoroutineGrowth"`
		MaxClientHeapGrowthMB    int64    `json:"maxClientHeapGrowthMb"`
		BaselineTolerancePercent float64  `json:"baselineTolerancePercent"`
		WebSearch                bool     `json:"webSearch"`
		Cancellation             bool     `json:"cancellation"`
		AllModels                bool     `json:"allModels"`
		Models                   []string `json:"models,omitempty"`
		Scenarios                []string `json:"scenarios,omitempty"`
	}{
		BaseURL: opts.baseURL, Suite: opts.suite, TimeoutMillis: opts.timeout.Milliseconds(),
		Concurrency: opts.concurrency, Requests: opts.requests, SoakMillis: opts.soakDuration.Milliseconds(),
		LoadProfile: opts.loadProfile, LoadPattern: opts.loadPattern, LoadMaxTokens: opts.loadMaxTokens,
		WarmupRequests: opts.warmupRequests, TargetRPS: opts.targetRPS, RampMillis: opts.rampDuration.Milliseconds(), AllowHighLoad: opts.allowHighLoad,
		PostRecovery:    opts.postLoadRecovery,
		SoakMaxRequests: opts.soakMaxRequests, SoakTokenBudget: opts.soakTokenBudget,
		StaircaseHoldMillis: opts.staircaseHold.Milliseconds(), StaircaseCooldownMillis: opts.staircaseCooldown.Milliseconds(), StaircaseMaxRequests: opts.staircaseMaxRequests,
		ResourceSampleMillis: opts.resourceSampleInterval.Milliseconds(), MinSuccessRate: opts.minSuccessRate,
		CollectServerStats: opts.collectServerStats,
		RequireServerStats: opts.requireServerStats,
		MaxP95Millis:       opts.maxP95Millis, MaxTTFTP95Millis: opts.maxTTFTP95Millis, MaxStreamGapMillis: opts.maxStreamGapMillis,
		MaxClientOverloadRate: opts.maxClientOverloadRate, MaxClientGoroutineGrowth: opts.maxClientGoroutineGrowth,
		MaxClientHeapGrowthMB: opts.maxClientHeapGrowthMB, BaselineTolerancePercent: opts.baselineTolerancePercent,
		WebSearch: opts.webSearch, Cancellation: opts.cancellation, AllModels: opts.allModels,
		Models: append([]string(nil), opts.models...), Scenarios: scenarios,
	}
	encoded, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:12])
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func compactDetail(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	const limit = 240
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func percentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	if math.IsNaN(percentile) || percentile < 0 {
		percentile = 0
	}
	if percentile > 1 {
		percentile = 1
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(float64(len(ordered))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}
