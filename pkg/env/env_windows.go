//go:build windows

package env

import (
	"fmt"
	"syscall"

	"agy-patcher/internal/hostenv"
)

func setPersistentProxyVar(proxyURL string) error {
	return hostenv.WriteUserEnv(ProxyVar, proxyURL)
}

func removePersistentProxyVar() error {
	_, err := hostenv.DeleteUserEnv(ProxyVar)
	return err
}

func getPersistentProxyVar() string {
	return hostenv.ReadUserEnv(ProxyVar)
}

func foreignProxyOS(ours string) foreignProxyInfo {
	scopes := []struct {
		root  syscall.Handle
		path  string
		label string
	}{
		{syscall.HKEY_CURRENT_USER, hostenv.UserEnvSubkey, "User"},
		{syscall.HKEY_LOCAL_MACHINE, hostenv.MachineEnvSubkey, "Machine"},
	}

	for _, sc := range scopes {
		for _, envName := range []string{"HTTPS_PROXY", "https_proxy"} {
			if val := hostenv.ReadRegString(sc.root, sc.path, envName); val != "" && !isSameProxyAddr(val, ours) {
				return foreignProxyInfo{
					Value:  val,
					Source: fmt.Sprintf("HTTPS_PROXY (%s)", sc.label),
					isSet:  true,
				}
			}
		}
	}
	return foreignProxyInfo{}
}
