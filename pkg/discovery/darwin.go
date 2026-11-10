//go:build darwin

package discovery

import (
	"os"
	"path/filepath"
	"strings"

	"agy-patcher/internal/hostenv"
)

func findPlatformInstalls() []Install {
	var results []Install
	home := hostenv.EffectiveHomeDir()

	appRoots := []string{"/Applications"}
	if home != "" {
		appRoots = append(appRoots, filepath.Join(home, "Applications"))
	}

	seen := make(map[string]bool)
	for _, appRoot := range appRoots {
		entries, err := os.ReadDir(appRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			lower := strings.ToLower(e.Name())
			if strings.HasSuffix(lower, ".app") && strings.Contains(lower, "antigravity") {
				contents := filepath.Join(appRoot, e.Name(), "Contents")
				if seen[contents] {
					continue
				}
				seen[contents] = true
				if inst, ok := inspectDirInstall(contents); ok {
					results = append(results, inst)
				}
			}
		}
	}

	if home != "" {
		for _, dir := range []string{
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".agy", "bin"),
			filepath.Join(home, ".agy"),
		} {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if inst, ok := inspectDirInstall(dir); ok {
				results = append(results, inst)
			}
		}
	}

	results = append(results, scanGeminiBin(home)...)
	return results
}
