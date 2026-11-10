//go:build darwin

package patcher

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func prepareSwappedBinary(tmpPath, finalTarget string) error {
	if isMachOFile(tmpPath) {
		if err := resignMachO(tmpPath); err != nil {
			return err
		}
		stripDarwinQuarantine(tmpPath)
	}
	return nil
}

func afterBinaryWrite(finalTarget string) {
	if bundle := darwinBundleOf(finalTarget); bundle != "" {
		stripDarwinQuarantine(bundle)
	}
}

func isMachOFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return bytes.Equal(hdr[:], []byte{0xcf, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(hdr[:], []byte{0xce, 0xfa, 0xed, 0xfe}) ||
		bytes.Equal(hdr[:], []byte{0xca, 0xfe, 0xba, 0xbe}) ||
		bytes.Equal(hdr[:], []byte{0xca, 0xfe, 0xba, 0xbf})
}

func resignMachO(path string) error {
	if err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", "--preserve-metadata=identifier", path).Run(); err == nil {
		return nil
	}
	out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func darwinBundleOf(path string) string {
	cur := filepath.Clean(path)
	for {
		if strings.EqualFold(filepath.Ext(cur), ".app") {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func stripDarwinQuarantine(path string) {
	_ = exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", path).Run()
}
