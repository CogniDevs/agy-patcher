package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsLanguageServerBinaryIgnoresBackupsAndTemp(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"language_server.exe", true},
		{"language_server_windows_x64.exe", true},
		{"language_server_linux_x64", true},
		{"language_server_v1.2.3", true},
		{"language_server.exe.bak", false},
		{"language_server.exe.1234.agystg", false},
		{"language_server.bak", false},
		{"language_server.tmp", false},
		{"language_server.sig", false},
		{"other_binary.exe", false},
	}

	for _, tc := range cases {
		if got := isLanguageServerBinary(tc.name); got != tc.want {
			t.Errorf("isLanguageServerBinary(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestResolveManualPath(t *testing.T) {
	t.Run("IDE root", func(t *testing.T) {
		root := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		binDir := filepath.Join(root, "resources", "app", "extensions", "antigravity", "bin")
		jsDir := filepath.Join(root, "resources", "app", "out")
		if err := os.MkdirAll(binDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(jsDir, 0755); err != nil {
			t.Fatal(err)
		}
		binFile := filepath.Join(binDir, "language_server_windows_x64.exe")
		bakFile := binFile + ".bak"
		jsFile := filepath.Join(jsDir, "main.js")
		if err := os.WriteFile(bakFile, []byte("bak"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binFile, []byte("bin"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jsFile, []byte("js"), 0644); err != nil {
			t.Fatal(err)
		}

		inst := ResolveManualPath(`"` + binFile + `"`)
		if inst == nil {
			t.Fatalf("expected non-nil Install")
		}
		if inst.Type != TypeIDE {
			t.Fatalf("expected TypeIDE, got %v", inst.Type)
		}
		if inst.BinaryPath != binFile {
			t.Fatalf("expected BinaryPath %q, got %q", binFile, inst.BinaryPath)
		}
		if inst.JsPath != jsFile {
			t.Fatalf("expected JsPath %q, got %q", jsFile, inst.JsPath)
		}
	})

	t.Run("Parent folder subfolder lookup", func(t *testing.T) {
		parent := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			parent = resolved
		}
		appRoot := filepath.Join(parent, "Antigravity")
		if err := os.MkdirAll(filepath.Join(appRoot, "resources", "bin"), 0755); err != nil {
			t.Fatal(err)
		}
		binFile := filepath.Join(appRoot, "resources", "bin", "language_server.exe")
		if err := os.WriteFile(binFile, []byte("bin"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appRoot, "resources", "app.asar"), []byte("asar"), 0644); err != nil {
			t.Fatal(err)
		}

		inst := ResolveManualPath(parent)
		if inst == nil {
			t.Fatalf("expected Install resolved from subfolder")
		}
		if inst.Type != TypeDesktop || inst.Path != appRoot {
			t.Fatalf("unexpected resolved install: %+v", inst)
		}
	})

	t.Run("Backend dir", func(t *testing.T) {
		root := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		binFile := filepath.Join(root, "language_server_linux_x64")
		if err := os.WriteFile(binFile, []byte("elf"), 0755); err != nil {
			t.Fatal(err)
		}

		inst := ResolveManualPath(root)
		if inst == nil {
			t.Fatalf("expected non-nil Install for backend dir")
		}
		if inst.Type != TypeBackend {
			t.Fatalf("expected TypeBackend, got %v", inst.Type)
		}
		if inst.BinaryPath != binFile {
			t.Fatalf("expected %q, got %q", binFile, inst.BinaryPath)
		}
	})
}

func TestDedupeInstallsPreservesMultipleBackendsInSameDir(t *testing.T) {
	dir := filepath.Join("home", ".gemini", "bin")
	in := []Install{
		{Path: dir, Type: TypeBackend, BinaryPath: filepath.Join(dir, "language_server_1")},
		{Path: dir, Type: TypeBackend, BinaryPath: filepath.Join(dir, "language_server_2")},
		{Path: dir, Type: TypeBackend, BinaryPath: filepath.Join(dir, "language_server_1")},
	}
	got := dedupeInstalls(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique installs, got %d: %+v", len(got), got)
	}
}

func TestBinaryCollectorIgnoresScriptsAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sh := filepath.Join(dir, "agy")
	bin := filepath.Join(dir, "language_server_linux_x64")
	bak := filepath.Join(dir, "language_server_linux_x64.agy_orig")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\nexec /opt/agy \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("\x7fELFdummy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("\x7fELFbackup"), 0755); err != nil {
		t.Fatal(err)
	}
	col := newBinaryCollector(true)
	col.Add(sh)
	col.Add(bin)
	col.Add(bin)
	col.Add(bak)
	if len(col.Items()) != 1 || col.Primary() != bin {
		t.Fatalf("expected only %q once, got %v", bin, col.Items())
	}
}

func TestScanEditorExtensionsJetBrainsAndGeminiAGY(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	geminiBin := filepath.Join(home, ".gemini", "bin")
	if err := os.MkdirAll(geminiBin, 0755); err != nil {
		t.Fatal(err)
	}
	agyName := "agy"
	if filepath.Separator == '\\' {
		agyName = "agy.exe"
	}
	agyPath := filepath.Join(geminiBin, agyName)
	if err := os.WriteFile(agyPath, []byte("\x7fELFdummy"), 0755); err != nil {
		t.Fatal(err)
	}

	// scanGeminiBin discovers agy(.exe) as TypeCLI
	gemInstalls := scanGeminiBin(home)
	if len(gemInstalls) != 1 || gemInstalls[0].Type != TypeCLI || gemInstalls[0].BinaryPath != agyPath {
		t.Fatalf("unexpected scanGeminiBin results: %+v", gemInstalls)
	}

	// VS Code extension delegating to ~/.gemini/bin/agy
	extRoot := filepath.Join(home, ".vscode", "extensions", "google.google-antigravity-1.11.0")
	if err := os.MkdirAll(extRoot, 0755); err != nil {
		t.Fatal(err)
	}
	extJS := filepath.Join(extRoot, "extension.js")
	if err := os.WriteFile(extJS, []byte(`console.log("google.google-antigravity")`), 0644); err != nil {
		t.Fatal(err)
	}

	extInstalls := scanEditorExtensions(home)
	if len(extInstalls) != 1 || extInstalls[0].Type != TypeExtension || extInstalls[0].BinaryPath != agyPath {
		t.Fatalf("unexpected editor extensions: %+v", extInstalls)
	}

	// Manual resolution of extension.js resolves to TypeExtension
	manualExt := ResolveManualPath(extJS)
	if manualExt == nil || manualExt.Type != TypeExtension || manualExt.ExtensionJsPath != extJS {
		t.Fatalf("unexpected ResolveManualPath for extension.js: %+v", manualExt)
	}

	// JetBrains WebStorm acp-agents plugin with bundled binary
	jbBase := filepath.Join(home, "JetBrains")
	agentBinDir := filepath.Join(jbBase, "WebStorm2026.2", "acp-agents", "antigravity", "1.5.5", "bin")
	if err := os.MkdirAll(agentBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	jbBin := filepath.Join(agentBinDir, agyName)
	if err := os.WriteFile(jbBin, []byte("\x7fELFagent"), 0755); err != nil {
		t.Fatal(err)
	}

	jbInstalls := scanJetBrainsAt([]string{jbBase}, home)
	if len(jbInstalls) != 1 || jbInstalls[0].Type != TypeJetBrains || jbInstalls[0].BinaryPath != jbBin {
		t.Fatalf("unexpected JetBrains installs: %+v", jbInstalls)
	}

	manualJB := ResolveManualPath(jbBin)
	if manualJB == nil || manualJB.Type != TypeJetBrains || manualJB.BinaryPath != jbBin {
		t.Fatalf("unexpected ResolveManualPath for JetBrains binary: %+v", manualJB)
	}
}
