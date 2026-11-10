//go:build windows

package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"agy-patcher/internal/hostenv"
)

const (
	winRunSubkey = `Software\Microsoft\Windows\CurrentVersion\Run`
	winRunValue  = "AGPatcher"
)

// InstallDir returns %LOCALAPPDATA%\agpatcher on Windows.
func InstallDir() (string, error) {
	return hostenv.AppDataDir(hostenv.AppDirName)
}

func enableOSAutostart(exePath string) error {
	return hostenv.WriteRegString(syscall.HKEY_CURRENT_USER, winRunSubkey, winRunValue, `"`+exePath+`" --tray`)
}

func disableOSAutostart() error {
	_, err := hostenv.DeleteRegValue(syscall.HKEY_CURRENT_USER, winRunSubkey, winRunValue)
	return err
}

func isOSAutostartEnabled() (bool, string) {
	cmd := hostenv.ReadRegString(syscall.HKEY_CURRENT_USER, winRunSubkey, winRunValue)
	if !strings.Contains(strings.ToLower(cmd), "agpatcher") {
		return false, ""
	}
	target := extractQuotedPath(cmd)
	if _, err := os.Stat(target); err != nil {
		return false, target
	}
	return true, target
}

func removeInstalledFiles() error {
	exe, err := InstalledExePath()
	if err != nil {
		return err
	}
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		// If cleaning while running directly from the installed exe, let caller proceed cleanly.
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return err
	}
	_ = os.Remove(filepath.Dir(exe))
	return nil
}
