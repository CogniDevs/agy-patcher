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

const (
	shellHookBegin = "# >>> agy-patcher >>>"
	shellHookEnd   = "# <<< agy-patcher <<<"
)

func shellBlock(confPath string) string {
	return fmt.Sprintf("%s\n[ -r '%s' ] && { set -a; . '%s'; set +a; }\n%s\n", shellHookBegin, confPath, confPath, shellHookEnd)
}

func updateShellFile(path, block string, remove bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if remove || !os.IsNotExist(err) {
			return
		}
		// Create file if it doesn't exist and we want to install hooks
		_ = os.WriteFile(path, []byte(block), 0644)
		return
	}

	content := string(data)
	bIdx := strings.Index(content, shellHookBegin)
	eIdx := strings.Index(content, shellHookEnd)

	var newContent string
	if bIdx != -1 && eIdx != -1 && eIdx > bIdx {
		end := eIdx + len(shellHookEnd)
		if end < len(content) && content[end] == '\n' {
			end++
		}
		if remove {
			newContent = content[:bIdx] + content[end:]
		} else {
			newContent = content[:bIdx] + block + content[end:]
		}
	} else {
		if remove {
			return
		}
		sep := ""
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			sep = "\n"
		}
		newContent = content + sep + block
	}

	if newContent != content {
		_ = os.WriteFile(path, []byte(newContent), 0644)
	}
}

func ensureShellHooks(confPath string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	blk := shellBlock(confPath)
	updateShellFile(filepath.Join(home, ".profile"), blk, false)
	bashrc := filepath.Join(home, ".bashrc")
	if _, err := os.Stat(bashrc); err == nil {
		updateShellFile(bashrc, blk, false)
	}
}

func removeShellHooks() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	updateShellFile(filepath.Join(home, ".profile"), "", true)
	updateShellFile(filepath.Join(home, ".bashrc"), "", true)
}

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
		ensureShellHooks(path)
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
		removeShellHooks()
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
