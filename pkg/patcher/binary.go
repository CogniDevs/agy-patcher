package patcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/env"
)

const (
	primaryBackupExt = ".agy_orig"
	staleTempAge     = 10 * time.Minute

	// ProxyVar is the 11-byte dedicated proxy env literal written into Antigravity binaries.
	ProxyVar = env.ProxyVar
	// FieldToken is our 10-byte replacement for the "ineligible" marker (same length).
	FieldToken = "agy_pass_0"
)

var (
	ErrNoBackup       = errors.New("backup file not found")
	ErrNothingToPatch = errors.New("signature not found")
)

// PatchOutcome reports whether a target needed and received modifications.
type PatchOutcome int

const (
	OutcomePatched PatchOutcome = iota
	OutcomeAlreadyPatched
	OutcomeNoMatch
)

// BinaryResult breaks down patch outcomes for both the eligibility token and proxy variable.
type BinaryResult struct {
	FieldOutcome    PatchOutcome
	ProxyVarOutcome PatchOutcome
}

// FileState summarizes the inspected state of an Antigravity executable.
type FileState int

const (
	StatePatched FileState = iota
	StateUnpatched
	StateSignatureMissing
	StateUnreadable
)

// BinaryStatus provides inspection state for our Antigravity binary tokens.
type BinaryStatus struct {
	Field         FileState
	ProxyVar      bool
	HasOrigProxy  bool
	NeedsPatching bool
}

var (
	tokenOrigField = []byte("ineligible")
	tokenOurField  = []byte(FieldToken) // 10 bytes

	tokenOrigProxy = []byte("https_proxy")
	tokenOurProxy  = []byte(ProxyVar) // 11 bytes (AGY_PROXY_U)
)

// ApplyBinaryPatch applies both same-length binary substitutions (ineligible -> agy_pass_0 and
// https_proxy -> AGY_PROXY_U) in a bounded streaming pass.
func ApplyBinaryPatch(path string) (PatchOutcome, error) {
	res, err := ApplyBinaryPatchDetailed(path)
	return res.FieldOutcome, err
}

// ApplyBinaryPatchDetailed applies binary substitutions in a bounded <2 MiB streaming pass
// without loading large executables completely into RAM.
func ApplyBinaryPatchDetailed(path string) (BinaryResult, error) {
	var res BinaryResult
	st := InspectBinaryStatus(path)
	if st.Field == StateUnreadable {
		_, err := os.Stat(path)
		if err == nil {
			err = fmt.Errorf("cannot read %s", path)
		}
		return res, err
	}

	needFieldWrite := st.Field == StateUnpatched
	needProxyWrite := !st.ProxyVar && st.HasOrigProxy
	if !needFieldWrite && !needProxyWrite {
		if st.Field == StatePatched {
			res.FieldOutcome = OutcomeAlreadyPatched
		} else {
			res.FieldOutcome = OutcomeNoMatch
		}
		if st.ProxyVar {
			res.ProxyVarOutcome = OutcomeAlreadyPatched
		} else {
			res.ProxyVarOutcome = OutcomeNoMatch
		}
		return res, nil
	}

	rules := []byteRule{
		{from: tokenOrigField, to: tokenOurField},
		{from: tokenOrigProxy, to: tokenOurProxy},
	}

	var counts []int
	changed, err := rewriteBinaryStreaming(path, rules, &counts, needFieldWrite)
	if err != nil {
		return res, err
	}
	if !changed {
		return res, nil
	}

	fieldReplaced := counts[0]
	proxyReplaced := counts[1]

	switch {
	case fieldReplaced > 0:
		res.FieldOutcome = OutcomePatched
	case st.Field == StatePatched:
		res.FieldOutcome = OutcomeAlreadyPatched
	default:
		res.FieldOutcome = OutcomeNoMatch
	}

	switch {
	case proxyReplaced > 0:
		res.ProxyVarOutcome = OutcomePatched
	case st.ProxyVar:
		res.ProxyVarOutcome = OutcomeAlreadyPatched
	default:
		res.ProxyVarOutcome = OutcomeNoMatch
	}
	return res, nil
}

// PatchBinary patches an executable, returning ErrNothingToPatch if the eligibility token is absent.
func PatchBinary(path string) error {
	res, err := ApplyBinaryPatchDetailed(path)
	if err != nil {
		return err
	}
	if res.FieldOutcome == OutcomeNoMatch {
		return ErrNothingToPatch
	}
	return nil
}

// UnpatchBinary reverts agy_pass_0 / AGY_PROXY_U substitutions back to original bytes.
func UnpatchBinary(path string) (bool, error) {
	st := InspectBinaryStatus(path)
	if st.Field == StateUnreadable {
		_, err := os.Stat(path)
		if err == nil {
			err = fmt.Errorf("cannot read %s", path)
		}
		return false, err
	}
	if st.Field != StatePatched && !st.ProxyVar {
		return false, nil
	}

	rules := []byteRule{
		{from: tokenOurField, to: tokenOrigField},
		{from: tokenOurProxy, to: tokenOrigProxy},
	}
	var counts []int
	return rewriteBinaryStreaming(path, rules, &counts, true)
}

// RestoreBinary restores a binary from its .agy_orig backup or reverses our byte substitutions in a streaming pass.
func RestoreBinary(path string) error {
	bak := path + primaryBackupExt
	if HasBackupFile(bak) {
		return restoreFromBackup(path, bak)
	}
	reverted, err := UnpatchBinary(path)
	if err != nil {
		return err
	}
	if !reverted {
		return fmt.Errorf("%w: %s", ErrNoBackup, path)
	}
	return nil
}

