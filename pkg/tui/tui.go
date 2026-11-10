package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/autostart"
	"agy-patcher/pkg/cleaner"
	"agy-patcher/pkg/config"
	"agy-patcher/pkg/daemon"
	"agy-patcher/pkg/discovery"
	"agy-patcher/pkg/env"
	"agy-patcher/pkg/i18n"
	"agy-patcher/pkg/linuxinstall"
	"agy-patcher/pkg/patcher"
	"agy-patcher/pkg/proxy"
	"agy-patcher/pkg/rupack"
	"agy-patcher/pkg/tray"
)

type focusPane int

const (
	focusSections focusPane = iota
	focusDetail
	focusLogs
)

type menuSection int

const (
	sectionPatch menuSection = iota
	sectionProxy
	sectionDNS
	sectionRU
	sectionTray
	sectionLang
)

const sectionCount = 6

const (
	inputAddPath      = "add-path"
	inputEditPath     = "edit-path"
	inputAddDNS       = "add-dns"
	inputEditDNS      = "edit-dns"
	inputLinuxArchive = "linux-archive"
	inputLinuxDir     = "linux-dir"
)

type logLevel int

const (
	logInfo logLevel = iota
	logOK
	logWarn
	logErr
	logProxy
	logDNS
)

type logEntry struct {
	ts    string
	level logLevel
	msg   string
}

type installView struct {
	install         discovery.Install
	installKey      string
	overrideKey     string
	missing         bool
	noPatchNeeded   bool
	binaryPatched   bool
	proxyVarPatched bool
	jsPatched       bool
	hasJS           bool
	ruApplicable    bool
	ruInstalled     bool
	ruHasBackup     bool
	ruVersion       string
	targetsLine     string
}

type actionItem struct {
	id    string
	key   string
	label string
	badge string
}

type detailKind int

const (
	detailButton detailKind = iota
	detailInstall
	detailDNS
)

type detailItem struct {
	kind       detailKind
	id         string
	key        string
	label      string
	badge      string
	desc       string
	installIdx int
	dnsIdx     int
}

type uiMsg struct {
	kind             string
	level            logLevel
	text             string
	installs         []installView
	customInstallDir string
	dnsProbes        []proxy.DNSProbeResult
	dnsMerged        []proxy.DNSProviderSpec
	dnsReprobe       bool
	proxyEnv         string
	externalProxy    string
	proxyStarted     string
	proxyErr         error
	autostartOn      bool
	daemonClient     *daemon.Client
	daemonSnap       *daemon.StateSnapshot
}

type Options struct {
	Version        string
	Lang           string
	InitialPath    string
	AutoStartProxy bool
}

type model struct {
	version            string
	lang               string
	langAtom           atomic.Value
	osLabel            string
	osGOOS             string
	focus              focusPane
	sectionIdx         menuSection
	detailIdx          int
	logScroll          int
	spinIdx            int
	busy               string
	scanning           bool
	inputMode          bool
	inputKind          string
	inputEditIdx       int
	inputBuf           string
	manualPaths        []string
	replacedPaths      map[string]string
	installs           []installView
	selected           map[string]bool
	dnsMode            string
	dnsCatalogURL      string
	dnsCustomized      bool
	dnsProviders       []proxy.DNSProviderSpec
	hiddenDNSKeys      []string
	dnsProbes          map[string]proxy.DNSProbeResult
	linuxInstallView   bool
	linuxInstallInited bool
	linuxInstallOpts   linuxinstall.Options
	linuxInstallPatch  bool
	proxyEnv           string
	externalProxy      string
	proxyOwned         bool
	proxyAddr          string
	proxyNote          string
	proxyStats         proxy.Stats
	proxyServer        *proxy.Server
	proxyCancel        context.CancelFunc
	proxyDone          chan error
	daemonClient       *daemon.Client
	daemonSeq          uint64
	connectingDaemon   bool
	wantProxyPending   bool
	autostartOn        bool
	shutdownDaemon     bool
	persistConfig      bool
	logs               []logEntry
	lastLogExportAt    time.Time
	lastLogExportPath  string
	logExportDirs      []string
	toastText          string
	toastLevel         logLevel
	toastUntil         time.Time
	nowFn              func() time.Time
	events             chan uiMsg
	doneCh             chan struct{}
	quitting           bool
}

func (m *model) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

func (m *model) setToast(level logLevel, text string, ttl time.Duration) {
	m.toastLevel = level
	m.toastText = text
	m.toastUntil = m.now().Add(ttl)
}

// Run launches the interactive full-screen console UI.
func Run(version, lang, initialPath string) error {
	return RunWithOptions(Options{
		Version:     version,
		Lang:        lang,
		InitialPath: initialPath,
	})
}

// RunWithOptions launches the interactive TUI with optional proxy takeover state.
func RunWithOptions(opts Options) error {
	term, err := openTerminal()
	if err != nil {
		return err
	}
	defer term.Close()

	cfg := config.Load()
	lang := opts.Lang
	if lang == "" && cfg.Lang != "" {
		lang = cfg.Lang
	}

	m := newModel(opts.Version, lang, opts.InitialPath)
	m.persistConfig = true
	m.applyStoredConfig(cfg)
	m.connectingDaemon = true
	m.wantProxyPending = opts.AutoStartProxy
	i18n.SetLanguage(m.lang)

	defer close(m.doneCh)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	keys := startKeyReader(ctx)
	resizeCh := watchResize(ctx)

	m.addLog(logInfo, m.t("tui_log_scan_start"))
	m.triggerScan()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	lastW, lastH := 0, 0
	lastFrame := ""

	redraw := func(force bool) {
		w, h := term.Size()
		resized := w != lastW || h != lastH
		if force || resized {
			lastW, lastH = w, h
			lastFrame = ""
		}
		frame := renderFrame(m, w, h)
		if force || resized {
			_, _ = os.Stdout.WriteString("\x1b[2J" + frame)
			lastFrame = frame
			return
		}
		if frame != lastFrame {
			_, _ = os.Stdout.WriteString(frame)
			lastFrame = frame
		}
	}

	// Render first frame immediately so console never sits blank while attaching daemon.
	redraw(true)

	go func(initLang string, autoProxy bool) {
		cl, snap, dErr := tray.EnsureDaemon(tray.LaunchOptions{
			Version:    opts.Version,
			Lang:       initLang,
			StartProxy: autoProxy,
		})
		if dErr != nil || cl == nil {
			errText := "unavailable"
			if dErr != nil {
				errText = dErr.Error()
			}
			m.postEvent(uiMsg{kind: "daemon_failed", text: errText})
			return
		}
		if snap != nil && snap.Lang != initLang {
			if updated, lErr := cl.SetLang(initLang); lErr == nil && updated != nil {
				snap = updated
			}
		}
		var seq uint64
		if snap != nil {
			seq = snap.NextSeq
		}
		m.postEvent(uiMsg{
			kind:         "daemon_ready",
			daemonClient: cl,
			daemonSnap:   snap,
		})

		poll := time.NewTicker(250 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.doneCh:
				return
			case <-poll.C:
				nextSnap, err := cl.State(seq)
				if err != nil {
					m.postEvent(uiMsg{kind: "daemon_lost"})
					return
				}
				if nextSnap != nil {
					seq = nextSnap.NextSeq
					m.postEvent(uiMsg{kind: "daemon_state", daemonSnap: nextSnap})
				}
			}
		}
	}(m.lang, opts.AutoStartProxy)

	for !m.quitting {
		select {
		case <-ctx.Done():
			m.quitting = true
		case <-resizeCh:
			redraw(true)
		case ev, ok := <-keys:
			if !ok {
				m.quitting = true
				break
			}
			m.handleKey(ev)
			redraw(false)
		case msg := <-m.events:
			m.handleMsg(msg)
			redraw(false)
		case <-ticker.C:
			w, h := term.Size()
			resized := w != lastW || h != lastH
			needRedraw := resized
			if m.busy != "" || m.scanning {
				m.spinIdx++
				needRedraw = true
			}
			if m.proxyOwned {
				needRedraw = true
			}
			if needRedraw {
				redraw(resized)
			}
		}
	}

	if m.shutdownDaemon {
		if m.daemonClient != nil {
			_ = m.daemonClient.Shutdown()
		} else {
			m.stopProxyIfOwned()
		}
	} else if m.daemonClient == nil {
		// Standalone fallback without daemon
		m.stopProxyIfOwned()
	}
	return nil
}

func newModel(version, lang, initialPath string) *model {
	norm := i18n.NormalizeLang(lang)
	autoOn, _ := autostart.IsEnabled()
	m := &model{
		version:       version,
		lang:          norm,
		osLabel:       fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		osGOOS:        runtime.GOOS,
		selected:      make(map[string]bool),
		replacedPaths: make(map[string]string),
		dnsMode:       proxy.DNSModeAuto,
		dnsProviders:  proxy.DefaultDNSProviders(),
		dnsProbes:     make(map[string]proxy.DNSProbeResult),
		autostartOn:   autoOn,
		events:        make(chan uiMsg, 128),
		doneCh:        make(chan struct{}),
	}
	m.langAtom.Store(norm)
	if initialPath != "" {
		m.manualPaths = append(m.manualPaths, initialPath)
	}
	return m
}

func (m *model) applyStoredConfig(cfg config.Config) {
	seenManual := make(map[string]bool, len(m.manualPaths))
	for _, p := range m.manualPaths {
		seenManual[strings.ToLower(filepath.Clean(p))] = true
	}
	for _, p := range cfg.ManualPaths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		norm := strings.ToLower(filepath.Clean(trimmed))
		if !seenManual[norm] {
			seenManual[norm] = true
			m.manualPaths = append(m.manualPaths, trimmed)
		}
	}
	for origKey, customPath := range cfg.ReplacedPaths {
		if origKey != "" && strings.TrimSpace(customPath) != "" {
			m.replacedPaths[origKey] = strings.TrimSpace(customPath)
		}
	}
	for _, key := range cfg.Deselected {
		if key != "" {
			m.selected[key] = false
		}
	}
	m.dnsMode, m.dnsProviders = daemon.EffectiveDNS(cfg)
	m.dnsCustomized = cfg.DNSCustomized
	m.dnsCatalogURL = strings.TrimSpace(cfg.DNSCatalogURL)
	m.hiddenDNSKeys = append([]string(nil), cfg.HiddenDNSKeys...)
}

func (m *model) saveConfig() {
	if !m.persistConfig {
		return
	}
	var deselected []string
	for key, on := range m.selected {
		if !on {
			deselected = append(deselected, key)
		}
	}
	dnsMode, dnsProviders := daemon.ConfigFromDNS(m.dnsMode, m.dnsProviders)
	userDNS := daemon.ExtractUserDNSProviders(m.dnsProviders)
	_ = config.Save(config.Config{
		Lang:             m.lang,
		ManualPaths:      m.manualPaths,
		ReplacedPaths:    m.replacedPaths,
		Deselected:       deselected,
		DNSMode:          dnsMode,
		DNSCatalogURL:    m.dnsCatalogURL,
		DNSCustomized:    m.dnsCustomized,
		DNSProviders:     dnsProviders,
		UserDNSProviders: userDNS,
		HiddenDNSKeys:    append([]string(nil), m.hiddenDNSKeys...),
	})
}

func (m *model) currentLang() string {
	if v, ok := m.langAtom.Load().(string); ok && v != "" {
		return v
	}
	return m.lang
}

func (m *model) t(key string, args ...any) string {
	return i18n.TFor(m.currentLang(), key, args...)
}

func (m *model) postEvent(msg uiMsg) {
	select {
	case m.events <- msg:
	case <-m.doneCh:
	}
}

func (m *model) selectedCount() (int, int) {
	sel := 0
	for _, iv := range m.installs {
		if m.selected[iv.installKey] {
			sel++
		}
	}
	return sel, len(m.installs)
}

