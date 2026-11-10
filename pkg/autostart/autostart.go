package autostart

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

// InstalledExePath returns the canonical target executable path in the user's application data directory.
func InstalledExePath() (string, error) {
	dir, err := InstallDir()
	if err != nil {
		return "", err
	}
	name := "agpatcher"
	if runtime.GOOS == "windows" {
		name = "agpatcher.exe"
	}
	return filepath.Join(dir, name), nil
}

// PreferredExePath returns the installed autostart binary path when present, or the current executable path.
func PreferredExePath() (string, error) {
	if enabled, target := IsEnabled(); enabled && target != "" {
		if _, err := os.Stat(target); err == nil {
			return target, nil
		}
	}
	if installed, err := InstalledExePath(); err == nil {
		if _, err := os.Stat(installed); err == nil {
			return installed, nil
		}
	}
	return os.Executable()
}

// EnsureInstalled copies the currently running binary into InstallDir if needed.
func EnsureInstalled() (string, error) {
	dst, err := InstalledExePath()
	if err != nil {
		return "", err
	}
	src, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	if err := copyExecutable(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Enable installs the binary into the user data directory and registers OS logon autostart (--tray).
func Enable() (string, error) {
	dst, err := EnsureInstalled()
	if err != nil {
		return "", err
	}
	if err := enableOSAutostart(dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Disable unregisters OS logon autostart.
func Disable() error {
	return disableOSAutostart()
}

// IsEnabled reports whether OS logon autostart is currently configured and returns its command/path.
func IsEnabled() (bool, string) {
	return isOSAutostartEnabled()
}

// RemoveAll disables autostart and deletes the installed binary/assets from InstallDir.
func RemoveAll() error {
	disableErr := disableOSAutostart()
	rmErr := removeInstalledFiles()
	if disableErr != nil {
		return disableErr
	}
	return rmErr
}

func copyExecutable(src, dst string) error {
	if hostenv.SamePath(src, dst) {
		return nil
	}
	srcSum, srcMode, err := fileSHA256AndMode(src)
	if err != nil {
		return err
	}
	if dstSum, _, err := fileSHA256AndMode(dst); err == nil && srcSum == dstSum {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	perm := srcMode.Perm()
	if perm == 0 || runtime.GOOS != "windows" {
		perm |= 0755
	}
	_, err = hostenv.StreamTransformToTarget(src, dst, perm, nil, func(w io.Writer, r io.Reader) (bool, error) {
		_, copyErr := io.Copy(w, r)
		return copyErr == nil, copyErr
	})
	return err
}

func fileSHA256AndMode(path string) ([32]byte, os.FileMode, error) {
	var zero [32]byte
	info, err := os.Stat(path)
	if err != nil {
		return zero, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return zero, 0, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, 0, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, info.Mode(), nil
}

func extractQuotedPath(cmd string) string {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if idx := strings.IndexByte(s, ' '); idx > 0 {
		return s[:idx]
	}
	return s
}
