package linuxinstall

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/asar"
	"agy-patcher/pkg/assets"
)

// Scope controls the destination directory layout for Linux archive installations.
type Scope string

const (
	ScopeUser   Scope = "user"
	ScopeSystem Scope = "system"
	ScopeCustom Scope = "custom"
)

const SystemInstallDir = "/opt/Antigravity"

var (
	ErrNoArchive      = errors.New("archive path is required")
	ErrInvalidArchive = errors.New("archive does not appear to be an Antigravity Linux bundle")
)

// ProgressEvent reports a localized progress milestone during installation.
type ProgressEvent struct {
	Key  string
	Args []any
}

// Options configures installing Antigravity Desktop from a Linux .tar.gz bundle.
type Options struct {
	ArchivePath   string
	Scope         Scope
	CustomDir     string
	CreateDesktop bool
	CreateBinLink bool
	NoSandbox     bool

	// Optional test/custom path overrides.
	HomeDir    string
	DesktopDir string
	IconDir    string
	BinDir     string

	OnProgress func(ProgressEvent)
}

// Result summarizes what was installed and written on disk.
type Result struct {
	InstallDir    string
	BinaryPath    string
	AppBinaryPath string
	DesktopPath   string
	IconPath      string
	BinLinkPath   string
	AppVersion    string
	HasSnap       bool
}

// DefaultOptions returns recommended per-user Linux installation defaults with auto-detected archive and sandbox setting.
func DefaultOptions() Options {
	opts := Options{
		Scope:         ScopeUser,
		CreateDesktop: true,
		CreateBinLink: true,
		NoSandbox:     DetectSandboxIssueDefault(),
	}
	if found := FindArchives(); len(found) > 0 {
		opts.ArchivePath = found[0]
	}
	return opts
}

// DetectSandboxIssueDefault reports whether unprivileged Electron sandboxing is likely blocked (e.g. Ubuntu 24.04+ AppArmor userns restriction).
func DetectSandboxIssueDefault() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == "1"
}

// DetectSnapInstalled reports whether Antigravity is currently present via Snap confinement.
func DetectSnapInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for _, p := range []string{"/snap/antigravity", "/snap/bin/antigravity", "/var/lib/snapd/snaps"} {
		if p == "/var/lib/snapd/snaps" {
			entries, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if strings.HasPrefix(strings.ToLower(e.Name()), "antigravity") {
					return true
				}
			}
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

// FindArchives searches common user download and working directories for Antigravity .tar.gz archives, newest first.
func FindArchives(extraDirs ...string) []string {
	var dirs []string
	if home := hostenv.EffectiveHomeDir(); home != "" {
		dirs = append(dirs,
			filepath.Join(home, "Downloads"),
			filepath.Join(home, "Загрузки"),
			home,
		)
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, extraDirs...)

	type candidate struct {
		path    string
		modTime int64
	}
	seenDirs := make(map[string]bool)
	seenFiles := make(map[string]bool)
	var hits []candidate

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		cleanDir := filepath.Clean(dir)
		key := hostenv.NormalizePathKey(cleanDir)
		if seenDirs[key] {
			continue
		}
		seenDirs[key] = true

		entries, err := os.ReadDir(cleanDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			lower := strings.ToLower(e.Name())
			if !strings.HasPrefix(lower, "antigravity") {
				continue
			}
			if !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") {
				continue
			}
			full := filepath.Join(cleanDir, e.Name())
			if resolved, rErr := filepath.EvalSymlinks(full); rErr == nil && resolved != "" {
				full = resolved
			}
			if !hostenv.IsRegularFile(full) {
				continue
			}
			fKey := hostenv.NormalizePathKey(full)
			if seenFiles[fKey] {
				continue
			}
			seenFiles[fKey] = true
			var mt int64
			if info, sErr := os.Stat(full); sErr == nil {
				mt = info.ModTime().UnixNano()
			}
			hits = append(hits, candidate{path: full, modTime: mt})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].modTime > hits[j].modTime
	})

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.path
	}
	return out
}