func (m *model) toggleInstallAt(idx int) {
	if idx < 0 || idx >= len(m.installs) {
		return
	}
	key := m.installs[idx].installKey
	m.selected[key] = !m.selected[key]
	m.saveConfig()
}

func (m *model) toggleAllInstalls() {
	if len(m.installs) == 0 {
		return
	}
	sel, tot := m.selectedCount()
	target := sel < tot
	for _, iv := range m.installs {
		m.selected[iv.installKey] = target
	}
	m.saveConfig()
}

func (m *model) selectedInstalls() []discovery.Install {
	var out []discovery.Install
	for _, iv := range m.installs {
		if m.selected[iv.installKey] && !iv.missing {
			out = append(out, iv.install)
		}
	}
	return out
}

func (m *model) desktopRUStatus() (applicable bool, installed bool, ver string) {
	for _, iv := range m.installs {
		if iv.ruApplicable {
			return true, iv.ruInstalled, iv.ruVersion
		}
	}
	return false, false, ""
}

func (m *model) dnsCounts() (active, total int) {
	total = len(m.dnsProviders)
	for _, p := range m.dnsProviders {
		if p.Enabled {
			active++
		}
	}
	return active, total
}

func (m *model) dnsModeLabel() string {
	if proxy.NormalizeDNSMode(m.dnsMode) == proxy.DNSModeStrict {
		return m.t("tui_dns_mode_strict")
	}
	return m.t("tui_dns_mode_auto")
}

func (m *model) formatDNSSummary() string {
	var names []string
	for _, p := range m.dnsProviders {
		if p.Enabled {
			names = append(names, p.Name)
		}
	}
	list := m.t("tui_dns_summary_none")
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	return m.t("tui_dns_summary", m.dnsModeLabel(), list)
}

func (m *model) firstDetailIdxOfKind(k detailKind) int {
	_, _, items := m.detailItems()
	for i, it := range items {
		if it.kind == k {
			return i
		}
	}
	return 0
}

func (m *model) isLinux() bool {
	return m.osGOOS == "linux"
}

func (m *model) ensureLinuxInstallDefaults() {
	if m.linuxInstallInited {
		return
	}
	m.linuxInstallOpts = linuxinstall.DefaultOptions()
	m.linuxInstallPatch = true
	m.linuxInstallInited = true
}

func (m *model) linuxInstallTargetDir() string {
	m.ensureLinuxInstallDefaults()
	return linuxinstall.DefaultTargetDir(
		m.linuxInstallOpts.Scope,
		m.linuxInstallOpts.CustomDir,
		m.linuxInstallOpts.HomeDir,
	)
}

func (m *model) onOffBadge(on bool) string {
	if on {
		return fgGreen + "[✓]" + ansiReset
	}
	return fgMuted + "[ ]" + ansiReset
}

func (m *model) inputPromptMeta() (string, string) {
	switch m.inputKind {
	case inputEditPath:
		return m.t("tui_prompt_edit_path"), m.t("tui_prompt_edit_hint")
	case inputAddDNS:
		return m.t("tui_prompt_dns_add"), m.t("tui_prompt_dns_hint")
	case inputEditDNS:
		return m.t("tui_prompt_dns_edit"), m.t("tui_prompt_edit_hint")
	case inputLinuxArchive:
		return m.t("tui_prompt_linux_archive"), m.t("tui_prompt_edit_hint")
	case inputLinuxDir:
		return m.t("tui_prompt_linux_dir"), m.t("tui_prompt_edit_hint")
	default:
		return m.t("tui_prompt_path"), m.t("tui_prompt_hint")
	}
}

func (m *model) sectionItems() []actionItem {
	proxyBadge := fgMuted + "○ OFF" + ansiReset
	if m.proxyOwned {
		proxyBadge = fgGreen + "● ON" + ansiReset
	} else if m.externalProxy != "" {
		proxyBadge = fgCyan + "● EXT" + ansiReset
	}

	langBadge := fgAccent + strings.ToUpper(m.lang) + ansiReset
	autoBadge := fgMuted + "Auto ○" + ansiReset
	if m.autostartOn {
		autoBadge = fgGreen + "Auto ●" + ansiReset
	}

	ruApp, ruInst, ruVer := m.desktopRUStatus()
	ruBadge := fgMuted + "—" + ansiReset
	if ruApp {
		if ruInst {
			if ruVer != "" {
				ruBadge = fgGreen + "● " + ruVer + ansiReset
			} else {
				ruBadge = fgGreen + "● RU" + ansiReset
			}
		} else {
			ruBadge = fgMuted + "○ OFF" + ansiReset
		}
	}

	sel, tot := m.selectedCount()
	patchedCnt := 0
	for _, iv := range m.installs {
		if iv.binaryPatched && iv.proxyVarPatched && (!iv.hasJS || iv.jsPatched) {
			patchedCnt++
		}
	}
	patchBadge := fgMuted + fmt.Sprintf("(%d/%d)", sel, tot) + ansiReset
	if tot > 0 && patchedCnt == tot {
		patchBadge = fgGreen + fmt.Sprintf("(%d/%d ✓)", sel, tot) + ansiReset
	} else if patchedCnt > 0 {
		patchBadge = fgYellow + fmt.Sprintf("(%d/%d)", sel, tot) + ansiReset
	}

	dnsActive, dnsTotal := m.dnsCounts()
	dnsBadge := fgRed + fmt.Sprintf("(%d/%d)", dnsActive, dnsTotal) + ansiReset
	if dnsActive > 0 {
		dnsBadge = fgCyan + fmt.Sprintf("(%d/%d)", dnsActive, dnsTotal) + ansiReset
	}

	return []actionItem{
		{id: "sec-patch", key: "1", label: m.t("tui_sec_patch"), badge: patchBadge},
		{id: "sec-proxy", key: "P", label: m.t("tui_sec_proxy"), badge: proxyBadge},
		{id: "sec-dns", key: "D", label: m.t("tui_sec_dns"), badge: dnsBadge},
		{id: "sec-ru", key: "6", label: m.t("tui_sec_ru"), badge: ruBadge},
		{id: "sec-tray", key: "T", label: m.t("tui_sec_tray"), badge: autoBadge},
		{id: "sec-lang", key: "L", label: m.t("tui_sec_lang"), badge: langBadge},
	}
}

func (m *model) linuxInstallDetailItems() (title string, headerLines []string, items []detailItem) {
	m.ensureLinuxInstallDefaults()
	title = m.t("tui_li_title")
	headerLines = []string{
		fgMuted + m.t("tui_li_header_nav") + ansiReset,
	}
	if linuxinstall.DetectSnapInstalled() {
		headerLines = append(headerLines, fgYellow+m.t("tui_li_snap_detected")+ansiReset)
	}

	arch := strings.TrimSpace(m.linuxInstallOpts.ArchivePath)
	archBadge := fgYellow + m.t("tui_li_not_selected") + ansiReset
	archDesc := m.t("tui_li_desc_archive_missing")
	if arch != "" {
		archBadge = fgGreen + filepath.Base(arch) + ansiReset
		archDesc = m.t("tui_li_desc_archive_found", arch)
	}

	targetDir := m.linuxInstallTargetDir()
	var scopeBadge, scopeDesc string
	switch m.linuxInstallOpts.Scope {
	case linuxinstall.ScopeSystem:
		scopeBadge = fgYellow + m.t("tui_li_scope_system") + ansiReset
		scopeDesc = m.t("tui_li_desc_scope_system", targetDir)
	case linuxinstall.ScopeCustom:
		scopeBadge = fgCyan + m.t("tui_li_scope_custom") + ansiReset
		scopeDesc = m.t("tui_li_desc_scope_custom", targetDir)
	default:
		scopeBadge = fgCyan + m.t("tui_li_scope_user") + ansiReset
		scopeDesc = m.t("tui_li_desc_scope_user", targetDir)
	}

	runDesc := m.t("tui_li_desc_run_ready", targetDir)
	if arch == "" || !hostenv.IsRegularFile(arch) {
		runDesc = m.t("tui_li_desc_run_need_archive")
	} else if !linuxinstall.TargetDirWritable(targetDir) {
		runDesc = m.t("tui_li_desc_run_need_sudo", targetDir)
	}

	items = []detailItem{
		{
			kind:  detailButton,
			id:    "li-archive",
			key:   "Enter",
			label: m.t("tui_li_opt_archive"),
			badge: archBadge,
			desc:  archDesc,
		},
		{
			kind:  detailButton,
			id:    "li-scope",
			key:   "Enter",
			label: m.t("tui_li_opt_scope"),
			badge: scopeBadge,
			desc:  scopeDesc,
		},
		{
			kind:  detailButton,
			id:    "li-desktop",
			label: m.t("tui_li_opt_desktop"),
			badge: m.onOffBadge(m.linuxInstallOpts.CreateDesktop),
			desc:  m.t("tui_li_desc_desktop"),
		},
		{
			kind:  detailButton,
			id:    "li-sandbox",
			label: m.t("tui_li_opt_nosandbox"),
			badge: m.onOffBadge(m.linuxInstallOpts.NoSandbox),
			desc:  m.t("tui_li_desc_nosandbox"),
		},
		{
			kind:  detailButton,
			id:    "li-bin",
			label: m.t("tui_li_opt_bin"),
			badge: m.onOffBadge(m.linuxInstallOpts.CreateBinLink),
			desc:  m.t("tui_li_desc_bin"),
		},
		{
			kind:  detailButton,
			id:    "li-patch",
			label: m.t("tui_li_opt_patch"),
			badge: m.onOffBadge(m.linuxInstallPatch),
			desc:  m.t("tui_li_desc_patch"),
		},
		{
			kind:  detailButton,
			id:    "li-run",
			key:   "1",
			label: m.t("tui_li_opt_run"),
			desc:  runDesc,
		},
		{
			kind:  detailButton,
			id:    "li-back",
			key:   "Esc",
			label: m.t("tui_li_opt_back"),
			desc:  m.t("tui_li_desc_back"),
		},
	}
	return title, headerLines, items
}

