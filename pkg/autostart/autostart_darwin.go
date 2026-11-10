//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agy-patcher/internal/hostenv"
)

const darwinAgentLabel = "com.cognidevs.agpatcher"

// InstallDir returns ~/Library/Application Support/agpatcher on macOS.
func InstallDir() (string, error) {
	return hostenv.AppDataDir(hostenv.AppDirName)
}

func darwinPlistPath() (string, error) {
	home := hostenv.EffectiveHomeDir()
	if home == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(home, "Library", "LaunchAgents", darwinAgentLabel+".plist"), nil
}

func enableOSAutostart(exePath string) error {
	plistPath, err := darwinPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--tray</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <false/>
</dict>
</plist>
`, darwinAgentLabel, xmlEscape(exePath))
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
	_ = exec.Command("launchctl", "load", "-w", plistPath).Run()
	return nil
}

func disableOSAutostart() error {
	plistPath, err := darwinPlistPath()
	if err != nil {
		return nil
	}
	if _, statErr := os.Stat(plistPath); statErr == nil {
		_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
	}
	if rmErr := os.Remove(plistPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}

func isOSAutostartEnabled() (bool, string) {
	plistPath, err := darwinPlistPath()
	if err != nil {
		return false, ""
	}
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return false, ""
	}
	text := string(raw)
	start := strings.Index(text, "<string>")
	for start >= 0 {
		rest := text[start+8:]
		end := strings.Index(rest, "</string>")
		if end < 0 {
			break
		}
		val := rest[:end]
		if strings.Contains(val, "agpatcher") && val != darwinAgentLabel {
			if _, statErr := os.Stat(val); statErr == nil {
				return true, val
			}
			return false, val
		}
		nextRel := strings.Index(rest[end:], "<string>")
		if nextRel < 0 {
			break
		}
		text = rest[end+nextRel:]
		start = 0
	}
	return false, ""
}

func removeInstalledFiles() error {
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

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
