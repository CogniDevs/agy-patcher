package hostenv

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicAndStreamTransform(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sample.bin")
	if err := WriteFileAtomic(target, []byte("hello world"), 0644, nil); err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("unexpected content %q err=%v", string(got), err)
	}

	changed, err := StreamTransformToTarget(target, target, 0644, nil, func(dst io.Writer, src io.Reader) (bool, error) {
		b, _ := io.ReadAll(src)
		_, _ = dst.Write(bytes.ReplaceAll(b, []byte("world"), []byte("gopher")))
		return true, nil
	})
	if err != nil || !changed {
		t.Fatalf("StreamTransformToTarget failed: changed=%v err=%v", changed, err)
	}
	got2, _ := os.ReadFile(target)
	if string(got2) != "hello gopher" {
		t.Fatalf("expected transformed content, got %q", string(got2))
	}

	// Unchanged transform must leave file untouched and return changed=false.
	changed2, err := StreamTransformToTarget(target, target, 0644, nil, func(dst io.Writer, src io.Reader) (bool, error) {
		return false, nil
	})
	if err != nil || changed2 {
		t.Fatalf("expected noop stream transform, got changed=%v err=%v", changed2, err)
	}
}

func TestIsNativeBinaryAndShebang(t *testing.T) {
	dir := t.TempDir()
	elf := filepath.Join(dir, "bin")
	sh := filepath.Join(dir, "run.sh")
	_ = os.WriteFile(elf, []byte{0x7f, 'E', 'L', 'F', 0x02}, 0755)
	_ = os.WriteFile(sh, []byte("#!/bin/sh\necho hi\n"), 0755)

	if !IsNativeBinary(elf) || IsNativeBinary(sh) {
		t.Fatalf("native binary detection mismatch")
	}
	if !IsShebangScript(sh) || IsShebangScript(elf) {
		t.Fatalf("shebang detection mismatch")
	}
}