// RestoreFile restores either a JS bundle or a native executable.
func RestoreFile(path string) error {
	if strings.HasSuffix(strings.ToLower(path), ".js") {
		return RestoreJS(path)
	}
	return RestoreBinary(path)
}

func restoreFromBackup(target, bakPath string) error {
	perm := hostenv.FileModeOr(target, 0755)
	if strings.HasSuffix(strings.ToLower(target), ".js") {
		data, err := os.ReadFile(bakPath)
		if err != nil {
			return err
		}
		if err := hostenv.WriteFileAtomic(target, data, perm, nil); err != nil {
			return err
		}
	} else {
		swapHook := func(tmp string) error { return prepareSwappedBinary(tmp, target) }
		if _, err := rewriteFileWithCopy(bakPath, target, perm, swapHook, true); err != nil {
			return err
		}
		afterBinaryWrite(target)
	}
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("restored %s, but failed to remove backup %s: %w", target, bakPath, err)
	}
	return nil
}

// InspectBinaryStatus scans path in a single 1 MiB streaming pass to inspect our active patch tokens.
func InspectBinaryStatus(path string) BinaryStatus {
	st := BinaryStatus{Field: StateUnreadable}
	if path == "" {
		return st
	}
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()

	needles := [][]byte{
		tokenOurField,  // 0
		tokenOrigField, // 1
		tokenOurProxy,  // 2
		tokenOrigProxy, // 3
	}
	var seen [4]bool
	if scanErr := streamScanNeedles(f, needles, func(idx int) bool {
		seen[idx] = true
		return false
	}); scanErr != nil {
		return st
	}

	st.ProxyVar = seen[2]
	st.HasOrigProxy = seen[3]

	switch {
	case seen[0] && !seen[1]:
		st.Field = StatePatched
	case seen[1]:
		st.Field = StateUnpatched
	case seen[0]:
		st.Field = StatePatched
	default:
		st.Field = StateSignatureMissing
	}

	st.NeedsPatching = seen[1] || (!seen[2] && seen[3])
	return st
}

// InspectBinary returns the primary field patch state of an executable.
func InspectBinary(path string) FileState {
	return InspectBinaryStatus(path).Field
}

// HasProxyVarPatched reports whether path carries the active dedicated proxy variable literal.
func HasProxyVarPatched(path string) bool {
	return InspectBinaryStatus(path).ProxyVar
}

// IsPatched reports whether path carries agy-patcher modifications.
func IsPatched(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasSuffix(strings.ToLower(path), ".js") {
		return IsJSPatched(path)
	}
	st := InspectBinaryStatus(path)
	return st.Field == StatePatched || st.ProxyVar
}

// HasBackup reports whether our .agy_orig backup file exists for path.
func HasBackup(path string) bool {
	if path == "" {
		return false
	}
	return HasBackupFile(path + primaryBackupExt)
}

// HasBackupFile reports whether p is an existing regular file.
func HasBackupFile(p string) bool {
	return hostenv.IsRegularFile(p)
}

// CleanArtifacts removes backup and temporary patch files created by agy-patcher for path.
func CleanArtifacts(path string) error {
	if path == "" {
		return nil
	}
	var errs []error
	if err := os.Remove(path + primaryBackupExt); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	hostenv.SweepTempFiles(dir, []string{".agy_stg_"}, nil, 0)
	hostenv.SweepTempFiles(dir, []string{base + "."}, []string{".agystg", ".tmp"}, 0)
	return errors.Join(errs...)
}

func sweepBinaryTemps(target string) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	hostenv.SweepTempFiles(dir, []string{".agy_stg_"}, nil, staleTempAge)
	hostenv.SweepTempFiles(dir, []string{base + "."}, []string{".agystg", ".tmp"}, staleTempAge)
}

func rewriteBinaryStreaming(path string, rules []byteRule, outCounts *[]int, killOnLock bool) (bool, error) {
	sweepBinaryTemps(path)
	perm := hostenv.FileModeOr(path, 0755)
	swapHook := func(tmp string) error { return prepareSwappedBinary(tmp, path) }

	runOnce := func() (bool, error) {
		return hostenv.StreamTransformToTarget(path, path, perm, swapHook, func(dst io.Writer, src io.Reader) (bool, error) {
			c, err := streamRewriteRules(dst, src, rules)
			if outCounts != nil {
				*outCounts = c
			}
			if err != nil {
				return false, err
			}
			total := 0
			for _, n := range c {
				total += n
			}
			return total > 0, nil
		})
	}

	changed, err := runOnce()
	if err != nil && killOnLock && (runtime.GOOS == "windows" || !errors.Is(err, os.ErrPermission)) {
		hostenv.TerminateProcessByBinary(path)
		time.Sleep(500 * time.Millisecond)
		changed, err = runOnce()
	}
	if err == nil && changed {
		afterBinaryWrite(path)
	}
	return changed, err
}

func rewriteFileWithCopy(srcPath, target string, perm os.FileMode, swapHook func(string) error, killOnLock bool) (bool, error) {
	sweepBinaryTemps(target)
	runOnce := func() (bool, error) {
		return hostenv.StreamTransformToTarget(srcPath, target, perm, swapHook, func(dst io.Writer, src io.Reader) (bool, error) {
			_, err := io.Copy(dst, src)
			return err == nil, err
		})
	}
	changed, err := runOnce()
	if err != nil && killOnLock && (runtime.GOOS == "windows" || !errors.Is(err, os.ErrPermission)) {
		hostenv.TerminateProcessByBinary(target)
		time.Sleep(500 * time.Millisecond)
		changed, err = runOnce()
	}
	return changed, err
}
