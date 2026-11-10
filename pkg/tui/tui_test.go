package tui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agy-patcher/pkg/config"
	"agy-patcher/pkg/daemon"
	"agy-patcher/pkg/discovery"
	"agy-patcher/pkg/linuxinstall"
	"agy-patcher/pkg/patcher"
	"agy-patcher/pkg/proxy"
)

func TestANSIHelpers(t *testing.T) {
	colored := fgGreen + "Привет" + ansiReset + " world"
	if got := visibleLen(colored); got != 12 {
		t.Fatalf("visibleLen = %d, want 12", got)
	}

	clipped := truncateANSI(colored, 8)
	if visibleLen(clipped) != 8 {
		t.Fatalf("clipped visibleLen = %d, want 8 (%q)", visibleLen(clipped), clipped)
	}
	if !strings.HasSuffix(clipped, ansiReset) {
		t.Fatalf("expected clipped ANSI string to end with reset")
	}

	padded := padRightANSI(colored, 16)
	if visibleLen(padded) != 16 {
		t.Fatalf("padded visibleLen = %d, want 16", visibleLen(padded))
	}
}

func TestParseEscapeSeq(t *testing.T) {
	cases := []struct {
		in   []byte
		want KeyKind
		n    int
	}{
		{[]byte("\x1b[A"), KeyUp, 3},
		{[]byte("\x1b[B"), KeyDown, 3},
		{[]byte("\x1b[C"), KeyRight, 3},
		{[]byte("\x1b[D"), KeyLeft, 3},
		{[]byte("\x1b[5~"), KeyPgUp, 4},
		{[]byte("\x1b[6~"), KeyPgDown, 4},
		{[]byte("\x1bOA"), KeyUp, 3},
		{[]byte("\x1b["), KeyNone, 0},
		{[]byte("\x1b[<35;123;26M"), KeyNone, 13},
		{[]byte("\x1b[<0;12;5m"), KeyNone, 10},
		{[]byte("\x1b[M !#"), KeyNone, 6},
		{[]byte("\x1b[M"), KeyNone, 0},
		{[]byte("\x1b[I"), KeyNone, 3},
		{[]byte("\x1b[O"), KeyNone, 3},
	}

	for _, tc := range cases {
		ev, n := parseEscapeSeq(tc.in)
		if ev.Kind != tc.want || n != tc.n {
			t.Errorf("parseEscapeSeq(%q) = (%v, %d), want (%v, %d)", tc.in, ev.Kind, n, tc.want, tc.n)
		}
	}
}

func TestRenderFrameAndLanguageSwitch(t *testing.T) {
	m := newModel("1.0.0", "ru", "")
	m.addLog(logOK, "Тестовое сообщение")

	frameRU := renderFrame(m, 110, 32)
	if !strings.Contains(frameRU, "Меню") || !strings.Contains(frameRU, "[RU]") {
		t.Fatalf("expected Russian UI elements in rendered frame")
	}

	m.toggleLang()
	frameEN := renderFrame(m, 110, 32)
	if !strings.Contains(frameEN, "Menu") || !strings.Contains(frameEN, "[EN]") {
		t.Fatalf("expected English UI elements after language switch")
	}

	// Narrow layout renders cleanly without overflowing rows.
	narrow := renderFrame(m, 72, 20)
	if len(narrow) == 0 {
		t.Fatalf("expected non-empty narrow frame")
	}
	if !strings.Contains(narrow, "\x1b[20;1H") || strings.Contains(narrow, "\x1b[21;1H") {
		t.Fatalf("expected exact 20-row addressing on narrow frame")
	}

	// Selected section row keeps highlight across label width.
	rows := m.renderSections(40, 8)
	if len(rows) == 0 || !strings.HasPrefix(rows[0], bgSelect) || !strings.HasSuffix(rows[0], ansiReset) {
		t.Fatalf("expected full-width highlight on selected row: %q", rows)
	}
	if strings.Contains(strings.TrimSuffix(rows[0], ansiReset), ansiReset) {
		t.Fatalf("unexpected mid-row reset breaking bgSelect highlight: %q", rows[0])
	}
}

