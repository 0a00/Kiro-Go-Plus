package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogArchiveWritesJSONLWithSecurePermissions(t *testing.T) {
	dir := t.TempDir()
	archive := newLogArchiveWithRotateBytes(config.LogArchiveConfig{
		Enabled:        true,
		IncludeDetails: true,
		RetentionDays:  0,
		MaxBytes:       config.MinLogArchiveMaxBytes,
	}, dir, 220)
	t.Cleanup(archive.Close)
	fakeKey := "sk-" + strings.Repeat("x", 24)

	archive.appendRequest(requestLogEntry{
		RequestID:    "req-1",
		Protocol:     "claude.messages",
		Model:        "claude-sonnet-4.6",
		AccountEmail: "operator@example.com",
		Error:        fakeKey,
	})
	detail := requestDetail{
		Version:   requestDetailStateVersion,
		RequestID: "req-1",
		Protocol:  "claude.messages",
		Request: requestDetailRequest{
			BodyJSON: `{"accessToken":"should-not-be-stored-in-raw-form"}`,
		},
		Response: requestDetailResponse{
			VisibleOutput: "data:image/png;base64,aGVsbG8gd29ybGQ=",
		},
	}
	archive.appendDetail(detail)
	archive.appendDiagnostic(diagnosticLogEntry{
		RequestID: "req-1",
		Error:     "diagnostic " + fakeKey,
	})
	if err := archive.Flush(); err != nil {
		t.Fatalf("flush archive: %v", err)
	}

	files, err := listLogArchiveFiles(dir)
	if err != nil {
		t.Fatalf("list archive files: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("expected rotation to create at least two files, got %d", len(files))
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat archive directory: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("archive directory mode = %o, want 700", got)
	}

	seenKinds := make(map[string]bool)
	for _, file := range files {
		if got := file.modTime; got.IsZero() {
			t.Fatalf("archive file has no modification time: %+v", file)
		}
		if info, err := os.Stat(file.path); err != nil {
			t.Fatalf("stat archive file: %v", err)
		} else if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("archive file mode = %o, want 600", got)
		}
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("read archive file: %v", err)
		}
		if strings.Contains(string(data), fakeKey) || strings.Contains(string(data), "should-not-be-stored-in-raw-form") || strings.Contains(string(data), "aGVsbG8gd29ybGQ=") {
			t.Fatal("archive contains an unsanitized secret")
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var record logArchiveRecord
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("invalid JSONL record: %v; line=%s", err, line)
			}
			seenKinds[record.Kind] = true
		}
	}
	for _, kind := range []string{"request", "detail", "diagnostic"} {
		if !seenKinds[kind] {
			t.Fatalf("archive did not contain %s record", kind)
		}
	}
	archive.Close()
	restored := newLogArchive(config.LogArchiveConfig{
		Enabled:        true,
		IncludeDetails: true,
		RetentionDays:  0,
		MaxBytes:       config.MinLogArchiveMaxBytes,
	}, dir)
	if status := restored.Status(); status.FileCount < 2 || status.TotalBytes <= 0 {
		t.Fatalf("archive was not recoverable after restart: %+v", status)
	}
	restored.Close()
}

func TestLogArchiveRetentionRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "events-00000000000000000001.jsonl")
	newPath := filepath.Join(dir, "events-00000000000000000002.jsonl")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write old archive: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("write new archive: %v", err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("set old archive time: %v", err)
	}

	archive := newLogArchive(config.LogArchiveConfig{
		Enabled:       true,
		RetentionDays: 1,
		MaxBytes:      config.MinLogArchiveMaxBytes,
	}, dir)
	t.Cleanup(archive.Close)
	archive.Close()
	archive.cleanup()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired archive still exists, stat error=%v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("recent archive was removed: %v", err)
	}
}

func TestLogArchiveCapacityRemovesOldestFiles(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "events-00000000000000000001.jsonl"),
		filepath.Join(dir, "events-00000000000000000002.jsonl"),
		filepath.Join(dir, "events-00000000000000000003.jsonl"),
	}
	baseTime := time.Unix(1000, 0)
	for index, path := range paths {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
			t.Fatalf("write archive fixture: %v", err)
		}
		if err := os.Chtimes(path, baseTime.Add(time.Duration(index)*time.Second), baseTime.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("set archive fixture time: %v", err)
		}
	}
	archive := newLogArchive(config.LogArchiveConfig{Enabled: true, MaxBytes: config.MinLogArchiveMaxBytes}, dir)
	t.Cleanup(archive.Close)
	archive.Close()
	archive.configMu.Lock()
	archive.cfg.MaxBytes = 200
	archive.configMu.Unlock()
	archive.cleanup()

	files, err := listLogArchiveFiles(dir)
	if err != nil {
		t.Fatalf("list archive after capacity cleanup: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("capacity cleanup kept %d files, want 2", len(files))
	}
	if files[0].name != filepath.Base(paths[1]) || files[1].name != filepath.Base(paths[2]) {
		t.Fatalf("capacity cleanup kept wrong files: %+v", files)
	}
}

