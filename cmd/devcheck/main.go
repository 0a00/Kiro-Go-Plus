package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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
	baseURL       string
	model         string
	thinkingModel string
	suite         string
	timeout       time.Duration
	concurrency   int
	requests      int
	webSearch     bool
	cancellation  bool
	reportPath    string
	failOnWarning bool
	allowRemote   bool
}

type scenarioResult struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	Protocol       string `json:"protocol,omitempty"`
	HTTPStatus     int    `json:"httpStatus,omitempty"`
	TTFTMillis     int64  `json:"ttftMillis,omitempty"`
	TotalMillis    int64  `json:"totalMillis,omitempty"`
	Events         int    `json:"events,omitempty"`
	ContentDeltas  int    `json:"contentDeltas,omitempty"`
	ThinkingDeltas int    `json:"thinkingDeltas,omitempty"`
	ToolCalls      int    `json:"toolCalls,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

type devReport struct {
	GeneratedAt string           `json:"generatedAt"`
	BaseURL     string           `json:"baseUrl"`
	Suite       string           `json:"suite"`
	Model       string           `json:"model,omitempty"`
	Results     []scenarioResult `json:"results"`
	Summary     map[string]int   `json:"summary"`
}

type runner struct {
	opts      options
	apiKey    string
	client    *http.Client
	results   []scenarioResult
	models    []string
	model     string
	thinking  string
	startedAt time.Time
	userAgent string
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
	r.printResults()

	if opts.reportPath != "" {
		if err := r.writeReport(opts.reportPath); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
			return 1
		}
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
	set.StringVar(&opts.suite, "suite", "smoke", "suite: smoke, full, or load")
	set.DurationVar(&opts.timeout, "timeout", 150*time.Second, "timeout for each live scenario")
	set.IntVar(&opts.concurrency, "concurrency", 5, "parallel requests for the load suite")
	set.IntVar(&opts.requests, "requests", 10, "total requests for the load suite")
	set.BoolVar(&opts.webSearch, "web-search", false, "run the network-dependent native WebSearch scenario")
	set.BoolVar(&opts.cancellation, "cancellation", true, "run client cancellation and recovery in the full suite")
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
	if opts.suite != "smoke" && opts.suite != "full" && opts.suite != "load" {
		return options{}, fmt.Errorf("invalid --suite %q: expected smoke, full, or load", opts.suite)
	}
	if opts.timeout < time.Second {
		return options{}, errors.New("--timeout must be at least 1s")
	}
	if opts.concurrency < 1 || opts.concurrency > 100 {
		return options{}, errors.New("--concurrency must be between 1 and 100")
	}
	if opts.requests < 1 || opts.requests > 1000 {
		return options{}, errors.New("--requests must be between 1 and 1000")
	}
	return opts, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (r *runner) add(result scenarioResult) {
	result.Detail = compactDetail(result.Detail)
	r.results = append(r.results, result)
}

func (r *runner) scenarioContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, r.opts.timeout)
}

func (r *runner) printResults() {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tSCENARIO\tHTTP\tTTFT\tTOTAL\tEVENTS\tDETAIL")
	for _, result := range r.results {
		ttft := "-"
		if result.TTFTMillis > 0 {
			ttft = fmt.Sprintf("%dms", result.TTFTMillis)
		}
		total := "-"
		if result.TotalMillis > 0 {
			total = fmt.Sprintf("%dms", result.TotalMillis)
		}
		httpStatus := "-"
		if result.HTTPStatus > 0 {
			httpStatus = fmt.Sprint(result.HTTPStatus)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			result.Status, result.Name, httpStatus, ttft, total, result.Events, result.Detail)
	}
	_ = w.Flush()

	summary := r.summary()
	fmt.Printf("\nSummary: pass=%d warn=%d fail=%d skip=%d model=%s elapsed=%s\n",
		summary[statusPass], summary[statusWarn], summary[statusFail], summary[statusSkip], r.model, time.Since(r.startedAt).Round(time.Millisecond))
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
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		BaseURL:     r.opts.baseURL,
		Suite:       r.opts.suite,
		Model:       r.model,
		Results:     r.results,
		Summary:     r.summary(),
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(float64(len(ordered)-1) * percentile)
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}
