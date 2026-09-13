package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prepareImportWatcherTest(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{
		pool:          accountpool.GetPool(),
		backgroundCtx: context.Background(),
		importWatcher: newImportWatcherState(),
	}
	if err := os.MkdirAll(importWatcherDirectory(), 0o700); err != nil {
		t.Fatalf("mkdir imports: %v", err)
	}
	return h
}

func backdateImportWatcherFile(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-importWatcherStableAge - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate %s: %v", path, err)
	}
}

func TestImportWatcherMovesInvalidFilesAndKeepsPermissionsTight(t *testing.T) {
	h := prepareImportWatcherTest(t)
	path := filepath.Join(importWatcherDirectory(), "broken.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdateImportWatcherFile(t, path)
	h.scanImportDirectory()

	dest := filepath.Join(importWatcherDirectory(), "failed", "broken.json")
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("failed archive missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archived credential mode=%o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(dest + ".error.txt"); err != nil {
		t.Fatalf("error sidecar missing: %v", err)
	}
}

func TestImportWatcherImportsApiKeyFileAndArchivesIt(t *testing.T) {
	h := prepareImportWatcherTest(t)
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1")
	stubKiroAPIKeyProbe(t, func(_ context.Context, key, region, _ string) (*config.AccountInfo, error) {
		if key != "ksk_watch" || region != "us-east-1" {
			t.Fatalf("unexpected probe key/region: %s/%s", key, region)
		}
		return &config.AccountInfo{
			Email: "watch@example.invalid", UserId: "watch-user",
			SubscriptionType: "POWER", UsageLimit: 1000,
		}, nil
	})

	path := filepath.Join(importWatcherDirectory(), "accounts.json")
	body, _ := json.Marshal([]map[string]string{{"authMethod": "api_key", "kiroApiKey": "ksk_watch"}})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	backdateImportWatcherFile(t, path)
	h.scanImportDirectory()

	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].KiroApiKey != "ksk_watch" {
		t.Fatalf("unexpected imported accounts: %+v", accounts)
	}
	if _, err := os.Stat(filepath.Join(importWatcherDirectory(), "processed", "accounts.json")); err != nil {
		t.Fatalf("processed archive missing: %v", err)
	}
}

func TestImportWatcherSkipsFreshAndSymlinkFiles(t *testing.T) {
	h := prepareImportWatcherTest(t)
	fresh := filepath.Join(importWatcherDirectory(), "fresh.json")
	if err := os.WriteFile(fresh, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(importWatcherDirectory(), "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h.scanImportDirectory()
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh file was not deferred: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was not left untouched: %v", err)
	}
	if status := h.importWatcherSnapshot(); status.LastSkipped < 2 {
		t.Fatalf("status=%+v, expected fresh and symlink skips", status)
	}
}

func TestImportWatcherRecoveryFileIsBoundedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	recoveries := []importWatcherRecovery{{Index: 1, RotatedRefreshToken: strings.Repeat("r", 32)}}
	if err := writeImportWatcherRecoveries(dir, "credentials.json", recoveries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "failed", "credentials.json.recovery.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery file mode=%o, want 600", info.Mode().Perm())
	}
}