// DefaultTargetDir resolves the destination installation directory for scope.
func DefaultTargetDir(scope Scope, customDir, homeDir string) string {
	switch scope {
	case ScopeSystem:
		return SystemInstallDir
	case ScopeCustom:
		if clean := strings.TrimSpace(customDir); clean != "" {
			return filepath.Clean(expandTilde(clean, homeDir))
		}
	}
	if homeDir != "" {
		return filepath.Join(homeDir, ".local", "share", "Antigravity")
	}
	if dataHome := hostenv.XDGDataHome(); dataHome != "" {
		return filepath.Join(dataHome, "Antigravity")
	}
	return filepath.Join(".", "Antigravity")
}

// TargetDirWritable checks whether installing to dir is likely permitted without elevation.
func TargetDirWritable(targetDir string) bool {
	if targetDir == "" {
		return false
	}
	dir := filepath.Clean(targetDir)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return false
			}
			probe, pErr := os.CreateTemp(dir, ".agy-probe-*")
			if pErr != nil {
				return false
			}
			name := probe.Name()
			_ = probe.Close()
			_ = os.Remove(name)
			return true
		}
		if !os.IsNotExist(err) {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// Install extracts an Antigravity .tar.gz bundle, sets permissions, and optionally registers desktop menu & bin links.
func Install(opts Options) (*Result, error) {
	emit := func(key string, args ...any) {
		if opts.OnProgress != nil {
			opts.OnProgress(ProgressEvent{Key: key, Args: args})
		}
	}

	home := opts.HomeDir
	if home == "" {
		home = hostenv.EffectiveHomeDir()
	}

	archivePath := strings.TrimSpace(expandTilde(opts.ArchivePath, home))
	if archivePath == "" {
		return nil, ErrNoArchive
	}
	archivePath = filepath.Clean(archivePath)
	if !hostenv.IsRegularFile(archivePath) {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, archivePath)
	}

	targetDir := DefaultTargetDir(opts.Scope, opts.CustomDir, home)
	parentDir := filepath.Dir(targetDir)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return nil, err
	}

	stagingDir, err := os.MkdirTemp(parentDir, ".antigravity-install-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()

	emit("li_prog_extracting", filepath.Base(archivePath), targetDir)
	if err := extractTarGz(archivePath, stagingDir); err != nil {
		return nil, err
	}

	payloadRoot, err := locatePayloadRoot(stagingDir)
	if err != nil {
		return nil, err
	}

	emit("li_prog_replacing", targetDir)
	if err := replaceDirectoryAtomic(payloadRoot, targetDir); err != nil {
		return nil, err
	}

	appBin := findAppBinary(targetDir)
	lsBin := findLanguageServerBinary(targetDir)
	ver := readInstalledVersion(targetDir)

	res := &Result{
		InstallDir:    targetDir,
		BinaryPath:    lsBin,
		AppBinaryPath: appBin,
		AppVersion:    ver,
		HasSnap:       DetectSnapInstalled(),
	}

	if opts.CreateDesktop && appBin != "" {
		desktopDir, iconDir := desktopAndIconDirs(opts, home)
		if iconDir != "" {
			if iconPath, iErr := writeInstalledIcon(targetDir, iconDir); iErr == nil {
				res.IconPath = iconPath
			}
		}
		if desktopDir != "" {
			desktopPath, dErr := writeDesktopEntry(desktopDir, appBin, res.IconPath, opts.NoSandbox)
			if dErr != nil {
				return res, dErr
			}
			res.DesktopPath = desktopPath
			emit("li_prog_desktop_ok", desktopPath)
			if runtime.GOOS == "linux" {
				if _, lpErr := exec.LookPath("update-desktop-database"); lpErr == nil {
					_ = exec.Command("update-desktop-database", desktopDir).Run()
				}
			}
		}
	}

	if opts.CreateBinLink && appBin != "" {
		binDir := opts.BinDir
		if binDir == "" {
			if opts.Scope == ScopeSystem && opts.CustomDir == "" {
				binDir = "/usr/local/bin"
			} else if home != "" {
				binDir = filepath.Join(home, ".local", "bin")
			}
		}
		if binDir != "" {
			binLink, bErr := writeBinWrapper(binDir, appBin, opts.NoSandbox)
			if bErr != nil {
				return res, bErr
			}
			res.BinLinkPath = binLink
			emit("li_prog_bin_ok", binLink)
		}
	}

	return res, nil
}