func TestTUISelectionToggleAndFilter(t *testing.T) {
	m := newModel("1.0.0", "ru", "")
	inst1 := discovery.Install{Path: `C:\ag\desktop`, Type: discovery.TypeDesktop, Label: "Antigravity Desktop", BinaryPath: `C:\ag\desktop\language_server.exe`}
	inst2 := discovery.Install{Path: `C:\Users\u\.vscode\extensions\google.google-antigravity-1.11.0`, Type: discovery.TypeExtension, Label: "VS Code (google.google-antigravity-1.11.0)", BinaryPath: `C:\Users\u\.gemini\bin\agy.exe`}

	m.handleMsg(uiMsg{
		kind: "scan_done",
		installs: []installView{
			{install: inst1, installKey: installSelectKey(inst1), targetsLine: "language_server.exe"},
			{install: inst2, installKey: installSelectKey(inst2), targetsLine: "agy.exe", binaryPatched: true, proxyVarPatched: true},
		},
	})

	if sel, tot := m.selectedCount(); sel != 2 || tot != 2 {
		t.Fatalf("expected 2/2 default selected, got %d/%d", sel, tot)
	}

	m.sectionIdx = sectionPatch
	m.focus = focusDetail
	m.detailIdx = m.firstDetailIdxOfKind(detailInstall) // first install target row after action buttons
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if sel, tot := m.selectedCount(); sel != 1 || tot != 2 {
		t.Fatalf("expected 1/2 after Enter toggle, got %d/%d", sel, tot)
	}
	picked := m.selectedInstalls()
	if len(picked) != 1 || picked[0].Type != discovery.TypeExtension {
		t.Fatalf("expected only extension selected, got %+v", picked)
	}

	// Space on target opens path edit modal prefilled with current path, Del clears it cleanly.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: ' '})
	if !m.inputMode || m.inputKind != inputEditPath || m.inputBuf != inst1.Path {
		t.Fatalf("expected prefilled edit-path prompt, got mode=%v kind=%q buf=%q", m.inputMode, m.inputKind, m.inputBuf)
	}
	m.handleKey(KeyEvent{Kind: KeyDelete})
	if m.inputBuf != "" {
		t.Fatalf("expected KeyDelete to clear inputBuf, got %q", m.inputBuf)
	}
	m.handleKey(KeyEvent{Kind: KeyEsc})
	if m.inputMode {
		t.Fatalf("expected KeyEsc to exit inputMode")
	}

	frame := renderFrame(m, 120, 32)
	if !strings.Contains(frame, "[ ]") || !strings.Contains(frame, "[✓]") || !strings.Contains(frame, "language_server.exe") || !strings.Contains(frame, "agy.exe") {
		t.Fatalf("expected checkboxes and target filenames in rendered frame")
	}

	// 'A' toggles all (1/2 -> 2/2, then -> 0/2)
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'a'})
	if sel, _ := m.selectedCount(); sel != 2 {
		t.Fatalf("expected 2 selected after A, got %d", sel)
	}
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'A'})
	if sel, _ := m.selectedCount(); sel != 0 {
		t.Fatalf("expected 0 selected after second A, got %d", sel)
	}

	// Path middle truncation keeps trailing directory name readable.
	mid := truncateMiddle(`C:\Users\Emil\.vscode\extensions\google.google-antigravity-1.11.0`, 42)
	if visibleLen(mid) > 42 || !strings.Contains(mid, "google-antigravity-1.11.0") {
		t.Fatalf("unexpected truncateMiddle result: %q", mid)
	}

	// Long log message wraps cleanly without losing suffix text.
	m.addLog(logInfo, "Поиск установок (файлы не меняются, пока вы не нажмёте Патч).")
	wrapped := m.renderLogs(45, 6)
	joined := strings.Join(wrapped, "\n")
	if len(wrapped) < 2 || !strings.Contains(joined, "Патч)") {
		t.Fatalf("expected wrapped log preserving end text, got %v", wrapped)
	}
}

func TestTUIPersistsLangAndSelections(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	m := newModel("1.0.0", "ru", "")
	m.persistConfig = true
	logCountBefore := len(m.logs)
	m.toggleLang() // ru -> en
	if cfg := config.Load(); cfg.Lang != "en" {
		t.Fatalf("expected saved lang 'en', got %q", cfg.Lang)
	}
	if len(m.logs) != logCountBefore {
		t.Fatalf("expected language toggle not to emit log spam, got %d entries", len(m.logs)-logCountBefore)
	}

	m.toggleLang() // en -> ru
	if cfg := config.Load(); cfg.Lang != "ru" {
		t.Fatalf("expected saved lang 'ru', got %q", cfg.Lang)
	}
}

func TestSaveLogsToFileAndLabelRename(t *testing.T) {
	outDir := t.TempDir()
	m := newModel("1.0.0", "ru", "")
	m.proxyOwned = true
	m.proxyAddr = "127.0.0.1:38451"
	m.proxyEnv = "http://127.0.0.1:38451"
	m.addLog(logOK, "Диагностическое событие")

	frame := stripANSITest(renderFrame(m, 120, 32))
	if !strings.Contains(frame, "Логи в реальном времени") || strings.Contains(frame, "Журнал") {
		t.Fatalf("expected 'Логи' box title instead of 'Журнал':\n%s", frame)
	}

	report := m.buildDiagnosticLogReport()
	path, err := writeDiagnosticLogFile(outDir, report)
	if err != nil {
		t.Fatalf("writeDiagnosticLogFile failed: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile saved log failed: %v", err)
	}
	text := string(saved)
	if !strings.Contains(text, "[PROXY & ENV]") || !strings.Contains(text, "127.0.0.1:38451") || !strings.Contains(text, "Диагностическое событие") {
		t.Fatalf("unexpected diagnostic log content:\n%s", text)
	}
}

