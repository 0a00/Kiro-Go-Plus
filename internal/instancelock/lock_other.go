//go:build !linux

package instancelock

import (
	"fmt"
	"os"
	"path/filepath"
)

type Lock struct {
	file *os.File
	path string
}

func Acquire(dir string) (*Lock, error) {
	path := filepath.Join(dir, ".kiro-go.lock.exclusive")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("data directory is already in use by another Kiro-Go process")
		}
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	return &Lock{file: file, path: path}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
	return err
}
