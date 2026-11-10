package discovery

import (
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

// InstallType classifies how an Antigravity runtime is packaged on disk.
type InstallType string

const (
	TypeIDE       InstallType = "ide"
	TypeDesktop   InstallType = "desktop"
	TypeExtension InstallType = "extension"
	TypeJetBrains InstallType = "jetbrains"
	TypeCLI       InstallType = "cli"
	TypeBackend   InstallType = "backend"
)

// Install describes a discovered Antigravity or editor-integrated target.
type Install struct {
	Path            string
	Type            InstallType
	Label           string
	BinaryPath      string
	Binaries        []string
	JsPath          string
	ExtensionJsPath string
	ResourcesDir    string
	AsarPath        string
	Manual          bool
}

// TargetPaths returns the deduplicated list of candidate patch targets (binaries + JS files) for inst.
func (inst Install) TargetPaths() []string {
	var raw []string
	raw = append(raw, inst.Binaries...)
	if inst.BinaryPath != "" {
		raw = append(raw, inst.BinaryPath)
	}
	if inst.JsPath != "" {
		raw = append(raw, inst.JsPath)
	}
	if inst.ExtensionJsPath != "" {
		raw = append(raw, inst.ExtensionJsPath)
	}

	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
		k := hostenv.NormalizePathKey(p)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// FindInstalls discovers Antigravity installations across the host OS, editor extensions,
// JetBrains plugins, and PATH.
func FindInstalls() []Install {
	home := hostenv.EffectiveHomeDir()
	installs := findPlatformInstalls()
	installs = append(installs, scanEditorExtensions(home)...)
	installs = append(installs, scanJetBrainsInstalls(home)...)
	installs = append(installs, findCLIInPath()...)
	return dedupeInstalls(installs)
}

func dedupeInstalls(installs []Install) []Install {
	seen := make(map[string]bool, len(installs))
	unique := make([]Install, 0, len(installs))
	for _, inst := range installs {
		if runtime.GOOS != "windows" && isSnapPath(inst.Path) {
			continue
		}
		key := hostenv.NormalizePathKey(inst.Path) + "|" +
			hostenv.NormalizePathKey(inst.BinaryPath) + "|" +
			hostenv.NormalizePathKey(inst.JsPath) + "|" +
			hostenv.NormalizePathKey(inst.ExtensionJsPath) + "|" +
			hostenv.NormalizePathKey(inst.AsarPath)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, inst)
		}
	}
	return unique
}

// DetectInstallLabel returns a human-readable product label for an installation.
func DetectInstallLabel(inst Install) string {
	if isGeminiBin(inst.Path) && VSCodeExtensionInstalled() {
		return "Antigravity for VS Code"
	}
	switch inst.Type {
	case TypeCLI:
		return "Antigravity CLI"
	case TypeIDE:
		return "Antigravity IDE"
	case TypeDesktop:
		return "Antigravity Desktop"
	case TypeExtension:
		if inst.Label != "" {
			return inst.Label
		}
		return "Antigravity Editor Extension"
	case TypeJetBrains:
		if inst.Label != "" {
			return inst.Label
		}
		return "Antigravity JetBrains Plugin"
	default:
		return "Antigravity Backend"
	}
}

func isGeminiBin(path string) bool {
	return filepath.Base(path) == "bin" && filepath.Base(filepath.Dir(path)) == ".gemini"
}

func resourcesDirFor(root string) string {
	if runtime.GOOS == "darwin" {
		upper := filepath.Join(root, "Resources")
		if hostenv.IsDirectory(upper) {
			return upper
		}
	}
	return filepath.Join(root, "resources")
}

func isSnapPath(p string) bool {
	return strings.HasPrefix(p, "/snap") || strings.HasPrefix(p, "/var/lib/snapd")
}

func isBlockedSystemDir(path string) bool {
	lower := strings.ToLower(path)
	return lower == `c:\windows` ||
		strings.HasPrefix(lower, `c:\windows\`) ||
		strings.Contains(lower, `\windows\system32`) ||
		strings.Contains(lower, `\windows\syswow64`) ||
		(strings.Contains(lower, "windows") && (strings.Contains(lower, "system32") || strings.Contains(lower, "syswow64")))
}