func stripANSITest(s string) string {
	var out strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if inEsc {
			if s[i] >= 0x40 && s[i] <= 0x7E {
				inEsc = false
			}
			continue
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i++
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func TestTUIDaemonSnapshotAndRUPackBadges(t *testing.T) {
	m := newModel("1.0.0", "ru", "")
	inst := discovery.Install{Path: `C:\ag\desktop`, Type: discovery.TypeDesktop, Label: "Antigravity Desktop", AsarPath: `C:\ag\desktop\resources\app.asar`}
	m.handleMsg(uiMsg{
		kind: "scan_done",
		installs: []installView{
			{install: inst, installKey: installSelectKey(inst), targetsLine: "language_server.exe", ruApplicable: true, ruInstalled: true, ruVersion: "2.31.0"},
		},
	})
	m.applyDaemonSnapshot(&daemon.StateSnapshot{
		ProxyOwned:  true,
		ProxyAddr:   "127.0.0.1:38451",
		AutostartOn: true,
		Logs: []daemon.LogEntry{
			{Seq: 1, TS: "12:00:00", Level: "dns", Msg: "aicode.googleapis.com -> 1.2.3.4"},
		},
		NextSeq: 1,
	})

	frame := stripANSITest(renderFrame(m, 120, 34))
	if !strings.Contains(frame, "ru:✓") || !strings.Contains(frame, "● 2.31.0") || !strings.Contains(frame, "127.0.0.1:38451") || !strings.Contains(frame, "aicode.googleapis.com") {
		t.Fatalf("expected Desktop RU badge and daemon snapshot in rendered frame:\n%s", frame)
	}
}

func TestContextualSectionsAndCustomPathRemoval(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	// Prepare a valid replacement target file on disk so ResolveManualPath succeeds on any OS.
	replDir := filepath.Join(tmp, "custom-ag")
	if err := os.MkdirAll(replDir, 0755); err != nil {
		t.Fatal(err)
	}
	replBin := filepath.Join(replDir, "language_server_custom.exe")
	if err := os.WriteFile(replBin, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	autoInst := discovery.Install{
		Path:       filepath.Join(tmp, "auto-ag"),
		Type:       discovery.TypeDesktop,
		Label:      "Antigravity Desktop",
		BinaryPath: filepath.Join(tmp, "auto-ag", "language_server.exe"),
	}
	autoKey := installSelectKey(autoInst)

	m := newModel("1.0.0", "ru", "")
	m.persistConfig = true
	m.manualPaths = []string{`C:\Custom\Antigravity`, `D:\IDE\WebStormPlugin`}
	m.saveConfig()

	manual1 := discovery.Install{Path: `C:\Custom\Antigravity`, Type: "manual", Label: "Antigravity", Manual: true}
	manual2 := discovery.Install{Path: `D:\IDE\WebStormPlugin`, Type: "manual", Label: "WebStormPlugin", Manual: true}
	m.handleMsg(uiMsg{
		kind: "scan_done",
		installs: []installView{
			{install: autoInst, installKey: autoKey, targetsLine: "language_server.exe"},
			{install: manual1, installKey: installSelectKey(manual1), missing: true, targetsLine: "путь не найден"},
			{install: manual2, installKey: installSelectKey(manual2), missing: true, targetsLine: "путь не найден"},
		},
	})

	// Merged Patch+Paths section shows action buttons (+ Добавить путь, Пересканировать) and targets.
	m.sectionIdx = sectionPatch
	framePatch := stripANSITest(renderFrame(m, 120, 32))
	if !strings.Contains(framePatch, "Добавить путь") || !strings.Contains(framePatch, "Пересканировать") || !strings.Contains(framePatch, `C:\Custom\Antigravity`) {
		t.Fatalf("expected merged paths & targets inside sectionPatch:\n%s", framePatch)
	}

	// Replace auto-discovered target path via Space -> edit-path prompt -> Enter.
	m.handleKey(KeyEvent{Kind: KeyRight}) // focusDetail
	m.detailIdx = m.firstDetailIdxOfKind(detailInstall)
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: ' '})
	if !m.inputMode || m.inputKind != inputEditPath {
		t.Fatalf("expected inputEditPath on Space, got mode=%v kind=%q", m.inputMode, m.inputKind)
	}
	m.handleKey(KeyEvent{Kind: KeyDelete})
	for _, r := range replBin {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.replacedPaths[autoKey] != replBin {
		t.Fatalf("expected replacedPaths[%q] = %q, got %v", autoKey, replBin, m.replacedPaths)
	}
	if cfg := config.Load(); cfg.ReplacedPaths[autoKey] != replBin {
		t.Fatalf("expected persisted ReplacedPaths, got %v", cfg.ReplacedPaths)
	}

	// Simulate scan_done with overridden target and revert override via Del.
	m.installs[0].overrideKey = autoKey
	m.detailIdx = m.firstDetailIdxOfKind(detailInstall)
	m.handleKey(KeyEvent{Kind: KeyDelete})
	if _, still := m.replacedPaths[autoKey]; still {
		t.Fatalf("expected Del on replaced target to revert override")
	}

	// Move to first manual path and delete it with Del.
	m.detailIdx = m.firstDetailIdxOfKind(detailInstall) + 1
	m.handleKey(KeyEvent{Kind: KeyDelete})
	if len(m.manualPaths) != 1 || m.manualPaths[0] != `D:\IDE\WebStormPlugin` {
		t.Fatalf("expected first manual path removed, got %v", m.manualPaths)
	}
	if cfg := config.Load(); len(cfg.ManualPaths) != 1 || cfg.ManualPaths[0] != `D:\IDE\WebStormPlugin` {
		t.Fatalf("expected persisted config updated, got %v", cfg.ManualPaths)
	}

	// Switch to RU section and verify unified install + force + rollback view renders cleanly.
	m.sectionIdx = sectionRU
	frameRU := stripANSITest(renderFrame(m, 120, 32))
	if !strings.Contains(frameRU, "--force") || !strings.Contains(frameRU, "Откатить русификатор") {
		t.Fatalf("expected unified RU controls in sectionRU:\n%s", frameRU)
	}

	// Switch to Language section and verify radio options switch language.
	m.sectionIdx = sectionLang
	m.focus = focusDetail
	m.detailIdx = 1 // English (EN)
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.lang != "en" {
		t.Fatalf("expected lang 'en' selected from Language section, got %q", m.lang)
	}
}

func TestTUIDNSSectionCustomizeReorderAndPersist(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	m := newModel("1.0.0", "en", "")
	m.persistConfig = true

	// Hotkey 'd' jumps straight into DNS settings section.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'd'})
	if m.sectionIdx != sectionDNS || m.focus != focusDetail {
		t.Fatalf("expected sectionDNS + focusDetail on 'd', got sec=%v focus=%v", m.sectionIdx, m.focus)
	}

	frame := stripANSITest(renderFrame(m, 120, 32))
	if !strings.Contains(frame, "DNS Settings") || !strings.Contains(frame, "dns-ai.ru") {
		t.Fatalf("expected DNS settings list in frame:\n%s", frame)
	}

	firstDNS := m.firstDetailIdxOfKind(detailDNS)
	m.detailIdx = firstDNS
	firstName := m.dnsProviders[0].Name
	secondName := m.dnsProviders[1].Name

	// Enter toggles enabled state.
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.dnsProviders[0].Enabled {
		t.Fatalf("expected Enter to disable first DNS provider")
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if !m.dnsProviders[0].Enabled {
		t.Fatalf("expected second Enter to re-enable first DNS provider")
	}

	// ']' reorders #1 -> #2 cleanly.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: ']'})
	if m.dnsProviders[0].Name != secondName || m.dnsProviders[1].Name != firstName {
		t.Fatalf("expected reorder swap %q <-> %q, got %q, %q", firstName, secondName, m.dnsProviders[0].Name, m.dnsProviders[1].Name)
	}
	if m.detailIdx != firstDNS+1 {
		t.Fatalf("expected cursor to follow moved DNS entry to %d, got %d", firstDNS+1, m.detailIdx)
	}

	// Space opens prefilled edit-dns modal, Del clears, type new UDP entry and Enter saves.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: ' '})
	if !m.inputMode || m.inputKind != inputEditDNS {
		t.Fatalf("expected inputEditDNS on Space, got mode=%v kind=%q", m.inputMode, m.inputKind)
	}
	m.handleKey(KeyEvent{Kind: KeyDelete})
	for _, r := range "Quad9 | 9.9.9.9:53" {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.dnsProviders[1].Name != "Quad9" || m.dnsProviders[1].Kind != proxy.DNSKindUDP || m.dnsProviders[1].Target != "9.9.9.9:53" {
		t.Fatalf("expected updated Quad9 UDP entry, got %+v", m.dnsProviders[1])
	}

	// '+' adds a custom DoH provider.
	origCount := len(m.dnsProviders)
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: '+'})
	if !m.inputMode || m.inputKind != inputAddDNS {
		t.Fatalf("expected inputAddDNS on '+'")
	}
	for _, r := range "https://custom.doh.test/dns-query" {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if len(m.dnsProviders) != origCount+1 || m.dnsProviders[origCount].Kind != proxy.DNSKindDoH {
		t.Fatalf("expected custom DoH added, got %+v", m.dnsProviders)
	}

	// 'm' toggles DNS mode to strict and persists to config.json.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'm'})
	if m.dnsMode != proxy.DNSModeStrict {
		t.Fatalf("expected DNSModeStrict after 'm', got %q", m.dnsMode)
	}
	cfg := config.Load()
	if cfg.DNSMode != proxy.DNSModeStrict || !cfg.DNSCustomized || len(cfg.DNSProviders) != origCount+1 {
		t.Fatalf("unexpected persisted DNS config: %+v", cfg)
	}

	// Del deletes selected DNS provider and reset restores defaults.
	m.handleKey(KeyEvent{Kind: KeyDelete})
	if len(m.dnsProviders) != origCount {
		t.Fatalf("expected deleted DNS entry, got len=%d", len(m.dnsProviders))
	}
	m.executeAction("dns-reset")
	if m.dnsMode != proxy.DNSModeAuto || len(m.dnsProviders) != len(proxy.DefaultDNSProviders()) {
		t.Fatalf("expected reset to defaults, got mode=%q len=%d", m.dnsMode, len(m.dnsProviders))
	}
}

func makeTestTarGzAsar(t *testing.T, archivePath string) {
	t.Helper()
	type entry struct {
		Size   int    `json:"size"`
		Offset string `json:"offset"`
	}
	pkgData := []byte(`{"name":"antigravity","version":"2.31.0"}`)
	iconData := []byte("\x89PNG\r\n\x1a\nICON")
	filesMap := map[string]entry{
		"package.json": {Size: len(pkgData), Offset: "0"},
		"icon.png":     {Size: len(iconData), Offset: "40"},
	}
	// Put exact offsets matching lengths cleanly:
	filesMap["icon.png"] = entry{Size: len(iconData), Offset: "40"}
	hdrBytes, _ := json.Marshal(map[string]any{"files": filesMap})
	aligned := (len(hdrBytes) + 3) &^ 3
	padded := append([]byte{}, hdrBytes...)
	for len(padded) < aligned {
		padded = append(padded, 0)
	}
	var asarBuf bytes.Buffer
	_ = binary.Write(&asarBuf, binary.LittleEndian, uint32(4))
	_ = binary.Write(&asarBuf, binary.LittleEndian, uint32(aligned+8))
	_ = binary.Write(&asarBuf, binary.LittleEndian, uint32(aligned+4))
	_ = binary.Write(&asarBuf, binary.LittleEndian, uint32(len(hdrBytes)))
	asarBuf.Write(padded)
	asarBuf.Write(pkgData)
	// Pad gap to offset 40 if pkgData != 40:
	for asarBuf.Len() < 16+aligned+40 {
		asarBuf.WriteByte(0)
	}
	asarBuf.Write(iconData)

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	writeFile := func(name string, mode int64, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	// Make language_server bytes large enough to look like a real binary + carry both markers.
	lsPayload := append([]byte("\x7fELF"), bytes.Repeat([]byte{0}, 64)...)
	lsPayload = append(lsPayload, []byte("ineligible\x00https_proxy\x00")...)
	writeFile("Antigravity-x64/antigravity", 0755, []byte("\x7fELFmain"))
	writeFile("Antigravity-x64/resources/bin/language_server", 0755, lsPayload)
	writeFile("Antigravity-x64/resources/app.asar", 0644, asarBuf.Bytes())
	_ = tw.Close()
	_ = gz.Close()
}

func TestTUILinuxInstallerWizardAndOptions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	// Hidden on Windows/macOS.
	mWin := newModel("1.0.0", "ru", "")
	mWin.osGOOS = "windows"
	_, _, winItems := mWin.detailItems()
	for _, it := range winItems {
		if it.id == "linux-install-open" {
			t.Fatalf("did not expect linux-install-open button on windows")
		}
	}

	// Shown on Linux with interactive click-through options & descriptions.
	m := newModel("1.0.0", "ru", "")
	m.osGOOS = "linux"
	m.persistConfig = true
	_, _, patchItems := m.detailItems()
	foundOpen := false
	for _, it := range patchItems {
		if it.id == "linux-install-open" {
			foundOpen = true
			break
		}
	}
	if !foundOpen {
		t.Fatalf("expected linux-install-open button inside sectionPatch on linux")
	}

	// Press 'i' in sectionPatch to open installer wizard.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'i'})
	if !m.linuxInstallView || m.focus != focusDetail {
		t.Fatalf("expected linuxInstallView opened on 'i', got view=%v focus=%v", m.linuxInstallView, m.focus)
	}

	frame := stripANSITest(renderFrame(m, 120, 34))
	if !strings.Contains(frame, "Установка Antigravity (.tar.gz)") ||
		!strings.Contains(frame, "Ярлык в меню приложений") ||
		!strings.Contains(frame, "--no-sandbox") {
		t.Fatalf("expected localized installer options & descriptions in frame:\n%s", frame)
	}

	// Cycle installation scope: user -> system -> custom, and edit custom directory with Space.
	m.detailIdx = 1 // li-scope
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.linuxInstallOpts.Scope != linuxinstall.ScopeSystem {
		t.Fatalf("expected ScopeSystem after Enter on li-scope, got %q", m.linuxInstallOpts.Scope)
	}
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: ' '})
	if !m.inputMode || m.inputKind != inputLinuxDir {
		t.Fatalf("expected inputLinuxDir on Space, got mode=%v kind=%q", m.inputMode, m.inputKind)
	}
	customDest := filepath.Join(tmp, "custom-ag-dest")
	m.handleKey(KeyEvent{Kind: KeyDelete})
	for _, r := range customDest {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.linuxInstallOpts.Scope != linuxinstall.ScopeCustom || m.linuxInstallOpts.CustomDir != customDest {
		t.Fatalf("expected ScopeCustom=%q, got %q %q", customDest, m.linuxInstallOpts.Scope, m.linuxInstallOpts.CustomDir)
	}

	// Build a valid test .tar.gz and wire test overrides + archive via inputLinuxArchive.
	archivePath := filepath.Join(tmp, "Antigravity.tar.gz")
	makeTestTarGzAsar(t, archivePath)
	m.detailIdx = 0 // li-archive
	m.handleKey(KeyEvent{Kind: KeyEnter})
	m.handleKey(KeyEvent{Kind: KeyDelete})
	for _, r := range archivePath {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	if m.linuxInstallOpts.ArchivePath != archivePath {
		t.Fatalf("expected ArchivePath=%q, got %q", archivePath, m.linuxInstallOpts.ArchivePath)
	}

	// Override desktop/icon/bin dirs into temp dir and enable NoSandbox + Desktop + Bin + AutoPatch.
	m.linuxInstallOpts.HomeDir = tmp
	m.linuxInstallOpts.DesktopDir = filepath.Join(tmp, "apps")
	m.linuxInstallOpts.IconDir = filepath.Join(tmp, "icons")
	m.linuxInstallOpts.BinDir = filepath.Join(tmp, "bin")
	m.linuxInstallOpts.NoSandbox = true

	// Run installation with '1'.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: '1'})
	select {
	case msg := <-m.events:
		// Wait if logs queued ahead of linux_install_done:
		for msg.kind != "linux_install_done" {
			m.handleMsg(msg)
			select {
			case msg = <-m.events:
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for linux_install_done")
			}
		}
		m.handleMsg(msg)
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for linux install events")
	}

	if m.linuxInstallView {
		t.Fatalf("expected linuxInstallView closed after successful install")
	}
	installedLS := filepath.Join(customDest, "resources", "bin", "language_server")
	st := patcher.InspectBinaryStatus(installedLS)
	if st.Field != patcher.StatePatched || !st.ProxyVar {
		t.Fatalf("expected auto-patched language_server in %q, got %+v", installedLS, st)
	}
	if _, err := os.Stat(filepath.Join(tmp, "apps", "antigravity.desktop")); err != nil {
		t.Fatalf("expected created .desktop entry: %v", err)
	}
}

func TestTUIDNSProbeBadgesAndCatalogSync(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	m := newModel("1.0.0", "en", "")
	m.persistConfig = true
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'd'})

	// Verify V/U action buttons exist in DNS detail items.
	_, _, items := m.detailItems()
	hasCheck, hasSync := false, false
	for _, it := range items {
		switch it.id {
		case "dns-check":
			hasCheck = true
		case "dns-sync":
			hasSync = true
		}
	}
	if !hasCheck || !hasSync {
		t.Fatalf("expected dns-check and dns-sync buttons in sectionDNS (check=%v sync=%v)", hasCheck, hasSync)
	}

	// Find a DoT provider from the embedded catalog (e.g. w.quic.best DoT) and ensure we have at least 4 providers.
	if len(m.dnsProviders) < 4 {
		t.Fatalf("expected at least 4 catalog providers, got %d", len(m.dnsProviders))
	}

	// Add a custom user DNS server via '+' so we can verify separation from catalog sync & quarantine.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: '+'})
	for _, r := range "MyUser | tls://user.custom.test" {
		m.handleKey(KeyEvent{Kind: KeyRune, Rune: r})
	}
	m.handleKey(KeyEvent{Kind: KeyEnter})
	userIdx := len(m.dnsProviders) - 1
	if m.dnsProviders[userIdx].Source != proxy.DNSSourceUser {
		t.Fatalf("expected added DNS entry to carry DNSSourceUser, got %+v", m.dnsProviders[userIdx])
	}

	firstDNS := m.firstDetailIdxOfKind(detailDNS)
	m.detailIdx = firstDNS
	failingCatKey := proxy.ProviderSyncKey(m.dnsProviders[2])

	probes := []proxy.DNSProbeResult{
		{
			Key:       proxy.ProviderSyncKey(m.dnsProviders[0]),
			Name:      m.dnsProviders[0].Name,
			Kind:      m.dnsProviders[0].Kind,
			Target:    m.dnsProviders[0].Target,
			Status:    proxy.ProbeSub,
			LatencyMs: 24,
			Summary:   "gemini.google.com -> 194.87.196.13",
		},
		{
			Key:       proxy.ProviderSyncKey(m.dnsProviders[1]),
			Name:      m.dnsProviders[1].Name,
			Kind:      m.dnsProviders[1].Kind,
			Target:    m.dnsProviders[1].Target,
			Status:    proxy.ProbeDirect,
			LatencyMs: 18,
		},
		{
			Key:       failingCatKey,
			Name:      m.dnsProviders[2].Name,
			Kind:      m.dnsProviders[2].Kind,
			Target:    m.dnsProviders[2].Target,
			Status:    proxy.ProbeTLSErr,
			LatencyMs: 55,
			Summary:   "bad cert",
		},
		{
			Key:       proxy.ProviderSyncKey(m.dnsProviders[userIdx]),
			Name:      m.dnsProviders[userIdx].Name,
			Kind:      m.dnsProviders[userIdx].Kind,
			Target:    m.dnsProviders[userIdx].Target,
			Status:    proxy.ProbeErr,
			LatencyMs: 120,
			Summary:   "i/o timeout",
		},
	}

	origCount := len(m.dnsProviders)
	m.busy = m.t("tui_busy_dns_check")
	m.handleMsg(uiMsg{kind: "dns_probe_done", dnsProbes: probes})
	if m.busy != "" {
		t.Fatalf("expected busy cleared after dns_probe_done, got %q", m.busy)
	}
	// Failing catalog entry must be quarantined until [U] sync, while user entry stays visible.
	if len(m.dnsProviders) != origCount-1 || len(m.hiddenDNSKeys) != 1 || m.hiddenDNSKeys[0] != failingCatKey {
		t.Fatalf("expected 1 catalog DNS quarantined, got len=%d hidden=%v", len(m.dnsProviders), m.hiddenDNSKeys)
	}
	cfgAfterProbe := config.Load()
	if len(cfgAfterProbe.HiddenDNSKeys) != 1 || len(cfgAfterProbe.UserDNSProviders) != 1 || cfgAfterProbe.UserDNSProviders[0].Target != "tls://user.custom.test" {
		t.Fatalf("expected separated UserDNSProviders and HiddenDNSKeys persisted: %+v", cfgAfterProbe)
	}

	// Focused DNS line must not drop background selection highlight with an embedded reset before its end.
	focusedRow := renderDNSEntry(0, m.dnsProviders[0], &probes[0], true, 60)[0]
	if !strings.HasPrefix(focusedRow, bgSelect) || !strings.HasSuffix(focusedRow, ansiReset) {
		t.Fatalf("expected focused DNS row to start with bgSelect and end with ansiReset: %q", focusedRow)
	}
	if strings.Contains(strings.TrimSuffix(focusedRow, ansiReset), ansiReset) {
		t.Fatalf("focused DNS row contains mid-line reset breaking bgSelect: %q", focusedRow)
	}

	rawFrame := renderFrame(m, 140, 44)
	plainFrame := stripANSITest(rawFrame)
	for _, want := range []string{"[DoT]", "[user]", "✓24ms", "○dir 18ms", "✗err", "+1 hidden", "1 of 4 servers actively substitute"} {
		if !strings.Contains(plainFrame, want) {
			t.Fatalf("expected %q in rendered DNS frame:\n%s", want, plainFrame)
		}
	}

	// Applying dns_sync_done un-quarantines catalog entries, prunes retired ones, and keeps user entry + disabled preference intact.
	m.dnsProviders[0].Enabled = false
	synced, stats := proxy.SyncCatalogProviders(m.dnsProviders, nil, []proxy.DNSProviderSpec{
		{Name: m.dnsProviders[0].Name, Kind: m.dnsProviders[0].Kind, Target: m.dnsProviders[0].Target, Enabled: true, Source: proxy.DNSSourceCatalog},
		{Name: "restored-cat", Kind: proxy.DNSKindDoH, Target: "https://restored.cat/dns-query", Enabled: false, Source: proxy.DNSSourceCatalog},
		{Name: "community-dot.test", Kind: proxy.DNSKindDoT, Target: "tls://community-dot.test", Enabled: false, Source: proxy.DNSSourceCatalog},
	})
	if stats.Removed == 0 || stats.Added < 2 {
		t.Fatalf("expected retired catalog entries removed and new/restored added, got %+v", stats)
	}
	m.handleMsg(uiMsg{kind: "dns_sync_done", dnsMerged: synced})
	if len(m.hiddenDNSKeys) != 0 {
		t.Fatalf("expected quarantine cleared on dns_sync_done, got %v", m.hiddenDNSKeys)
	}
	if m.dnsProviders[0].Enabled {
		t.Fatalf("expected dns_sync_done to preserve user disabled flag on %q", m.dnsProviders[0].Name)
	}
	foundUser, foundNew := false, false
	for _, sp := range m.dnsProviders {
		if sp.Target == "tls://user.custom.test" && sp.Source == proxy.DNSSourceUser {
			foundUser = true
		}
		if sp.Target == "tls://community-dot.test" {
			foundNew = true
		}
	}
	if !foundUser || !foundNew {
		t.Fatalf("expected user entry preserved (%v) and new catalog entry merged (%v): %+v", foundUser, foundNew, m.dnsProviders)
	}

	// Resetting DNS defaults clears stale probe cache & quarantine.
	m.resetDNSDefaults()
	if len(m.dnsProbes) != 0 || len(m.hiddenDNSKeys) != 0 {
		t.Fatalf("expected resetDNSDefaults to clear probe cache and quarantine, got probes=%d hidden=%d", len(m.dnsProbes), len(m.hiddenDNSKeys))
	}
}

func TestSaveLogsDebounceAndToast(t *testing.T) {
	outDir := t.TempDir()
	now := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)

	m := newModel("1.0.0", "ru", "")
	m.logExportDirs = []string{outDir}
	m.nowFn = func() time.Time { return now }
	m.addLog(logOK, "Тест записи")
	logsBefore := len(m.logs)

	// First 'w' exports 1 file, emits 1 log, and shows toast in footer.
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'w'})
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 exported log file, got %d (err=%v)", len(entries), err)
	}
	if len(m.logs) != logsBefore+1 {
		t.Fatalf("expected 1 saved-log entry, got %d", len(m.logs)-logsBefore)
	}
	footer1 := stripANSITest(m.renderFooter())
	if !strings.Contains(footer1, "Логи сохранены") {
		t.Fatalf("expected save notification toast in footer, got %q", footer1)
	}

	// Rapid repeat presses within cooldown do not create duplicate files or spam logs.
	now = now.Add(400 * time.Millisecond)
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'W'})
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'w'})
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'ц'})
	entries, err = os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected debounce to prevent extra log files, got %d (err=%v)", len(entries), err)
	}
	if len(m.logs) != logsBefore+1 {
		t.Fatalf("expected no duplicate log spam during cooldown, got %d new logs", len(m.logs)-logsBefore)
	}
	footerCached := stripANSITest(m.renderFooter())
	if !strings.Contains(footerCached, "уже сохранены") {
		t.Fatalf("expected cached save toast in footer, got %q", footerCached)
	}

	// After toast TTL expires, footer returns to keyboard hints; after cooldown, 'w' writes again.
	now = now.Add(5 * time.Second)
	footerExpired := stripANSITest(m.renderFooter())
	if strings.Contains(footerExpired, "сохранены") {
		t.Fatalf("expected toast to disappear after TTL, got %q", footerExpired)
	}
	m.handleKey(KeyEvent{Kind: KeyRune, Rune: 'w'})
	entries, err = os.ReadDir(outDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2nd log file after cooldown, got %d (err=%v)", len(entries), err)
	}
}

