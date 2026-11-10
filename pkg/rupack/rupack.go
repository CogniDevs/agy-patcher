package rupack

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"agy-patcher/pkg/asar"
	"agy-patcher/pkg/discovery"
)

const (
	DefaultAPIURL  = "https://api.github.com/repos/SPIDIKSY/antigravity-ru/contents/resources"
	DefaultZipURL  = "https://github.com/SPIDIKSY/antigravity-ru/archive/refs/heads/main.zip"
	BackupAsarName = "app.asar.original_backup"
	BundleDirName  = "web_bundle_ru"

	maxDownloadBytes = 512 << 20 // 512 MiB safety cap
)

var (
	ErrNotDesktop = errors.New("target installation is not Antigravity Desktop")
	ErrNoBackup   = errors.New("app.asar.original_backup not found")
)

// UnsupportedVersionError is returned when the installed Desktop version is missing from antigravity-ru and Force is false.
type UnsupportedVersionError struct {
	Detected  string
	Available []string
}

func (e *UnsupportedVersionError) Error() string {
	ver := e.Detected
	if ver == "" {
		ver = "unknown"
	}
	return fmt.Sprintf("antigravity desktop version %s is not in supported packs (%s)", ver, strings.Join(e.Available, ", "))
}

// Status describes the Desktop Russian localization state for an installation.
type Status struct {
	Applicable    bool
	Version       string
	Installed     bool
	BundleExists  bool
	AsarLocalized bool
	HasBackup     bool
}

// InstallOptions controls remote pack download, version matching, and logging.
type InstallOptions struct {
	Force  bool
	Logf   func(level, msg string)
	APIURL string
	ZipURL string
	Client *http.Client
}

// InstallResult reports what changed during localization installation.
type InstallResult struct {
	DesktopVersion string
	PackVersion    string
	BackupCreated  bool
	BackupKept     bool
}

// UninstallResult reports what changed during localization rollback.
type UninstallResult struct {
	AsarRestored  bool
	BundleRemoved bool
	MissingBackup bool
}

// InspectDesktop checks offline whether inst is an Antigravity Desktop install and whether RU pack is installed.
func InspectDesktop(inst discovery.Install) Status {
	var st Status
	if inst.Type != discovery.TypeDesktop || inst.ResourcesDir == "" {
		return st
	}
	appAsar := filepath.Join(inst.ResourcesDir, "app.asar")
	bakAsar := filepath.Join(inst.ResourcesDir, BackupAsarName)
	bundleDir := filepath.Join(inst.ResourcesDir, BundleDirName)

	asarExists := fileExists(appAsar)
	bakExists := fileExists(bakAsar)
	if !asarExists && !bakExists {
		return st
	}

	st.Applicable = true
	st.HasBackup = bakExists
	if info, err := os.Stat(bundleDir); err == nil && info.IsDir() {
		st.BundleExists = true
	}
	if asarExists {
		st.AsarLocalized = isAsarLocalized(appAsar)
		st.Version = detectAsarVersion(appAsar)
	}
	if st.Version == "" && bakExists {
		st.Version = detectAsarVersion(bakAsar)
	}
	if st.Version == "" && inst.Path != "" {
		if raw, err := os.ReadFile(filepath.Join(inst.Path, "version")); err == nil {
			st.Version = strings.TrimSpace(string(raw))
		}
	}
	st.Installed = st.BundleExists && st.AsarLocalized
	return st
}

