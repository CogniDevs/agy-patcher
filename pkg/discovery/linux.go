//go:build !windows && !darwin

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

	var candidates []string
	for _, base := range []string{"/opt", "/usr/share", "/usr/lib", "/usr/local/share"} {
		for _, name := range []string{"Antigravity", "Antigravity IDE", "antigravity", "antigravity-ide"} {
			candidates = append(candidates, filepath.Join(base, name))
		}
	}

	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "Antigravity"),
			filepath.Join(home, ".local", "share", "Antigravity IDE"),
			filepath.Join(home, "Antigravity"),
			filepath.Join(home, "Antigravity IDE"),
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".agy", "bin"),
			filepath.Join(home, ".agy"),
			filepath.Join(home, ".local", "share", "agy", "bin"),
		)
	}

	scanBases := []string{"/opt", "/usr/share", "/usr/lib", "/usr/local/share"}
	if home != "" {
		scanBases = append(scanBases, filepath.Join(home, ".local", "share"))
	}
	for _, base := range scanBases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			lowerName := strings.ToLower(e.Name())
			if strings.Contains(lowerName, "antigravity") || lowerName == "agy" {
				candidates = append(candidates, filepath.Join(base, e.Name()))
			}
		}
	}

	seen := make(map[string]bool)
	for _, cand := range candidates {
		cleaned := filepath.Clean(cand)
		if isSnapPath(cleaned) || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		if inst, ok := inspectDirInstall(cleaned); ok {
			results = append(results, inst)
		}
	}

	results = append(results, scanGeminiBin(home)...)
	return results
}
