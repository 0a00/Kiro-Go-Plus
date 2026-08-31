package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/internal/awsregion"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxKiroAPIKeyBatchEntries    = 1000
	maxKiroAPIKeyBatchBodyBytes  = 1 << 20
	maxKiroAPIKeyBatchKeyBytes   = 4096
	maxKiroAPIKeyBatchWorkers    = 20
	maxKiroAPIKeyBatchErrorRunes = 800
	maxKiroAPIKeyBatchDuration   = 15 * time.Minute
)

type kiroAPIKeyBatchRequest struct {
	Keys           string `json:"keys"`
	NicknamePrefix string `json:"nicknamePrefix,omitempty"`
	Region         string `json:"region,omitempty"`
	ProxyURL       string `json:"proxyURL,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

type kiroAPIKeyBatchResult struct {
	Index     int    `json:"index"`
	KeyMasked string `json:"keyMasked"`
	Status    string `json:"status"`
	ID        string `json:"id,omitempty"`
	Region    string `json:"region,omitempty"`
	Error     string `json:"error,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

type kiroAPIKeyBatchCounts struct {
	Total             int `json:"total"`
	NonEmpty          int `json:"nonEmpty"`
	Created           int `json:"created"`
	Updated           int `json:"updated"`
	Duplicates        int `json:"duplicates"`
	Failed            int `json:"failed"`
	IgnoredEmptyLines int `json:"ignoredEmptyLines"`
}

type kiroAPIKeyBatchLine struct {
	Index int
	Key   string
}

type kiroAPIKeyBatchPrepared struct {
	line      kiroAPIKeyBatchLine
	resultIdx int
	account   config.Account
	retryable bool
	err       error
}

// apiAddKiroAPIKeysBatch adds upstream ksk_ credentials from one key per line.
// Region discovery is bounded by a worker pool, while account persistence is a
// single atomic upsert. Responses contain only masked keys and line numbers.
func (h *Handler) apiAddKiroAPIKeysBatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	request, err := decodeKiroAPIKeyBatchRequest(w, r)
	if err != nil {
		w.WriteHeader(requestBodyErrorStatus(err))
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid Kiro API Key batch request"})
		return
	}

	lines := splitKiroAPIKeyBatchLines(request.Keys)
	if len(lines) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "keys must contain at least one non-empty line"})
		return
	}
	if len(lines) > maxKiroAPIKeyBatchEntries {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("at most %d Kiro API keys can be added at once", maxKiroAPIKeyBatchEntries)})
		return
	}

	request.NicknamePrefix = strings.TrimSpace(request.NicknamePrefix)
	request.Region = strings.TrimSpace(request.Region)
	request.ProxyURL = strings.TrimSpace(request.ProxyURL)
	if request.Region != "" {
		normalized, normalizeErr := awsregion.Normalize(request.Region)
		if normalizeErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": normalizeErr.Error()})
			return
		}
		request.Region = normalized
	}
	if err := validateAccountProxyURL(request.ProxyURL); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}

	results := make([]kiroAPIKeyBatchResult, 0, len(lines))
	pending := make([]kiroAPIKeyBatchPrepared, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	existing := existingKiroAPIKeyAccounts()
	for _, line := range lines {
		result := kiroAPIKeyBatchResult{
			Index:     line.Index,
			KeyMasked: maskedKiroAPIKeyLabel(line.Key),
			Status:    "pending",
		}
		resultIdx := len(results)
		results = append(results, result)

		if err := validateKiroAPIKeyBatchKey(line.Key); err != nil {
			results[resultIdx].Status = "failed"
			results[resultIdx].Error = err.Error()
			continue
		}
		if _, duplicate := seen[line.Key]; duplicate {
			results[resultIdx].Status = "duplicate"
			results[resultIdx].Error = "duplicate key in this batch"
			continue
		}
		seen[line.Key] = struct{}{}
		if account, duplicate := existing[line.Key]; duplicate {
			results[resultIdx].Status = "duplicate"
			results[resultIdx].ID = account.ID
			results[resultIdx].Region = strings.TrimSpace(account.Region)
			results[resultIdx].Error = "key already exists"
			continue
		}
		pending = append(pending, kiroAPIKeyBatchPrepared{
			line:      line,
			resultIdx: resultIdx,
		})
	}

	batchContext, cancel := context.WithTimeout(r.Context(), maxKiroAPIKeyBatchDuration)
	defer cancel()
	prepareKiroAPIKeyBatch(batchContext, pending, request, enabled)

	accountsToPersist := make([]config.Account, 0, len(pending))
	preparedIndexes := make([]int, 0, len(pending))
	for i := range pending {
		prepared := &pending[i]
		if prepared.err != nil {
			results[prepared.resultIdx].Status = "failed"
			results[prepared.resultIdx].Retryable = prepared.retryable
			results[prepared.resultIdx].Error = safeKiroAPIKeyBatchError(prepared.line.Key, prepared.err)
			continue
		}
		accountsToPersist = append(accountsToPersist, prepared.account)
		preparedIndexes = append(preparedIndexes, i)
	}

	warm := make([]config.Account, 0, len(accountsToPersist))
	if len(accountsToPersist) > 0 {
		persisted, persistErr := config.UpsertAccountsByIdentity(accountsToPersist)
		if persistErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to persist Kiro API Key batch"})
			return
		}
		for i, persistedResult := range persisted {
			prepared := pending[preparedIndexes[i]]
			status := "created"
			if persistedResult.Updated {
				status = "updated"
			}
			results[prepared.resultIdx].Status = status
			results[prepared.resultIdx].ID = persistedResult.Account.ID
			results[prepared.resultIdx].Region = persistedResult.Account.Region
			if persistedResult.Account.Enabled {
				warm = append(warm, persistedResult.Account)
			}
		}
		if h != nil && h.pool != nil {
			h.pool.Reload()
			if len(warm) > 0 && h.backgroundCtx != nil {
				h.refreshModelCachesAsync(warm)
			}
		}
	}

	ignoredLines := countIgnoredKiroAPIKeyBatchLines(request.Keys)
	counts := summarizeKiroAPIKeyBatch(results, len(lines)+ignoredLines, ignoredLines)
	response := map[string]interface{}{
		"success": resultsHaveNoKiroAPIKeyBatchFailures(results),
		"counts":  counts,
		"results": results,
	}
	_ = json.NewEncoder(w).Encode(response)
}