// Install downloads ONLY the version-matched resources/<ver>/{app.asar,web_bundle_ru} from GitHub
// and installs them into inst.ResourcesDir while preserving app.asar.original_backup.
func Install(ctx context.Context, inst discovery.Install, opts InstallOptions) (*InstallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	st := InspectDesktop(inst)
	if !st.Applicable {
		return nil, ErrNotDesktop
	}

	resDir := inst.ResourcesDir
	targetAsar := filepath.Join(resDir, "app.asar")
	bakAsar := filepath.Join(resDir, BackupAsarName)
	targetBundle := filepath.Join(resDir, BundleDirName)

	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	zipURL := opts.ZipURL
	if zipURL == "" {
		zipURL = DefaultZipURL
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}

	logMsg := func(level, format string, args ...any) {
		if opts.Logf != nil {
			opts.Logf(level, fmt.Sprintf(format, args...))
		}
	}

	detectedVer := st.Version
	logMsg("info", "Desktop version: %s", orUnknown(detectedVer))

	// Fast pre-check via GitHub API before downloading the repository archive.
	if remoteVers, err := fetchVersionsFromAPI(ctx, client, apiURL); err == nil && len(remoteVers) > 0 {
		sortVersions(remoteVers)
		if !opts.Force && detectedVer != "" && !containsString(remoteVers, detectedVer) {
			return nil, &UnsupportedVersionError{Detected: detectedVer, Available: remoteVers}
		}
	}

	logMsg("info", "Downloading localization pack from GitHub...")
	tmpZip, err := downloadToTemp(ctx, client, zipURL)
	if err != nil {
		return nil, fmt.Errorf("download antigravity-ru archive: %w", err)
	}
	defer os.Remove(tmpZip)

	zr, err := zip.OpenReader(tmpZip)
	if err != nil {
		return nil, fmt.Errorf("open downloaded zip: %w", err)
	}
	defer zr.Close()

	available := discoverZipVersions(zr.File)
	if len(available) == 0 {
		return nil, errors.New("no resources/<version>/app.asar packs found inside archive")
	}

	chosenVer := ""
	if detectedVer != "" && containsString(available, detectedVer) {
		chosenVer = detectedVer
	} else if !opts.Force {
		return nil, &UnsupportedVersionError{Detected: detectedVer, Available: available}
	} else {
		chosenVer = available[len(available)-1]
		logMsg("warn", "Force mode: using %s for Desktop %s", chosenVer, orUnknown(detectedVer))
	}

	stageDir, err := os.MkdirTemp("", "agpatcher-ru-stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	if err := extractVersionFromZip(zr.File, chosenVer, stageDir); err != nil {
		return nil, fmt.Errorf("extract pack %s: %w", chosenVer, err)
	}

	stagedAsar := filepath.Join(stageDir, "app.asar")
	stagedBundle := filepath.Join(stageDir, BundleDirName)
	if !fileExists(stagedAsar) || !dirExists(stagedBundle) {
		return nil, fmt.Errorf("incomplete localization files for version %s", chosenVer)
	}

	res := &InstallResult{
		DesktopVersion: detectedVer,
		PackVersion:    chosenVer,
	}

	// Preserve untouched original app.asar before any replacement.
	if fileExists(targetAsar) && !isAsarLocalized(targetAsar) {
		logMsg("info", "Saving backup: %s", BackupAsarName)
		if err := copyFileAtomic(targetAsar, bakAsar); err != nil {
			return nil, fmt.Errorf("create original backup: %w", err)
		}
		res.BackupCreated = true
	} else if fileExists(bakAsar) {
		res.BackupKept = true
	} else {
		logMsg("warn", "app.asar is already modified and %s is absent; rollback will not restore original app.asar", BackupAsarName)
	}

	logMsg("info", "Installing %s (v%s)...", BundleDirName, chosenVer)
	if err := replaceDirWithRetry(stagedBundle, targetBundle, inst.Path, logMsg); err != nil {
		return nil, fmt.Errorf("install %s: %w", BundleDirName, err)
	}

	logMsg("info", "Installing localized app.asar (v%s)...", chosenVer)
	if err := replaceFileWithRetry(stagedAsar, targetAsar, inst.Path, logMsg); err != nil {
		return nil, fmt.Errorf("install app.asar: %w", err)
	}

	return res, nil
}