func desktopAndIconDirs(opts Options, home string) (string, string) {
	desktopDir := opts.DesktopDir
	iconDir := opts.IconDir

	if opts.Scope == ScopeSystem && opts.CustomDir == "" {
		if desktopDir == "" {
			desktopDir = "/usr/share/applications"
		}
		if iconDir == "" {
			iconDir = "/usr/share/icons/hicolor/256x256/apps"
		}
		return desktopDir, iconDir
	}

	dataHome := ""
	if opts.HomeDir != "" {
		dataHome = filepath.Join(opts.HomeDir, ".local", "share")
	} else {
		dataHome = hostenv.XDGDataHome()
		if dataHome == "" && home != "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
	}
	if desktopDir == "" && dataHome != "" {
		desktopDir = filepath.Join(dataHome, "applications")
	}
	if iconDir == "" && dataHome != "" {
		iconDir = filepath.Join(dataHome, "icons", "hicolor", "256x256", "apps")
	}
	return desktopDir, iconDir
}

func extractTarGz(archivePath, destRoot string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanRoot := filepath.Clean(destRoot)
	rootPrefix := cleanRoot + string(filepath.Separator)

	type dirTime struct {
		path string
		mode os.FileMode
	}
	var dirs []dirTime

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		cleanName := path.Clean(strings.TrimLeft(strings.ReplaceAll(hdr.Name, "\\", "/"), "/"))
		if cleanName == "." || cleanName == "" {
			continue
		}
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || strings.Contains(cleanName, "/../") {
			return fmt.Errorf("unsafe archive entry path: %q", hdr.Name)
		}

		target := filepath.Join(cleanRoot, filepath.FromSlash(cleanName))
		if target != cleanRoot && !strings.HasPrefix(target, rootPrefix) {
			return fmt.Errorf("archive path escapes staging root: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0777
			if mode == 0 {
				mode = 0755
			}
			dirs = append(dirs, dirTime{path: target, mode: mode})

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			mode := resolvedFileMode(cleanName, hdr.Mode)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if runtime.GOOS != "windows" {
				_ = os.Chmod(target, mode)
			}

		case tar.TypeSymlink:
			linkTarget := strings.TrimSpace(hdr.Linkname)
			if linkTarget == "" || strings.HasPrefix(linkTarget, "/") || filepath.IsAbs(linkTarget) {
				return fmt.Errorf("unsafe symlink target %q -> %q", hdr.Name, hdr.Linkname)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(linkTarget)))
			if resolved != cleanRoot && !strings.HasPrefix(resolved, rootPrefix) {
				return fmt.Errorf("symlink escapes staging root: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(filepath.FromSlash(linkTarget), target); err != nil {
				return err
			}
		}
	}

	if runtime.GOOS != "windows" {
		for i := len(dirs) - 1; i >= 0; i-- {
			_ = os.Chmod(dirs[i].path, dirs[i].mode|0700)
		}
	}
	return nil
}

func resolvedFileMode(relSlash string, rawMode int64) os.FileMode {
	mode := os.FileMode(rawMode) & 0777
	if mode == 0 {
		mode = 0644
	}
	base := strings.ToLower(path.Base(relSlash))
	if (rawMode&0111) != 0 ||
		base == "antigravity" ||
		base == "chrome-sandbox" ||
		base == "chrome_crashpad_handler" ||
		strings.Contains(relSlash, "/resources/bin/") ||
		strings.HasPrefix(relSlash, "resources/bin/") ||
		strings.Contains(base, ".so") {
		mode |= 0755
	}
	return mode
}

func locatePayloadRoot(stagingDir string) (string, error) {
	if isAntigravityBundleDir(stagingDir) {
		return stagingDir, nil
	}
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(stagingDir, entries[0].Name())
		if isAntigravityBundleDir(sub) {
			return sub, nil
		}
	}
	return "", ErrInvalidArchive
}

func isAntigravityBundleDir(dir string) bool {
	hasApp := hostenv.IsRegularFile(filepath.Join(dir, "antigravity")) ||
		hostenv.IsRegularFile(filepath.Join(dir, "Antigravity"))
	hasRes := hostenv.IsDirectory(filepath.Join(dir, "resources"))
	if !hasApp && !hasRes {
		return false
	}
	if findLanguageServerBinary(dir) != "" {
		return true
	}
	return hasApp && hostenv.IsRegularFile(filepath.Join(dir, "resources", "app.asar"))
}