func decodeKiroAPIKeyBatchRequest(w http.ResponseWriter, r *http.Request) (kiroAPIKeyBatchRequest, error) {
	if r == nil || r.Body == nil {
		return kiroAPIKeyBatchRequest{}, fmt.Errorf("request body is empty")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKiroAPIKeyBatchBodyBytes))
	if err != nil {
		return kiroAPIKeyBatchRequest{}, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return kiroAPIKeyBatchRequest{}, fmt.Errorf("request body is empty")
	}
	if trimmed[0] != '{' {
		// The admin endpoint also accepts text/plain for scripts and curl users.
		return kiroAPIKeyBatchRequest{Keys: string(raw)}, nil
	}
	var request kiroAPIKeyBatchRequest
	if err := json.Unmarshal(trimmed, &request); err != nil {
		return kiroAPIKeyBatchRequest{}, err
	}
	return request, nil
}

func splitKiroAPIKeyBatchLines(raw string) []kiroAPIKeyBatchLine {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	result := make([]kiroAPIKeyBatchLine, 0, len(lines))
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result = append(result, kiroAPIKeyBatchLine{Index: index + 1, Key: line})
	}
	return result
}

func validateKiroAPIKeyBatchKey(key string) error {
	if !strings.HasPrefix(key, "ksk_") {
		return fmt.Errorf("Kiro API Key must start with ksk_")
	}
	if len(key) <= len("ksk_") {
		return fmt.Errorf("Kiro API Key is empty after the ksk_ prefix")
	}
	if len(key) > maxKiroAPIKeyBatchKeyBytes {
		return fmt.Errorf("Kiro API Key exceeds the %d-byte limit", maxKiroAPIKeyBatchKeyBytes)
	}
	return nil
}

