package asar

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agy-patcher/internal/hostenv"
)

type header struct {
	Tree       entryNode
	BaseOffset int64
}

type entryNode struct {
	Files    map[string]entryNode `json:"files"`
	Size     int64                `json:"size"`
	Offset   string               `json:"offset"`
	Unpacked bool                 `json:"unpacked"`
}

func readHeader(f *os.File) (*header, error) {
	var raw [16]byte
	if _, err := io.ReadFull(f, raw[:]); err != nil {
		return nil, fmt.Errorf("read asar header: %w", err)
	}

	headerSize := binary.LittleEndian.Uint32(raw[4:8])
	headerStringSize := binary.LittleEndian.Uint32(raw[12:16])
	if headerStringSize == 0 || headerStringSize > 64*1024*1024 {
		return nil, fmt.Errorf("invalid asar header size: %d", headerStringSize)
	}

	jsonBytes := make([]byte, headerStringSize)
	if _, err := io.ReadFull(f, jsonBytes); err != nil {
		return nil, fmt.Errorf("read asar header json: %w", err)
	}

	var tree entryNode
	if err := json.Unmarshal(jsonBytes, &tree); err != nil {
		return nil, fmt.Errorf("parse asar header json: %w", err)
	}

	return &header{
		Tree:       tree,
		BaseOffset: int64(headerSize) + 8,
	}, nil
}

// ReadEntry reads a single file inside an asar archive without unpacking the archive.
// relPath uses forward slashes (e.g. "dist/main.js").
func ReadEntry(asarPath, relPath string) ([]byte, error) {
	f, err := os.Open(asarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, err := readHeader(f)
	if err != nil {
		return nil, err
	}

	node := hdr.Tree
	parts := strings.Split(strings.Trim(relPath, "/"), "/")
	for _, part := range parts {
		if part == "" || node.Files == nil {
			return nil, os.ErrNotExist
		}
		child, ok := node.Files[part]
		if !ok {
			return nil, os.ErrNotExist
		}
		node = child
	}

	if node.Files != nil || node.Unpacked {
		return nil, os.ErrNotExist
	}

	offset, err := strconv.ParseInt(node.Offset, 10, 64)
	if err != nil || offset < 0 || node.Size < 0 {
		return nil, fmt.Errorf("invalid entry metadata for %s", relPath)
	}

	if _, err := f.Seek(hdr.BaseOffset+offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, node.Size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Extract unpacks asarPath into appDir and renames the original archive to app.asar.bak.
// If appDir already exists or asarPath does not exist, it returns nil.
func Extract(asarPath, appDir string) error {
	if _, err := os.Stat(asarPath); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(appDir); err == nil {
		return nil
	}

	f, err := os.Open(asarPath)
	if err != nil {
		return err
	}

	hdr, err := readHeader(f)
	if err != nil {
		_ = f.Close()
		return err
	}

	if err := extractNode(f, hdr.BaseOffset, hdr.Tree, appDir, appDir); err != nil {
		_ = f.Close()
		_ = os.RemoveAll(appDir)
		return err
	}
	_ = f.Close()

	unpackedDir := asarPath + ".unpacked"
	if info, err := os.Stat(unpackedDir); err == nil && info.IsDir() {
		if err := copyDirAll(unpackedDir, appDir); err != nil {
			_ = os.RemoveAll(appDir)
			return err
		}
	}

	bakPath := asarPath + ".bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		if err := os.Rename(asarPath, bakPath); err != nil {
			_ = os.RemoveAll(appDir)
			return err
		}
	}
	return nil
}

// RestorePristine removes an unpacked resources/app directory and restores app.asar from app.asar.bak.
// Only touches resources/app if app.asar or app.asar.bak is present so naturally unpacked IDE layouts are never deleted.
// Returns true if anything was removed or restored.
func RestorePristine(resourcesDir string) (bool, error) {
	appDir := filepath.Join(resourcesDir, "app")
	appAsar := filepath.Join(resourcesDir, "app.asar")
	asarBak := filepath.Join(resourcesDir, "app.asar.bak")

	_, asarErr := os.Stat(appAsar)
	_, bakErr := os.Stat(asarBak)
	if os.IsNotExist(asarErr) && os.IsNotExist(bakErr) {
		return false, nil
	}

	changed := false
	if _, err := os.Stat(appDir); err == nil {
		if err := os.RemoveAll(appDir); err != nil {
			return changed, fmt.Errorf("remove unpacked resources/app: %w", err)
		}
		changed = true
	}

	if bakErr == nil && os.IsNotExist(asarErr) {
		if err := os.Rename(asarBak, appAsar); err != nil {
			return changed, fmt.Errorf("restore app.asar from backup: %w", err)
		}
		changed = true
	}
	return changed, nil
}

func extractNode(f *os.File, baseOffset int64, node entryNode, currentPath, rootDir string) error {
	if node.Files != nil {
		for name, child := range node.Files {
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
				return fmt.Errorf("unsafe asar entry name: %q", name)
			}
			nextPath := filepath.Join(currentPath, name)
			rel, err := filepath.Rel(rootDir, nextPath)
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("asar path traversal: %q", nextPath)
			}
			if err := extractNode(f, baseOffset, child, nextPath, rootDir); err != nil {
				return err
			}
		}
		return nil
	}

	if node.Unpacked {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(currentPath), 0755); err != nil {
		return err
	}

	offset, err := strconv.ParseInt(node.Offset, 10, 64)
	if err != nil || offset < 0 || node.Size < 0 {
		return fmt.Errorf("invalid asar entry offset/size for %s", currentPath)
	}

	if _, err := f.Seek(baseOffset+offset, io.SeekStart); err != nil {
		return err
	}

	out, err := os.Create(currentPath)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(out, f, node.Size)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDirAll(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := hostenv.CopyFile(srcPath, dstPath, 0); err != nil {
				return err
			}
		}
	}
	return nil
}
