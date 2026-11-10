//go:build !windows

package env

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

func setPersistentProxyVar(proxyURL string) error {
	switch runtime.GOOS {
	case "linux":
		path := environmentDPath()
		body := fmt.Sprintf("# agy-patcher bypass proxy\n%s=%s\n", ProxyVar, proxyURL)
		if err := hostenv.WriteFileAtomic(path, []byte(body), 0644, nil); err != nil {
			return err
		}
		if _, err := exec.LookPath("systemctl"); err == nil {
			_ = exec.Command("systemctl", "--user", "set-environment", fmt.Sprintf("%s=%s", ProxyVar, proxyURL)).Run()
		}
		return nil
	case "darwin":
		if out, err := exec.Command("launchctl", "setenv", ProxyVar, proxyURL).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl setenv failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("setting persistent proxy variable is not supported on %s", runtime.GOOS)
	}
}

func removePersistentProxyVar() error {
	switch runtime.GOOS {
	case "linux":
		if err := os.Remove(environmentDPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := exec.LookPath("systemctl"); err == nil {
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			_ = exec.Command("systemctl", "--user", "unset-environment", ProxyVar).Run()
		}
		return nil
	case "darwin":
		_ = exec.Command("launchctl", "unsetenv", ProxyVar).Run()
		return nil
	default:
		return nil
	}
}

func getPersistentProxyVar() string {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err == nil {
			if out, err := exec.Command("systemctl", "--user", "show-environment").Output(); err == nil {
				if val := parseSystemdEnv(string(out))[ProxyVar]; val != "" {
					return val
				}
			}
		}
		if data, err := os.ReadFile(environmentDPath()); err == nil {
			if val := parseEnvFileVar(string(data), ProxyVar); val != "" {
				return val
			}
		}
	case "darwin":
		if out, err := exec.Command("launchctl", "getenv", ProxyVar).Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

func foreignProxyOS(ours string) foreignProxyInfo {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("systemctl"); err == nil {
			if out, err := exec.Command("systemctl", "--user", "show-environment").Output(); err == nil {
				envMap := parseSystemdEnv(string(out))
				for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
					if val := envMap[k]; val != "" && !isSameProxyAddr(val, ours) {
						return foreignProxyInfo{Value: val, Source: "systemd user session", isSet: true}
					}
				}
			}
		}
	} else if runtime.GOOS == "darwin" {
		for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
			if out, err := exec.Command("launchctl", "getenv", k).Output(); err == nil {
				val := strings.TrimSpace(string(out))
				if val != "" && !isSameProxyAddr(val, ours) {
					return foreignProxyInfo{Value: val, Source: "launchctl", isSet: true}
				}
			}
		}
	}
	return foreignProxyInfo{}
}

func environmentDPath() string {
	return filepath.Join(hostenv.XDGConfigHome(), "environment.d", "agpatcher.conf")
}