func (m *model) detailItems() (title string, headerLines []string, items []detailItem) {
	switch m.sectionIdx {
	case sectionPatch:
		if m.isLinux() && m.linuxInstallView {
			return m.linuxInstallDetailItems()
		}
		sel, tot := m.selectedCount()
		title = m.t("tui_title_patch", sel, tot)
		items = append(items,
			detailItem{
				kind:  detailButton,
				id:    "patch",
				key:   "1",
				label: m.t("tui_btn_patch", sel, tot),
			},
			detailItem{
				kind:  detailButton,
				id:    "restore",
				key:   "3",
				label: m.t("tui_btn_restore", sel, tot),
			},
			detailItem{
				kind:  detailButton,
				id:    "path",
				key:   "5",
				label: m.t("tui_btn_path_add"),
			},
			detailItem{
				kind:  detailButton,
				id:    "scan",
				key:   "R",
				label: m.t("tui_btn_scan"),
			},
		)
		if m.isLinux() {
			items = append(items, detailItem{
				kind:  detailButton,
				id:    "linux-install-open",
				key:   "I",
				label: m.t("tui_btn_linux_install"),
				badge: fgCyan + "tar.gz" + ansiReset,
			})
		}
		if m.scanning && len(m.installs) == 0 {
			headerLines = append(headerLines, fgYellow+m.t("tui_scanning")+ansiReset)
		} else if len(m.installs) == 0 {
			headerLines = append(headerLines,
				fgMuted+m.t("tui_no_installs")+ansiReset,
			)
		}
		for i := range m.installs {
			items = append(items, detailItem{
				kind:       detailInstall,
				id:         "toggle-install",
				installIdx: i,
			})
		}
		return title, headerLines, items

	case sectionProxy:
		title = m.t("tui_title_proxy")
		proxyLabel := m.t("tui_btn_proxy_start")
		proxyBadge := fgMuted + "○ OFF" + ansiReset
		modeText := m.t("tui_state_stopped")
		if m.proxyOwned {
			proxyLabel = m.t("tui_btn_proxy_stop")
			proxyBadge = fgGreen + "● ON" + ansiReset
			if m.daemonClient != nil {
				modeText = fmt.Sprintf("%s (%s)", m.proxyAddr, m.t("tui_mode_daemon"))
			} else {
				modeText = fmt.Sprintf("%s (%s)", m.proxyAddr, m.t("tui_mode_embedded"))
			}
		} else if m.externalProxy != "" {
			proxyBadge = fgCyan + "● EXT" + ansiReset
			modeText = fmt.Sprintf("%s (%s)", m.externalProxy, m.t("tui_mode_external"))
		} else if m.proxyNote != "" {
			modeText = m.proxyNote
		}
		envVal := m.proxyEnv
		if envVal == "" {
			envVal = m.t("tui_not_set")
		}
		headerLines = []string{
			fmt.Sprintf("%s%s:%s %s", fgMuted, m.t("tui_label_status"), ansiReset, modeText),
			fmt.Sprintf("%s%s:%s %s", fgMuted, env.ProxyVar, ansiReset, envVal),
			fgMuted + m.formatDNSSummary() + ansiReset,
		}
		items = []detailItem{
			{kind: detailButton, id: "proxy", key: "P", label: proxyLabel, badge: proxyBadge},
		}
		return title, headerLines, items

	case sectionDNS:
		activeCnt, totalCnt := m.dnsCounts()
		modeStr := m.dnsModeLabel()
		title = m.t("tui_title_dns", activeCnt, totalCnt, modeStr)
		modeBadge := fgCyan + modeStr + ansiReset
		if proxy.NormalizeDNSMode(m.dnsMode) == proxy.DNSModeStrict {
			modeBadge = fgYellow + modeStr + ansiReset
		}
		headerLines = []string{
			fgMuted + m.t("tui_dns_header_mode") + ansiReset,
			fgMuted + m.t("tui_dns_header_keys") + ansiReset,
		}
		if len(m.dnsProviders) == 0 {
			headerLines = append(headerLines, fgYellow+m.t("tui_dns_empty")+ansiReset)
		}
		syncBadge := ""
		if len(m.hiddenDNSKeys) > 0 {
			syncBadge = fgYellow + m.t("tui_dns_hidden_badge", len(m.hiddenDNSKeys)) + ansiReset
		}
		items = []detailItem{
			{kind: detailButton, id: "dns-mode", key: "M", label: m.t("tui_btn_dns_mode"), badge: modeBadge},
			{kind: detailButton, id: "dns-add", key: "+", label: m.t("tui_btn_dns_add")},
			{kind: detailButton, id: "dns-check", key: "V", label: m.t("tui_btn_dns_check")},
			{kind: detailButton, id: "dns-sync", key: "U", label: m.t("tui_btn_dns_sync"), badge: syncBadge},
		}
		for i := range m.dnsProviders {
			items = append(items, detailItem{
				kind:   detailDNS,
				id:     "dns-item",
				dnsIdx: i,
			})
		}
		items = append(items, detailItem{
			kind:  detailButton,
			id:    "dns-reset",
			label: m.t("tui_btn_dns_reset"),
		})
		return title, headerLines, items

	case sectionRU:
		title = m.t("tui_title_ru")
		ruApp, ruInst, ruVer := m.desktopRUStatus()
		ruBadge := fgMuted + "—" + ansiReset
		if !ruApp {
			headerLines = []string{
				fgMuted + m.t("tui_ru_not_found") + ansiReset,
			}
		} else {
			if ruInst {
				ruBadge = fgGreen + "● RU" + ansiReset
			} else {
				ruBadge = fgMuted + "○ OFF" + ansiReset
			}
			for _, iv := range m.installs {
				if !iv.ruApplicable {
					continue
				}
				stateStr := fgMuted + m.t("tui_state_stock") + ansiReset
				if iv.ruInstalled {
					stateStr = fgGreen + m.t("tui_ru_localized") + ansiReset
				}
				bakStr := fgMuted + m.t("tui_word_no") + ansiReset
				if iv.ruHasBackup {
					bakStr = fgGreen + "✓" + ansiReset
				}
				verStr := iv.ruVersion
				if verStr == "" {
					verStr = "unknown"
				}
				headerLines = append(headerLines,
					fmt.Sprintf("%sDesktop:%s v%s  •  %s  •  %s %s", fgMuted, ansiReset, verStr, stateStr, fgMuted+m.t("tui_label_backup")+ansiReset, bakStr),
				)
				break
			}
			_ = ruVer
		}
		items = []detailItem{
			{kind: detailButton, id: "ru-install", key: "6", label: m.t("tui_btn_ru_install"), badge: ruBadge},
			{kind: detailButton, id: "ru-force", label: m.t("tui_btn_ru_force")},
			{kind: detailButton, id: "ru-restore", key: "7", label: m.t("tui_btn_ru_restore")},
		}
		return title, headerLines, items

	case sectionTray:
		title = m.t("tui_title_tray")
		autoBadge := fgMuted + "○ OFF" + ansiReset
		if m.autostartOn {
			autoBadge = fgGreen + "● ON" + ansiReset
		}
		trayState := fgMuted + m.t("tui_daemon_standalone") + ansiReset
		if m.daemonClient != nil {
			trayState = fgGreen + m.t("tui_daemon_active", m.daemonClient.Info().PID) + ansiReset
		} else if m.connectingDaemon {
			trayState = fgYellow + m.t("tui_daemon_connecting") + ansiReset
		}
		headerLines = []string{
			fmt.Sprintf("%s%s:%s %s", fgMuted, m.t("tui_label_daemon"), ansiReset, trayState),
		}
		trayLabel := m.t("tui_btn_tray_win")
		if runtime.GOOS != "windows" {
			trayLabel = m.t("tui_btn_tray_unix")
		}
		items = []detailItem{
			{kind: detailButton, id: "tray", key: "T", label: trayLabel},
			{kind: detailButton, id: "autostart", key: "B", label: m.t("tui_btn_autostart"), badge: autoBadge},
			{kind: detailButton, id: "save-logs", key: "W", label: m.t("tui_btn_save_logs")},
			{kind: detailButton, id: "clean", key: "4", label: m.t("tui_btn_clean")},
			{kind: detailButton, id: "quit", key: "Q", label: m.t("tui_btn_quit")},
		}
		return title, headerLines, items

	default: // sectionLang
		title = m.t("tui_title_lang")
		headerLines = []string{
			fgMuted + m.t("tui_lang_hint") + ansiReset,
		}
		ruPrefix := "( ) "
		enPrefix := "( ) "
		if m.lang == "ru" {
			ruPrefix = fgGreen + "(●) " + ansiReset
		} else {
			enPrefix = fgGreen + "(●) " + ansiReset
		}
		items = []detailItem{
			{kind: detailButton, id: "lang-ru", label: ruPrefix + "Русский (RU)"},
			{kind: detailButton, id: "lang-en", label: enPrefix + "English (EN)"},
		}
		return title, headerLines, items
	}
}

const maxTUILogs = 1500

func (m *model) addLog(level logLevel, msg string) {
	ts := time.Now().Format("15:04:05")
	m.logs = append(m.logs, logEntry{ts: ts, level: level, msg: msg})
	if len(m.logs) > maxTUILogs {
		m.logs = m.logs[len(m.logs)-maxTUILogs:]
	}
}

func (m *model) postLog(level logLevel, msg string) {
	select {
	case m.events <- uiMsg{kind: "log", level: level, text: msg}:
	default:
	}
}

func (m *model) clampDetailIdx() {
	_, _, items := m.detailItems()
	if len(items) == 0 {
		m.detailIdx = 0
		return
	}
	if m.detailIdx < 0 {
		m.detailIdx = 0
	} else if m.detailIdx >= len(items) {
		m.detailIdx = len(items) - 1
	}
}

func (m *model) handleKey(ev KeyEvent) {
	if ev.Kind == KeyCtrlC {
		m.shutdownDaemon = true
		m.quitting = true
		return
	}

	if m.inputMode {
		m.handleInputKey(ev)
		return
	}

	switch ev.Kind {
	case KeyTab:
		m.focus = (m.focus + 1) % 3
		m.clampDetailIdx()
	case KeyUp:
		m.moveSelection(-1)
	case KeyDown:
		m.moveSelection(1)
	case KeyRight:
		if m.focus == focusSections {
			m.focus = focusDetail
			m.clampDetailIdx()
		}
	case KeyLeft, KeyEsc:
		if m.focus == focusDetail {
			if m.sectionIdx == sectionPatch && m.linuxInstallView {
				m.linuxInstallView = false
				m.detailIdx = 0
				m.clampDetailIdx()
			} else {
				m.focus = focusSections
			}
		}
	case KeyPgUp:
		m.scrollLogs(5)
	case KeyPgDown:
		m.scrollLogs(-5)
	case KeyDelete, KeyBackspace:
		if m.focus == focusDetail {
			_, _, items := m.detailItems()
			if m.detailIdx >= 0 && m.detailIdx < len(items) {
				it := items[m.detailIdx]
				switch it.kind {
				case detailInstall:
					m.deleteOrRevertInstallAt(it.installIdx)
				case detailDNS:
					m.removeDNSAt(it.dnsIdx)
				}
			}
		}
	case KeyEnter:
		if m.focus == focusSections {
			m.focus = focusDetail
			m.detailIdx = 0
			m.clampDetailIdx()
		} else if m.focus == focusDetail {
			_, _, items := m.detailItems()
			if m.detailIdx >= 0 && m.detailIdx < len(items) {
				m.activateDetailItem(items[m.detailIdx], true)
			}
		}
	case KeyRune:
		switch ev.Rune {
		case ' ':
			if m.focus == focusDetail {
				_, _, items := m.detailItems()
				if m.detailIdx >= 0 && m.detailIdx < len(items) {
					m.configureDetailItem(items[m.detailIdx])
				}
			} else if m.sectionIdx == sectionPatch && len(m.installs) > 0 {
				m.focus = focusDetail
				m.detailIdx = m.firstDetailIdxOfKind(detailInstall)
				m.clampDetailIdx()
			} else if m.sectionIdx == sectionDNS && len(m.dnsProviders) > 0 {
				m.focus = focusDetail
				m.detailIdx = m.firstDetailIdxOfKind(detailDNS)
				m.clampDetailIdx()
			} else {
				m.focus = focusDetail
				m.clampDetailIdx()
			}
		case '+', '=':
			if ev.Rune == '+' && m.sectionIdx == sectionDNS {
				m.executeAction("dns-add")
			} else if ev.Rune == '=' && m.focus == focusDetail && m.sectionIdx == sectionDNS {
				_, _, items := m.detailItems()
				if m.detailIdx >= 0 && m.detailIdx < len(items) && items[m.detailIdx].kind == detailDNS {
					m.moveDNSAt(items[m.detailIdx].dnsIdx, 1)
				}
			}
		case '-', '[', 'х', 'Х':
			if m.focus == focusDetail && m.sectionIdx == sectionDNS {
				_, _, items := m.detailItems()
				if m.detailIdx >= 0 && m.detailIdx < len(items) && items[m.detailIdx].kind == detailDNS {
					m.moveDNSAt(items[m.detailIdx].dnsIdx, -1)
				}
			}
		case ']', 'ъ', 'Ъ':
			if m.focus == focusDetail && m.sectionIdx == sectionDNS {
				_, _, items := m.detailItems()
				if m.detailIdx >= 0 && m.detailIdx < len(items) && items[m.detailIdx].kind == detailDNS {
					m.moveDNSAt(items[m.detailIdx].dnsIdx, 1)
				}
			}
		case 'm', 'M', 'ь', 'Ь':
			if m.sectionIdx == sectionDNS {
				m.toggleDNSMode()
			}
		case 'v', 'V', 'м', 'М':
			if m.sectionIdx == sectionDNS {
				m.executeAction("dns-check")
			}
		case 'u', 'U', 'г', 'Г':
			if m.sectionIdx == sectionDNS {
				m.executeAction("dns-sync")
			}
		case 'd', 'D', 'в', 'В':
			m.sectionIdx = sectionDNS
			m.focus = focusDetail
			m.detailIdx = 0
			m.clampDetailIdx()
		case 'a', 'A', 'ф', 'Ф':
			m.toggleAllInstalls()
		case 's', 'S', 'ы', 'Ы':
			m.sectionIdx = sectionPatch
			m.focus = focusDetail
			m.detailIdx = m.firstDetailIdxOfKind(detailInstall)
			m.clampDetailIdx()
		case 't', 'T', 'е', 'Е':
			m.executeAction("tray")
		case 'b', 'B', 'и', 'И':
			m.executeAction("autostart")
		case 'q', 'Q', 'й', 'Й':
			m.shutdownDaemon = true
			m.quitting = true
		case 'l', 'L', 'д', 'Д':
			m.toggleLang()
		case 'r', 'R', 'к', 'К':
			m.triggerScan()
		case 'p', 'P', 'з', 'З':
			m.toggleProxy()
		case 'c', 'C', 'с', 'С':
			m.logs = nil
			m.logScroll = 0
		case 'w', 'W', 'ц', 'Ц':
			m.exportLogsToFile()
		case 'i', 'I', 'ш', 'Ш':
			if m.isLinux() && m.sectionIdx == sectionPatch && !m.linuxInstallView {
				m.executeAction("linux-install-open")
			}
		case '1':
			if m.isLinux() && m.sectionIdx == sectionPatch && m.linuxInstallView {
				m.executeAction("li-run")
			} else {
				m.executeAction("patch")
			}
		case '2':
			m.executeAction("proxy")
		case '3':
			m.executeAction("restore")
		case '4':
			m.executeAction("clean")
		case '5':
			m.executeAction("path")
		case '6':
			m.executeAction("ru-install")
		case '7':
			m.executeAction("ru-restore")
		case 'j', 'J', 'о', 'О':
			m.moveSelection(1)
		case 'k', 'K', 'л', 'Л':
			m.moveSelection(-1)
		}
	}
}

