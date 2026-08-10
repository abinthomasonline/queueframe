//go:build windows

package processlock

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type Lock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func Acquire(path string) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &Lock{file: file}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("another Queueframe process holds %s: %w", path, err)
	}
	return lock, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	return l.file.Close()
}