// Uninstall restores app.asar from app.asar.original_backup and removes web_bundle_ru.
func Uninstall(inst discovery.Install, logf func(level, msg string)) (*UninstallResult, error) {
	if inst.Type != discovery.TypeDesktop || inst.ResourcesDir == "" {
		return nil, ErrNotDesktop
	}
	resDir := inst.ResourcesDir
	targetAsar := filepath.Join(resDir, "app.asar")
	bakAsar := filepath.Join(resDir, BackupAsarName)
	targetBundle := filepath.Join(resDir, BundleDirName)

	logMsg := func(level, format string, args ...any) {
		if logf != nil {
			logf(level, fmt.Sprintf(format, args...))
		}
	}

	res := &UninstallResult{}
	var errs []error

	if fileExists(bakAsar) {
		logMsg("info", "Restoring original app.asar from %s...", BackupAsarName)
		if err := replaceFileWithRetry(bakAsar, targetAsar, inst.Path, logMsg); err != nil {
			errs = append(errs, fmt.Errorf("restore app.asar: %w", err))
		} else {
			res.AsarRestored = true
			if rmErr := os.Remove(bakAsar); rmErr != nil && !os.IsNotExist(rmErr) {
				errs = append(errs, fmt.Errorf("remove %s: %w", BackupAsarName, rmErr))
			}
		}
	} else if fileExists(targetAsar) && isAsarLocalized(targetAsar) {
		res.MissingBackup = true
	}

	if dirExists(targetBundle) {
		logMsg("info", "Removing %s...", BundleDirName)
		if err := removeDirWithRetry(targetBundle, inst.Path, logMsg); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", BundleDirName, err))
		} else {
			res.BundleRemoved = true
		}
	}

	if len(errs) > 0 {
		return res, errors.Join(errs...)
	}
	if res.MissingBackup {
		return res, ErrNoBackup
	}
	return res, nil
}

func detectAsarVersion(asarPath string) string {
	raw, err := asar.ReadEntry(asarPath, "package.json")
	if err != nil {
		return ""
	}
	var pj struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pj); err != nil {
		return ""
	}
	return strings.TrimSpace(pj.Version)
}

func isAsarLocalized(asarPath string) bool {
	if src, err := asar.ReadEntry(asarPath, "dist/languageServer.js"); err == nil {
		return bytes.Contains(src, []byte(BundleDirName))
	}
	f, err := os.Open(asarPath)
	if err != nil {
		return false
	}
	defer f.Close()

	needle := []byte(BundleDirName)
	buf := make([]byte, 512*1024)
	overlap := len(needle) - 1
	carry := 0
	for {
		n, err := f.Read(buf[carry:])
		total := carry + n
		if total > 0 && bytes.Contains(buf[:total], needle) {
			return true
		}
		if err != nil {
			return false
		}
		if total >= overlap {
			copy(buf[:overlap], buf[total-overlap:total])
			carry = overlap
		} else {
			carry = total
		}
	}
}

func fetchVersionsFromAPI(ctx context.Context, client *http.Client, apiURL string) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agpatcher")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api http %d", resp.StatusCode)
	}

	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&entries); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type == "dir" && e.Name != "" && !strings.HasPrefix(e.Name, ".") {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

func downloadToTemp(ctx context.Context, client *http.Client, zipURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zipURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "agpatcher")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d from %s", resp.StatusCode, zipURL)
	}

	tmp, err := os.CreateTemp("", "agpatcher-ru-*.zip")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	limited := io.LimitReader(resp.Body, maxDownloadBytes+1)
	n, err := io.Copy(tmp, limited)
	closeErr := tmp.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > maxDownloadBytes {
		return "", errors.New("downloaded archive exceeded size limit")
	}
	cleanup = false
	return tmpName, nil
}

// discoverZipVersions finds every <ver> under any "<root>/resources/<ver>/app.asar" or "resources/<ver>/app.asar".
func discoverZipVersions(files []*zip.File) []string {
	seen := make(map[string]bool)
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		if ver, rel, ok := parseZipResourceEntry(f.Name); ok && rel == "app.asar" {
			seen[ver] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sortVersions(out)
	return out
}

func parseZipResourceEntry(rawName string) (ver, rel string, ok bool) {
	norm := strings.TrimPrefix(strings.ReplaceAll(rawName, "\\", "/"), "./")
	parts := strings.Split(norm, "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "resources" && parts[i+1] != "" {
			rest := strings.Join(parts[i+2:], "/")
			if rest != "" {
				return parts[i+1], rest, true
			}
		}
	}
	return "", "", false
}

