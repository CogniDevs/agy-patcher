package tray

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agy-patcher/pkg/daemon"
)

var ErrAlreadyRunning = errors.New("tray instance is already running")

// LaunchOptions controls tray/daemon runtime behavior.
type LaunchOptions struct {
	Version    string
	Lang       string
	StartProxy bool
}

// EnsureDaemon attaches to an already-running daemon (when matching current executable),
// recovers from a wedged daemon holding the instance lock, or spawns a fresh daemon.
func EnsureDaemon(opts LaunchOptions) (*daemon.Client, *daemon.StateSnapshot, error) {
	curExe, _ := os.Executable()
	if cl, snap, err := daemon.Connect(); err == nil {
		versionMatch := opts.Version == "" || snap.Version == "" || snap.Version == opts.Version
		if versionMatch && daemon.SameExecutable(curExe, snap.Exe) {
			if opts.StartProxy && !snap.ProxyOwned {
				if updated, pErr := cl.StartProxy(); pErr == nil && updated != nil {
					snap = updated
				}
			}
			return cl, snap, nil
		}
		_ = cl.Shutdown()
		waitUnlocked(2 * time.Second)
	}

	// Self-heal if a prior daemon holds the instance lock but failed loopback HTTP health check.
	if daemon.IsLocked() {
		if info, err := daemon.ReadStateFile(); err == nil && info.PID > 0 && info.PID != os.Getpid() && daemon.IsPIDAlive(info.PID) {
			if looksLikeAGPatcherExe(curExe, info.Exe) {
				_ = daemon.ForceStopPID(info.PID)
				waitUnlocked(2 * time.Second)
			}
		}
	}

	if err := SpawnTrayBackground(opts); err != nil {
		return nil, nil, err
	}
	return daemon.WaitUntilReady(4 * time.Second)
}

func looksLikeAGPatcherExe(curExe, recordedExe string) bool {
	if daemon.SameExecutable(curExe, recordedExe) {
		return true
	}
	base := strings.ToLower(filepath.Base(recordedExe))
	return strings.Contains(base, "agpatcher")
}

func waitUnlocked(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemon.IsLocked() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
}
