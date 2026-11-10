//go:build !windows && !darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/assets"
)

// InstallDir returns ~/.local/share/agpatcher on Linux.
func InstallDir() (string, error) {
	return hostenv.AppDataDir(hostenv.AppDirName)
}

func linuxIconPath() (string, error) {
	data := hostenv.XDGDataHome()
	if data == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(data, "icons", "hicolor", "256x256", "apps", "agpatcher.png"), nil
}

func linuxAutostartDesktopPath() (string, error) {
	cfg := hostenv.XDGConfigHome()
	if cfg == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(cfg, "autostart", "agpatcher.desktop"), nil
}

func linuxAppDesktopPath() (string, error) {
	data := hostenv.XDGDataHome()
	if data == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(data, "applications", "agpatcher.desktop"), nil
}

func enableOSAutostart(exePath string) error {
	iconPath, err := linuxIconPath()
	if err == nil {
		if mkErr := os.MkdirAll(filepath.Dir(iconPath), 0755); mkErr == nil {
			_ = os.WriteFile(iconPath, assets.IconPNG, 0644)
		}
	}

	autoPath, err := linuxAutostartDesktopPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(autoPath), 0755); err != nil {
		return err
	}
	autoContent := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=agy-patcher Proxy
Comment=Antigravity local DoH/CONNECT proxy
Exec="%s" --tray
Icon=agpatcher
Terminal=false
X-GNOME-Autostart-enabled=true
`, exePath)
	if err := os.WriteFile(autoPath, []byte(autoContent), 0644); err != nil {
		return err
	}

	if appPath, err := linuxAppDesktopPath(); err == nil {
		if mkErr := os.MkdirAll(filepath.Dir(appPath), 0755); mkErr == nil {
			appContent := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=agy-patcher
Comment=Antigravity Patcher and Proxy Console
Exec="%s"
Icon=agpatcher
Terminal=true
Categories=Development;Network;
`, exePath)
			_ = os.WriteFile(appPath, []byte(appContent), 0644)
		}
	}
	return nil
}

func disableOSAutostart() error {
	if autoPath, err := linuxAutostartDesktopPath(); err == nil {
		if rmErr := os.Remove(autoPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
	}
	return nil
}

func isOSAutostartEnabled() (bool, string) {
	autoPath, err := linuxAutostartDesktopPath()
	if err != nil {
		return false, ""
	}
	raw, err := os.ReadFile(autoPath)
	if err != nil {
		return false, ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Exec=") {
			target := extractQuotedPath(strings.TrimPrefix(line, "Exec="))
			if _, statErr := os.Stat(target); statErr == nil {
				return true, target
			}
			return false, target
		}
	}
	return false, ""
}

func removeInstalledFiles() error {
	if appPath, err := linuxAppDesktopPath(); err == nil {
		_ = os.Remove(appPath)
	}
	if iconPath, err := linuxIconPath(); err == nil {
		_ = os.Remove(iconPath)
	}
	exe, err := InstalledExePath()
	if err != nil {
		return err
	}
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(exe))
	return nil
}
