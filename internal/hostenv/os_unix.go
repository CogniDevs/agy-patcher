//go:build !windows

package hostenv

import (
	"bufio"
	"bytes"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// TerminateProcessByBinary terminates running processes matching the exact binary name.
func TerminateProcessByBinary(binPath string) {
	name := filepath.Base(binPath)
	if name == "" {
		return
	}
	_ = exec.Command("pkill", "-x", name).Run()
}

// TerminateOrphanProcesses scans active processes on Unix and terminates orphaned helper processes
// whose parent PID is 1 (init / subreaper) or dead.
func TerminateOrphanProcesses(targetNames []string) int {
	if len(targetNames) == 0 {
		return 0
	}
	targetMap := make(map[string]bool, len(targetNames))
	for _, n := range targetNames {
		targetMap[strings.ToLower(strings.TrimSpace(filepath.Base(n)))] = true
	}

	out, err := exec.Command("ps", "-eo", "pid,ppid,comm").Output()
	if err != nil {
		return 0
	}

	killed := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		comm := filepath.Base(fields[2])
		if err1 != nil || err2 != nil || pid <= 1 {
			continue
		}
		if targetMap[strings.ToLower(comm)] && ppid == 1 {
			if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
				killed++
			}
		}
	}
	return killed
}

// CloseProcessesByNames forcefully terminates all processes matching any of the given names.
func CloseProcessesByNames(names []string) int {
	killed := 0
	for _, n := range names {
		base := filepath.Base(n)
		if base == "" {
			continue
		}
		if err := exec.Command("pkill", "-9", "-f", base).Run(); err == nil {
			killed++
		}
	}
	return killed
}

// InServiceSession reports whether the current process runs in a headless service session without a GUI display.
func InServiceSession() bool {
	return false
}

// LaunchInUserSession is a no-op on Unix platforms as scheduled task session isolation is Windows-specific.
func LaunchInUserSession(app string) error {
	return nil
}
