package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var logArchiveDataURLPattern = regexp.MustCompile(`(?i)data:[^;\s]+;base64,[A-Za-z0-9+/=_-]+`)

const (
	logArchiveDirectoryName = "log-archive"
	logArchiveFilePrefix    = "events-"
	logArchiveFileSuffix    = ".jsonl"
	logArchiveRotateBytes   = int64(64 << 20)
	logArchiveQueueCapacity = 256
	logArchiveEnqueueWait   = 250 * time.Millisecond
	logArchiveFlushInterval = time.Second
	logArchiveCleanupPeriod = 5 * time.Minute
)

type logArchiveRecord struct {
	Version    int             `json:"version"`
	Kind       string          `json:"kind"`
	RecordedAt int64           `json:"recordedAt"`
	RequestID  string          `json:"requestId,omitempty"`
	Data       json.RawMessage `json:"data"`
}

type logArchiveItem struct {
	line           []byte
	includeDetails bool
}

type logArchiveCommand struct {
	kind   string
	result chan logArchiveCommandResult
}

type logArchiveCommandResult struct {
	deleted int
	err     error
}

type logArchiveFileInfo struct {
	path    string
	name    string
	size    int64
	modTime time.Time
}

// LogArchiveStatus is the operational snapshot exposed by the admin API.
type LogArchiveStatus struct {
	Config                   config.LogArchiveConfig `json:"config"`
	Directory                string                  `json:"directory"`
	FileCount                int                     `json:"fileCount"`
	TotalBytes               int64                   `json:"totalBytes"`
	OldestFileAt             int64                   `json:"oldestFileAt,omitempty"`
	NewestFileAt             int64                   `json:"newestFileAt,omitempty"`
	QueuedRecords            int                     `json:"queuedRecords"`
	WrittenRecordsSinceStart uint64                  `json:"writtenRecordsSinceStart"`
	DroppedRecords           uint64                  `json:"droppedRecords"`
	LastError                string                  `json:"lastError,omitempty"`
}

type logArchive struct {
	dir         string
	rotateBytes int64

	queue     chan logArchiveItem
	commands  chan logArchiveCommand
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool

	configMu sync.RWMutex
	cfg      config.LogArchiveConfig

	statusMu  sync.RWMutex
	lastError string

	written atomic.Uint64
	dropped atomic.Uint64

	file      *os.File
	filePath  string
	fileBytes int64
	fileDate  string
}

func logArchivePath() string {
	return filepath.Join(config.GetConfigDir(), logArchiveDirectoryName)
}

func newLogArchive(cfg config.LogArchiveConfig, dir string) *logArchive {
	return newLogArchiveWithRotateBytes(cfg, dir, logArchiveRotateBytes)
}

func newLogArchiveWithRotateBytes(cfg config.LogArchiveConfig, dir string, rotateBytes int64) *logArchive {
	if strings.TrimSpace(dir) == "" {
		dir = logArchivePath()
	}
	if rotateBytes <= 0 {
		rotateBytes = logArchiveRotateBytes
	}
	archive := &logArchive{
		dir:         filepath.Clean(dir),
		rotateBytes: rotateBytes,
		queue:       make(chan logArchiveItem, logArchiveQueueCapacity),
		commands:    make(chan logArchiveCommand, 8),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		cfg:         normalizeLogArchiveConfig(cfg),
	}
	go archive.run()
	if archive.configSnapshot().Enabled {
		select {
		case archive.commands <- logArchiveCommand{kind: "reconfigure"}:
		default:
		}
	}
	return archive
}

func normalizeLogArchiveConfig(value config.LogArchiveConfig) config.LogArchiveConfig {
	if value.RetentionDays < config.MinLogArchiveRetentionDays {
		value.RetentionDays = config.DefaultLogArchiveRetentionDays
	}
	if value.RetentionDays > config.MaxLogArchiveRetentionDays {
		value.RetentionDays = config.MaxLogArchiveRetentionDays
	}
	if value.MaxBytes < config.MinLogArchiveMaxBytes {
		value.MaxBytes = config.DefaultLogArchiveMaxBytes
	}
	if value.MaxBytes > config.MaxLogArchiveMaxBytes {
		value.MaxBytes = config.MaxLogArchiveMaxBytes
	}
	return value
}

