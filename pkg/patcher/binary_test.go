package patcher

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agy-patcher/pkg/discovery"
)

func TestBinaryPatchRestoreByteExactAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "language_server.exe")
	orig := []byte("prefix ineligible middle https_proxy suffix")
	if err := os.WriteFile(binPath, orig, 0755); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyBinaryPatchDetailed(binPath)
	if err != nil {
		t.Fatalf("ApplyBinaryPatchDetailed failed: %v", err)
	}
	if res.FieldOutcome != OutcomePatched || res.ProxyVarOutcome != OutcomePatched {
		t.Fatalf("unexpected outcomes: %+v", res)
	}

	if !IsPatched(binPath) || InspectBinary(binPath) != StatePatched || !HasProxyVarPatched(binPath) {
		t.Fatalf("expected patched state")
	}

	patched, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte("prefix " + FieldToken + " middle " + ProxyVar + " suffix")
	if !bytes.Equal(patched, expected) {
		t.Fatalf("unexpected patched bytes: %q, want %q", patched, expected)
	}
	if len(patched) != len(orig) {
		t.Fatalf("binary size changed: %d -> %d", len(orig), len(patched))
	}

	// Second patch is an idempotent no-op.
	res2, err := ApplyBinaryPatchDetailed(binPath)
	if err != nil {
		t.Fatalf("second patch failed: %v", err)
	}
	if res2.FieldOutcome != OutcomeAlreadyPatched || res2.ProxyVarOutcome != OutcomeAlreadyPatched {
		t.Fatalf("expected OutcomeAlreadyPatched, got %+v", res2)
	}

	// Restore is byte-exact without needing a huge binary backup file.
	if err := RestoreBinary(binPath); err != nil {
		t.Fatalf("RestoreBinary failed: %v", err)
	}
	if IsPatched(binPath) {
		t.Fatalf("expected IsPatched to be false after restore")
	}

	restored, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, orig) {
		t.Fatalf("restored bytes mismatch: %q", restored)
	}

	// Restoring again returns ErrNoBackup.
	if err := RestoreBinary(binPath); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("expected ErrNoBackup, got %v", err)
	}
}

func TestStreamingChunkBoundaryPatchAndRestore(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "large.bin")

	// Place target tokens straddling 1 MiB streaming window boundaries.
	data := bytes.Repeat([]byte{'A'}, 2*streamChunkSize+1024)
	copy(data[streamChunkSize-5:], tokenOrigField)
	copy(data[2*streamChunkSize-5:], tokenOrigProxy)
	if err := os.WriteFile(binPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	stBefore := InspectBinaryStatus(binPath)
	if stBefore.Field != StateUnpatched || !stBefore.HasOrigProxy || stBefore.ProxyVar || !stBefore.NeedsPatching {
		t.Fatalf("expected unpatched state across boundaries, got %+v", stBefore)
	}

	res, err := ApplyBinaryPatchDetailed(binPath)
	if err != nil {
		t.Fatalf("streaming boundary patch failed: %v", err)
	}
	if res.FieldOutcome != OutcomePatched || res.ProxyVarOutcome != OutcomePatched {
		t.Fatalf("expected patched outcomes across boundary, got %+v", res)
	}

	stAfter := InspectBinaryStatus(binPath)
	if stAfter.Field != StatePatched || !stAfter.ProxyVar || stAfter.NeedsPatching {
		t.Fatalf("expected clean patched state, got %+v", stAfter)
	}

	raw, _ := os.ReadFile(binPath)
	if !bytes.Equal(raw[streamChunkSize-5:streamChunkSize-5+len(tokenOurField)], tokenOurField) {
		t.Fatalf("patched field token mismatch across boundary")
	}
	if !bytes.Equal(raw[2*streamChunkSize-5:2*streamChunkSize-5+len(tokenOurProxy)], tokenOurProxy) {
		t.Fatalf("patched proxy token mismatch across boundary")
	}

	if err := RestoreBinary(binPath); err != nil {
		t.Fatalf("RestoreBinary boundary failed: %v", err)
	}
	restored, _ := os.ReadFile(binPath)
	if !bytes.Equal(restored, data) {
		t.Fatalf("expected byte-exact restore across 1 MiB chunk boundaries")
	}
}

func TestBinaryNoMatch(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "clean.exe")
	if err := os.WriteFile(binPath, []byte("nothing matching here"), 0644); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyBinaryPatch(binPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != OutcomeNoMatch {
		t.Fatalf("expected OutcomeNoMatch, got %v", outcome)
	}
	if InspectBinary(binPath) != StateSignatureMissing {
		t.Fatalf("expected StateSignatureMissing")
	}
}