func (m *model) activateDetailItem(it detailItem, allowDestructive bool) {
	switch it.kind {
	case detailInstall:
		m.toggleInstallAt(it.installIdx)
	case detailDNS:
		m.toggleDNSAt(it.dnsIdx)
	default:
		if !allowDestructive && (it.id == "clean" || it.id == "quit" || it.id == "restore" || it.id == "ru-restore") {
			return
		}
		m.executeAction(it.id)
	}
}

func (m *model) configureDetailItem(it detailItem) {
	switch it.kind {
	case detailInstall:
		m.openPathEdit(it.installIdx)
	case detailDNS:
		m.openDNSEdit(it.dnsIdx)
	default:
		switch it.id {
		case "path", "dns-add", "dns-mode", "lang-ru", "lang-en", "lang",
			"linux-install-open", "li-archive", "li-desktop", "li-sandbox", "li-bin", "li-patch", "li-back":
			m.executeAction(it.id)
		case "li-scope":
			m.openInput(inputLinuxDir, m.linuxInstallTargetDir(), -1)
		}
	}
}

func (m *model) openInput(kind, prefill string, editIdx int) {
	m.inputMode = true
	m.inputKind = kind
	m.inputBuf = prefill
	m.inputEditIdx = editIdx
}

func (m *model) openPathEdit(idx int) {
	if idx < 0 || idx >= len(m.installs) {
		return
	}
	m.openInput(inputEditPath, m.installs[idx].install.Path, idx)
}

func (m *model) openDNSEdit(idx int) {
	if idx < 0 || idx >= len(m.dnsProviders) {
		return
	}
	m.openInput(inputEditDNS, proxy.FormatDNSProviderInput(m.dnsProviders[idx]), idx)
}

func (m *model) deleteOrRevertInstallAt(idx int) {
	if idx < 0 || idx >= len(m.installs) {
		return
	}
	iv := m.installs[idx]
	if iv.overrideKey != "" {
		delete(m.replacedPaths, iv.overrideKey)
		m.saveConfig()
		m.clampDetailIdx()
		m.addLog(logOK, m.t("tui_log_path_restored", iv.install.Label))
		m.triggerScan()
		return
	}
	if !iv.install.Manual {
		m.addLog(logInfo, m.t("tui_log_auto_path_readonly"))
		return
	}
	targetClean := strings.ToLower(filepath.Clean(iv.install.Path))
	removed := false
	next := make([]string, 0, len(m.manualPaths))
	for _, p := range m.manualPaths {
		if !removed && strings.ToLower(filepath.Clean(p)) == targetClean {
			removed = true
			continue
		}
		next = append(next, p)
	}
	if !removed {
		return
	}
	m.manualPaths = next
	m.saveConfig()
	m.clampDetailIdx()
	m.addLog(logOK, m.t("tui_log_path_removed", iv.install.Path))
	m.triggerScan()
}

func cloneDNSList(list []proxy.DNSProviderSpec) []proxy.DNSProviderSpec {
	if len(list) == 0 {
		return nil
	}
	out := make([]proxy.DNSProviderSpec, 0, len(list))
	for _, p := range list {
		out = append(out, proxy.DNSProviderSpec{
			Name:        p.Name,
			Kind:        p.Kind,
			Target:      p.Target,
			FallbackIPs: append([]string(nil), p.FallbackIPs...),
			Enabled:     p.Enabled,
		})
	}
	return out
}

func (m *model) syncDNS(customized bool) {
	m.dnsMode = proxy.NormalizeDNSMode(m.dnsMode)
	m.dnsCustomized = customized
	m.saveConfig()
	if m.proxyOwned && m.proxyServer != nil && m.proxyServer.Pool != nil {
		m.proxyServer.Pool.UpdateDNS(m.dnsMode, cloneDNSList(m.dnsProviders))
	}
	if m.daemonClient != nil {
		cl := m.daemonClient
		mode := m.dnsMode
		list := cloneDNSList(m.dnsProviders)
		go func() {
			_, _ = cl.SetDNS(mode, list)
		}()
	}
}

func (m *model) toggleDNSAt(idx int) {
	if idx < 0 || idx >= len(m.dnsProviders) {
		return
	}
	m.dnsProviders[idx].Enabled = !m.dnsProviders[idx].Enabled
	m.syncDNS(true)
}

func (m *model) moveDNSAt(idx, delta int) {
	if idx < 0 || idx >= len(m.dnsProviders) {
		return
	}
	newIdx := idx + delta
	if newIdx < 0 || newIdx >= len(m.dnsProviders) {
		return
	}
	m.dnsProviders[idx], m.dnsProviders[newIdx] = m.dnsProviders[newIdx], m.dnsProviders[idx]
	m.detailIdx += delta
	m.clampDetailIdx()
	m.syncDNS(true)
}

func (m *model) removeDNSAt(idx int) {
	if idx < 0 || idx >= len(m.dnsProviders) {
		return
	}
	removed := m.dnsProviders[idx]
	m.dnsProviders = append(m.dnsProviders[:idx], m.dnsProviders[idx+1:]...)
	m.clampDetailIdx()
	m.syncDNS(true)
	m.addLog(logOK, m.t("tui_log_dns_removed", removed.Name))
}

func (m *model) toggleDNSMode() {
	if proxy.NormalizeDNSMode(m.dnsMode) == proxy.DNSModeStrict {
		m.dnsMode = proxy.DNSModeAuto
	} else {
		m.dnsMode = proxy.DNSModeStrict
	}
	m.syncDNS(true)
	m.addLog(logOK, m.t("tui_log_dns_mode", m.dnsModeLabel()))
}

func (m *model) resetDNSDefaults() {
	m.dnsMode = proxy.DNSModeAuto
	m.dnsProviders = proxy.DefaultDNSProviders()
	m.hiddenDNSKeys = nil
	m.dnsProbes = make(map[string]proxy.DNSProbeResult)
	m.clampDetailIdx()
	m.syncDNS(false)
	m.addLog(logOK, m.t("tui_log_dns_reset", len(m.dnsProviders)))
}

func (m *model) triggerDNSProbe() {
	if m.busy != "" || len(m.dnsProviders) == 0 {
		return
	}
	list := append([]proxy.DNSProviderSpec(nil), m.dnsProviders...)
	m.busy = m.t("tui_busy_dns_check")
	m.addLog(logInfo, m.t("tui_log_dns_check_start", len(list)))
	go func() {
		res := proxy.ProbeProviders(context.Background(), list, proxy.ProbeOptions{})
		m.postEvent(uiMsg{kind: "dns_probe_done", dnsProbes: res})
	}()
}

func (m *model) triggerDNSSync() {
	if m.busy != "" {
		return
	}
	curList := append([]proxy.DNSProviderSpec(nil), m.dnsProviders...)
	catalogURL := m.dnsCatalogURL
	m.busy = m.t("tui_busy_dns_sync")
	go func() {
		var userSaved []proxy.DNSProviderSpec
		for _, sp := range curList {
			if sp.Source == proxy.DNSSourceUser {
				userSaved = append(userSaved, sp)
			}
		}
		cat, err := proxy.FetchRepoCatalog(context.Background(), catalogURL, "")
		if err != nil {
			cat = proxy.EmbeddedCatalog()
			m.postLog(logWarn, m.t("tui_log_dns_sync_err", err, len(cat.Providers)))
		}
		merged, stats := proxy.SyncCatalogProviders(curList, userSaved, cat.Providers)
		if err == nil {
			m.postLog(logOK, m.t("tui_log_dns_sync_ok", stats.Added, stats.Updated, stats.Removed, len(merged)))
		}
		m.postEvent(uiMsg{kind: "dns_sync_done", dnsMerged: merged, dnsReprobe: true})
	}()
}

func (m *model) handleInputKey(ev KeyEvent) {
	switch ev.Kind {
	case KeyEsc:
		m.inputMode = false
		m.inputKind = ""
		m.inputBuf = ""
		m.inputEditIdx = -1
	case KeyDelete:
		m.inputBuf = ""
	case KeyBackspace:
		runes := []rune(m.inputBuf)
		if len(runes) > 0 {
			m.inputBuf = string(runes[:len(runes)-1])
		}
	case KeyEnter:
		kind := m.inputKind
		editIdx := m.inputEditIdx
		buf := m.inputBuf
		m.inputMode = false
		m.inputKind = ""
		m.inputBuf = ""
		m.inputEditIdx = -1
		switch kind {
		case inputEditPath:
			m.submitEditPath(editIdx, buf)
		case inputAddDNS:
			m.submitAddDNS(buf)
		case inputEditDNS:
			m.submitEditDNS(editIdx, buf)
		case inputLinuxArchive:
			m.submitLinuxArchive(buf)
		case inputLinuxDir:
			m.submitLinuxDir(buf)
		default:
			m.submitAddPath(buf)
		}
	case KeyRune:
		if ev.Rune >= 32 {
			m.inputBuf += string(ev.Rune)
		}
	}
}

func (m *model) submitLinuxArchive(rawBuf string) {
	raw := discovery.CleanInputPath(rawBuf)
	if raw == "" {
		return
	}
	if !hostenv.IsRegularFile(raw) {
		m.addLog(logErr, m.t("tui_log_li_archive_missing"))
		return
	}
	m.ensureLinuxInstallDefaults()
	m.linuxInstallOpts.ArchivePath = raw
	m.addLog(logOK, m.t("tui_log_li_archive_set", raw))
}

