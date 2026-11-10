package hostenv

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FileModeOr returns path's permission bits or fallback if stat fails.
func FileModeOr(path string, fallback os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return fallback
}

// IsRegularFile reports whether path exists and is not a directory.
func IsRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// IsDirectory reports whether path exists and is a directory.
func IsDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// IsNativeBinary reports whether path starts with a PE, ELF, or Mach-O executable header.
func IsNativeBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var magic [4]byte
	n, err := f.Read(magic[:])
	if err != nil || n < 2 {
		return false
	}
	if magic[0] == 'M' && magic[1] == 'Z' {
		return true
	}
	if n >= 4 && magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
		return true
	}
	if n >= 4 && ((magic[0] == 0xFE && magic[1] == 0xED && magic[2] == 0xFA && (magic[3] == 0xCE || magic[3] == 0xCF)) ||
		(magic[3] == 0xFE && magic[2] == 0xED && magic[1] == 0xFA && (magic[0] == 0xCE || magic[0] == 0xCF)) ||
		bytes.Equal(magic[:], []byte{0xCA, 0xFE, 0xBA, 0xBE}) ||
		bytes.Equal(magic[:], []byte{0xCA, 0xFE, 0xBA, 0xBF})) {
		return true
	}
	return false
}

// IsShebangScript reports whether path begins with a #! script header.
func IsShebangScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [2]byte
	n, _ := f.Read(hdr[:])
	return n == 2 && hdr[0] == '#' && hdr[1] == '!'
}

// SweepTempFiles removes stale temporary files matching any provided prefix/suffix filter.
func SweepTempFiles(dir string, prefixes, suffixes []string, minAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !matchesTempPattern(name, prefixes, suffixes) {
			continue
		}
		if minAge > 0 {
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) <= minAge {
				continue
			}
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func matchesTempPattern(name string, prefixes, suffixes []string) bool {
	if len(prefixes) > 0 {
		matched := false
		for _, p := range prefixes {
			if p != "" && strings.HasPrefix(name, p) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(suffixes) > 0 {
		matched := false
		for _, s := range suffixes {
			if s != "" && strings.HasSuffix(name, s) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// WriteFileAtomic writes data to a temporary file alongside target, fsyncs, applies permissions,
// runs an optional prepare hook (such as Mach-O signing), and atomically renames over target.
func WriteFileAtomic(target string, data []byte, perm os.FileMode, beforeSwap func(tmpPath string) error) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := finalizeAndSwap(tmp, tmpPath, target, perm, beforeSwap); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// StreamTransformToTarget creates a temporary file next to target, invokes writeFn with src -> tmp,
// and swaps it atomically over target only when changed is true.
func StreamTransformToTarget(
	srcPath, target string,
	perm os.FileMode,
	beforeSwap func(tmpPath string) error,
	writeFn func(dst io.Writer, src io.Reader) (changed bool, err error),
) (bool, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return false, err
	}
	inClosed := false
	defer func() {
		if !inClosed {
			_ = in.Close()
		}
	}()

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".*.agystg")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	changed, err := writeFn(tmp, in)
	_ = in.Close()
	inClosed = true
	if err != nil {
		_ = tmp.Close()
		return false, err
	}
	if !changed {
		_ = tmp.Close()
		return false, nil
	}

	if err := finalizeAndSwap(tmp, tmpPath, target, perm, beforeSwap); err != nil {
		return false, err
	}
	cleanup = false
	return true, nil
}

func finalizeAndSwap(tmp *os.File, tmpPath, target string, perm os.FileMode, beforeSwap func(tmpPath string) error) error {
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		perm |= 0200
	}
	if perm != 0 {
		if err := os.Chmod(tmpPath, perm); err != nil {
			return err
		}
	}
	if beforeSwap != nil {
		if err := beforeSwap(tmpPath); err != nil {
			return err
		}
	}
	return os.Rename(tmpPath, target)
}

// CopyFile copies src to dst preserving mode (or forceMode when non-zero).
func CopyFile(src, dst string, forceMode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	mode := forceMode
	if mode == 0 {
		info, err := in.Stat()
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
