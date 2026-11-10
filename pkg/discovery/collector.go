package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

var ignoredExtensions = map[string]bool{
	".bak":      true,
	".tmp":      true,
	".old":      true,
	".orig":     true,
	".sig":      true,
	".sha256":   true,
	".log":      true,
	".txt":      true,
	".json":     true,
	".pdb":      true,
	".dll":      true,
	".so":       true,
	".dylib":    true,
	".jar":      true,
	".zip":      true,
	".xml":      true,
	".md":       true,
	".js":       true,
	".cjs":      true,
	".mjs":      true,
	".map":      true,
	".agy_orig": true,
	".agystg":   true,
}

// BinaryCollector gathers verified, deduplicated native executable paths.
type BinaryCollector struct {
	checkNative bool
	seen        map[string]bool
	items       []string
}

func newBinaryCollector(checkNative bool) *BinaryCollector {
	return &BinaryCollector{
		checkNative: checkNative,
		seen:        make(map[string]bool),
	}
}

func (c *BinaryCollector) Add(p string) {
	if p == "" || hasIgnoredArtifactName(filepath.Base(p)) || !hostenv.IsRegularFile(p) {
		return
	}
	if runtime.GOOS != "windows" {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		if isSnapPath(p) || hostenv.IsShebangScript(p) {
			return
		}
	}
	if c.checkNative && !hostenv.IsNativeBinary(p) {
		return
	}
	key := hostenv.NormalizePathKey(p)
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.items = append(c.items, p)
}

func (c *BinaryCollector) AddDirMatching(dir string, matchFn func(name string) bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && matchFn(e.Name()) {
			c.Add(filepath.Join(dir, e.Name()))
		}
	}
}

func (c *BinaryCollector) AddAll(paths []string) {
	for _, p := range paths {
		c.Add(p)
	}
}

func (c *BinaryCollector) Items() []string {
	return c.items
}

func (c *BinaryCollector) Primary() string {
	if len(c.items) == 0 {
		return ""
	}
	return c.items[0]
}

func hasIgnoredArtifactName(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, ".agystg") || strings.Contains(lower, ".agy_stg_") {
		return true
	}
	return ignoredExtensions[strings.ToLower(filepath.Ext(name))]
}

func isLanguageServerBinary(name string) bool {
	if !strings.HasPrefix(name, "language_server") {
		return false
	}
	return !hasIgnoredArtifactName(name)
}

func isAGYBinaryName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "agy" || lower == "agy.exe"
}

func isJetBrainsBinaryName(name string) bool {
	if isLanguageServerBinary(name) || isAGYBinaryName(name) {
		return true
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "agpatcher") || hasIgnoredArtifactName(name) {
		return false
	}
	return strings.Contains(lower, "antigravity") || strings.HasPrefix(lower, "agy")
}

func appendUniquePath(list []string, candidate string) []string {
	if candidate == "" {
		return list
	}
	want := hostenv.NormalizePathKey(candidate)
	for _, item := range list {
		if hostenv.NormalizePathKey(item) == want {
			return list
		}
	}
	return append(list, candidate)
}

func prependUniquePath(list []string, candidate string) []string {
	if candidate == "" {
		return list
	}
	want := hostenv.NormalizePathKey(candidate)
	filtered := make([]string, 0, len(list)+1)
	filtered = append(filtered, candidate)
	for _, item := range list {
		if hostenv.NormalizePathKey(item) != want {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func geminiBinTargets(home string) []string {
	if home == "" {
		return nil
	}
	col := newBinaryCollector(true)
	col.AddDirMatching(filepath.Join(home, ".gemini", "bin"), func(name string) bool {
		return isLanguageServerBinary(name) || isAGYBinaryName(name)
	})
	return col.Items()
}

func platformCLITargets(home string) []string {
	col := newBinaryCollector(true)
	if home != "" {
		for _, p := range []string{
			filepath.Join(home, ".agy", "bin", "agy"),
			filepath.Join(home, ".agy", "bin", "agy.exe"),
			filepath.Join(home, ".local", "bin", "agy"),
			filepath.Join(home, ".local", "bin", "agy.exe"),
		} {
			col.Add(p)
		}
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		col.Add(filepath.Join(localAppData, "agy", "bin", "agy.exe"))
	} else if home != "" && runtime.GOOS == "windows" {
		col.Add(filepath.Join(home, "AppData", "Local", "agy", "bin", "agy.exe"))
	}
	for _, name := range []string{"agy", "agy.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			col.Add(p)
		}
	}
	return col.Items()
}

func scanGeminiBin(home string) []Install {
	if home == "" {
		return nil
	}
	geminiBin := filepath.Join(home, ".gemini", "bin")
	col := newBinaryCollector(true)
	col.AddDirMatching(geminiBin, func(name string) bool {
		return isLanguageServerBinary(name) || isAGYBinaryName(name)
	})
	bins := col.Items()
	if len(bins) == 0 {
		return nil
	}

	results := make([]Install, 0, len(bins))
	for _, binPath := range bins {
		instType := TypeBackend
		label := "Antigravity Backend (.gemini/bin)"
		if isAGYBinaryName(filepath.Base(binPath)) {
			instType = TypeCLI
			label = "Antigravity CLI (.gemini/bin)"
		}
		results = append(results, Install{
			Path:       geminiBin,
			Type:       instType,
			Label:      label,
			BinaryPath: binPath,
			Binaries:   []string{binPath},
		})
	}
	return results
}

func findCLIInPath() []Install {
	names := []string{"agy"}
	if runtime.GOOS == "windows" {
		names = []string{"agy.exe"}
	}

	var results []Install
	for _, name := range names {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		if runtime.GOOS != "windows" && (isSnapPath(p) || hostenv.IsShebangScript(p)) {
			continue
		}
		if hostenv.IsRegularFile(p) && hostenv.IsNativeBinary(p) {
			inst := buildInstall(filepath.Dir(p), false)
			if inst.BinaryPath == "" {
				inst.BinaryPath = p
				inst.Binaries = []string{p}
			}
			results = append(results, inst)
		}
	}
	return results
}
