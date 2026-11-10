package cleaner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"agy-patcher/pkg/discovery"
	"agy-patcher/pkg/patcher"
	"agy-patcher/pkg/rupack"
)

func TestRestoreAndCleanArtifacts(t *testing.T) {
	dir := t.TempDir()
	binFile := filepath.Join(dir, "language_server.exe")
	jsDir := filepath.Join(dir, "resources", "app", "out")
	if err := os.MkdirAll(jsDir, 0755); err != nil {
		t.Fatal(err)
	}
	jsFile := filepath.Join(jsDir, "main.js")
	origBin := []byte("orig ineligible https_proxy")
	origJS := []byte(`"ineligible"`)

	if err := os.WriteFile(binFile, origBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsFile, origJS, 0644); err != nil {
		t.Fatal(err)
	}
	if err := patcher.PatchBinary(binFile); err != nil {
		t.Fatal(err)
	}
	if err := patcher.PatchJS(jsFile); err != nil {
		t.Fatal(err)
	}
	tmpLeftover := filepath.Join(dir, "language_server.exe.9999.agystg")
	if err := os.WriteFile(tmpLeftover, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := patcher.RestoreFile(binFile); err != nil {
		t.Fatal(err)
	}
	if err := patcher.RestoreFile(jsFile); err != nil {
		t.Fatal(err)
	}
	if err := patcher.CleanArtifacts(binFile); err != nil {
		t.Fatal(err)
	}
	if err := patcher.CleanArtifacts(jsFile); err != nil {
		t.Fatal(err)
	}

	gotBin, _ := os.ReadFile(binFile)
	gotJS, _ := os.ReadFile(jsFile)
	if !bytes.Equal(gotBin, origBin) {
		t.Fatalf("binary not restored: %q", gotBin)
	}
	if !bytes.Equal(gotJS, origJS) {
		t.Fatalf("js not restored: %q", gotJS)
	}
	if _, err := os.Stat(tmpLeftover); !os.IsNotExist(err) {
		t.Fatalf("expected temp leftover removed")
	}

	_ = discovery.Install{Path: dir, BinaryPath: binFile, Binaries: []string{binFile}, JsPath: jsFile}

	// Verify CleanInstalls restores Desktop RU localization backup and deletes web_bundle_ru.
	desktopDir := t.TempDir()
	resDir := filepath.Join(desktopDir, "resources")
	bundleDir := filepath.Join(resDir, rupack.BundleDirName)
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bundleDir, "index.html"), []byte("ru"), 0644)
	_ = os.WriteFile(filepath.Join(resDir, "app.asar"), []byte("localized web_bundle_ru"), 0644)
	_ = os.WriteFile(filepath.Join(resDir, rupack.BackupAsarName), []byte("pristine original"), 0644)

	rep := CleanInstalls([]discovery.Install{{
		Type:         discovery.TypeDesktop,
		Path:         desktopDir,
		ResourcesDir: resDir,
		AsarPath:     filepath.Join(resDir, "app.asar"),
	}})
	if rep.Err() != nil {
		t.Fatalf("CleanInstalls returned error: %v", rep.Err())
	}
	if got, _ := os.ReadFile(filepath.Join(resDir, "app.asar")); string(got) != "pristine original" {
		t.Fatalf("expected app.asar restored from original_backup, got %q", string(got))
	}
	if _, err := os.Stat(bundleDir); !os.IsNotExist(err) {
		t.Fatalf("expected web_bundle_ru removed during CleanInstalls")
	}
}