func (m *model) submitLinuxDir(rawBuf string) {
	raw := discovery.CleanInputPath(rawBuf)
	if raw == "" {
		return
	}
	m.ensureLinuxInstallDefaults()
	m.linuxInstallOpts.Scope = linuxinstall.ScopeCustom
	m.linuxInstallOpts.CustomDir = raw
}

func (m *model) submitAddPath(rawBuf string) {
	raw := discovery.CleanInputPath(rawBuf)
	if raw == "" {
		return
	}
	inst := discovery.ResolveManualPath(raw)
	if inst == nil {
		m.addLog(logErr, m.t("scan_manual_not_found", raw))
		return
	}
	normNew := strings.ToLower(filepath.Clean(raw))
	exists := false
	for _, old := range m.manualPaths {
		if strings.ToLower(filepath.Clean(old)) == normNew {
			exists = true
			break
		}
	}
	if !exists {
		m.manualPaths = append(m.manualPaths, raw)
	}
	m.selected[installSelectKey(*inst)] = true
	m.sectionIdx = sectionPatch
	m.focus = focusDetail
	m.saveConfig()
	m.addLog(logOK, m.t("tui_log_path_added", inst.Label, inst.Path))
	m.triggerScan()
}

func (m *model) submitEditPath(idx int, rawBuf string) {
	raw := discovery.CleanInputPath(rawBuf)
	if raw == "" || idx < 0 || idx >= len(m.installs) {
		return
	}
	oldIV := m.installs[idx]
	if strings.EqualFold(filepath.Clean(raw), filepath.Clean(oldIV.install.Path)) {
		return
	}
	inst := discovery.ResolveManualPath(raw)
	if inst == nil {
		m.addLog(logErr, m.t("scan_manual_not_found", raw))
		return
	}
	newKey := installSelectKey(*inst)
	if sel, ok := m.selected[oldIV.installKey]; ok {
		m.selected[newKey] = sel
	} else {
		m.selected[newKey] = true
	}

	if oldIV.overrideKey != "" {
		if newKey == oldIV.overrideKey {
			delete(m.replacedPaths, oldIV.overrideKey)
		} else {
			m.replacedPaths[oldIV.overrideKey] = raw
		}
	} else if oldIV.install.Manual {
		oldClean := strings.ToLower(filepath.Clean(oldIV.install.Path))
		replaced := false
		for i, p := range m.manualPaths {
			if strings.ToLower(filepath.Clean(p)) == oldClean {
				m.manualPaths[i] = raw
				replaced = true
				break
			}
		}
		if !replaced {
			m.manualPaths = append(m.manualPaths, raw)
		}
	} else {
		if m.replacedPaths == nil {
			m.replacedPaths = make(map[string]string)
		}
		m.replacedPaths[oldIV.installKey] = raw
	}

	m.sectionIdx = sectionPatch
	m.focus = focusDetail
	m.saveConfig()
	m.addLog(logOK, m.t("tui_log_path_replaced", oldIV.install.Path, inst.Path))
	m.triggerScan()
}

func (m *model) submitAddDNS(rawBuf string) {
	if strings.TrimSpace(rawBuf) == "" {
		return
	}
	sp, err := proxy.ParseDNSProviderInput(rawBuf)
	if err != nil {
		m.addLog(logErr, m.t("tui_log_dns_err", err))
		return
	}
	sp.Source = proxy.DNSSourceUser
	m.dnsProviders = append(m.dnsProviders, sp)
	m.sectionIdx = sectionDNS
	m.focus = focusDetail
	m.detailIdx = m.firstDetailIdxOfKind(detailDNS) + len(m.dnsProviders) - 1
	m.clampDetailIdx()
	m.syncDNS(true)
	m.addLog(logOK, m.t("tui_log_dns_added", strings.ToUpper(sp.Kind), sp.Name, sp.Target))
}

func (m *model) submitEditDNS(idx int, rawBuf string) {
	if strings.TrimSpace(rawBuf) == "" || idx < 0 || idx >= len(m.dnsProviders) {
		return
	}
	sp, err := proxy.ParseDNSProviderInput(rawBuf)
	if err != nil {
		m.addLog(logErr, m.t("tui_log_dns_err", err))
		return
	}
	sp.Enabled = m.dnsProviders[idx].Enabled
	sp.Source = proxy.DNSSourceUser
	m.dnsProviders[idx] = sp
	m.syncDNS(true)
	m.addLog(logOK, m.t("tui_log_dns_updated", idx+1, sp.Name, sp.Target))
}

func (m *model) moveSelection(delta int) {
	switch m.focus {
	case focusSections:
		m.sectionIdx = menuSection((int(m.sectionIdx) + delta + sectionCount) % sectionCount)
		m.detailIdx = 0
	case focusDetail:
		_, _, items := m.detailItems()
		n := len(items)
		if n == 0 {
			return
		}
		m.detailIdx = (m.detailIdx + delta + n) % n
	case focusLogs:
		m.scrollLogs(-delta)
	}
}

func (m *model) scrollLogs(delta int) {
	m.logScroll += delta
	if m.logScroll < 0 {
		m.logScroll = 0
	}
	if max := len(m.logs) - 1; max > 0 && m.logScroll > max {
		m.logScroll = max
	}
}

func (m *model) executeAction(id string) {
	switch id {
	case "patch":
		m.triggerPatch()
	case "proxy":
		m.toggleProxy()
	case "restore":
		m.triggerRestore()
	case "clean":
		m.triggerClean()
	case "tray":
		m.minimizeToTray()
	case "autostart":
		m.toggleAutostart()
	case "path":
		m.openInput(inputAddPath, "", -1)
	case "dns-add":
		m.openInput(inputAddDNS, "", -1)
	case "dns-check":
		m.triggerDNSProbe()
	case "dns-sync":
		m.triggerDNSSync()
	case "dns-mode":
		m.toggleDNSMode()
	case "dns-reset":
		m.resetDNSDefaults()
	case "linux-install-open":
		if m.isLinux() {
			m.ensureLinuxInstallDefaults()
			m.sectionIdx = sectionPatch
			m.focus = focusDetail
			m.linuxInstallView = true
			m.detailIdx = 0
			m.clampDetailIdx()
		}
	case "li-archive":
		m.ensureLinuxInstallDefaults()
		m.openInput(inputLinuxArchive, m.linuxInstallOpts.ArchivePath, -1)
	case "li-scope":
		m.ensureLinuxInstallDefaults()
		switch m.linuxInstallOpts.Scope {
		case linuxinstall.ScopeUser:
			m.linuxInstallOpts.Scope = linuxinstall.ScopeSystem
		case linuxinstall.ScopeSystem:
			m.linuxInstallOpts.Scope = linuxinstall.ScopeCustom
		default:
			m.linuxInstallOpts.Scope = linuxinstall.ScopeUser
		}
	case "li-desktop":
		m.ensureLinuxInstallDefaults()
		m.linuxInstallOpts.CreateDesktop = !m.linuxInstallOpts.CreateDesktop
	case "li-sandbox":
		m.ensureLinuxInstallDefaults()
		m.linuxInstallOpts.NoSandbox = !m.linuxInstallOpts.NoSandbox
	case "li-bin":
		m.ensureLinuxInstallDefaults()
		m.linuxInstallOpts.CreateBinLink = !m.linuxInstallOpts.CreateBinLink
	case "li-patch":
		m.ensureLinuxInstallDefaults()
		m.linuxInstallPatch = !m.linuxInstallPatch
	case "li-run":
		m.triggerLinuxInstall()
	case "li-back":
		m.linuxInstallView = false
		m.detailIdx = 0
		m.clampDetailIdx()
	case "ru-install":
		m.triggerRUInstall(false)
	case "ru-force":
		m.triggerRUInstall(true)
	case "ru-restore":
		m.triggerRURestore()
	case "scan":
		m.triggerScan()
	case "lang":
		m.toggleLang()
	case "lang-ru":
		m.setLang("ru")
	case "lang-en":
		m.setLang("en")
	case "save-logs":
		m.exportLogsToFile()
	case "quit":
		m.shutdownDaemon = true
		m.quitting = true
	}
}

func (m *model) setLang(target string) {
	norm := i18n.NormalizeLang(target)
	if m.lang == norm {
		return
	}
	m.lang = norm
	m.langAtom.Store(norm)
	i18n.SetLanguage(norm)
	m.saveConfig()
	if m.daemonClient != nil {
		cl := m.daemonClient
		go func() {
			_, _ = cl.SetLang(norm)
		}()
	}
}

func (m *model) toggleLang() {
	if m.lang == "ru" {
		m.setLang("en")
	} else {
		m.setLang("ru")
	}
}

func parseDaemonLevel(s string) logLevel {
	switch s {
	case "ok":
		return logOK
	case "warn":
		return logWarn
	case "err":
		return logErr
	case "proxy":
		return logProxy
	case "dns":
		return logDNS
	default:
		return logInfo
	}
}

func (m *model) applyDaemonSnapshot(snap *daemon.StateSnapshot) {
	if snap == nil {
		return
	}
	m.proxyOwned = snap.ProxyOwned
	m.proxyAddr = snap.ProxyAddr
	m.externalProxy = snap.ExternalProxy
	m.proxyNote = snap.ProxyNote
	m.proxyStats = snap.Stats
	m.autostartOn = snap.AutostartOn
	if snap.ProxyOwned {
		if pv := env.GetProxyVar(); pv != "" {
			m.proxyEnv = pv
		} else if snap.ProxyAddr != "" {
			m.proxyEnv = "http://" + snap.ProxyAddr
		}
	} else {
		m.proxyEnv = env.GetProxyVar()
	}
	for _, entry := range snap.Logs {
		if entry.Seq <= m.daemonSeq {
			continue
		}
		m.daemonSeq = entry.Seq
		m.logs = append(m.logs, logEntry{
			ts:    entry.TS,
			level: parseDaemonLevel(entry.Level),
			msg:   entry.Msg,
		})
		if len(m.logs) > maxTUILogs {
			m.logs = m.logs[len(m.logs)-maxTUILogs:]
		}
	}
	if snap.NextSeq > m.daemonSeq {
		m.daemonSeq = snap.NextSeq
	}
}