func (a *logArchive) configSnapshot() config.LogArchiveConfig {
	if a == nil {
		return config.LogArchiveConfig{}
	}
	a.configMu.RLock()
	cfg := a.cfg
	a.configMu.RUnlock()
	return cfg
}

func (a *logArchive) Configure(cfg config.LogArchiveConfig) {
	if a == nil || a.closed.Load() {
		return
	}
	cfg = normalizeLogArchiveConfig(cfg)
	a.configMu.Lock()
	a.cfg = cfg
	a.configMu.Unlock()
	select {
	case a.commands <- logArchiveCommand{kind: "reconfigure"}:
	default:
	}
}

func (a *logArchive) appendValue(kind, requestID string, value interface{}, includeDetails bool) {
	if a == nil || a.closed.Load() {
		return
	}
	cfg := a.configSnapshot()
	if !cfg.Enabled || (includeDetails && !cfg.IncludeDetails) {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		a.recordError(fmt.Errorf("encode %s record: %w", kind, err))
		return
	}
	a.enqueue(kind, requestID, raw, includeDetails)
}

func (a *logArchive) appendRaw(kind, requestID string, raw json.RawMessage, includeDetails bool) {
	if a == nil || a.closed.Load() {
		return
	}
	cfg := a.configSnapshot()
	if !cfg.Enabled || (includeDetails && !cfg.IncludeDetails) {
		return
	}
	a.enqueue(kind, requestID, raw, includeDetails)
}

func (a *logArchive) enqueue(kind, requestID string, raw json.RawMessage, includeDetails bool) {
	record := logArchiveRecord{
		Version:    1,
		Kind:       strings.TrimSpace(kind),
		RecordedAt: time.Now().Unix(),
		RequestID:  strings.TrimSpace(requestID),
		Data:       append(json.RawMessage(nil), raw...),
	}
	line, err := json.Marshal(record)
	if err != nil {
		a.recordError(fmt.Errorf("encode archive record: %w", err))
		return
	}
	line = append(line, '\n')
	item := logArchiveItem{line: line, includeDetails: includeDetails}
	select {
	case <-a.stop:
		return
	default:
	}
	timer := time.NewTimer(logArchiveEnqueueWait)
	defer timer.Stop()
	select {
	case a.queue <- item:
	case <-a.stop:
	case <-timer.C:
		a.dropped.Add(1)
		a.recordError(fmt.Errorf("archive queue is full"))
	}
}

func (a *logArchive) run() {
	flushTicker := time.NewTicker(logArchiveFlushInterval)
	cleanupTicker := time.NewTicker(logArchiveCleanupPeriod)
	defer flushTicker.Stop()
	defer cleanupTicker.Stop()
	defer close(a.done)

	for {
		select {
		case item := <-a.queue:
			a.writeItem(item)
		case command := <-a.commands:
			a.handleCommand(command)
		case <-flushTicker.C:
			if err := a.syncFile(); err != nil {
				a.recordError(err)
			}
		case <-cleanupTicker.C:
			a.cleanup()
		case <-a.stop:
			a.drainAndClose()
			return
		}
	}
}

func (a *logArchive) drainAndClose() {
	for {
		select {
		case item := <-a.queue:
			a.writeItem(item)
		default:
			if err := a.syncFile(); err != nil {
				a.recordError(err)
			}
			a.closeFile()
			return
		}
	}
}