func extractVersionFromZip(files []*zip.File, chosenVer, stageDir string) error {
	for _, f := range files {
		if f.FileInfo().IsDir() || f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		ver, rel, ok := parseZipResourceEntry(f.Name)
		if !ok || ver != chosenVer {
			continue
		}
		if rel != "app.asar" && !strings.HasPrefix(rel, BundleDirName+"/") {
			continue
		}
		cleanRel := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") || cleanRel == "." {
			return fmt.Errorf("unsafe zip entry path: %q", f.Name)
		}
		dst := filepath.Join(stageDir, cleanRel)
		relCheck, err := filepath.Rel(stageDir, dst)
		if err != nil || strings.HasPrefix(relCheck, "..") {
			return fmt.Errorf("zip-slip path rejected: %q", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(rc, maxDownloadBytes))
		closeOutErr := out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeOutErr != nil {
			return closeOutErr
		}
	}
	return nil
}

func replaceFileWithRetry(src, dst, appDir string, logMsg func(level, format string, args ...any)) error {
	err := copyFileAtomic(src, dst)
	if err == nil {
		return nil
	}
	if shouldRetryAfterKill(err) {
		killDesktopProcess(appDir, logMsg)
		time.Sleep(700 * time.Millisecond)
		err = copyFileAtomic(src, dst)
	}
	return err
}

func replaceDirWithRetry(srcDir, dstDir, appDir string, logMsg func(level, format string, args ...any)) error {
	stageNew := dstDir + ".new." + strconv.FormatInt(time.Now().UnixNano(), 36)
	_ = os.RemoveAll(stageNew)
	if err := copyDirAll(srcDir, stageNew); err != nil {
		_ = os.RemoveAll(stageNew)
		return err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageNew)
		}
	}()

	if err := swapDir(stageNew, dstDir); err != nil {
		if shouldRetryAfterKill(err) {
			killDesktopProcess(appDir, logMsg)
			time.Sleep(700 * time.Millisecond)
			err = swapDir(stageNew, dstDir)
		}
		if err != nil {
			return err
		}
	}
	cleanupStage = false
	return nil
}

func removeDirWithRetry(targetDir, appDir string, logMsg func(level, format string, args ...any)) error {
	err := os.RemoveAll(targetDir)
	if err == nil {
		return nil
	}
	if shouldRetryAfterKill(err) {
		killDesktopProcess(appDir, logMsg)
		time.Sleep(700 * time.Millisecond)
		err = os.RemoveAll(targetDir)
	}
	return err
}

func swapDir(srcNew, dst string) error {
	if dirExists(dst) {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	return os.Rename(srcNew, dst)
}

func shouldRetryAfterKill(err error) bool {
	if err == nil {
		return false
	}
	return runtime.GOOS == "windows" || !errors.Is(err, os.ErrPermission)
}

func killDesktopProcess(appDir string, logMsg func(level, format string, args ...any)) {
	if runtime.GOOS == "windows" {
		if logMsg != nil {
			logMsg("warn", "Closing running Antigravity Desktop before replacing locked resources...")
		}
		_ = exec.Command("taskkill", "/F", "/IM", "Antigravity.exe").Run()
		_ = exec.Command("taskkill", "/F", "/IM", "language_server.exe").Run()
		return
	}
	if appDir != "" {
		base := filepath.Base(appDir)
		if runtime.GOOS == "darwin" && strings.HasSuffix(base, ".app") {
			name := strings.TrimSuffix(base, ".app")
			if name != "" {
				if logMsg != nil {
					logMsg("warn", "Closing running %s before replacing resources...", name)
				}
				_ = exec.Command("pkill", "-x", name).Run()
			}
			return
		}
	}
	if logMsg != nil {
		logMsg("warn", "Closing running Antigravity Desktop before replacing resources...")
	}
	_ = exec.Command("pkill", "-x", "antigravity").Run()
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Chmod(tmpName, 0644)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func copyDirAll(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDirAll(sp, dp); err != nil {
				return err
			}
			continue
		}
		if err := copyFileAtomic(sp, dp); err != nil {
			return err
		}
	}
	return nil
}

func sortVersions(vers []string) {
	sort.Slice(vers, func(i, j int) bool {
		return compareSemver(vers[i], vers[j]) < 0
	})
}

func compareSemver(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var va, vb int
		if i < len(pa) {
			va, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			vb, _ = strconv.Atoi(pb[i])
		}
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}
