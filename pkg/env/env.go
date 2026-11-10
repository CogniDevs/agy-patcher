package env

import (
	"fmt"
	"os"
	"strings"
)

// ProxyVar is the 11-byte dedicated proxy environment variable patched into Antigravity binaries.
const ProxyVar = "AGY_PROXY_U"

// ForeignProxyError indicates that an external HTTPS proxy is already configured.
type ForeignProxyError struct {
	Source string
	Value  string
}

func (e *ForeignProxyError) Error() string {
	return fmt.Sprintf("foreign proxy configured (%s=%s)", e.Source, e.Value)
}

// DetectExternalProxy checks read-only whether a system/user HTTPS_PROXY is already set outside agy-patcher.
func DetectExternalProxy() *ForeignProxyError {
	if fp := foreignProxy(""); fp.isSet {
		return &ForeignProxyError{Source: fp.Source, Value: fp.Value}
	}
	return nil
}

// SetProxyVar ensures no external HTTPS_PROXY would be conflicted with and configures ProxyVar persistently.
func SetProxyVar(proxyURL string) error {
	if fp := foreignProxy(proxyURL); fp.isSet {
		return &ForeignProxyError{Source: fp.Source, Value: fp.Value}
	}

	if err := os.Setenv(ProxyVar, proxyURL); err != nil {
		return err
	}
	if getPersistentProxyVar() == proxyURL {
		return nil
	}
	return setPersistentProxyVar(proxyURL)
}

// RemoveProxyVar clears ProxyVar from the current process and persistent OS user configuration.
func RemoveProxyVar() error {
	_ = os.Unsetenv(ProxyVar)
	return removePersistentProxyVar()
}

// GetProxyVar returns the configured ProxyVar value from persistent config or process environment.
func GetProxyVar() string {
	if val := getPersistentProxyVar(); val != "" {
		return val
	}
	return os.Getenv(ProxyVar)
}

// IsProxyOurs reports whether ProxyVar is currently set to proxyURL.
func IsProxyOurs(proxyURL string) bool {
	val := GetProxyVar()
	if val == "" {
		return false
	}
	return isSameProxyAddr(val, proxyURL)
}

type foreignProxyInfo struct {
	Value  string
	Source string
	isSet  bool
}

func foreignProxy(ours string) foreignProxyInfo {
	for _, envName := range []string{"HTTPS_PROXY", "https_proxy"} {
		if val := strings.TrimSpace(os.Getenv(envName)); val != "" && !isSameProxyAddr(val, ours) {
			return foreignProxyInfo{Value: val, Source: envName + " (process)", isSet: true}
		}
	}
	return foreignProxyOS(ours)
}

func isSameProxyAddr(value, proxyURL string) bool {
	strip := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.ToLower(s)
		s = strings.TrimSuffix(s, "/")
		s = strings.TrimPrefix(s, "https://")
		s = strings.TrimPrefix(s, "http://")
		return s
	}

	ourAddr := strip(proxyURL)
	valAddr := strip(value)
	return ourAddr != "" && valAddr == ourAddr
}

func parseSystemdEnv(output string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return m
}

func parseEnvFileVar(content, key string) string {
	prefix := key + "="
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
