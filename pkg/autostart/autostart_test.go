package autostart

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyExecutableAtomicAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.exe")
	dst := filepath.Join(dir, "installed", "agpatcher.exe")

	payload := []byte("MZdummy-executable-content-12345")
	if err := os.WriteFile(src, payload, 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("copied content mismatch")
	}

	// Second call with identical file is a clean no-op.
	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("second copyExecutable failed: %v", err)
	}

	// Self-copy is a no-op.
	if err := copyExecutable(dst, dst); err != nil {
		t.Fatalf("self copyExecutable failed: %v", err)
	}
}

func TestExtractQuotedPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"C:\Users\Emil\AppData\Local\agpatcher\agpatcher.exe" --tray`, `C:\Users\Emil\AppData\Local\agpatcher\agpatcher.exe`},
		{`/home/user/.local/share/agpatcher/agpatcher --tray`, `/home/user/.local/share/agpatcher/agpatcher`},
		{`   `, ``},
	}
	for _, tc := range cases {
		if got := extractQuotedPath(tc.in); got != tc.want {
			t.Errorf("extractQuotedPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
