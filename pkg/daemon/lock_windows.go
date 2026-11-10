//go:build windows

package daemon

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	winInstanceMutex = `Local\AgyPatcherDaemon_v1`
	stillActive      = 259
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW       = kernel32.NewProc("CreateMutexW")
	procOpenMutexW         = kernel32.NewProc("OpenMutexW")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
)

// InstanceLock holds the single-instance daemon mutex on Windows.
type InstanceLock struct {
	handle syscall.Handle
}

// AcquireLock claims the global single-instance daemon/tray mutex.
func AcquireLock() (*InstanceLock, error) {
	namePtr, err := syscall.UTF16PtrFromString(winInstanceMutex)
	if err != nil {
		return nil, err
	}
	h, _, lastErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return nil, lastErr
	}
	if errors.Is(lastErr, syscall.Errno(183)) { // ERROR_ALREADY_EXISTS
		_ = syscall.CloseHandle(syscall.Handle(h))
		return nil, ErrAlreadyRunning
	}
	return &InstanceLock{handle: syscall.Handle(h)}, nil
}

// Release frees the single-instance mutex.
func (l *InstanceLock) Release() {
	if l != nil && l.handle != 0 {
		_ = syscall.CloseHandle(syscall.Handle(hCast(l.handle)))
		l.handle = 0
	}
}

func hCast(h syscall.Handle) uintptr {
	return uintptr(h)
}

// IsLocked reports whether another daemon/tray holds the Windows instance mutex.
func IsLocked() bool {
	namePtr, err := syscall.UTF16PtrFromString(winInstanceMutex)
	if err != nil {
		return false
	}
	const synchronize = 0x00100000
	h, _, _ := procOpenMutexW.Call(synchronize, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return false
	}
	_ = syscall.CloseHandle(syscall.Handle(h))
	return true
}

// IsPIDAlive checks whether a Windows process with pid is still running.
func IsPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInfo = 0x1000
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	var code uint32
	r1, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	return r1 != 0 && code == stillActive
}

// ForceStopPID terminates an unresponsive background daemon process by PID.
func ForceStopPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	const processTerminate = 0x0001
	h, _, err := kernel32.NewProc("OpenProcess").Call(processTerminate, 0, uintptr(pid))
	if h == 0 {
		return err
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	r1, _, termErr := kernel32.NewProc("TerminateProcess").Call(h, 1)
	if r1 == 0 {
		return termErr
	}
	return nil
}
