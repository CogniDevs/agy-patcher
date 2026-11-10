//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// InstanceLock holds an advisory flock on daemon.lock on Unix systems.
type InstanceLock struct {
	file *os.File
	path string
}

// AcquireLock acquires a non-blocking exclusive flock on daemon.lock in StateDir.
func AcquireLock() (*InstanceLock, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "daemon.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &InstanceLock{file: f, path: lockPath}, nil
}

// Release releases the flock and closes daemon.lock.
func (l *InstanceLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

// IsLocked reports whether another daemon currently holds daemon.lock.
func IsLocked() bool {
	lock, err := AcquireLock()
	if err != nil {
		return errors.Is(err, ErrAlreadyRunning)
	}
	lock.Release()
	return false
}

// IsPIDAlive checks whether pid exists on Unix.
func IsPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ForceStopPID terminates an unresponsive background daemon process by PID.
func ForceStopPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