func TestJetBrainsNoPatchNeededAndEngineSkip(t *testing.T) {
	jbDir := t.TempDir()
	jbBin := filepath.Join(jbDir, "antigravity-acp")
	if err := os.WriteFile(jbBin, []byte("\x7fELFplain-agent"), 0755); err != nil {
		t.Fatal(err)
	}
	inst := discovery.Install{
		Path:       jbDir,
		Type:       discovery.TypeJetBrains,
		Label:      "WebStorm2026.2 (antigravity)",
		BinaryPath: jbBin,
		Binaries:   []string{jbBin},
	}

	views := buildInstallViews([]collectedInstall{{install: inst}}, "ru")
	if len(views) != 1 || !views[0].noPatchNeeded {
		t.Fatalf("expected TypeJetBrains without ineligible signature to be marked noPatchNeeded, got %+v", views)
	}

	m := newModel("1.0.0", "ru", "")
	m.installs = views
	m.selected[views[0].installKey] = true
	frame := stripANSITest(renderFrame(m, 120, 32))
	if !strings.Contains(frame, "работает без патча") || !strings.Contains(frame, "патч:не требуется") {
		t.Fatalf("expected JetBrains 'no patch needed' status in TUI frame:\n%s", frame)
	}
	wideEntry := stripANSITest(strings.Join(m.renderInstallEntry(views[0], false, 80), "\n"))
	if !strings.Contains(wideEntry, "патч не нужен") {
		t.Fatalf("expected header label 'патч не нужен' in wide install entry:\n%s", wideEntry)
	}

	var sawNoPatch bool
	rep := patcher.ApplyInstalls([]discovery.Install{inst}, func(ev patcher.EngineEvent) {
		if ev.Code == patcher.EvInstallNoPatchNeeded {
			sawNoPatch = true
		}
	})
	if rep.HasErrors || !sawNoPatch {
		t.Fatalf("expected JetBrains patch skip without errors (hasErr=%v sawNoPatch=%v)", rep.HasErrors, sawNoPatch)
	}
}
