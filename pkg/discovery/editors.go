package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agy-patcher/internal/hostenv"
)

type editorRoot struct {
	name string
	dir  string
}

func editorRoots(home string) []editorRoot {
	if home == "" {
		return nil
	}
	specs := []struct {
		dotDir string
		label  string
	}{
		{".vscode", "VS Code"},
		{".vscode-insiders", "VS Code Insiders"},
		{".vscode-oss", "VSCodium / OSS"},
		{".vscodium", "VSCodium"},
		{".vscode-server", "VS Code Server"},
		{".vscode-server-insiders", "VS Code Server Insiders"},
		{".cursor", "Cursor"},
		{".windsurf", "Windsurf"},
		{".trae", "Trae"},
	}
	roots := make([]editorRoot, 0, len(specs)+2)
	for _, sp := range specs {
		roots = append(roots, editorRoot{
			name: sp.label,
			dir:  filepath.Join(home, sp.dotDir, "extensions"),
		})
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		cfg := hostenv.XDGConfigHome()
		if cfg != "" {
			roots = append(roots,
				editorRoot{name: "VS Code", dir: filepath.Join(cfg, "Code", "extensions")},
				editorRoot{name: "VSCodium", dir: filepath.Join(cfg, "VSCodium", "extensions")},
			)
		}
	}
	return roots
}

// VSCodeExtensionInstalled reports whether any VS Code-family editor carries an Antigravity extension.
func VSCodeExtensionInstalled() bool {
	home := hostenv.EffectiveHomeDir()
	for _, root := range editorRoots(home) {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && strings.Contains(strings.ToLower(e.Name()), "antigravity") {
				return true
			}
		}
	}
	return false
}

func scanEditorExtensions(home string) []Install {
	return scanEditorExtensionsAt(editorRoots(home), home)
}

func scanEditorExtensionsAt(roots []editorRoot, home string) []Install {
	var results []Install
	seen := make(map[string]bool)
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), "antigravity") {
				continue
			}
			extDir := filepath.Join(root.dir, e.Name())
			if !isEditorExtensionDir(extDir) {
				continue
			}
			key := hostenv.NormalizePathKey(extDir)
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, buildExtensionInstall(extDir, root.name, e.Name(), home, false))
		}
	}
	return results
}

func isEditorExtensionDir(dir string) bool {
	if !hostenv.IsDirectory(dir) || isBlockedSystemDir(dir) {
		return false
	}
	if !hostenv.IsRegularFile(filepath.Join(dir, "extension.js")) &&
		!hostenv.IsRegularFile(filepath.Join(dir, "dist", "extension.js")) &&
		!hostenv.IsRegularFile(filepath.Join(dir, "package.json")) {
		return false
	}
	lower := strings.ToLower(filepath.Base(dir))
	if strings.Contains(lower, "antigravity") {
		return true
	}
	parent := strings.ToLower(filepath.Base(filepath.Dir(dir)))
	return parent == "extensions" && (hostenv.IsRegularFile(filepath.Join(dir, "extension.js")) || hostenv.IsRegularFile(filepath.Join(dir, "dist", "extension.js")))
}

func detectEditorNameFromPath(extDir string) string {
	norm := strings.ToLower(filepath.ToSlash(extDir))
	switch {
	case strings.Contains(norm, "/.vscode-insiders/"):
		return "VS Code Insiders"
	case strings.Contains(norm, "/.vscode-oss/"), strings.Contains(norm, "/.vscodium/"), strings.Contains(norm, "/vscodium/"):
		return "VSCodium"
	case strings.Contains(norm, "/.vscode-server-insiders/"):
		return "VS Code Server Insiders"
	case strings.Contains(norm, "/.vscode-server/"):
		return "VS Code Server"
	case strings.Contains(norm, "/.cursor/"):
		return "Cursor"
	case strings.Contains(norm, "/.windsurf/"):
		return "Windsurf"
	case strings.Contains(norm, "/.trae/"):
		return "Trae"
	default:
		return "VS Code"
	}
}

func buildExtensionInstall(extDir, editorName, folderName, home string, manual bool) Install {
	var extJS string
	for _, cand := range []string{
		filepath.Join(extDir, "extension.js"),
		filepath.Join(extDir, "dist", "extension.js"),
	} {
		if hostenv.IsRegularFile(cand) {
			extJS = cand
			break
		}
	}

	col := newBinaryCollector(true)
	for _, dir := range []string{filepath.Join(extDir, "bin"), filepath.Join(extDir, "resources", "bin"), extDir} {
		col.AddDirMatching(dir, func(name string) bool {
			return isLanguageServerBinary(name) || isAGYBinaryName(name)
		})
	}
	if len(col.Items()) == 0 {
		col.AddAll(geminiBinTargets(home))
	}
	if len(col.Items()) == 0 {
		col.AddAll(platformCLITargets(home))
	}

	if editorName == "" {
		editorName = "VS Code"
	}
	return Install{
		Path:            extDir,
		Type:            TypeExtension,
		Label:           fmt.Sprintf("%s (%s)", editorName, folderName),
		BinaryPath:      col.Primary(),
		Binaries:        col.Items(),
		ExtensionJsPath: extJS,
		Manual:          manual,
	}
}

