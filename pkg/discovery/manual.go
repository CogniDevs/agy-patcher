package discovery

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

// CleanInputPath trims whitespace and surrounding quotes from a user-provided path.
func CleanInputPath(input string) string {
	s := strings.TrimSpace(input)
	s = strings.Trim(s, `"'`)
	return strings.TrimSpace(s)
}

// ResolveManualPath resolves a user-supplied file or directory into a manual Install if valid.
func ResolveManualPath(raw string) *Install {
	cleaned := CleanInputPath(raw)
	if cleaned == "" {
		return nil
	}
	p := filepath.Clean(cleaned)
	if runtime.GOOS != "windows" {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil
	}

	searchPath := p
	var explicitBin, explicitJS string
	if !info.IsDir() {
		searchPath = filepath.Dir(p)
		base := filepath.Base(p)
		lowerBase := strings.ToLower(base)
		if isLanguageServerBinary(base) || isAGYBinaryName(base) ||
			(hostenv.IsNativeBinary(p) && !hostenv.IsShebangScript(p) &&
				(strings.Contains(lowerBase, "antigravity") || strings.HasPrefix(lowerBase, "agy"))) {
			explicitBin = p
		}
		if lowerBase == "extension.js" {
			explicitJS = p
		}
	}

	home := hostenv.EffectiveHomeDir()
	if extRoot := resolveExtensionRoot(searchPath, explicitJS); extRoot != "" {
		inst := buildExtensionInstall(extRoot, detectEditorNameFromPath(extRoot), filepath.Base(extRoot), home, true)
		attachExplicitBinary(&inst, explicitBin)
		return &inst
	}

	if jbRoot := resolveJetBrainsRoot(searchPath); jbRoot != "" {
		inst := buildJetBrainsInstall(jbRoot, detectJetBrainsProductFromPath(jbRoot), filepath.Base(jbRoot), home, true)
		attachExplicitBinary(&inst, explicitBin)
		return &inst
	}

	root := findRoot(searchPath)
	if root == "" {
		if explicitBin != "" && !isBlockedSystemDir(searchPath) {
			instType := TypeBackend
			if isAGYBinaryName(filepath.Base(explicitBin)) {
				instType = TypeCLI
			}
			inst := Install{
				Path:       searchPath,
				Type:       instType,
				BinaryPath: explicitBin,
				Binaries:   []string{explicitBin},
				Manual:     true,
			}
			inst.Label = DetectInstallLabel(inst)
			return &inst
		}
		return nil
	}

	inst := buildInstall(root, true)
	attachExplicitBinary(&inst, explicitBin)
	return &inst
}

func attachExplicitBinary(inst *Install, explicitBin string) {
	if explicitBin == "" {
		return
	}
	inst.Binaries = prependUniquePath(inst.Binaries, explicitBin)
	inst.BinaryPath = explicitBin
}

func findRoot(start string) string {
	current := start
	for i := 0; i < 6; i++ {
		if isAppRoot(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	subfolders := []string{
		"Antigravity IDE",
		"Antigravity",
		"agy",
		filepath.Join("Programs", "Antigravity IDE"),
		filepath.Join("Programs", "Antigravity"),
		"resources",
		"Contents",
	}
	for _, sub := range subfolders {
		candidate := filepath.Join(start, sub)
		if isAppRoot(candidate) {
			return candidate
		}
	}

	if hostenv.IsDirectory(start) && !isBlockedSystemDir(start) {
		entries, err := os.ReadDir(start)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && isLanguageServerBinary(e.Name()) {
					return start
				}
			}
		}
	}
	return ""
}

func isAppRoot(path string) bool {
	if !hostenv.IsDirectory(path) || isBlockedSystemDir(path) {
		return false
	}

	resources := resourcesDirFor(path)
	if hostenv.IsDirectory(resources) {
		if hostenv.IsRegularFile(filepath.Join(resources, "app.asar")) ||
			hostenv.IsRegularFile(filepath.Join(resources, "app.asar.bak")) ||
			hostenv.IsDirectory(filepath.Join(resources, "app")) ||
			hostenv.IsDirectory(filepath.Join(resources, "bin")) {
			return true
		}
	}

	for _, exe := range []string{"agy.exe", "agy", "Antigravity.exe", "Antigravity IDE.exe", "antigravity.exe"} {
		if hostenv.IsRegularFile(filepath.Join(path, exe)) {
			return true
		}
	}

	if runtime.GOOS != "windows" {
		for _, exe := range []string{"antigravity", "Antigravity", "antigravity-ide"} {
			if hostenv.IsRegularFile(filepath.Join(path, exe)) {
				return true
			}
		}
	}

	return hostenv.IsRegularFile(filepath.Join(path, "out", "main.js")) ||
		hostenv.IsRegularFile(filepath.Join(path, "dist", "main.js"))
}

