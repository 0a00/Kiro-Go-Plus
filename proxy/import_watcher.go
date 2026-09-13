package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	importWatcherDirectoryName = "imports"
	importWatcherStableAge     = 2 * time.Second
	maxImportWatcherFileBytes  = int64(8 << 20)
	maxImportWatcherErrorBytes = 4096
)

type importWatcherStatus struct {
	Enabled         bool   `json:"enabled"`
	Directory       string `json:"directory"`
	IntervalSeconds int    `json:"intervalSeconds"`
	Running         bool   `json:"running"`
	LastScanAt      int64  `json:"lastScanAt,omitempty"`
	LastImported    int    `json:"lastImported"`
	LastFailed      int    `json:"lastFailed"`
	LastSkipped     int    `json:"lastSkipped"`
	Pending         int    `json:"pending"`
	LastError       string `json:"lastError,omitempty"`
}

type importWatcherState struct {
	mu      sync.RWMutex
	scanMu  sync.Mutex
	running atomic.Bool
	status  importWatcherStatus
}

type importWatcherFileResult struct {
	Imported   int
	Failed     int
	Recoveries []importWatcherRecovery
	Errors     []string
}

type importWatcherRecovery struct {
	Index               int    `json:"index"`
	RotatedRefreshToken string `json:"rotatedRefreshToken"`
}

func newImportWatcherState() *importWatcherState {
	return &importWatcherState{}
}

func importWatcherDirectory() string {
	return filepath.Join(config.GetConfigDir(), importWatcherDirectoryName)
}

func (h *Handler) backgroundImportWatcher() {
	for {
		watch := config.GetImportWatcherConfig()
		interval := time.Duration(watch.IntervalSeconds) * time.Second
		if interval < 5*time.Second {
			interval = 15 * time.Second
		}
		if watch.Enabled {
			h.scanImportDirectory()
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-h.stopRefresh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (h *Handler) importWatcherSnapshot() importWatcherStatus {
	if h == nil || h.importWatcher == nil {
		watch := config.GetImportWatcherConfig()
		return importWatcherStatus{Enabled: watch.Enabled, Directory: importWatcherDirectory(), IntervalSeconds: watch.IntervalSeconds}
	}
	state := h.importWatcher
	state.mu.RLock()
	status := state.status
	state.mu.RUnlock()
	status.Enabled = config.GetImportWatcherConfig().Enabled
	status.Directory = importWatcherDirectory()
	status.IntervalSeconds = config.GetImportWatcherConfig().IntervalSeconds
	status.Running = state.running.Load()
	return status
}

func (h *Handler) scanImportDirectory() {
	if h == nil {
		return
	}
	if h.importWatcher == nil {
		h.importWatcher = newImportWatcherState()
	}
	state := h.importWatcher
	if !state.running.CompareAndSwap(false, true) {
		return
	}
	defer state.running.Store(false)
	state.scanMu.Lock()
	defer state.scanMu.Unlock()

	dir := importWatcherDirectory()
	status := importWatcherStatus{
		Enabled:         config.GetImportWatcherConfig().Enabled,
		Directory:       dir,
		IntervalSeconds: config.GetImportWatcherConfig().IntervalSeconds,
		LastScanAt:      time.Now().Unix(),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		status.LastError = truncateImportWatcherError(err.Error())
		h.setImportWatcherStatus(status)
		return
	}
	_ = os.Chmod(dir, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		status.LastError = truncateImportWatcherError(err.Error())
		h.setImportWatcherStatus(status)
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			status.LastSkipped++
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			status.LastSkipped++
			continue
		}
		if time.Since(info.ModTime()) < importWatcherStableAge {
			status.LastSkipped++
			continue
		}
		result := h.processImportWatcherFile(dir, name)
		status.LastImported += result.Imported
		status.LastFailed += result.Failed
		if result.Imported == 0 && result.Failed == 0 {
			status.LastSkipped++
		}
	}
	status.Pending = countImportWatcherFiles(dir)
	h.setImportWatcherStatus(status)
}

func (h *Handler) setImportWatcherStatus(status importWatcherStatus) {
	if h == nil || h.importWatcher == nil {
		return
	}
	h.importWatcher.mu.Lock()
	h.importWatcher.status = status
	h.importWatcher.mu.Unlock()
}

func countImportWatcherFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			count++
		}
	}
	return count
}

func (h *Handler) processImportWatcherFile(dir, name string) importWatcherFileResult {
	path := filepath.Join(dir, name)
	file, err := os.Open(path)
	if err != nil {
		return h.failImportWatcherFile(dir, name, err.Error())
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxImportWatcherFileBytes+1))
	_ = file.Close()
	if readErr != nil {
		return h.failImportWatcherFile(dir, name, readErr.Error())
	}
	if int64(len(raw)) > maxImportWatcherFileBytes {
		return h.failImportWatcherFile(dir, name, "credential file exceeds the 8 MiB limit")
	}

	requests, batch, decodeErr := decodeImportCredentialsRequests(raw)
	if decodeErr != nil {
		return h.failImportWatcherFile(dir, name, "invalid credential JSON")
	}
	if !batch {
		batch = true
	}
	if len(requests) == 0 || len(requests) > maxCredentialImportBatch {
		return h.failImportWatcherFile(dir, name, fmt.Sprintf("credential count must be between 1 and %d", maxCredentialImportBatch))
	}
	if err := validateCredentialImportBatch(requests); err != nil {
		return h.failImportWatcherFile(dir, name, err.Error())
	}

	result := h.prepareImportWatcherBatch(requests)
	if len(result.Recoveries) > 0 {
		if err := writeImportWatcherRecoveries(dir, name, result.Recoveries); err != nil {
			result.Errors = append(result.Errors, "could not write credential recovery file")
		}
	}
	if len(result.Errors) > 0 {
		result.Failed = len(result.Errors)
		return h.moveImportWatcherFile(dir, name, "failed", strings.Join(result.Errors, "; "), result)
	}
	return h.moveImportWatcherFile(dir, name, "processed", "", result)
}