func (a *logArchive) handleCommand(command logArchiveCommand) {
	result := logArchiveCommandResult{}
	switch command.kind {
	case "reconfigure":
		a.drainQueue()
		if !a.configSnapshot().Enabled {
			if err := a.syncFile(); err != nil {
				a.recordError(err)
			}
			a.closeFile()
		} else {
			a.cleanup()
		}
	case "flush":
		a.drainQueue()
		result.err = a.syncFile()
	case "clear":
		a.discardQueue()
		result.deleted, result.err = a.clearFiles()
	default:
		result.err = fmt.Errorf("unknown archive command %q", command.kind)
	}
	if command.result != nil {
		command.result <- result
	}
}

func (a *logArchive) drainQueue() {
	for {
		select {
		case item := <-a.queue:
			a.writeItem(item)
		default:
			return
		}
	}
}

func (a *logArchive) discardQueue() {
	for {
		select {
		case <-a.queue:
			a.dropped.Add(1)
		default:
			return
		}
	}
}

func (a *logArchive) writeItem(item logArchiveItem) {
	cfg := a.configSnapshot()
	if !cfg.Enabled || (item.includeDetails && !cfg.IncludeDetails) {
		if !cfg.Enabled {
			a.closeFile()
		}
		return
	}
	if a.file == nil {
		if err := a.openFile(); err != nil {
			a.recordError(err)
			a.dropped.Add(1)
			return
		}
	}
	if a.fileDate != time.Now().UTC().Format("2006-01-02") {
		if err := a.syncFile(); err != nil {
			a.recordError(err)
		}
		a.closeFile()
		if err := a.openFile(); err != nil {
			a.recordError(err)
			a.dropped.Add(1)
			return
		}
	}
	if a.fileBytes > 0 && a.fileBytes+int64(len(item.line)) > a.rotateBytes {
		if err := a.syncFile(); err != nil {
			a.recordError(err)
		}
		a.closeFile()
		if err := a.openFile(); err != nil {
			a.recordError(err)
			a.dropped.Add(1)
			return
		}
	}
	written, err := a.file.Write(item.line)
	if err != nil {
		a.recordError(fmt.Errorf("write archive: %w", err))
		a.dropped.Add(1)
		a.closeFile()
		return
	}
	a.fileBytes += int64(written)
	if written != len(item.line) {
		a.recordError(fmt.Errorf("short archive write: %d/%d bytes", written, len(item.line)))
		a.dropped.Add(1)
		a.closeFile()
		return
	}
	a.written.Add(1)
	if a.fileBytes >= a.rotateBytes {
		if err := a.syncFile(); err != nil {
			a.recordError(err)
		}
		a.closeFile()
		a.cleanup()
	}
}