func countIgnoredKiroAPIKeyBatchLines(raw string) int {
	lineCount := len(strings.Split(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"), "\n"))
	return lineCount - len(splitKiroAPIKeyBatchLines(raw))
}

func existingKiroAPIKeyAccounts() map[string]config.Account {
	accounts := config.GetAccounts()
	result := make(map[string]config.Account, len(accounts))
	for _, account := range accounts {
		key := strings.TrimSpace(account.KiroApiKey)
		if key == "" && isKiroAPIKeyAccount(&account) {
			key = strings.TrimSpace(account.AccessToken)
		}
		if key != "" {
			result[key] = account
		}
	}
	return result
}

func prepareKiroAPIKeyBatch(ctx context.Context, pending []kiroAPIKeyBatchPrepared, request kiroAPIKeyBatchRequest, enabled bool) {
	if len(pending) == 0 {
		return
	}
	workers := config.GetAutoRefreshConfig().RefreshConcurrency
	if workers < 1 {
		workers = 5
	}
	if workers > maxKiroAPIKeyBatchWorkers {
		workers = maxKiroAPIKeyBatchWorkers
	}
	if workers > len(pending) {
		workers = len(pending)
	}
	jobs := make(chan int)
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for index := range jobs {
				item := &pending[index]
				item.account, item.retryable, item.err = prepareKiroAPIKeyBatchAccount(
					ctx, item.line.Key, request.NicknamePrefix, item.line.Index,
					request.Region, request.ProxyURL, enabled,
				)
			}
		}()
	}
	for index := range pending {
		jobs <- index
	}
	close(jobs)
	waitGroup.Wait()
}

func prepareKiroAPIKeyBatchAccount(ctx context.Context, key, nicknamePrefix string, lineIndex int, explicitRegion, proxyURL string, enabled bool) (config.Account, bool, error) {
	region, info, retryable, err := resolveKiroAPIKeyRegion(ctx, key, explicitRegion, proxyURL)
	if err != nil {
		return config.Account{}, retryable, err
	}
	region, err = awsregion.Normalize(region)
	if err != nil {
		return config.Account{}, false, err
	}
	account := config.Account{
		ID:          auth.GenerateAccountID(),
		Email:       maskedKiroAPIKeyLabel(key),
		Nickname:    batchKiroAPIKeyNickname(nicknamePrefix, lineIndex),
		AccessToken: key,
		KiroApiKey:  key,
		AuthMethod:  "api_key",
		Provider:    "API Key",
		Region:      region,
		ProxyURL:    proxyURL,
		Enabled:     enabled,
		MachineId:   config.GenerateMachineId(),
	}
	applyProbedAccountInfo(&account, info)
	return account, false, nil
}

func batchKiroAPIKeyNickname(prefix string, lineIndex int) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	return fmt.Sprintf("%s-%d", prefix, lineIndex)
}

func safeKiroAPIKeyBatchError(key string, err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if key != "" {
		message = strings.ReplaceAll(message, key, "[REDACTED]")
	}
	message = redactDiagnosticText(message)
	return truncateDiagnosticText(message, maxKiroAPIKeyBatchErrorRunes)
}

func summarizeKiroAPIKeyBatch(results []kiroAPIKeyBatchResult, total, ignored int) kiroAPIKeyBatchCounts {
	counts := kiroAPIKeyBatchCounts{Total: total, NonEmpty: len(results), IgnoredEmptyLines: ignored}
	for _, result := range results {
		switch result.Status {
		case "created":
			counts.Created++
		case "updated":
			counts.Updated++
		case "duplicate":
			counts.Duplicates++
		case "failed":
			counts.Failed++
		}
	}
	return counts
}

func resultsHaveNoKiroAPIKeyBatchFailures(results []kiroAPIKeyBatchResult) bool {
	for _, result := range results {
		if result.Status == "failed" {
			return false
		}
	}
	return true
}