func (m *model) handleMsg(msg uiMsg) {
	switch msg.kind {
	case "log":
		m.addLog(msg.level, msg.text)
	case "daemon_ready":
		m.connectingDaemon = false
		m.daemonClient = msg.daemonClient
		m.applyDaemonSnapshot(msg.daemonSnap)
		if msg.daemonClient != nil {
			cl := msg.daemonClient
			curLang := m.lang
			if msg.daemonSnap != nil && msg.daemonSnap.Lang != curLang {
				go func() {
					_, _ = cl.SetLang(curLang)
				}()
			}
			if msg.daemonSnap != nil && !equalDNSConfig(m.dnsMode, m.dnsProviders, msg.daemonSnap.DNSMode, msg.daemonSnap.DNSProviders) {
				mode := m.dnsMode
				list := cloneDNSList(m.dnsProviders)
				go func() {
					_, _ = cl.SetDNS(mode, list)
				}()
			}
		}
		if m.wantProxyPending {
			m.wantProxyPending = false
			if !m.proxyOwned {
				m.toggleProxy()
			}
		}
	case "daemon_failed":
		m.connectingDaemon = false
		m.addLog(logWarn, m.t("tui_log_daemon_failed", msg.text))
		if m.wantProxyPending {
			m.wantProxyPending = false
			m.toggleProxy()
		}
	case "daemon_state":
		m.applyDaemonSnapshot(msg.daemonSnap)
	case "daemon_lost":
		if m.daemonClient != nil {
			m.daemonClient = nil
			m.proxyOwned = false
			m.proxyAddr = ""
			m.proxyStats = proxy.Stats{}
			m.addLog(logWarn, m.t("tui_log_daemon_lost"))
			m.triggerScan()
		}
	case "scan_done":
		m.scanning = false
		m.installs = msg.installs
		for _, iv := range m.installs {
			if _, ok := m.selected[iv.installKey]; !ok {
				m.selected[iv.installKey] = true
			}
		}
		m.proxyEnv = msg.proxyEnv
		if m.daemonClient == nil {
			m.externalProxy = msg.externalProxy
			m.autostartOn = msg.autostartOn
		}
		m.clampDetailIdx()
	case "task_done":
		m.busy = ""
		m.triggerScan()
	case "linux_install_done":
		m.busy = ""
		if msg.customInstallDir != "" {
			norm := strings.ToLower(filepath.Clean(msg.customInstallDir))
			exists := false
			for _, old := range m.manualPaths {
				if strings.ToLower(filepath.Clean(old)) == norm {
					exists = true
					break
				}
			}
			if !exists {
				m.manualPaths = append(m.manualPaths, msg.customInstallDir)
				m.saveConfig()
			}
		}
		m.linuxInstallView = false
		m.detailIdx = 0
		m.triggerScan()
	case "dns_probe_done":
		m.busy = ""
		if m.dnsProbes == nil {
			m.dnsProbes = make(map[string]proxy.DNSProbeResult)
		}
		subCount := 0
		reachableCount := 0
		failedKeys := make(map[string]bool)
		for _, r := range msg.dnsProbes {
			m.dnsProbes[r.Key] = r
			switch r.Status {
			case proxy.ProbeSub:
				subCount++
				reachableCount++
				m.addLog(logOK, m.t("tui_log_dns_check_sub", r.Name, r.LatencyMs, r.Summary))
			case proxy.ProbeDirect:
				reachableCount++
				m.addLog(logWarn, m.t("tui_log_dns_check_direct", r.Name, r.LatencyMs))
			case proxy.ProbeTLSErr:
				failedKeys[r.Key] = true
				m.addLog(logErr, m.t("tui_log_dns_check_tlserr", r.Name, r.LatencyMs, r.Summary))
			default:
				failedKeys[r.Key] = true
				m.addLog(logErr, m.t("tui_log_dns_check_err", r.Name, r.LatencyMs, r.Summary))
			}
		}
		m.addLog(logInfo, m.t("tui_log_dns_check_done", subCount, len(msg.dnsProbes)))
		if len(failedKeys) > 0 {
			if reachableCount == 0 {
				m.addLog(logWarn, m.t("tui_log_dns_quarantine_skip_offline"))
			} else {
				hiddenSet := make(map[string]bool, len(m.hiddenDNSKeys)+len(failedKeys))
				for _, k := range m.hiddenDNSKeys {
					hiddenSet[k] = true
				}
				kept := make([]proxy.DNSProviderSpec, 0, len(m.dnsProviders))
				newlyHidden := 0
				for _, sp := range m.dnsProviders {
					key := proxy.ProviderSyncKey(sp)
					// Quarantine unreachable catalog servers until next [U] sync; never hide user-added entries.
					if sp.Source != proxy.DNSSourceUser && failedKeys[key] {
						if !hiddenSet[key] {
							hiddenSet[key] = true
							m.hiddenDNSKeys = append(m.hiddenDNSKeys, key)
						}
						newlyHidden++
						continue
					}
					kept = append(kept, sp)
				}
				if newlyHidden > 0 {
					m.dnsProviders = kept
					m.clampDetailIdx()
					m.syncDNS(m.dnsCustomized)
					m.addLog(logWarn, m.t("tui_log_dns_quarantined", newlyHidden))
				}
			}
		}
	case "dns_sync_done":
		m.busy = ""
		m.hiddenDNSKeys = nil
		m.dnsProbes = make(map[string]proxy.DNSProbeResult)
		if len(msg.dnsMerged) > 0 {
			m.dnsProviders = msg.dnsMerged
			m.clampDetailIdx()
			m.syncDNS(m.dnsCustomized)
		}
		if msg.dnsReprobe && len(m.dnsProviders) > 0 {
			m.triggerDNSProbe()
		}
	case "proxy_ready":
		m.proxyOwned = true
		m.proxyAddr = msg.proxyStarted
		m.proxyEnv = env.GetProxyVar()
		m.addLog(logProxy, m.t("daemon_proxy_listening", msg.proxyStarted))
	case "proxy_exit":
		m.proxyOwned = false
		m.proxyAddr = ""
		m.proxyServer = nil
		if msg.proxyErr != nil {
			m.addLog(logErr, m.t("daemon_proxy_err", msg.proxyErr))
		} else {
			m.addLog(logInfo, m.t("daemon_proxy_stopped"))
		}
		m.triggerScan()
	}
}

func cloneReplacedMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func equalDNSConfig(modeA string, listA []proxy.DNSProviderSpec, modeB string, listB []proxy.DNSProviderSpec) bool {
	if proxy.NormalizeDNSMode(modeA) != proxy.NormalizeDNSMode(modeB) || len(listA) != len(listB) {
		return false
	}
	for i := range listA {
		a, b := listA[i], listB[i]
		if a.Name != b.Name || a.Kind != b.Kind || a.Target != b.Target || a.Enabled != b.Enabled || len(a.FallbackIPs) != len(b.FallbackIPs) {
			return false
		}
		for j := range a.FallbackIPs {
			if a.FallbackIPs[j] != b.FallbackIPs[j] {
				return false
			}
		}
	}
	return true
}

func (m *model) triggerScan() {
	if m.scanning {
		return
	}
	m.scanning = true
	manualCopy := append([]string(nil), m.manualPaths...)
	replacedCopy := cloneReplacedMap(m.replacedPaths)
	lang := m.currentLang()
	owned := m.proxyOwned

	go func() {
		collected := collectInstalls(manualCopy, replacedCopy)
		views := buildInstallViews(collected, lang)

		extProxy := ""
		if !owned {
			if fp := env.DetectExternalProxy(); fp != nil {
				extProxy = fp.Value
			}
		}

		autoOn, _ := autostart.IsEnabled()
		m.postEvent(uiMsg{
			kind:          "scan_done",
			installs:      views,
			proxyEnv:      env.GetProxyVar(),
			externalProxy: extProxy,
			autostartOn:   autoOn,
		})
	}()
}

func buildInstallViews(collected []collectedInstall, lang string) []installView {
	views := make([]installView, 0, len(collected))
	for _, ci := range collected {
		inst := ci.install
		v := installView{
			install:     inst,
			installKey:  installSelectKey(inst),
			overrideKey: ci.overrideKey,
			missing:     ci.missing,
		}
		if ci.missing {
			v.targetsLine = i18n.TFor(lang, "tui_state_missing")
			views = append(views, v)
			continue
		}
		bins := inst.Binaries
		if len(bins) == 0 && inst.BinaryPath != "" {
			bins = []string{inst.BinaryPath}
		}
		var targetParts []string
		hasPatchableBinary := false
		if len(bins) > 0 {
			allField := true
			allProxy := true
			for _, b := range bins {
				st := patcher.InspectBinaryStatus(b)
				if st.Field == patcher.StatePatched || st.Field == patcher.StateUnpatched || st.ProxyVar || st.HasOrigProxy {
					hasPatchableBinary = true
				}
				if st.Field != patcher.StatePatched {
					allField = false
				}
				if !st.ProxyVar {
					allProxy = false
				}
				targetParts = append(targetParts, filepath.Base(b))
			}
			v.binaryPatched = allField
			v.proxyVarPatched = allProxy
		}
		var jsTargets []string
		if inst.JsPath != "" {
			jsTargets = append(jsTargets, inst.JsPath)
		}
		if inst.ExtensionJsPath != "" && patcher.NeedsExtensionJSPatch(inst.ExtensionJsPath) {
			jsTargets = append(jsTargets, inst.ExtensionJsPath)
		}
		if len(jsTargets) > 0 {
			v.hasJS = true
			allJS := true
			for _, jp := range jsTargets {
				targetParts = append(targetParts, filepath.Base(jp))
				if !patcher.IsJSPatched(jp) {
					allJS = false
				}
			}
			v.jsPatched = allJS
		}
		if inst.Type == discovery.TypeJetBrains && !hasPatchableBinary && (!v.hasJS || v.jsPatched) {
			v.noPatchNeeded = true
			v.targetsLine = i18n.TFor(lang, "tui_targets_no_patch_jb")
		} else if len(targetParts) > 0 {
			v.targetsLine = strings.Join(targetParts, ", ")
		} else if inst.Type == discovery.TypeExtension {
			v.targetsLine = i18n.TFor(lang, "tui_targets_missing_local")
		} else {
			v.targetsLine = i18n.TFor(lang, "tui_targets_none")
		}
		ruSt := rupack.InspectDesktop(inst)
		v.ruApplicable = ruSt.Applicable
		v.ruInstalled = ruSt.Installed
		v.ruHasBackup = ruSt.HasBackup
		v.ruVersion = ruSt.Version
		views = append(views, v)
	}
	return views
}

func (m *model) triggerPatch() {
	if m.busy != "" {
		m.addLog(logWarn, m.t("tui_log_busy"))
		return
	}
	selected := m.selectedInstalls()
	if len(selected) == 0 {
		m.addLog(logWarn, m.t("tui_log_nothing_selected"))
		return
	}

	m.busy = m.t("tui_busy_patch")
	m.addLog(logInfo, m.t("tui_log_patching_count", len(selected)))

	go func() {
		m.runPatchInstallsWithLogs(selected)
		m.postEvent(uiMsg{kind: "task_done"})
	}()
}

func (m *model) runPatchInstallsWithLogs(installs []discovery.Install) {
	rep := patcher.ApplyInstalls(installs, func(ev patcher.EngineEvent) {
		switch ev.Code {
		case patcher.EvInstallHeader:
			m.postLog(logInfo, fmt.Sprintf("%s (%s): %s", ev.Install.Label, ev.Install.Type, ev.Install.Path))
		case patcher.EvInstallNoPatchNeeded:
			m.postLog(logInfo, m.t("tui_log_jb_no_patch", ev.Install.Label))
		case patcher.EvBinarySkipped:
			m.postLog(logInfo, m.t("tui_log_bin_skipped", filepath.Base(ev.Path)))
		case patcher.EvBinaryPatched:
			m.postLog(logOK, m.t("tui_log_bin_patched", filepath.Base(ev.Path)))
		case patcher.EvBinaryAlready:
			m.postLog(logInfo, m.t("tui_log_bin_already", filepath.Base(ev.Path)))
		case patcher.EvBinaryNoMatch:
			m.postLog(logWarn, m.t("tui_log_bin_noop", filepath.Base(ev.Path)))
		case patcher.EvBinaryProxyMissing:
			m.postLog(logWarn, m.t("tui_log_bin_no_proxy", filepath.Base(ev.Path)))
		case patcher.EvBinaryError:
			m.postLog(logErr, fmt.Sprintf("%s: %v", filepath.Base(ev.Path), ev.Err))
		case patcher.EvAsarModern:
			m.postLog(logInfo, m.t("tui_log_asar_modern"))
		case patcher.EvAsarError:
			m.postLog(logErr, fmt.Sprintf("app.asar: %v", ev.Err))
		case patcher.EvJSPatched:
			m.postLog(logOK, m.t("tui_log_js_patched", filepath.Base(ev.Path)))
		case patcher.EvJSAlready:
			m.postLog(logInfo, m.t("tui_log_js_already", filepath.Base(ev.Path)))
		case patcher.EvJSNoMatch:
			m.postLog(logInfo, m.t("tui_log_js_noop", filepath.Base(ev.Path)))
		case patcher.EvJSError:
			m.postLog(engineLogLevel(ev.Level), fmt.Sprintf("%s: %v", filepath.Base(ev.Path), ev.Err))
		}
	})

	if rep.NeedsSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
		m.postLog(logWarn, m.t("tui_log_sudo_hint", "patch"))
	} else if rep.HasErrors {
		m.postLog(logWarn, m.t("tui_log_patch_err"))
	} else {
		m.postLog(logOK, m.t("tui_log_patch_ok"))
	}
}

