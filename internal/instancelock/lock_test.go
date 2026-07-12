package instancelock

import "testing"

func TestAcquirePreventsConcurrentDataDirectoryUse(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := Acquire(dir); err == nil {
		t.Fatal("expected second acquire to fail")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}
