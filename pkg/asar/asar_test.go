package asar

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func buildTestAsar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	tree := entryNode{Files: make(map[string]entryNode)}
	var payload bytes.Buffer

	for rel, data := range files {
		offset := payload.Len()
		payload.Write(data)
		// Only supports top-level or 1-dir nesting for this test helper.
		dir := filepath.ToSlash(filepath.Dir(rel))
		base := filepath.Base(rel)
		if dir == "." {
			tree.Files[base] = entryNode{Size: int64(len(data)), Offset: strconv.Itoa(offset)}
		} else {
			sub, ok := tree.Files[dir]
			if !ok {
				sub = entryNode{Files: make(map[string]entryNode)}
			}
			sub.Files[base] = entryNode{Size: int64(len(data)), Offset: strconv.Itoa(offset)}
			tree.Files[dir] = sub
		}
	}

	jsonBytes, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	// Pad JSON to 4-byte alignment per ASAR spec.
	aligned := (len(jsonBytes) + 3) &^ 3
	paddedJSON := make([]byte, aligned)
	copy(paddedJSON, jsonBytes)

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(4))
	_ = binary.Write(&out, binary.LittleEndian, uint32(aligned+8))
	_ = binary.Write(&out, binary.LittleEndian, uint32(aligned+4))
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(jsonBytes)))
	out.Write(paddedJSON)
	out.Write(payload.Bytes())
	return out.Bytes()
}

func TestReadEntryExtractAndRestorePristine(t *testing.T) {
	resources := t.TempDir()
	asarPath := filepath.Join(resources, "app.asar")
	appDir := filepath.Join(resources, "app")

	rawAsar := buildTestAsar(t, map[string][]byte{
		"package.json": []byte(`{"name":"antigravity"}`),
		"dist/main.js": []byte(`require("./languageServer");require("./ipcHandlers");`),
	})
	if err := os.WriteFile(asarPath, rawAsar, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadEntry(asarPath, "dist/main.js")
	if err != nil {
		t.Fatalf("ReadEntry failed: %v", err)
	}
	if string(got) != `require("./languageServer");require("./ipcHandlers");` {
		t.Fatalf("unexpected entry: %q", got)
	}

	if _, err := ReadEntry(asarPath, "dist"); err == nil {
		t.Fatalf("expected error reading directory entry")
	}

	if err := Extract(asarPath, appDir); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "dist", "main.js")); err != nil {
		t.Fatalf("extracted main.js missing: %v", err)
	}
	if _, err := os.Stat(asarPath + ".bak"); err != nil {
		t.Fatalf("app.asar.bak missing: %v", err)
	}

	restored, err := RestorePristine(resources)
	if err != nil {
		t.Fatalf("RestorePristine failed: %v", err)
	}
	if !restored {
		t.Fatalf("expected RestorePristine to report changes")
	}
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Fatalf("expected resources/app removed")
	}
	if _, err := os.Stat(asarPath); err != nil {
		t.Fatalf("expected app.asar restored: %v", err)
	}
}

func TestRestorePristineDoesNotDeleteNaturalUnpackedIDE(t *testing.T) {
	resources := t.TempDir()
	appDir := filepath.Join(resources, "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(appDir, "package.json")
	if err := os.WriteFile(marker, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	restored, err := RestorePristine(resources)
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatalf("expected RestorePristine to report no changes")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("natural unpacked resources/app should NOT be deleted: %v", err)
	}
}