func (m *model) triggerLinuxInstall() {
	if m.busy != "" {
		m.addLog(logWarn, m.t("tui_log_busy"))
		return
	}
	m.ensureLinuxInstallDefaults()
	opts := m.linuxInstallOpts
	if strings.TrimSpace(opts.ArchivePath) == "" || !hostenv.IsRegularFile(strings.TrimSpace(opts.ArchivePath)) {
		m.addLog(logErr, m.t("tui_log_li_archive_missing"))
		return
	}
	autoPatch := m.linuxInstallPatch

	m.busy = m.t("tui_busy_linux_install")
	opts.OnProgress = func(ev linuxinstall.ProgressEvent) {
		m.postLog(logInfo, m.t(ev.Key, ev.Args...))
	}

	go func() {
		res, err := linuxinstall.Install(opts)
		if err != nil {
			m.postLog(logErr, m.t("tui_log_li_err", err))
			if errors.Is(err, os.ErrPermission) {
				m.postLog(logWarn, m.t("tui_log_li_perm_hint"))
			}
			m.postEvent(uiMsg{kind: "task_done"})
			return
		}

		ver := res.AppVersion
		if ver == "" {
			ver = "unknown"
		}
		m.postLog(logOK, m.t("tui_log_li_ok", ver, res.InstallDir))
		if res.HasSnap {
			m.postLog(logWarn, m.t("tui_log_li_snap_warn"))
		}

		if autoPatch {
			if inst := discovery.ResolveManualPath(res.InstallDir); inst != nil {
				m.runPatchInstallsWithLogs([]discovery.Install{*inst})
			}
		}

		customDir := ""
		if opts.Scope == linuxinstall.ScopeCustom {
			customDir = res.InstallDir
		}
		m.postEvent(uiMsg{
			kind:             "linux_install_done",
			customInstallDir: customDir,
		})
	}()
}

func (m *model) triggerRestore() {
	if m.busy != "" {
		return
	}
	selected := m.selectedInstalls()
	if len(selected) == 0 {
		m.addLog(logWarn, m.t("tui_log_nothing_selected"))
		return
	}

	m.busy = m.t("tui_busy_restore")
	m.addLog(logInfo, m.t("tui_log_restoring_count", len(selected)))

	go func() {
		rep := patcher.RestoreInstalls(selected, patcher.RestoreOptions{
			OnEvent: func(ev patcher.EngineEvent) {
				switch ev.Code {
				case patcher.EvRestoreFileOK:
					m.postLog(logOK, m.t("tui_log_file_restored", filepath.Base(ev.Path)))
				case patcher.EvAsarRestored:
					m.postLog(logOK, m.t("tui_log_file_restored", "app.asar"))
				case patcher.EvRestoreFileOriginal:
					m.postLog(logInfo, m.t("tui_log_file_already_orig", filepath.Base(ev.Path)))
				case patcher.EvRestoreFileErr:
					m.postLog(logErr, fmt.Sprintf("%s: %v", filepath.Base(ev.Path), ev.Err))
				case patcher.EvAsarError:
					m.postLog(logErr, fmt.Sprintf("app.asar: %v", ev.Err))
				}
			},
		})

		if rep.NeedsSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			m.postLog(logWarn, m.t("tui_log_sudo_hint", "restore"))
		} else if rep.HasErrors {
			m.postLog(logWarn, m.t("tui_log_restore_err"))
		} else {
			m.postLog(logOK, m.t("tui_log_restore_ok"))
		}
		m.postEvent(uiMsg{kind: "task_done"})
	}()
}

func engineLogLevel(lvl patcher.EventLevel) logLevel {
	switch lvl {
	case patcher.LevelOK:
		return logOK
	case patcher.LevelWarn:
		return logWarn
	case patcher.LevelErr:
		return logErr
	default:
		return logInfo
	}
}

func (m *model) triggerRUInstall(force bool) {
	if m.busy != "" {
		m.addLog(logWarn, m.t("tui_log_busy"))
		return
	}
	var targets []discovery.Install
	for _, iv := range m.installs {
		if iv.ruApplicable {
			targets = append(targets, iv.install)
		}
	}
	if len(targets) == 0 {
		m.addLog(logWarn, m.t("tui_log_ru_no_desktop"))
		return
	}

	m.busy = m.t("tui_busy_ru_install")
	m.addLog(logInfo, m.t("tui_log_ru_fetching"))

	go func() {
		for _, inst := range targets {
			m.postLog(logInfo, fmt.Sprintf("%s: %s", inst.Label, inst.Path))
			res, err := rupack.Install(context.Background(), inst, rupack.InstallOptions{
				Force: force,
				Logf: func(lvl, msg string) {
					m.postLog(parseDaemonLevel(lvl), msg)
				},
			})
			if err != nil {
				var uve *rupack.UnsupportedVersionError
				if errors.As(err, &uve) {
					m.postLog(logWarn, m.t("tui_log_ru_unsupported", uve.Detected, strings.Join(uve.Available, ", ")))
				} else if errors.Is(err, os.ErrPermission) && runtime.GOOS != "windows" && os.Geteuid() != 0 {
					m.postLog(logWarn, m.t("tui_log_sudo_hint", "ru-install"))
				} else {
					m.postLog(logErr, fmt.Sprintf("ru-install: %v", err))
				}
				continue
			}
			m.postLog(logOK, m.t("ru_install_ok", res.PackVersion))
		}
		m.postEvent(uiMsg{kind: "task_done"})
	}()
}

func (m *model) triggerRURestore() {
	if m.busy != "" {
		m.addLog(logWarn, m.t("tui_log_busy"))
		return
	}
	var targets []discovery.Install
	for _, iv := range m.installs {
		if iv.ruApplicable {
			targets = append(targets, iv.install)
		}
	}
	if len(targets) == 0 {
		m.addLog(logWarn, m.t("ru_not_desktop"))
		return
	}

	m.busy = m.t("tui_busy_ru_restore")
	m.addLog(logInfo, m.t("ru_restore_start"))

	go func() {
		for _, inst := range targets {
			res, err := rupack.Uninstall(inst, func(lvl, msg string) {
				m.postLog(parseDaemonLevel(lvl), msg)
			})
			if err != nil {
				if errors.Is(err, rupack.ErrNoBackup) {
					m.postLog(logWarn, m.t("ru_restore_nobak"))
				} else if errors.Is(err, os.ErrPermission) && runtime.GOOS != "windows" && os.Geteuid() != 0 {
					m.postLog(logWarn, m.t("tui_log_sudo_hint", "ru-restore"))
				} else {
					m.postLog(logErr, fmt.Sprintf("ru-restore: %v", err))
				}
				continue
			}
			if !res.AsarRestored && !res.BundleRemoved {
				m.postLog(logInfo, m.t("ru_restore_noop"))
			} else {
				m.postLog(logOK, m.t("ru_restore_ok"))
			}
		}
		m.postEvent(uiMsg{kind: "task_done"})
	}()
}

func (m *model) triggerClean() {
	if m.busy != "" {
		return
	}
	if m.daemonClient != nil {
		cl := m.daemonClient
		m.daemonClient = nil
		_ = cl.Shutdown()
	}
	m.stopProxyIfOwned()
	m.busy = m.t("tui_busy_clean")
	m.addLog(logInfo, m.t("tui_log_cleaning"))

	manualCopy := append([]string(nil), m.manualPaths...)
	replacedCopy := cloneReplacedMap(m.replacedPaths)
	go func() {
		installs := validInstalls(collectInstalls(manualCopy, replacedCopy))
		rep := cleaner.CleanInstalls(installs)
		for _, f := range rep.Files {
			if f.Err != nil {
				m.postLog(logErr, fmt.Sprintf("%s: %v", filepath.Base(f.Path), f.Err))
			} else if f.Restored {
				m.postLog(logOK, m.t("tui_log_file_cleaned", filepath.Base(f.Path)))
			}
		}
		if rep.EnvErr != nil {
			m.postLog(logErr, fmt.Sprintf("env: %v", rep.EnvErr))
		} else if rep.EnvRemoved {
			m.postLog(logOK, m.t("tui_log_env_cleared", env.ProxyVar))
		}
		if err := autostart.RemoveAll(); err != nil {
			m.postLog(logWarn, fmt.Sprintf("autostart: %v", err))
		}
		if err := config.Remove(); err != nil {
			m.postLog(logWarn, fmt.Sprintf("config: %v", err))
		}
		m.postEvent(uiMsg{kind: "task_done"})
	}()
}

func (m *model) toggleAutostart() {
	if m.autostartOn {
		if err := autostart.Disable(); err != nil {
			m.addLog(logErr, m.t("tui_log_autostart_disable_err", err))
			return
		}
		m.autostartOn = false
		m.addLog(logOK, m.t("tui_log_autostart_disabled"))
	} else {
		installedPath, err := autostart.Enable()
		if err != nil {
			m.addLog(logErr, m.t("tui_log_autostart_enable_err", err))
			return
		}
		m.autostartOn = true
		m.addLog(logOK, m.t("tui_log_autostart_enabled", installedPath))
	}
	if m.daemonClient != nil {
		cl := m.daemonClient
		go func() {
			_, _ = cl.RefreshAutostart()
		}()
	}
}

func (m *model) minimizeToTray() {
	if m.daemonClient != nil {
		m.shutdownDaemon = false
		m.quitting = true
		return
	}
	hadProxy := m.proxyOwned
	m.stopProxy(hadProxy)
	if err := tray.SpawnTrayBackground(tray.LaunchOptions{
		Version:    m.version,
		Lang:       m.lang,
		StartProxy: hadProxy,
	}); err != nil {
		m.addLog(logErr, m.t("tui_log_tray_err", err))
		if hadProxy {
			m.toggleProxy()
		}
		return
	}
	m.quitting = true
}

func (m *model) toggleProxy() {
	if m.connectingDaemon {
		m.wantProxyPending = true
		m.addLog(logInfo, m.t("tui_log_connecting_daemon"))
		return
	}
	if m.daemonClient != nil {
		cl := m.daemonClient
		owned := m.proxyOwned
		go func() {
			var (
				snap *daemon.StateSnapshot
				err  error
			)
			if owned {
				snap, err = cl.StopProxy()
			} else {
				snap, err = cl.StartProxy()
			}
			if err != nil {
				m.postLog(logErr, fmt.Sprintf("proxy: %v", err))
				return
			}
			if snap != nil {
				m.postEvent(uiMsg{kind: "daemon_state", daemonSnap: snap})
			}
		}()
		return
	}
	if m.proxyOwned {
		m.stopProxyIfOwned()
		m.triggerScan()
		return
	}

	if addr, isDaemon, active := daemon.FindActiveProxy(); active && !isDaemon {
		m.addLog(logWarn, m.t("tui_log_cli_proxy_active", addr))
		return
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		m.addLog(logWarn, m.t("tui_log_sudo_proxy_warn", env.ProxyVar))
	}

	m.addLog(logInfo, m.t("daemon_proxy_start_log"))

	srv := proxy.NewServer(proxy.ListenAddr, proxy.DohURL)
	srv.Pool.UpdateDNS(m.dnsMode, cloneDNSList(m.dnsProviders))
	if pref := proxy.LoopbackAddrFromURL(env.GetProxyVar()); pref != "" && !proxy.IsProxyRunning(pref) {
		srv.PreferredAddr = pref
	}
	srv.Pool.OnResolve = func(d proxy.ResolveDetail) {
		subTag := "direct"
		if d.Substituted {
			subTag = "sub"
		}
		ipStr := ""
		if len(d.IPs) > 0 {
			ipStr = d.IPs[0]
		}
		m.postLog(logDNS, fmt.Sprintf("%s → %s (%s, %s, %dms)", d.Host, ipStr, d.Provider, subTag, d.Latency.Milliseconds()))
	}
	srv.OnConnect = func(host string, gate bool, target string, err error) {
		if err != nil {
			m.postLog(logErr, fmt.Sprintf("%s (%s): %v", host, target, err))
			return
		}
		if gate {
			m.postLog(logProxy, fmt.Sprintf("CONNECT %s → %s", host, target))
		}
	}

	pCtx, pCancel := context.WithCancel(context.Background())
	m.proxyServer = srv
	m.proxyCancel = pCancel
	m.proxyDone = make(chan error, 1)

	go func() {
		err := srv.Run(pCtx, func(addr string) {
			proxyURL := "http://" + addr
			if setErr := env.SetProxyVar(proxyURL); setErr != nil {
				var fpe *env.ForeignProxyError
				if errors.As(setErr, &fpe) {
					m.postLog(logWarn, m.t("daemon_proxy_foreign_skipped", fpe.Source, fpe.Value, env.ProxyVar))
				} else {
					m.postLog(logWarn, fmt.Sprintf("%s: %v", env.ProxyVar, setErr))
				}
			} else if runtime.GOOS == "linux" {
				m.postLog(logInfo, m.t("proxy_linux_env_tip", env.ProxyVar, proxyURL))
			}
			m.postEvent(uiMsg{kind: "proxy_ready", proxyStarted: addr})
		})
		m.proxyDone <- err
		m.postEvent(uiMsg{kind: "proxy_exit", proxyErr: err})
	}()
}