func resolveExtensionRoot(searchPath, explicitJS string) string {
	if explicitJS != "" {
		dir := filepath.Dir(explicitJS)
		if strings.EqualFold(filepath.Base(dir), "dist") {
			dir = filepath.Dir(dir)
		}
		if isEditorExtensionDir(dir) {
			return dir
		}
	}
	cur := searchPath
	for i := 0; i < 4; i++ {
		if isEditorExtensionDir(cur) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if found := scanEditorExtensionsAt([]editorRoot{{dir: searchPath}}, hostenv.EffectiveHomeDir()); len(found) > 0 {
		return found[0].Path
	}
	return ""
}

func jetbrainsBaseDirs(home string) []string {
	var bases []string
	add := func(p string) {
		bases = appendUniquePath(bases, p)
	}
	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			add(filepath.Join(local, "JetBrains"))
		} else if home != "" {
			add(filepath.Join(home, "AppData", "Local", "JetBrains"))
		}
		if roaming := os.Getenv("APPDATA"); roaming != "" {
			add(filepath.Join(roaming, "JetBrains"))
		} else if home != "" {
			add(filepath.Join(home, "AppData", "Roaming", "JetBrains"))
		}
	case "darwin":
		if home != "" {
			add(filepath.Join(home, "Library", "Caches", "JetBrains"))
			add(filepath.Join(home, "Library", "Application Support", "JetBrains"))
		}
	default:
		if cache := hostenv.XDGCacheHome(); cache != "" {
			add(filepath.Join(cache, "JetBrains"))
		}
		if data := hostenv.XDGDataHome(); data != "" {
			add(filepath.Join(data, "JetBrains"))
		}
		if cfg := hostenv.XDGConfigHome(); cfg != "" {
			add(filepath.Join(cfg, "JetBrains"))
		}
	}
	return bases
}

func scanJetBrainsInstalls(home string) []Install {
	return scanJetBrainsAt(jetbrainsBaseDirs(home), home)
}

func scanJetBrainsAt(bases []string, home string) []Install {
	var results []Install
	seen := make(map[string]bool)

	for _, base := range bases {
		prodEntries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, prod := range prodEntries {
			if !prod.IsDir() {
				continue
			}
			prodDir := filepath.Join(base, prod.Name())
			for _, sub := range []string{"acp-agents", "plugins"} {
				container := filepath.Join(prodDir, sub)
				plugEntries, err := os.ReadDir(container)
				if err != nil {
					continue
				}
				for _, pe := range plugEntries {
					if !pe.IsDir() {
						continue
					}
					lower := strings.ToLower(pe.Name())
					if !strings.Contains(lower, "antigravity") && !strings.Contains(lower, "agy") {
						continue
					}
					pluginDir := filepath.Join(container, pe.Name())
					key := hostenv.NormalizePathKey(pluginDir)
					if seen[key] {
						continue
					}
					seen[key] = true
					results = append(results, buildJetBrainsInstall(pluginDir, prod.Name(), pe.Name(), home, false))
				}
			}
		}
	}
	return results
}

func isJetBrainsPluginDir(dir string) bool {
	if !hostenv.IsDirectory(dir) || isBlockedSystemDir(dir) {
		return false
	}
	lowerPath := strings.ToLower(filepath.ToSlash(dir))
	if !strings.Contains(lowerPath, "/jetbrains/") &&
		!strings.Contains(lowerPath, "/acp-agents/") &&
		!strings.Contains(lowerPath, "/webstorm") {
		return false
	}
	base := strings.ToLower(filepath.Base(dir))
	return strings.Contains(base, "antigravity") || strings.Contains(base, "agy")
}

func detectJetBrainsProductFromPath(dir string) string {
	cur := dir
	for i := 0; i < 4; i++ {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		base := strings.ToLower(filepath.Base(cur))
		if base == "acp-agents" || base == "plugins" {
			return filepath.Base(parent)
		}
		cur = parent
	}
	return "JetBrains"
}

func buildJetBrainsInstall(pluginDir, product, pluginName, home string, manual bool) Install {
	col := newBinaryCollector(true)
	var jsPath string

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 4 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			if e.IsDir() {
				if name != ".agy_orig" && name != "node_modules" {
					walk(full, depth+1)
				}
				continue
			}
			if isJetBrainsBinaryName(name) {
				col.Add(full)
			}
			if jsPath == "" && (strings.EqualFold(name, "extension.js") || strings.EqualFold(name, "main.js")) {
				jsPath = full
			}
		}
	}
	walk(pluginDir, 0)

	if product == "" {
		product = "JetBrains"
	}
	label := fmt.Sprintf("%s (%s)", product, pluginName)
	if !strings.Contains(strings.ToLower(product), "webstorm") && !strings.Contains(strings.ToLower(product), "jetbrains") {
		label = fmt.Sprintf("JetBrains %s (%s)", product, pluginName)
	}
	return Install{
		Path:            pluginDir,
		Type:            TypeJetBrains,
		Label:           label,
		BinaryPath:      col.Primary(),
		Binaries:        col.Items(),
		ExtensionJsPath: jsPath,
		Manual:          manual,
	}
}

func resolveJetBrainsRoot(searchPath string) string {
	cur := searchPath
	for i := 0; i < 5; i++ {
		if isJetBrainsPluginDir(cur) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for _, container := range []string{
		searchPath,
		filepath.Join(searchPath, "acp-agents"),
		filepath.Join(searchPath, "plugins"),
	} {
		entries, err := os.ReadDir(container)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				cand := filepath.Join(container, e.Name())
				if isJetBrainsPluginDir(cand) {
					return cand
				}
			}
		}
	}
	return ""
}
