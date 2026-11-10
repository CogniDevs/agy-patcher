//go:build !windows

package hostenv

import (
	"os/exec"
	"path/filepath"
)

// TerminateProcessByBinary terminates running processes matching the exact binary name.
func TerminateProcessByBinary(binPath string) {
	name := filepath.Base(binPath)
	if name == "" {
		return
	}
	_ = exec.Command("pkill", "-x", name).Run()
}