func TestIDEAndExtensionJSPatchAndRestore(t *testing.T) {
	dir := t.TempDir()

	ideJS := filepath.Join(dir, "resources", "app", "out", "main.js")
	if err := os.MkdirAll(filepath.Dir(ideJS), 0755); err != nil {
		t.Fatal(err)
	}
	origIDE := `class Auth{async signIn(t){if(this.s.send({type:x.isGcpTos?"GCP_SIGN_IN":"SIGN_IN"}),this.y.resetIsTierGCPTos(),this.f.isGoogleInternal){try{await this.y.loadCodeAssist(t);const{settings:a,userTier:b}=await this.refreshUserStatus(t),i=fn(t);this.g.pushUpdate(i),this.s.send({type:"AUTH_SUCCESS",tokenInfo:t}),this.h.fire({settings:a,userTier:b})}catch(e){return}}}}`
	if err := os.WriteFile(ideJS, []byte(origIDE), 0644); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyJSPatch(ideJS)
	if err != nil {
		t.Fatalf("ApplyJSPatch IDE failed: %v", err)
	}
	if outcome != OutcomePatched {
		t.Fatalf("expected OutcomePatched, got %v", outcome)
	}
	if !IsJSPatched(ideJS) || !HasBackup(ideJS) {
		t.Fatalf("expected IDE JS patched with .agy_orig")
	}
	outcome2, err := ApplyJSPatch(ideJS)
	if err != nil || outcome2 != OutcomeAlreadyPatched {
		t.Fatalf("expected OutcomeAlreadyPatched, got %v err=%v", outcome2, err)
	}

	if err := RestoreJS(ideJS); err != nil {
		t.Fatalf("RestoreJS failed: %v", err)
	}
	restoredIDE, _ := os.ReadFile(ideJS)
	if string(restoredIDE) != origIDE {
		t.Fatalf("IDE JS not byte-exact restored: %q", string(restoredIDE))
	}

	extJS := filepath.Join(dir, "resources", "app", "extensions", "antigravity", "dist", "extension.js")
	if err := os.MkdirAll(filepath.Dir(extJS), 0755); err != nil {
		t.Fatal(err)
	}
	origExt := `function f(){const t=await ns.UserStatus.getUserStatus();if(!t)return[];const n=(0,p2)(t,dz7),{email:e,name:u}=n;return""===e?[]:[e];}`
	if err := os.WriteFile(extJS, []byte(origExt), 0644); err != nil {
		t.Fatal(err)
	}

	if out, err := PatchExtensionJS(extJS); err != nil || out != OutcomePatched {
		t.Fatalf("PatchExtensionJS failed: out=%v err=%v", out, err)
	}
	gotExt, _ := os.ReadFile(extJS)
	if !strings.Contains(string(gotExt), `antigravity-user`) || !strings.HasPrefix(string(gotExt), extHeaderTag) {
		t.Fatalf("unexpected extension.js content: %q", string(gotExt))
	}
	if err := RestoreJS(extJS); err != nil {
		t.Fatalf("RestoreJS ext failed: %v", err)
	}
	restoredExt, _ := os.ReadFile(extJS)
	if string(restoredExt) != origExt {
		t.Fatalf("extension.js not byte-exact restored")
	}
}

func TestDesktopArchitectureDetectionAndCleanup(t *testing.T) {
	dir := t.TempDir()
	distJS := filepath.Join(dir, "resources", "app", "dist", "main.js")
	if err := os.MkdirAll(filepath.Dir(distJS), 0755); err != nil {
		t.Fatal(err)
	}

	newShell := `require("./languageServer");require("./ipcHandlers");`
	if !IsNewDesktopArchitecture(newShell) {
		t.Fatalf("expected v2.4+ detection")
	}
	if err := os.WriteFile(distJS, []byte(newShell), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := PatchDesktopJS(distJS)
	if err != nil || out != OutcomeNoMatch {
		t.Fatalf("expected OutcomeNoMatch for v2.4+ shell, got %v err=%v", out, err)
	}

	inlineShell := `if(e?.failureDetails?.case==="ineligible"?this._authActor.send({type:"SET_INELIGIBLE"}):0);const other="ineligible";const obj={isEligible: false};`
	if IsNewDesktopArchitecture(inlineShell) {
		t.Fatalf("inline auth shell should not classify as modular")
	}
	if err := os.WriteFile(distJS, []byte(inlineShell), 0644); err != nil {
		t.Fatal(err)
	}
	out, err = PatchDesktopJS(distJS)
	if err != nil || out != OutcomePatched {
		t.Fatalf("expected OutcomePatched for inline shell, got %v err=%v", out, err)
	}
	got, _ := os.ReadFile(distJS)
	if !strings.Contains(string(got), `isEligible: true`) || !strings.Contains(string(got), `NEVER_MATCH`) || !strings.Contains(string(got), FieldToken) {
		t.Fatalf("unexpected desktop JS patch: %q", string(got))
	}
}

func TestEngineApplyAndRestoreDeduplicatesTargets(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "language_server.exe")
	if err := os.WriteFile(bin, []byte("head ineligible tail https_proxy"), 0755); err != nil {
		t.Fatal(err)
	}

	installs := []discovery.Install{
		{Path: dir, Type: discovery.TypeBackend, Label: "A", BinaryPath: bin, Binaries: []string{bin}},
		{Path: dir, Type: discovery.TypeExtension, Label: "B", BinaryPath: bin, Binaries: []string{bin}},
	}

	var events []EngineEventCode
	rep := ApplyInstalls(installs, func(ev EngineEvent) { events = append(events, ev.Code) })
	if rep.HasErrors {
		t.Fatalf("ApplyInstalls failed: %v", rep.Err())
	}

	skipped := 0
	for _, c := range events {
		if c == EvBinarySkipped {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("expected second duplicate binary reference to emit EvBinarySkipped once, got %d (%v)", skipped, events)
	}
	if !IsPatched(bin) {
		t.Fatalf("expected patched binary")
	}

	resRep := RestoreInstalls(installs, RestoreOptions{})
	if resRep.HasErrors {
		t.Fatalf("RestoreInstalls failed: %v", resRep.Err())
	}
	if IsPatched(bin) {
		t.Fatalf("expected original binary after RestoreInstalls")
	}
}

func TestCleanArtifacts(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.exe")
	_ = os.WriteFile(target, []byte("bin"), 0644)
	_ = os.WriteFile(target+primaryBackupExt, []byte("bak"), 0644)
	tmpFile := filepath.Join(dir, "app.exe.1234.agystg")
	_ = os.WriteFile(tmpFile, []byte("tmp"), 0644)

	if err := CleanArtifacts(target); err != nil {
		t.Fatalf("CleanArtifacts failed: %v", err)
	}
	if HasBackup(target) {
		t.Fatalf("expected backups to be removed")
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be removed")
	}
}
