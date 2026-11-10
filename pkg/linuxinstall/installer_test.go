package linuxinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildMiniAsar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	type entry struct {
		Size   int    `json:"size"`
		Offset string `json:"offset"`
	}
	filesMap := make(map[string]entry, len(files))
	var data bytes.Buffer
	offset := 0
	for name, content := range files {
		filesMap[name] = entry{
			Size:   len(content),
			Offset: itoa(offset),
		}
		data.Write(content)
		offset += len(content)
	}
	root := map[string]any{"files": filesMap}
	hdrJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	alignedLen := (len(hdrJSON) + 3) &^ 3
	padded := append([]byte{}, hdrJSON...)
	for len(padded) < alignedLen {
		padded = append(padded, 0)
	}

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(4))
	_ = binary.Write(&out, binary.LittleEndian, uint32(alignedLen+8))
	_ = binary.Write(&out, binary.LittleEndian, uint32(alignedLen+4))
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(hdrJSON)))
	out.Write(padded)
	out.Write(data.Bytes())
	return out.Bytes()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func buildTarGzArchive(t *testing.T, archivePath string, entries map[string]struct {
	mode int64
	data []byte
}) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, ent := range entries {
		mode := ent.mode
		if mode == 0 {
			mode = 0644
		}
		hdr := &tar.Header{
			Name: name,
			Mode: mode,
			Size: int64(len(ent.data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(ent.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromArchiveAndDesktopArtifacts(t *testing.T) {
	tmp := t.TempDir()
	downloads := filepath.Join(tmp, "Downloads")
	if err := os.MkdirAll(downloads, 0755); err != nil {
		t.Fatal(err)
	}

	miniIcon := []byte("\x89PNG\r\n\x1a\nFAKEICON")
	asarBytes := buildMiniAsar(t, map[string][]byte{
		"package.json": []byte(`{"name":"antigravity","version":"2.31.0"}`),
		"icon.png":     miniIcon,
	})

	archivePath := filepath.Join(downloads, "Antigravity.tar.gz")
	buildTarGzArchive(t, archivePath, map[string]struct {
		mode int64
		data []byte
	}{
		"Antigravity-x64/antigravity":                   {mode: 0755, data: []byte("elf-main")},
		"Antigravity-x64/resources/bin/language_server": {mode: 0755, data: []byte("ineligible https_proxy")},
		"Antigravity-x64/resources/app.asar":            {mode: 0644, data: asarBytes},
		"Antigravity-x64/version":                       {mode: 0644, data: []byte("44.9.1")},
	})

	wantArchive := archivePath
	if resolved, err := filepath.EvalSymlinks(archivePath); err == nil && resolved != "" {
		wantArchive = resolved
	}
	found := FindArchives(downloads)
	if len(found) == 0 {
		t.Fatalf("expected FindArchives to detect %q, got none", wantArchive)
	}
	fiGot, errGot := os.Stat(found[0])
	fiWant, errWant := os.Stat(wantArchive)
	if found[0] != wantArchive && (errGot != nil || errWant != nil || !os.SameFile(fiGot, fiWant)) {
		t.Fatalf("expected FindArchives to detect %q, got %v", wantArchive, found)
	}

	home := filepath.Join(tmp, "home")
	desktopDir := filepath.Join(home, ".local", "share", "applications")
	iconDir := filepath.Join(home, ".local", "share", "icons", "hicolor", "256x256", "apps")
	binDir := filepath.Join(home, ".local", "bin")

	res, err := Install(Options{
		ArchivePath:   archivePath,
		Scope:         ScopeUser,
		HomeDir:       home,
		DesktopDir:    desktopDir,
		IconDir:       iconDir,
		BinDir:        binDir,
		CreateDesktop: true,
		CreateBinLink: true,
		NoSandbox:     true,
	})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	wantDir := filepath.Join(home, ".local", "share", "Antigravity")
	if res.InstallDir != wantDir || res.AppVersion != "2.31.0" {
		t.Fatalf("unexpected install result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "resources", "bin", "language_server")); err != nil {
		t.Fatalf("missing extracted language_server: %v", err)
	}

	desktopRaw, err := os.ReadFile(res.DesktopPath)
	if err != nil {
		t.Fatalf("missing .desktop file: %v", err)
	}
	desktopStr := string(desktopRaw)
	if !strings.Contains(desktopStr, "--no-sandbox") || !strings.Contains(desktopStr, res.IconPath) {
		t.Fatalf("unexpected .desktop content:\n%s", desktopStr)
	}

	iconRaw, err := os.ReadFile(res.IconPath)
	if err != nil || string(iconRaw) != string(miniIcon) {
		t.Fatalf("expected extracted icon.png from app.asar, got %q, err=%v", string(iconRaw), err)
	}

	binRaw, err := os.ReadFile(res.BinLinkPath)
	if err != nil || !strings.Contains(string(binRaw), "--no-sandbox") || !strings.Contains(string(binRaw), res.AppBinaryPath) {
		t.Fatalf("unexpected bin wrapper script:\n%s (err=%v, want %q)", string(binRaw), err, res.AppBinaryPath)
	}
}

func TestInstallRejectsPathTraversalAndNonAntigravityBundle(t *testing.T) {
	tmp := t.TempDir()
	evilArchive := filepath.Join(tmp, "evil.tar.gz")
	buildTarGzArchive(t, evilArchive, map[string]struct {
		mode int64
		data []byte
	}{
		"../outside.txt": {mode: 0644, data: []byte("oops")},
	})

	if _, err := Install(Options{ArchivePath: evilArchive, HomeDir: tmp}); err == nil {
		t.Fatalf("expected error on path traversal tar entry")
	}

	unrelated := filepath.Join(tmp, "unrelated.tar.gz")
	buildTarGzArchive(t, unrelated, map[string]struct {
		mode int64
		data []byte
	}{
		"other-app/readme.txt": {mode: 0644, data: []byte("hello")},
	})

	_, err := Install(Options{ArchivePath: unrelated, HomeDir: tmp})
	if !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}
