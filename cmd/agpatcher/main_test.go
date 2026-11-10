package main

import (
	"os"
	"path/filepath"
	"testing"

	"agy-patcher/pkg/tui"
)

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"patch", "--lang", "ru", "--path=C:\\ag"})
	if err != nil {
		t.Fatalf("parseArgs failed: %v", err)
	}
	if opts.command != "patch" || opts.lang != "ru" || opts.path != "C:\\ag" {
		t.Fatalf("unexpected opts: %+v", opts)
	}

	optsTUI, err := parseArgs([]string{"--tui"})
	if err != nil || optsTUI.command != "--tui" {
		t.Fatalf("expected --tui command, got %+v, err=%v", optsTUI, err)
	}

	optsTray, err := parseArgs([]string{"--tray", "--no-proxy", "--lang", "en"})
	if err != nil || !optsTray.tray || !optsTray.noProxy || optsTray.lang != "en" {
		t.Fatalf("unexpected tray opts: %+v, err=%v", optsTray, err)
	}

	optsRU, err := parseArgs([]string{"ru-install", "--force", "--path", "C:\\ag"})
	if err != nil || optsRU.command != "ru-install" || !optsRU.force || optsRU.path != "C:\\ag" {
		t.Fatalf("unexpected ru-install opts: %+v, err=%v", optsRU, err)
	}

	if _, err := parseArgs([]string{"--lang"}); err == nil {
		t.Fatalf("expected error for missing --lang value")
	}
	if _, err := parseArgs([]string{"patch", "--path"}); err == nil {
		t.Fatalf("expected error for missing --path value")
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	if code := run([]string{"--version"}); code != 0 {
		t.Fatalf("expected exit 0 for --version, got %d", code)
	}
	if code := run([]string{"--help", "--lang", "ru"}); code != 0 {
		t.Fatalf("expected exit 0 for --help, got %d", code)
	}
	if code := run([]string{"unknowncmd"}); code != 1 {
		t.Fatalf("expected exit 1 for unknown command, got %d", code)
	}
	if !tui.IsInteractive() {
		if code := run(nil); code != 1 {
			t.Fatalf("expected exit 1 for empty args in non-interactive mode, got %d", code)
		}
	}
}

func TestRunPatchStatusAndRestoreWithManualPath(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "resources", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	binFile := filepath.Join(binDir, "language_server.exe")
	orig := []byte("ineligible https_proxy")
	if err := os.WriteFile(binFile, orig, 0755); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{"patch", "--path", dir}); code != 0 {
		t.Fatalf("patch command failed with code %d", code)
	}
	got, _ := os.ReadFile(binFile)
	if string(got) != "agy_pass_0 AGY_PROXY_U" {
		t.Fatalf("unexpected patched content: %q", string(got))
	}

	if code := run([]string{"status", "--path", dir, "--lang", "ru"}); code != 0 {
		t.Fatalf("status command failed with code %d", code)
	}

	if code := run([]string{"restore", "--path", dir}); code != 0 {
		t.Fatalf("restore command failed with code %d", code)
	}
	restored, _ := os.ReadFile(binFile)
	if string(restored) != string(orig) {
		t.Fatalf("unexpected restored content: %q", string(restored))
	}

	// Foreign/unrecognized binaries are never touched and report failure clearly.
	foreign := []byte("externally modified binary without stock tokens")
	_ = os.WriteFile(binFile, foreign, 0755)
	if code := run([]string{"patch", "--path", dir}); code == 0 {
		t.Fatalf("expected non-zero exit when signature is missing")
	}
	after, _ := os.ReadFile(binFile)
	if string(after) != string(foreign) {
		t.Fatalf("expected unrecognized file untouched, got %q", string(after))
	}
}