func replaceDirectoryAtomic(srcDir, targetDir string) error {
	targetClean := filepath.Clean(targetDir)
	backupDir := ""
	if _, err := os.Stat(targetClean); err == nil {
		backupDir = fmt.Sprintf("%s.prev-%d", targetClean, time.Now().UnixNano())
		if err := os.Rename(targetClean, backupDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Rename(srcDir, targetClean); err != nil {
		if backupDir != "" {
			_ = os.Rename(backupDir, targetClean)
		}
		return err
	}

	if backupDir != "" {
		_ = os.RemoveAll(backupDir)
	}
	return nil
}

func findAppBinary(root string) string {
	for _, name := range []string{"antigravity", "Antigravity"} {
		p := filepath.Join(root, name)
		if hostenv.IsRegularFile(p) {
			return p
		}
	}
	return ""
}

func findLanguageServerBinary(root string) string {
	binDir := filepath.Join(root, "resources", "bin")
	for _, name := range []string{"language_server", "language_server_linux_x64", "language_server.exe"} {
		p := filepath.Join(binDir, name)
		if hostenv.IsRegularFile(p) {
			return p
		}
	}
	entries, err := os.ReadDir(binDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "language_server") {
				return filepath.Join(binDir, e.Name())
			}
		}
	}
	return ""
}

func readInstalledVersion(root string) string {
	asarPath := filepath.Join(root, "resources", "app.asar")
	if raw, err := asar.ReadEntry(asarPath, "package.json"); err == nil {
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &pkg) == nil && strings.TrimSpace(pkg.Version) != "" {
			return strings.TrimSpace(pkg.Version)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(root, "version")); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func writeInstalledIcon(installDir, iconDir string) (string, error) {
	if err := os.MkdirAll(iconDir, 0755); err != nil {
		return "", err
	}
	asarPath := filepath.Join(installDir, "resources", "app.asar")
	var iconBytes []byte
	for _, candidate := range []string{"icon.png", "assets/icon.png", "build/icon.png"} {
		if data, err := asar.ReadEntry(asarPath, candidate); err == nil && len(data) > 0 {
			iconBytes = data
			break
		}
	}
	if len(iconBytes) == 0 {
		iconBytes = assets.IconPNG
	}
	iconPath := filepath.Join(iconDir, "antigravity.png")
	if err := hostenv.WriteFileAtomic(iconPath, iconBytes, 0644, nil); err != nil {
		return "", err
	}
	return iconPath, nil
}

func writeDesktopEntry(desktopDir, appBin, iconPath string, noSandbox bool) (string, error) {
	if err := os.MkdirAll(desktopDir, 0755); err != nil {
		return "", err
	}
	execCmd := fmt.Sprintf("%q %%U", appBin)
	if noSandbox {
		execCmd = fmt.Sprintf("%q --no-sandbox %%U", appBin)
	}
	iconLine := "antigravity"
	if iconPath != "" {
		iconLine = iconPath
	}
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Antigravity
Comment=Antigravity Agentic Desktop Application
Exec=%s
Icon=%s
Terminal=false
Categories=Development;IDE;
StartupWMClass=Antigravity
`, execCmd, iconLine)

	desktopPath := filepath.Join(desktopDir, "antigravity.desktop")
	if err := hostenv.WriteFileAtomic(desktopPath, []byte(content), 0644, nil); err != nil {
		return "", err
	}
	return desktopPath, nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func writeBinWrapper(binDir, appBin string, noSandbox bool) (string, error) {
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	args := `"$@"`
	if noSandbox {
		args = `--no-sandbox "$@"`
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s %s\n", shellSingleQuote(appBin), args)
	binPath := filepath.Join(binDir, "antigravity")
	if err := hostenv.WriteFileAtomic(binPath, []byte(script), 0755, nil); err != nil {
		return "", err
	}
	return binPath, nil
}

func expandTilde(raw, homeDir string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || homeDir == "" {
		return trimmed
	}
	if trimmed == "~" {
		return homeDir
	}
	if strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~\\") {
		return filepath.Join(homeDir, trimmed[2:])
	}
	return trimmed
}
