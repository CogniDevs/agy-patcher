//go:build windows

package discovery

import (
	"os"
	"path/filepath"
	"strings"

	"agy-patcher/internal/hostenv"
)

func findPlatformInstalls() []Install {
	var candidates []string

	localAppData := os.Getenv("LOCALAPPDATA")
	progFiles := os.Getenv("PROGRAMFILES")
	progFilesX86 := os.Getenv("PROGRAMFILES(X86)")

	if localAppData != "" {
		candidates = append(candidates,
			filepath.Join(localAppData, "Programs", "Antigravity"),
			filepath.Join(localAppData, "Programs", "Antigravity IDE"),
			filepath.Join(localAppData, "Antigravity"),
			filepath.Join(localAppData, "Antigravity IDE"),
			filepath.Join(localAppData, "agy", "bin"),
			filepath.Join(localAppData, "agy"),
		)
	}
	if progFiles != "" {
		candidates = append(candidates,
			filepath.Join(progFiles, "Antigravity"),
			filepath.Join(progFiles, "Antigravity IDE"),
		)
	}
	if progFilesX86 != "" {
		candidates = append(candidates,
			filepath.Join(progFilesX86, "Antigravity"),
			filepath.Join(progFilesX86, "Antigravity IDE"),
		)
	}

	home := hostenv.EffectiveHomeDir()
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".agy", "bin"),
			filepath.Join(home, ".agy"),
			filepath.Join(home, ".local", "bin"),
		)
	}

	uninstallDirs := hostenv.EnumUninstallDirs(func(displayName, installLoc string) bool {
		dn := strings.ToLower(displayName)
		loc := strings.ToLower(installLoc)
		return strings.Contains(dn, "antigravity") ||
			strings.Contains(dn, "agy") ||
			strings.Contains(loc, "antigravity")
	})
	candidates = append(candidates, uninstallDirs...)

	var results []Install
	seenRoots := make(map[string]bool)
	for _, cand := range candidates {
		root := findRoot(cand)
		if root == "" {
			continue
		}
		key := hostenv.NormalizePathKey(root)
		if seenRoots[key] {
			continue
		}
		seenRoots[key] = true
		if inst, ok := inspectDirInstall(root); ok {
			results = append(results, inst)
		}
	}

	results = append(results, scanGeminiBin(home)...)
	return results
}