func (h *Handler) prepareImportWatcherBatch(requests []importCredentialsRequest) importWatcherFileResult {
	result := importWatcherFileResult{}
	prepared := make([]config.Account, len(requests))
	preparedOK := make([]bool, len(requests))
	recoveries := make([]importWatcherRecovery, 0)
	errors := make([]string, 0)
	var resultMu sync.Mutex
	concurrency := config.GetAutoRefreshConfig().RefreshConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 20 {
		concurrency = 20
	}
	if concurrency > len(requests) {
		concurrency = len(requests)
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				recovery := credentialImportRecovery{}
				refreshContext := context.Background()
				if h.backgroundCtx != nil {
					refreshContext = h.backgroundCtx
				}
				account, importErr := h.prepareCredentialsAccountContextWithRecovery(refreshContext, requests[index], &recovery)
				if importErr != nil {
					resultMu.Lock()
					errors = append(errors, fmt.Sprintf("account %d: %s", index+1, truncateImportWatcherError(importErr.Error())))
					if recovery.rotatedRefreshToken != "" {
						recoveries = append(recoveries, importWatcherRecovery{Index: index + 1, RotatedRefreshToken: recovery.rotatedRefreshToken})
					}
					resultMu.Unlock()
					continue
				}
				prepared[index] = account
				preparedOK[index] = true
			}
		}()
	}
	for index := range requests {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	accounts := make([]config.Account, 0, len(prepared))
	for index, account := range prepared {
		if preparedOK[index] {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) > 0 {
		if _, err := config.UpsertAccountsByIdentity(accounts); err != nil {
			errors = append(errors, "batch persist failed: "+truncateImportWatcherError(err.Error()))
		} else {
			if h.pool != nil {
				h.pool.Reload()
			}
			result.Imported = len(accounts)
		}
	}
	result.Errors = errors
	result.Recoveries = recoveries
	return result
}

func (h *Handler) failImportWatcherFile(dir, name, reason string) importWatcherFileResult {
	result := importWatcherFileResult{Failed: 1, Errors: []string{truncateImportWatcherError(reason)}}
	return h.moveImportWatcherFile(dir, name, "failed", reason, result)
}

func (h *Handler) moveImportWatcherFile(dir, name, subdir, reason string, result importWatcherFileResult) importWatcherFileResult {
	destDir := filepath.Join(dir, subdir)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		result.Errors = append(result.Errors, "could not create archive directory")
		return result
	}
	_ = os.Chmod(destDir, 0o700)
	src := filepath.Join(dir, name)
	dest := filepath.Join(destDir, name)
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		dest = filepath.Join(destDir, base+"."+time.Now().Format("20060102T150405.000000000")+ext)
	}
	if err := os.Rename(src, dest); err != nil {
		result.Errors = append(result.Errors, "could not archive credential file")
		return result
	}
	_ = os.Chmod(dest, 0o600)
	if reason != "" {
		sidecar := dest + ".error.txt"
		_ = os.WriteFile(sidecar, []byte(truncateImportWatcherError(redactDiagnosticText(reason))+"\n"), 0o600)
		_ = os.Chmod(sidecar, 0o600)
	}
	if result.Imported > 0 {
		logger.Infof("[Import] imported %d account(s) from %s", result.Imported, name)
	}
	return result
}

func writeImportWatcherRecoveries(dir, name string, recoveries []importWatcherRecovery) error {
	if len(recoveries) == 0 {
		return nil
	}
	destDir := filepath.Join(dir, "failed")
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(destDir, 0o700)
	data, err := json.MarshalIndent(map[string]interface{}{"recoveries": recoveries}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(destDir, name+".recovery.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_ = file.Chmod(0o600)
	_, err = file.Write(data)
	return err
}

func truncateImportWatcherError(value string) string {
	value = truncateDiagnosticText(redactDiagnosticText(value), maxImportWatcherErrorBytes)
	return value
}

func (h *Handler) apiGetImportWatcher(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(h.importWatcherSnapshot())
}

func (h *Handler) apiUpdateImportWatcher(w http.ResponseWriter, r *http.Request) {
	var request config.ImportWatcherConfig
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if request.IntervalSeconds < 5 || request.IntervalSeconds > 3600 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "intervalSeconds must be between 5 and 3600"})
		return
	}
	if err := config.UpdateImportWatcherConfig(request); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": h.importWatcherSnapshot()})
}

func (h *Handler) apiScanImportWatcher(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.importWatcher == nil || h.importWatcher.running.Load() {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "import scan is already running"})
		return
	}
	if !h.startBackgroundTask(h.scanImportDirectory) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "service is shutting down"})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": h.importWatcherSnapshot()})
}