func (a *logArchive) openFile() error {
	if a.file != nil {
		return nil
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return fmt.Errorf("create log archive directory: %w", err)
	}
	if err := os.Chmod(a.dir, 0o700); err != nil {
		return fmt.Errorf("secure log archive directory: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	for attempt := int64(0); attempt < 100; attempt++ {
		name := fmt.Sprintf("%s%020d%s", logArchiveFilePrefix, now+attempt, logArchiveFileSuffix)
		path := filepath.Join(a.dir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return fmt.Errorf("open log archive: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("secure log archive file: %w", err)
		}
		a.file = file
		a.filePath = path
		a.fileBytes = 0
		a.fileDate = time.Now().UTC().Format("2006-01-02")
		return nil
	}
	return fmt.Errorf("could not allocate a unique log archive filename")
}

func (a *logArchive) syncFile() error {
	if a == nil || a.file == nil {
		return nil
	}
	if err := a.file.Sync(); err != nil {
		return fmt.Errorf("sync log archive: %w", err)
	}
	return nil
}

func (a *logArchive) closeFile() {
	if a == nil || a.file == nil {
		return
	}
	if err := a.file.Close(); err != nil {
		a.recordError(fmt.Errorf("close log archive: %w", err))
	}
	a.file = nil
	a.filePath = ""
	a.fileBytes = 0
	a.fileDate = ""
}

func (a *logArchive) cleanup() {
	if a == nil {
		return
	}
	cfg := a.configSnapshot()
	if !cfg.Enabled {
		return
	}
	files, err := listLogArchiveFiles(a.dir)
	if err != nil {
		a.recordError(err)
		return
	}
	now := time.Now()
	if cfg.RetentionDays > 0 {
		cutoff := now.Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
		for _, file := range files {
			if file.modTime.Before(cutoff) {
				a.removeFile(file)
			}
		}
		updated, listErr := listLogArchiveFiles(a.dir)
		if listErr != nil {
			a.recordError(listErr)
			return
		}
		files = updated
	}
	var total int64
	for _, file := range files {
		total += file.size
	}
	for total > cfg.MaxBytes && len(files) > 0 {
		file := files[0]
		files = files[1:]
		total -= file.size
		a.removeFile(file)
	}
}

func (a *logArchive) removeFile(file logArchiveFileInfo) {
	if file.path == a.filePath {
		a.closeFile()
	}
	if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
		a.recordError(fmt.Errorf("remove archived log: %w", err))
	}
}

func (a *logArchive) clearFiles() (int, error) {
	a.closeFile()
	files, err := listLogArchiveFiles(a.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	deleted := 0
	for _, file := range files {
		if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			return deleted, fmt.Errorf("clear archived logs: %w", err)
		}
		deleted++
	}
	a.written.Store(0)
	a.dropped.Store(0)
	a.statusMu.Lock()
	a.lastError = ""
	a.statusMu.Unlock()
	return deleted, nil
}

func listLogArchiveFiles(dir string) ([]logArchiveFileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list log archive: %w", err)
	}
	files := make([]logArchiveFileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isLogArchiveFilename(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, logArchiveFileInfo{
			path:    filepath.Join(dir, entry.Name()),
			name:    entry.Name(),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name < files[j].name
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	return files, nil
}

func isLogArchiveFilename(name string) bool {
	if !strings.HasPrefix(name, logArchiveFilePrefix) || !strings.HasSuffix(name, logArchiveFileSuffix) {
		return false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, logArchiveFilePrefix), logArchiveFileSuffix)
	if len(value) != 20 {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

func (a *logArchive) recordError(err error) {
	if a == nil || err == nil {
		return
	}
	message := truncateDiagnosticText(redactRequestDetailText(err.Error()), 1000)
	a.statusMu.Lock()
	alreadySame := a.lastError == message
	a.lastError = message
	a.statusMu.Unlock()
	if !alreadySame {
		logger.Warnf("[LogArchive] %s", message)
	}
}

func (a *logArchive) Status() LogArchiveStatus {
	if a == nil {
		return LogArchiveStatus{Config: config.GetLogArchiveConfig(), Directory: logArchivePath()}
	}
	cfg := a.configSnapshot()
	files, err := listLogArchiveFiles(a.dir)
	if err != nil {
		a.recordError(err)
	}
	var total int64
	var oldest, newest int64
	for _, file := range files {
		total += file.size
		stamp := file.modTime.Unix()
		if oldest == 0 || stamp < oldest {
			oldest = stamp
		}
		if stamp > newest {
			newest = stamp
		}
	}
	a.statusMu.RLock()
	lastError := a.lastError
	a.statusMu.RUnlock()
	return LogArchiveStatus{
		Config:                   cfg,
		Directory:                a.dir,
		FileCount:                len(files),
		TotalBytes:               total,
		OldestFileAt:             oldest,
		NewestFileAt:             newest,
		QueuedRecords:            len(a.queue),
		WrittenRecordsSinceStart: a.written.Load(),
		DroppedRecords:           a.dropped.Load(),
		LastError:                lastError,
	}
}

func (a *logArchive) Flush() error {
	if a == nil || a.closed.Load() {
		return nil
	}
	result := make(chan logArchiveCommandResult, 1)
	select {
	case a.commands <- logArchiveCommand{kind: "flush", result: result}:
	case <-a.stop:
		return nil
	}
	select {
	case value := <-result:
		return value.err
	case <-a.stop:
		return nil
	}
}

func (a *logArchive) Clear() (int, error) {
	if a == nil || a.closed.Load() {
		return 0, nil
	}
	result := make(chan logArchiveCommandResult, 1)
	select {
	case a.commands <- logArchiveCommand{kind: "clear", result: result}:
	case <-a.stop:
		return 0, nil
	}
	select {
	case value := <-result:
		return value.deleted, value.err
	case <-a.stop:
		return 0, nil
	}
}

func (a *logArchive) CopyTo(w io.Writer) error {
	if a == nil || w == nil {
		return nil
	}
	if err := a.Flush(); err != nil {
		return err
	}
	files, err := listLogArchiveFiles(a.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		input, err := os.Open(file.path)
		if err != nil {
			return fmt.Errorf("open archived log for download: %w", err)
		}
		_, copyErr := io.Copy(w, input)
		closeErr := input.Close()
		if copyErr != nil {
			return fmt.Errorf("read archived log for download: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archived log download: %w", closeErr)
		}
	}
	return nil
}

func (a *logArchive) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		close(a.stop)
		<-a.done
	})
}

func (h *Handler) ensureLogArchive() *logArchive {
	if h == nil {
		return nil
	}
	cfg := config.GetLogArchiveConfig()
	h.logArchiveMu.Lock()
	defer h.logArchiveMu.Unlock()
	if h.logArchive == nil {
		h.logArchive = newLogArchive(cfg, logArchivePath())
	} else if h.logArchive.configSnapshot() != cfg {
		h.logArchive.Configure(cfg)
	}
	return h.logArchive
}

func (h *Handler) currentLogArchive() *logArchive {
	if h == nil {
		return nil
	}
	h.logArchiveMu.Lock()
	archive := h.logArchive
	h.logArchiveMu.Unlock()
	return archive
}

func (h *Handler) archiveRequestLog(entry requestLogEntry) {
	if h == nil || !config.GetLogArchiveConfig().Enabled {
		return
	}
	archive := h.ensureLogArchive()
	if archive == nil {
		return
	}
	archive.appendRequest(entry)
}

func (h *Handler) archiveRequestDetail(detail requestDetail) {
	if h == nil || !config.GetLogArchiveConfig().Enabled || !config.GetLogArchiveConfig().IncludeDetails {
		return
	}
	archive := h.ensureLogArchive()
	if archive == nil {
		return
	}
	archive.appendDetail(detail)
}

func (a *logArchive) appendDetail(detail requestDetail) {
	if a == nil || a.closed.Load() {
		return
	}
	detail = boundRequestDetail(sanitizeLogArchiveDetail(detail), config.MaxRequestDetailMaxBytes)
	raw, err := json.Marshal(detail)
	if err != nil {
		a.recordError(fmt.Errorf("encode request detail: %w", err))
		return
	}
	a.appendRaw("detail", detail.RequestID, raw, true)
}

func (a *logArchive) appendRequest(entry requestLogEntry) {
	if a == nil {
		return
	}
	if entry.RequestToolNames != nil {
		entry.RequestToolNames = append([]string(nil), entry.RequestToolNames...)
		for i := range entry.RequestToolNames {
			entry.RequestToolNames[i] = truncateDiagnosticText(redactDiagnosticText(entry.RequestToolNames[i]), 200)
		}
	}
	// Request metadata is safe to retain after redacting identity and error text.
	entry.AccountEmail = redactDiagnosticText(entry.AccountEmail)
	entry.APIKeyName = redactDiagnosticText(entry.APIKeyName)
	entry.Error = truncateDiagnosticText(redactDiagnosticText(entry.Error), 4000)
	a.appendValue("request", entry.RequestID, entry, false)
}

func (a *logArchive) appendDiagnostic(entry diagnosticLogEntry) {
	if a == nil {
		return
	}
	if !a.configSnapshot().IncludeDetails {
		entry.RequestSummary = ""
	}
	entry.AccountEmail = redactDiagnosticText(entry.AccountEmail)
	entry.Error = truncateDiagnosticText(redactDiagnosticText(entry.Error), 2000)
	entry.RequestSummary = truncateDiagnosticText(redactDiagnosticText(entry.RequestSummary), 4000)
	a.appendValue("diagnostic", entry.RequestID, entry, false)
}

func sanitizeLogArchiveDetail(detail requestDetail) requestDetail {
	redact := func(value string) string {
		value = redactDiagnosticText(redactRequestDetailText(value))
		return logArchiveDataURLPattern.ReplaceAllString(value, "[binary payload redacted]")
	}
	if detail.Request.Headers != nil {
		headers := make(map[string]string, len(detail.Request.Headers))
		for key, value := range detail.Request.Headers {
			headers[key] = redact(value)
		}
		detail.Request.Headers = headers
	}
	if detail.Attempts != nil {
		detail.Attempts = append([]requestDetailAttempt(nil), detail.Attempts...)
	}
	detail.AccountEmail = redact(detail.AccountEmail)
	detail.Request.BodyJSON = sanitizeLogArchiveBody(detail.Request.BodyJSON)
	detail.Response.VisibleOutput = redact(detail.Response.VisibleOutput)
	detail.Response.ThinkingOutput = redact(detail.Response.ThinkingOutput)
	detail.Response.Error = redact(detail.Response.Error)
	for i := range detail.Attempts {
		detail.Attempts[i].AccountEmail = redact(detail.Attempts[i].AccountEmail)
		detail.Attempts[i].Error = redact(detail.Attempts[i].Error)
	}
	return detail
}

func sanitizeLogArchiveBody(body string) string {
	if strings.TrimSpace(body) == "" {
		return body
	}
	cleaned, _ := sanitizeRequestDetailBody([]byte(body), config.MaxRequestDetailMaxBytes)
	if cleaned == "" {
		cleaned = redactDiagnosticText(redactRequestDetailText(body))
	}
	return logArchiveDataURLPattern.ReplaceAllString(cleaned, "[binary payload redacted]")
}

func (h *Handler) archiveDiagnostic(entry diagnosticLogEntry) {
	if h == nil || !config.GetLogArchiveConfig().Enabled {
		return
	}
	archive := h.ensureLogArchive()
	if archive == nil {
		return
	}
	archive.appendDiagnostic(entry)
}

func (h *Handler) apiGetLogArchive(w http.ResponseWriter, _ *http.Request) {
	status := h.ensureLogArchive().Status()
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) apiUpdateLogArchive(w http.ResponseWriter, r *http.Request) {
	var request config.LogArchiveConfig
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if request.RetentionDays < config.MinLogArchiveRetentionDays || request.RetentionDays > config.MaxLogArchiveRetentionDays {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("retentionDays must be between %d and %d", config.MinLogArchiveRetentionDays, config.MaxLogArchiveRetentionDays)})
		return
	}
	if request.MaxBytes < config.MinLogArchiveMaxBytes || request.MaxBytes > config.MaxLogArchiveMaxBytes {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("maxBytes must be between %d and %d", config.MinLogArchiveMaxBytes, config.MaxLogArchiveMaxBytes)})
		return
	}
	if err := config.UpdateLogArchiveConfig(request); err != nil {
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	archive := h.ensureLogArchive()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"status":  archive.Status(),
	})
}

func (h *Handler) apiClearLogArchive(w http.ResponseWriter, _ *http.Request) {
	archive := h.ensureLogArchive()
	deleted, err := archive.Clear()
	if err != nil {
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"deleted": deleted,
		"status":  archive.Status(),
	})
}

func (h *Handler) apiDownloadLogArchive(w http.ResponseWriter, _ *http.Request) {
	archive := h.ensureLogArchive()
	if err := archive.Flush(); err != nil {
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="kiro-log-archive.jsonl"`)
	if err := archive.CopyTo(w); err != nil {
		archive.recordError(err)
	}
}
