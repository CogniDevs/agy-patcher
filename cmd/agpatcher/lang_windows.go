//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
)

var (
	kernel32Lang             = syscall.NewLazyDLL("kernel32.dll")
	procGetUserDefaultUILang = kernel32Lang.NewProc("GetUserDefaultUILanguage")
)

func detectEnvLang() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if val := strings.ToLower(os.Getenv(key)); strings.Contains(val, "ru") {
			return "ru"
		}
	}
	langID, _, _ := procGetUserDefaultUILang.Call()
	// Primary language ID 0x19 is LANG_RUSSIAN.
	if langID&0x3FF == 0x19 {
		return "ru"
	}
	return "en"
}