func (m *model) stopProxyIfOwned() {
	m.stopProxy(false)
}

func (m *model) stopProxy(preserveEnv bool) {
	if !m.proxyOwned && m.proxyCancel == nil {
		return
	}
	addr := m.proxyAddr
	if m.proxyCancel != nil {
		m.proxyCancel()
		m.proxyCancel = nil
	}
	if m.proxyDone != nil {
		<-m.proxyDone
		m.proxyDone = nil
	}
	if !preserveEnv && addr != "" && env.IsProxyOurs("http://"+addr) {
		if err := env.RemoveProxyVar(); err != nil {
			m.addLog(logWarn, fmt.Sprintf("%s cleanup: %v", env.ProxyVar, err))
		}
	}
	m.proxyOwned = false
	m.proxyAddr = ""
	m.proxyServer = nil
}

func installSelectKey(inst discovery.Install) string {
	return strings.ToLower(fmt.Sprintf("%s|%s|%s|%s",
		inst.Type,
		filepath.Clean(inst.Path),
		filepath.Clean(inst.BinaryPath),
		filepath.Clean(inst.ExtensionJsPath),
	))
}

type collectedInstall struct {
	install     discovery.Install
	overrideKey string
	missing     bool
}

func collectInstalls(manualPaths []string, replacedPaths map[string]string) []collectedInstall {
	discovered := discovery.FindInstalls()
	seen := make(map[string]bool, len(discovered)+len(manualPaths))
	out := make([]collectedInstall, 0, len(discovered)+len(manualPaths))

	for _, inst := range discovered {
		origKey := installSelectKey(inst)
		if customPath, ok := replacedPaths[origKey]; ok && strings.TrimSpace(customPath) != "" {
			if repl := discovery.ResolveManualPath(customPath); repl != nil {
				repl.Manual = true
				seen[installSelectKey(*repl)] = true
				out = append(out, collectedInstall{install: *repl, overrideKey: origKey})
			} else {
				out = append(out, collectedInstall{
					install: discovery.Install{
						Label:  inst.Label,
						Type:   inst.Type,
						Path:   customPath,
						Manual: true,
					},
					overrideKey: origKey,
					missing:     true,
				})
			}
			continue
		}
		if !seen[origKey] {
			seen[origKey] = true
			out = append(out, collectedInstall{install: inst})
		}
	}

	for _, p := range manualPaths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if inst := discovery.ResolveManualPath(trimmed); inst != nil {
			key := installSelectKey(*inst)
			if !seen[key] {
				seen[key] = true
				out = append(out, collectedInstall{install: *inst})
			}
		} else {
			fallback := discovery.Install{
				Label:  filepath.Base(filepath.Clean(trimmed)),
				Type:   "manual",
				Path:   trimmed,
				Manual: true,
			}
			key := installSelectKey(fallback)
			if !seen[key] {
				seen[key] = true
				out = append(out, collectedInstall{install: fallback, missing: true})
			}
		}
	}
	return out
}

func validInstalls(collected []collectedInstall) []discovery.Install {
	out := make([]discovery.Install, 0, len(collected))
	for _, ci := range collected {
		if !ci.missing {
			out = append(out, ci.install)
		}
	}
	return out
}

const logExportCooldown = 2 * time.Second

func (m *model) exportLogsToFile() {
	now := m.now()
	if !m.lastLogExportAt.IsZero() && m.lastLogExportPath != "" && now.Sub(m.lastLogExportAt) < logExportCooldown {
		m.setToast(logOK, m.t("tui_toast_logs_saved_cached", m.lastLogExportPath), 4*time.Second)
		return
	}

	candidates := append([]string(nil), m.logExportDirs...)
	if len(candidates) == 0 {
		if wd, err := os.Getwd(); err == nil && !isSystemCwd(wd) {
			candidates = append(candidates, wd)
		}
		if dataDir, err := hostenv.AppDataDir(hostenv.AppDirName); err == nil && dataDir != "" {
			candidates = append(candidates, filepath.Join(dataDir, "logs"))
		}
		if tmpDir := os.TempDir(); tmpDir != "" {
			candidates = append(candidates, tmpDir)
		}
	}

	report := m.buildDiagnosticLogReport()
	var (
		savedPath string
		lastErr   error
	)
	for _, dir := range candidates {
		p, err := writeDiagnosticLogFile(dir, report)
		if err == nil {
			savedPath = p
			break
		}
		lastErr = err
	}
	if savedPath != "" {
		m.lastLogExportAt = now
		m.lastLogExportPath = savedPath
		m.addLog(logOK, m.t("tui_log_logs_saved", savedPath))
		m.setToast(logOK, m.t("tui_toast_logs_saved", savedPath), 4*time.Second)
		return
	}
	m.addLog(logErr, m.t("tui_log_logs_save_err", lastErr))
	m.setToast(logErr, m.t("tui_toast_logs_save_err", lastErr), 4*time.Second)
}

func writeDiagnosticLogFile(dir, content string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	stamp := time.Now().Format("20060102-150405")
	base := fmt.Sprintf("agpatcher-%s.log", stamp)
	target := filepath.Join(dir, base)
	for i := 2; i < 100 && hostenv.IsRegularFile(target); i++ {
		target = filepath.Join(dir, fmt.Sprintf("agpatcher-%s-%d.log", stamp, i))
	}
	if err := hostenv.WriteFileAtomic(target, []byte(content), 0644, nil); err != nil {
		return "", err
	}
	return target, nil
}

func (m *model) buildDiagnosticLogReport() string {
	var b strings.Builder
	now := time.Now()
	exe, _ := os.Executable()
	wd, _ := os.Getwd()

	mode := "standalone"
	if m.daemonClient != nil {
		info := m.daemonClient.Info()
		mode = fmt.Sprintf("tray-daemon (pid=%d, ipc=%s)", info.PID, info.Addr)
	}

	st := m.proxyStats
	if m.proxyOwned && m.proxyServer != nil {
		st = m.proxyServer.Stats()
	}

	b.WriteString("=== agy-patcher diagnostic log ===\n")
	fmt.Fprintf(&b, "Generated: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "Version: agpatcher v%s\n", m.version)
	fmt.Fprintf(&b, "OS/Arch: %s\n", m.osLabel)
	fmt.Fprintf(&b, "Language: %s\n", m.lang)
	fmt.Fprintf(&b, "Mode: %s\n", mode)
	fmt.Fprintf(&b, "Executable: %s\n", exe)
	fmt.Fprintf(&b, "WorkingDir: %s\n", wd)

	b.WriteString("\n[PROXY & ENV]\n")
	fmt.Fprintf(&b, "%s: %s\n", env.ProxyVar, emptyOr(m.proxyEnv, "<not set>"))
	fmt.Fprintf(&b, "SystemHTTPSProxy: %s\n", emptyOr(m.externalProxy, "<none>"))
	fmt.Fprintf(&b, "ProxyOwned: %v\n", m.proxyOwned)
	fmt.Fprintf(&b, "ProxyAddr: %s\n", emptyOr(m.proxyAddr, "<none>"))
	if m.proxyNote != "" {
		fmt.Fprintf(&b, "ProxyNote: %s\n", m.proxyNote)
	}
	fmt.Fprintf(&b, "Stats: active=%d total=%d gate=%d\n", st.Active, st.Total, st.Gate)
	fmt.Fprintf(&b, "Autostart: %v\n", m.autostartOn)
	fmt.Fprintf(&b, "DNSMode: %s\n", proxy.NormalizeDNSMode(m.dnsMode))
	for i, sp := range m.dnsProviders {
		enTag := "off"
		if sp.Enabled {
			enTag = "on"
		}
		fmt.Fprintf(&b, "  DNS[%d]: [%s] %s (%s) %s", i+1, enTag, sp.Name, sp.Kind, sp.Target)
		if len(sp.FallbackIPs) > 0 {
			fmt.Fprintf(&b, " fallback=[%s]", strings.Join(sp.FallbackIPs, ", "))
		}
		b.WriteByte('\n')
	}

	b.WriteString("\n[INSTALLATIONS]\n")
	if len(m.manualPaths) > 0 {
		fmt.Fprintf(&b, "ManualPaths (%d): %s\n", len(m.manualPaths), strings.Join(m.manualPaths, " | "))
	}
	if len(m.replacedPaths) > 0 {
		fmt.Fprintf(&b, "ReplacedPaths (%d):\n", len(m.replacedPaths))
		for k, v := range m.replacedPaths {
			fmt.Fprintf(&b, "  %s -> %s\n", k, v)
		}
	}
	if len(m.installs) == 0 {
		b.WriteString("None discovered\n")
	} else {
		for _, iv := range m.installs {
			selMark := "[ ]"
			if m.selected[iv.installKey] {
				selMark = "[x]"
			}
			fmt.Fprintf(&b, "- %s %s (%s): %s\n", selMark, iv.install.Label, iv.install.Type, iv.install.Path)
			fmt.Fprintf(&b, "  targets=%q binaryPatched=%v proxyVarPatched=%v hasJS=%v jsPatched=%v ru=%v(installed=%v,ver=%s,backup=%v)\n",
				iv.targetsLine, iv.binaryPatched, iv.proxyVarPatched, iv.hasJS, iv.jsPatched,
				iv.ruApplicable, iv.ruInstalled, emptyOr(iv.ruVersion, "none"), iv.ruHasBackup)
		}
	}

	fmt.Fprintf(&b, "\n[LOGS (%d)]\n", len(m.logs))
	if len(m.logs) == 0 {
		b.WriteString("(empty)\n")
	} else {
		for _, entry := range m.logs {
			fmt.Fprintf(&b, "%s [%s] %s\n", entry.ts, logLevelLabel(entry.level), entry.msg)
		}
	}
	return b.String()
}

func logLevelLabel(lvl logLevel) string {
	switch lvl {
	case logOK:
		return "OK"
	case logWarn:
		return "WARN"
	case logErr:
		return "ERR"
	case logProxy:
		return "PROXY"
	case logDNS:
		return "DNS"
	default:
		return "INFO"
	}
}

func emptyOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func isSystemCwd(dir string) bool {
	if dir == "" || dir == string(filepath.Separator) {
		return true
	}
	if runtime.GOOS == "windows" {
		low := strings.ToLower(filepath.Clean(dir))
		if strings.HasSuffix(low, `\windows\system32`) || strings.HasSuffix(low, `\windows`) {
			return true
		}
	}
	return false
}