func TestLogArchiveConcurrentWritesAndClear(t *testing.T) {
	dir := t.TempDir()
	archive := newLogArchive(config.LogArchiveConfig{
		Enabled:  true,
		MaxBytes: config.MinLogArchiveMaxBytes,
	}, dir)
	t.Cleanup(archive.Close)
	const writers = 16
	const recordsPerWriter = 20
	var group sync.WaitGroup
	group.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func(index int) {
			defer group.Done()
			for record := 0; record < recordsPerWriter; record++ {
				archive.appendValue("request", "", map[string]interface{}{
					"writer": index,
					"record": record,
				}, false)
			}
		}(writer)
	}
	group.Wait()
	if err := archive.Flush(); err != nil {
		t.Fatalf("flush concurrent archive: %v", err)
	}
	status := archive.Status()
	if status.WrittenRecordsSinceStart != writers*recordsPerWriter {
		t.Fatalf("written records = %d, want %d; status=%+v", status.WrittenRecordsSinceStart, writers*recordsPerWriter, status)
	}
	deleted, err := archive.Clear()
	if err != nil {
		t.Fatalf("clear archive: %v", err)
	}
	if deleted == 0 || archive.Status().FileCount != 0 {
		t.Fatalf("clear did not remove archive files: deleted=%d status=%+v", deleted, archive.Status())
	}
	archive.Close()
}

func TestLogArchiveDisabledDoesNotCreateFiles(t *testing.T) {
	dir := t.TempDir()
	archive := newLogArchive(config.LogArchiveConfig{Enabled: false}, dir)
	t.Cleanup(archive.Close)
	archive.appendValue("request", "req", map[string]string{"value": "ignored"}, false)
	if err := archive.Flush(); err != nil {
		t.Fatalf("flush disabled archive: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read disabled archive directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled archive created files: %+v", entries)
	}
	archive.Close()
}

func TestLogArchiveAdminConfigAPI(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{}
	t.Cleanup(func() {
		if h.logArchive != nil {
			h.logArchive.Close()
		}
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/log-archive", strings.NewReader(`{
		"enabled":true,
		"includeDetails":true,
		"retentionDays":0,
		"maxBytes":67108864
	}`))
	h.apiUpdateLogArchive(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool             `json:"success"`
		Status  LogArchiveStatus `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if !response.Success || !response.Status.Config.Enabled || !response.Status.Config.IncludeDetails || response.Status.Config.RetentionDays != 0 {
		t.Fatalf("unexpected archive API response: %+v", response)
	}
	getRecorder := httptest.NewRecorder()
	h.apiGetLogArchive(getRecorder, httptest.NewRequest(http.MethodGet, "/log-archive", nil))
	if getRecorder.Code != http.StatusOK || !strings.Contains(getRecorder.Body.String(), `"enabled":true`) {
		t.Fatalf("get archive config failed: status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	h.logArchive.appendRequest(requestLogEntry{RequestID: "download-request", Protocol: "test"})
	if err := h.logArchive.Flush(); err != nil {
		t.Fatalf("flush archive before download: %v", err)
	}
	downloadRecorder := httptest.NewRecorder()
	h.apiDownloadLogArchive(downloadRecorder, httptest.NewRequest(http.MethodGet, "/log-archive/download", nil))
	if downloadRecorder.Code != http.StatusOK || downloadRecorder.Header().Get("Content-Type") != "application/x-ndjson" ||
		!strings.Contains(downloadRecorder.Body.String(), `"kind":"request"`) {
		t.Fatalf("download archive failed: status=%d headers=%v body=%s", downloadRecorder.Code, downloadRecorder.Header(), downloadRecorder.Body.String())
	}
}

func TestHandlerLogArchiveHooksPersistAllRecordKinds(t *testing.T) {
	configDir := t.TempDir()
	if err := config.Init(filepath.Join(configDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateDiagnosticConfig(config.DiagnosticConfig{Enabled: true, IncludeRequestSummary: true, MaxEntries: 10}); err != nil {
		t.Fatalf("enable diagnostics: %v", err)
	}
	archiveDir := filepath.Join(configDir, "archive")
	archiveConfig := config.LogArchiveConfig{
		Enabled:        true,
		IncludeDetails: true,
		RetentionDays:  0,
		MaxBytes:       config.MinLogArchiveMaxBytes,
	}
	if err := config.UpdateLogArchiveConfig(archiveConfig); err != nil {
		t.Fatalf("enable archive: %v", err)
	}
	archive := newLogArchive(archiveConfig, archiveDir)
	t.Cleanup(archive.Close)
	h := &Handler{
		requestLog:     newRequestLog(10),
		requestDetails: newRequestDetailStore(10, config.DefaultRequestDetailMaxBytes),
		logArchive:     archive,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test"}`))
	trace := newRequestDetailTrace(req, "test", []byte(`{"model":"test"}`), config.DefaultRequestDetailMaxBytes)
	trace.recordText("safe output", false)
	ctx := context.WithValue(req.Context(), requestDetailContextKey{}, trace)
	ctx = context.WithValue(ctx, requestIDContextKey{}, "hook-request")
	h.recordRequestLogForContext(ctx, requestLogEntry{
		RequestID:  "hook-request",
		Protocol:   "claude.messages",
		Model:      "claude-sonnet-4.6",
		Status:     "success",
		StatusCode: http.StatusOK,
	})
	h.recordDiagnosticFailure(diagnosticLogEntry{
		RequestID:  "hook-request",
		Protocol:   "claude.messages",
		StatusCode: http.StatusBadRequest,
		Error:      "diagnostic failure",
	})
	if err := archive.Flush(); err != nil {
		t.Fatalf("flush hooked archive: %v", err)
	}
	status := archive.Status()
	if status.WrittenRecordsSinceStart != 3 {
		t.Fatalf("hooked records = %d, want 3; status=%+v", status.WrittenRecordsSinceStart, status)
	}

	seen := make(map[string]bool)
	files, err := listLogArchiveFiles(archiveDir)
	if err != nil {
		t.Fatalf("list hooked archive: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("read hooked archive: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var record logArchiveRecord
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode hooked record: %v", err)
			}
			seen[record.Kind] = true
		}
	}
	for _, kind := range []string{"request", "detail", "diagnostic"} {
		if !seen[kind] {
			t.Fatalf("hook did not archive %s record", kind)
		}
	}
}
