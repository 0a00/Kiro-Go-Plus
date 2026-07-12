//go:build linux

package instancelock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct {
	file *os.File
}

func Acquire(dir string) (*Lock, error) {
	path := filepath.Join(dir, ".kiro-go.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("data directory is already in use by another Kiro-Go process")
	}
	if err := file.Truncate(0); err == nil {
		_, _ = file.Seek(0, 0)
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
		_ = file.Sync()
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
