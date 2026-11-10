//go:build !windows

package main

import (
	"os"
	"strings"
)

func detectEnvLang() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if val := strings.ToLower(os.Getenv(key)); strings.Contains(val, "ru") {
			return "ru"
		}
	}
	return "en"
}