func detectType(path, resources string) InstallType {
	if hostenv.IsRegularFile(filepath.Join(path, "agy.exe")) || hostenv.IsRegularFile(filepath.Join(path, "agy")) {
		return TypeCLI
	}
	if hostenv.IsRegularFile(filepath.Join(path, "Antigravity IDE.exe")) ||
		hostenv.IsDirectory(filepath.Join(resources, "app", "out")) ||
		hostenv.IsRegularFile(filepath.Join(path, "out", "main.js")) {
		return TypeIDE
	}
	if hostenv.IsRegularFile(filepath.Join(resources, "app.asar")) ||
		hostenv.IsRegularFile(filepath.Join(resources, "app.asar.bak")) ||
		hostenv.IsDirectory(filepath.Join(resources, "app", "dist")) ||
		hostenv.IsDirectory(filepath.Join(resources, "bin")) ||
		hostenv.IsRegularFile(filepath.Join(path, "dist", "main.js")) {
		return TypeDesktop
	}
	return TypeBackend
}

func buildInstall(root string, manual bool) Install {
	resources := resourcesDirFor(root)
	instType := detectType(root, resources)
	binaries := FindBinaryTargets(root)
	primaryBin := ""
	if len(binaries) > 0 {
		primaryBin = binaries[0]
	}

	asarPath := filepath.Join(resources, "app.asar")
	if !hostenv.IsRegularFile(asarPath) {
		asarPath = ""
	}

	extJS := filepath.Join(resources, "app", "extensions", "antigravity", "dist", "extension.js")
	if !hostenv.IsRegularFile(extJS) {
		extJS = ""
	}

	inst := Install{
		Path:            root,
		Type:            instType,
		BinaryPath:      primaryBin,
		Binaries:        binaries,
		JsPath:          findJsInDir(root, resources),
		ExtensionJsPath: extJS,
		ResourcesDir:    resources,
		AsarPath:        asarPath,
		Manual:          manual,
	}
	inst.Label = DetectInstallLabel(inst)
	return inst
}

// FindBinaryTargets enumerates all Antigravity language_server and agy binaries under root.
func FindBinaryTargets(root string) []string {
	resources := resourcesDirFor(root)
	resourcesBin := filepath.Join(resources, "bin")
	extBin := filepath.Join(resources, "app", "extensions", "antigravity", "bin")

	col := newBinaryCollector(false)
	for _, c := range []string{
		filepath.Join(resourcesBin, "language_server.exe"),
		filepath.Join(resourcesBin, "language_server"),
		filepath.Join(extBin, "language_server_windows_x64.exe"),
		filepath.Join(extBin, "language_server.exe"),
		filepath.Join(root, "agy.exe"),
		filepath.Join(root, "agy"),
	} {
		col.Add(c)
	}

	for _, dir := range []string{extBin, resourcesBin, root} {
		col.AddDirMatching(dir, isLanguageServerBinary)
	}

	// IDE 2.6.0 delegates backend execution to ~/.gemini/bin.
	if hostenv.IsDirectory(filepath.Join(resources, "app", "extensions", "agy")) {
		if home := hostenv.EffectiveHomeDir(); home != "" {
			geminiBin := filepath.Join(home, ".gemini", "bin")
			col.Add(filepath.Join(geminiBin, "agy.exe"))
			col.Add(filepath.Join(geminiBin, "agy"))
			col.AddDirMatching(geminiBin, isLanguageServerBinary)
		}
	}

	return col.Items()
}

func findJsInDir(root, resources string) string {
	for _, c := range []string{
		filepath.Join(resources, "app", "out", "main.js"),
		filepath.Join(resources, "app", "dist", "main.js"),
		filepath.Join(root, "out", "main.js"),
		filepath.Join(root, "dist", "main.js"),
	} {
		if hostenv.IsRegularFile(c) {
			return c
		}
	}
	return ""
}

func inspectDirInstall(path string) (Install, bool) {
	if !isAppRoot(path) {
		return Install{}, false
	}
	inst := buildInstall(path, false)
	if len(inst.Binaries) == 0 && inst.JsPath == "" && inst.AsarPath == "" {
		return Install{}, false
	}
	return inst, true
}
