package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

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
	"agy-patcher/pkg/tui"
)

var version = "1.0.0"

type cliOptions struct {
	lang          string
	path          string
	command       string
	subarg        string
	force         bool
	tray          bool
	trayWorker    bool
	withProxy     bool
	noProxy       bool
	systemInstall bool
	noDesktop     bool
	noSandbox     bool
	noPatch       bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	tui.EnsureUTF8Console()
	opts, parseErr := parseArgs(args)

	cfg := config.Load()
	lang := opts.lang
	if lang == "" {
		lang = cfg.Lang
	}
	if lang == "" {
		lang = detectEnvLang()
	}

	localesDir := ""
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "locales")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			localesDir = candidate
		}
	}

	if err := i18n.Init(lang, localesDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to load translations: %v. Using keys.\n", err)
	}

	if parseErr != nil {
		fmt.Fprintln(os.Stderr, parseErr.Error())
		printUsage()
		return 1
	}

	if opts.trayWorker {
		if err := tray.RunTray(tray.LaunchOptions{Version: version, Lang: lang, StartProxy: !opts.noProxy}); err != nil {
			if errors.Is(err, tray.ErrAlreadyRunning) {
				return 0
			}
			fmt.Fprintf(os.Stderr, "Tray error: %v\n", err)
			return 1
		}
		return 0
	}

	if opts.tray || opts.command == "tray" {
		if err := tray.HideAndRelaunchDetached(tray.LaunchOptions{Version: version, Lang: lang, StartProxy: !opts.noProxy}); err != nil {
			if errors.Is(err, tray.ErrAlreadyRunning) {
				return 0
			}
			fmt.Fprintf(os.Stderr, "Tray error: %v\n", err)
			return 1
		}
		return 0
	}

	if opts.command == "" {
		if tui.IsInteractive() {
			if err := tui.RunWithOptions(tui.Options{
				Version:        version,
				Lang:           lang,
				InitialPath:    opts.path,
				AutoStartProxy: opts.withProxy,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
				return 1
			}
			return 0
		}
		printUsage()
		return 1
	}

	switch opts.command {
	case "tui", "--tui":
		if err := tui.RunWithOptions(tui.Options{
			Version:        version,
			Lang:           lang,
			InitialPath:    opts.path,
			AutoStartProxy: opts.withProxy,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			return 1
		}
		return 0
	case "open-tui":
		if err := tray.SpawnTUIWindow(tray.LaunchOptions{Version: version, Lang: lang}); err != nil {
			fmt.Fprintf(os.Stderr, "Open TUI error: %v\n", err)
			return 1
		}
		return 0
	case "autostart":
		return handleAutostart(opts.subarg)
	case "patch":
		return handlePatch(opts.path)
	case "restore":
		return handleRestore(opts.path)
	case "ru-install", "localize":
		return handleRUInstall(opts.path, opts.force)
	case "ru-restore", "unlocalize":
		return handleRURestore(opts.path)
	case "install":
		return handleLinuxInstall(opts)
	case "proxy":
		return handleProxy()
	case "dns-check":
		return handleDNSCheck(opts.force)
	case "clean":
		return handleClean(opts.path)
	case "status":
		return handleStatus(opts.path)
	case "version", "--version", "-v":
		fmt.Println("agy-patcher CLI v" + version)
		return 0
	case "help", "--help", "-h":
		printUsage()
		return 0
	default:
		fmt.Printf(i18n.Get("cmd_unknown")+"\n", opts.command)
		printUsage()
		return 1
	}
}

func parseArgs(args []string) (cliOptions, error) {
	var opts cliOptions
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--lang" || arg == "-l":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing value for flag: %s", arg)
			}
			i++
			opts.lang = args[i]
		case strings.HasPrefix(arg, "--lang="):
			opts.lang = strings.TrimPrefix(arg, "--lang=")
		case arg == "--path" || arg == "-p":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing value for flag: %s", arg)
			}
			i++
			opts.path = args[i]
		case strings.HasPrefix(arg, "--path="):
			opts.path = strings.TrimPrefix(arg, "--path=")
		case arg == "--force" || arg == "-f":
			opts.force = true
		case arg == "--tray":
			opts.tray = true
		case arg == "--tray-worker":
			opts.trayWorker = true
		case arg == "--with-proxy":
			opts.withProxy = true
		case arg == "--no-proxy":
			opts.noProxy = true
		case arg == "--system":
			opts.systemInstall = true
		case arg == "--no-desktop":
			opts.noDesktop = true
		case arg == "--no-sandbox":
			opts.noSandbox = true
		case arg == "--no-patch":
			opts.noPatch = true
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) > 0 {
		opts.command = strings.ToLower(positional[0])
	}
	if len(positional) > 1 {
		opts.subarg = positional[1]
	}
	return opts, nil
}

func printUsage() {
	fmt.Println(i18n.Get("cmd_usage"))
	fmt.Println(i18n.Get("cmd_commands"))
	fmt.Printf("  tui        - %s\n", i18n.Get("cmd_tui"))
	fmt.Printf("  tray       - %s\n", i18n.Get("cmd_tray"))
	fmt.Printf("  autostart  - %s\n", i18n.Get("cmd_autostart"))
	fmt.Printf("  patch      - %s\n", i18n.Get("cmd_patch"))
	if runtime.GOOS == "linux" {
		fmt.Printf("  install    - %s\n", i18n.Get("cmd_install"))
	}
	fmt.Printf("  restore    - %s\n", i18n.Get("cmd_restore"))
	fmt.Printf("  ru-install - %s\n", i18n.Get("cmd_ru_install"))
	fmt.Printf("  ru-restore - %s\n", i18n.Get("cmd_ru_restore"))
	fmt.Printf("  proxy      - %s\n", i18n.Get("cmd_proxy"))
	fmt.Printf("  dns-check  - %s\n", i18n.Get("cmd_dns_check"))
	fmt.Printf("  clean      - %s\n", i18n.Get("cmd_clean"))
	fmt.Printf("  status     - %s\n", i18n.Get("cmd_status"))
	fmt.Printf("  version    - %s\n", i18n.Get("cmd_version"))
	fmt.Printf("  help       - %s\n", i18n.Get("cmd_help"))
}

func handleAutostart(sub string) int {
	switch strings.ToLower(sub) {
	case "", "status":
		enabled, target := autostart.IsEnabled()
		if enabled {
			fmt.Printf("Autostart: enabled (%s)\n", target)
		} else {
			fmt.Println("Autostart: disabled")
		}
		return 0
	case "on", "enable":
		p, err := autostart.Enable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Autostart enable failed: %v\n", err)
			return 1
		}
		fmt.Printf("Autostart enabled: %s --tray\n", p)
		return 0
	case "off", "disable":
		if err := autostart.Disable(); err != nil {
			fmt.Fprintf(os.Stderr, "Autostart disable failed: %v\n", err)
			return 1
		}
		fmt.Println("Autostart disabled.")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Usage: agpatcher autostart [on|off|status]\n")
		return 1
	}
}

func resolveInstalls(manualPath string) ([]discovery.Install, bool) {
	if manualPath != "" {
		inst := discovery.ResolveManualPath(manualPath)
		if inst == nil {
			fmt.Printf(i18n.Get("scan_manual_not_found")+"\n", manualPath)
			return nil, false
		}
		return []discovery.Install{*inst}, true
	}

	installs := discovery.FindInstalls()
	if len(installs) == 0 {
		fmt.Println(i18n.Get("scan_none"))
		return nil, false
	}
	return installs, true
}

func handlePatch(manualPath string) int {
	fmt.Println(i18n.Get("scan_start"))
	installs, ok := resolveInstalls(manualPath)
	if !ok {
		return 1
	}
	return patchInstallsCLI(installs)
}

func patchInstallsCLI(installs []discovery.Install) int {
	rep := patcher.ApplyInstalls(installs, func(ev patcher.EngineEvent) {
		switch ev.Code {
		case patcher.EvInstallHeader:
			fmt.Printf(i18n.Get("scan_found")+"\n", ev.Install.Label, ev.Install.Path, ev.Install.Type)
		case patcher.EvInstallNoPatchNeeded:
			fmt.Printf("  "+i18n.Get("tui_log_jb_no_patch")+"\n", ev.Install.Label)
		case patcher.EvBinaryStart:
			fmt.Printf(i18n.Get("patching_binary")+"\n", ev.Path)
		case patcher.EvBinaryPatched:
			fmt.Println(i18n.Get("patch_binary_ok"))
		case patcher.EvBinaryAlready:
			fmt.Println(i18n.Get("patch_binary_already"))
		case patcher.EvBinaryNoMatch:
			fmt.Println(i18n.Get("patch_binary_noop"))
		case patcher.EvBinaryProxyMissing:
			fmt.Printf(i18n.Get("patch_proxy_var_warn")+"\n", ev.Path)
		case patcher.EvBinaryError:
			fmt.Printf(i18n.Get("patch_binary_err")+"\n", ev.Err)
		case patcher.EvAsarModern:
			fmt.Println(i18n.Get("asar_modern_skip"))
		case patcher.EvAsarExtracting:
			fmt.Printf(i18n.Get("asar_extracting")+"\n", ev.Path)
		case patcher.EvAsarError:
			fmt.Printf(i18n.Get("asar_err")+"\n", ev.Err)
		case patcher.EvJSStart:
			fmt.Printf(i18n.Get("patching_js")+"\n", ev.Path)
		case patcher.EvJSPatched:
			fmt.Println(i18n.Get("patch_js_ok"))
		case patcher.EvJSAlready:
			fmt.Println(i18n.Get("patch_js_already"))
		case patcher.EvJSNoMatch:
			fmt.Println(i18n.Get("patch_js_noop"))
		case patcher.EvJSError:
			fmt.Printf(i18n.Get("patch_js_err")+"\n", ev.Err)
		}
	})

	if rep.HasErrors {
		if rep.NeedsSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Printf(i18n.Get("err_permission_sudo")+"\n", "patch")
		}
		fmt.Println(i18n.Get("patch_complete_errors"))
		return 1
	}
	fmt.Println(i18n.Get("patch_complete"))
	return 0
}

func handleLinuxInstall(opts cliOptions) int {
	if runtime.GOOS != "linux" {
		fmt.Println(i18n.Get("li_cli_linux_only"))
		return 1
	}

	instOpts := linuxinstall.DefaultOptions()
	if opts.path != "" {
		instOpts.ArchivePath = opts.path
	} else if opts.subarg != "" {
		instOpts.ArchivePath = opts.subarg
	}
	if instOpts.ArchivePath == "" {
		fmt.Println(i18n.Get("li_cli_no_archive"))
		return 1
	}
	if opts.systemInstall {
		instOpts.Scope = linuxinstall.ScopeSystem
	}
	if opts.noDesktop {
		instOpts.CreateDesktop = false
	}
	if opts.noSandbox {
		instOpts.NoSandbox = true
	}
	instOpts.OnProgress = func(ev linuxinstall.ProgressEvent) {
		fmt.Printf(i18n.Get(ev.Key)+"\n", ev.Args...)
	}

	targetDir := linuxinstall.DefaultTargetDir(instOpts.Scope, instOpts.CustomDir, instOpts.HomeDir)
	fmt.Printf(i18n.Get("li_cli_using_archive")+"\n", instOpts.ArchivePath, targetDir)

	res, err := linuxinstall.Install(instOpts)
	if err != nil {
		fmt.Printf(i18n.Get("tui_log_li_err")+"\n", err)
		if errors.Is(err, os.ErrPermission) {
			fmt.Println(i18n.Get("tui_log_li_perm_hint"))
		}
		return 1
	}
	ver := res.AppVersion
	if ver == "" {
		ver = "unknown"
	}
	fmt.Printf(i18n.Get("tui_log_li_ok")+"\n", ver, res.InstallDir)
	if res.HasSnap {
		fmt.Println(i18n.Get("tui_log_li_snap_warn"))
	}
	if !opts.noPatch {
		if inst := discovery.ResolveManualPath(res.InstallDir); inst != nil {
			if code := patchInstallsCLI([]discovery.Install{*inst}); code != 0 {
				return code
			}
		}
	}
	return 0
}

func handleRestore(manualPath string) int {
	fmt.Println(i18n.Get("restore_start"))
	installs, ok := resolveInstalls(manualPath)
	if !ok {
		return 1
	}

	rep := patcher.RestoreInstalls(installs, patcher.RestoreOptions{
		OnEvent: func(ev patcher.EngineEvent) {
			switch ev.Code {
			case patcher.EvRestoreFileOK, patcher.EvAsarRestored:
				fmt.Printf(i18n.Get("restore_file_ok")+"\n", ev.Path)
			case patcher.EvRestoreFileOriginal:
				fmt.Printf(i18n.Get("restore_file_missing")+"\n", ev.Path)
			case patcher.EvRestoreFileErr:
				fmt.Printf(i18n.Get("restore_file_err")+"\n", ev.Path, ev.Err)
			case patcher.EvAsarError:
				fmt.Printf(i18n.Get("asar_err")+"\n", ev.Err)
			}
		},
	})

	if rep.HasErrors {
		if rep.NeedsSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Printf(i18n.Get("err_permission_sudo")+"\n", "restore")
		}
		fmt.Println(i18n.Get("restore_complete_errors"))
		return 1
	}
	fmt.Println(i18n.Get("restore_complete"))
	return 0
}

func handleProxy() int {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		fmt.Printf(i18n.Get("proxy_sudo_warn")+"\n", env.ProxyVar)
	}
	fmt.Printf(i18n.Get("proxy_start")+"\n", proxy.ListenAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := proxy.NewServer(proxy.ListenAddr, proxy.DohURL)
	dnsMode, dnsProviders := daemon.EffectiveDNS(config.Load())
	srv.Pool.UpdateDNS(dnsMode, dnsProviders)
	if pref := proxy.LoopbackAddrFromURL(env.GetProxyVar()); pref != "" && !proxy.IsProxyRunning(pref) {
		srv.PreferredAddr = pref
	}

	proxyURL := "http://" + proxy.ListenAddr
	envApplied := false
	defer daemon.UnregisterStandaloneProxy()

	err := srv.Run(ctx, func(addr string) {
		_ = daemon.RegisterStandaloneProxy(addr)
		fmt.Printf(i18n.Get("proxy_listening")+"\n", addr)
		fmt.Println(i18n.Get("proxy_stop"))

		proxyURL = "http://" + addr
		if setErr := env.SetProxyVar(proxyURL); setErr != nil {
			var fpe *env.ForeignProxyError
			if errors.As(setErr, &fpe) {
				fmt.Printf(i18n.Get("proxy_foreign_warn")+"\n", fpe.Source, fpe.Value, env.ProxyVar)
			} else {
				fmt.Printf(i18n.Get("proxy_env_warn")+"\n", setErr)
			}
		} else {
			envApplied = true
			if runtime.GOOS == "linux" {
				fmt.Printf(i18n.Get("proxy_linux_env_tip")+"\n", env.ProxyVar, proxyURL)
			}
		}
	})

	if envApplied && env.IsProxyOurs(proxyURL) {
		fmt.Println(i18n.Get("proxy_stopping"))
		if rmErr := env.RemoveProxyVar(); rmErr != nil {
			fmt.Printf(i18n.Get("proxy_env_clean_warn")+"\n", rmErr)
		}
	}

	if err != nil {
		fmt.Printf(i18n.Get("proxy_err")+"\n", err)
		return 1
	}
	return 0
}

func handleDNSCheck(syncRepo bool) int {
	cfg := config.Load()
	_, list := daemon.EffectiveDNS(cfg)
	if syncRepo {
		if cat, err := proxy.FetchRepoCatalog(context.Background(), cfg.DNSCatalogURL, ""); err == nil {
			list, _ = proxy.SyncCatalogProviders(list, nil, cat.Providers)
		} else {
			list, _ = proxy.SyncCatalogProviders(list, nil, proxy.EmbeddedCatalog().Providers)
		}
	} else {
		list, _ = proxy.SyncCatalogProviders(list, nil, proxy.EmbeddedCatalog().Providers)
	}

	fmt.Printf(i18n.Get("tui_log_dns_check_start")+"\n", len(list))
	results := proxy.ProbeProviders(context.Background(), list, proxy.ProbeOptions{})
	subCount := 0
	for _, r := range results {
		label := fmt.Sprintf("%s [%s]", r.Name, strings.ToUpper(r.Kind))
		switch r.Status {
		case proxy.ProbeSub:
			subCount++
			fmt.Printf(i18n.Get("tui_log_dns_check_sub")+"\n", label, r.LatencyMs, r.Summary)
		case proxy.ProbeDirect:
			fmt.Printf(i18n.Get("tui_log_dns_check_direct")+"\n", label, r.LatencyMs)
		case proxy.ProbeTLSErr:
			fmt.Printf(i18n.Get("tui_log_dns_check_tlserr")+"\n", label, r.LatencyMs, r.Summary)
		default:
			fmt.Printf(i18n.Get("tui_log_dns_check_err")+"\n", label, r.LatencyMs, r.Summary)
		}
	}
	fmt.Printf(i18n.Get("tui_log_dns_check_done")+"\n", subCount, len(results))
	return 0
}

func handleRUInstall(manualPath string, force bool) int {
	installs, ok := resolveInstalls(manualPath)
	if !ok {
		return 1
	}
	var desktops []discovery.Install
	for _, inst := range installs {
		if rupack.InspectDesktop(inst).Applicable {
			desktops = append(desktops, inst)
		}
	}
	if len(desktops) == 0 {
		fmt.Println(i18n.Get("ru_not_desktop"))
		return 1
	}

	hasErrors := false
	needSudoHint := false
	for _, inst := range desktops {
		fmt.Printf(i18n.Get("ru_install_start")+"\n", inst.Path)
		res, err := rupack.Install(context.Background(), inst, rupack.InstallOptions{
			Force: force,
			Logf: func(lvl, msg string) {
				fmt.Printf("  [%s] %s\n", strings.ToUpper(lvl), msg)
			},
		})
		if err != nil {
			var uve *rupack.UnsupportedVersionError
			if errors.As(err, &uve) {
				fmt.Printf(i18n.Get("ru_install_unsupported")+"\n", uve.Detected, strings.Join(uve.Available, ", "))
			} else {
				fmt.Printf(i18n.Get("ru_install_err")+"\n", err)
				if errors.Is(err, os.ErrPermission) {
					needSudoHint = true
				}
			}
			hasErrors = true
			continue
		}
		fmt.Printf(i18n.Get("ru_install_ok")+"\n", res.PackVersion)
	}
	if hasErrors {
		if needSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Printf(i18n.Get("err_permission_sudo")+"\n", "ru-install")
		}
		return 1
	}
	return 0
}

func handleRURestore(manualPath string) int {
	installs, ok := resolveInstalls(manualPath)
	if !ok {
		return 1
	}
	var desktops []discovery.Install
	for _, inst := range installs {
		if rupack.InspectDesktop(inst).Applicable {
			desktops = append(desktops, inst)
		}
	}
	if len(desktops) == 0 {
		fmt.Println(i18n.Get("ru_not_desktop"))
		return 1
	}

	fmt.Println(i18n.Get("ru_restore_start"))
	hasErrors := false
	needSudoHint := false
	for _, inst := range desktops {
		res, err := rupack.Uninstall(inst, func(lvl, msg string) {
			fmt.Printf("  [%s] %s\n", strings.ToUpper(lvl), msg)
		})
		if err != nil {
			if errors.Is(err, rupack.ErrNoBackup) {
				fmt.Println(i18n.Get("ru_restore_nobak"))
			} else {
				fmt.Printf(i18n.Get("ru_restore_err")+"\n", err)
				if errors.Is(err, os.ErrPermission) {
					needSudoHint = true
				}
			}
			hasErrors = true
			continue
		}
		if !res.AsarRestored && !res.BundleRemoved {
			fmt.Println(i18n.Get("ru_restore_noop"))
		} else {
			fmt.Println(i18n.Get("ru_restore_ok"))
		}
	}
	if hasErrors {
		if needSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Printf(i18n.Get("err_permission_sudo")+"\n", "ru-restore")
		}
		return 1
	}
	return 0
}

func handleClean(manualPath string) int {
	fmt.Println(i18n.Get("clean_start"))

	if cl, _, err := daemon.Connect(); err == nil {
		_ = cl.Shutdown()
	}

	var installs []discovery.Install
	if manualPath != "" {
		if inst := discovery.ResolveManualPath(manualPath); inst != nil {
			installs = append(installs, *inst)
		} else {
			fmt.Printf(i18n.Get("scan_manual_not_found")+"\n", manualPath)
		}
	} else {
		installs = discovery.FindInstalls()
	}

	report := cleaner.CleanInstalls(installs)
	hasErrors := false
	needSudoHint := false

	for _, f := range report.Files {
		if f.Err != nil {
			fmt.Printf(i18n.Get("clean_file_err")+"\n", f.Path, f.Err)
			if errors.Is(f.Err, os.ErrPermission) {
				needSudoHint = true
			}
			hasErrors = true
		} else if f.Restored {
			fmt.Printf(i18n.Get("clean_file_ok")+"\n", f.Path)
		}
	}

	if report.EnvErr != nil {
		fmt.Printf(i18n.Get("clean_env_err")+"\n", report.EnvErr)
		hasErrors = true
	} else if report.EnvRemoved {
		fmt.Println(i18n.Get("clean_env_ok"))
	}

	if err := autostart.RemoveAll(); err != nil {
		fmt.Printf(i18n.Get("clean_env_err")+"\n", err)
		hasErrors = true
	}
	if err := config.Remove(); err != nil {
		fmt.Printf(i18n.Get("clean_env_err")+"\n", err)
		hasErrors = true
	}

	if hasErrors {
		if needSudoHint && runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Printf(i18n.Get("err_permission_sudo")+"\n", "clean")
		}
		fmt.Println(i18n.Get("clean_complete_errors"))
		return 1
	}
	fmt.Println(i18n.Get("clean_complete"))
	return 0
}

func handleStatus(manualPath string) int {
	fmt.Println(i18n.Get("status_title"))

	var installs []discovery.Install
	if manualPath != "" {
		if inst := discovery.ResolveManualPath(manualPath); inst != nil {
			installs = append(installs, *inst)
		} else {
			fmt.Printf(i18n.Get("scan_manual_not_found")+"\n", manualPath)
			return 1
		}
	} else {
		installs = discovery.FindInstalls()
	}

	fmt.Printf(i18n.Get("status_installs")+"\n", len(installs))
	for _, inst := range installs {
		fmt.Printf(i18n.Get("status_install_entry")+"\n", inst.Label, inst.Path, inst.Type)

		binaries := inst.Binaries
		if len(binaries) == 0 && inst.BinaryPath != "" {
			binaries = []string{inst.BinaryPath}
		}
		if inst.Type == discovery.TypeJetBrains {
			hasSig := false
			for _, bin := range binaries {
				st := patcher.InspectBinaryStatus(bin)
				if st.Field == patcher.StatePatched || st.Field == patcher.StateUnpatched || st.ProxyVar || st.HasOrigProxy {
					hasSig = true
					break
				}
			}
			if !hasSig && inst.JsPath == "" && (inst.ExtensionJsPath == "" || !patcher.NeedsExtensionJSPatch(inst.ExtensionJsPath)) {
				fmt.Printf("    %s\n", i18n.Get("tui_badge_patch_optional"))
				continue
			}
		}
		for _, bin := range binaries {
			st := patcher.InspectBinaryStatus(bin)
			fmt.Printf(i18n.Get("status_binary_patched")+"\n",
				bin, st.Field == patcher.StatePatched, st.ProxyVar)
		}
		if inst.JsPath != "" {
			fmt.Printf(i18n.Get("status_js_patched")+"\n",
				inst.JsPath, patcher.IsJSPatched(inst.JsPath), patcher.HasBackup(inst.JsPath))
		}
		if inst.ExtensionJsPath != "" && patcher.NeedsExtensionJSPatch(inst.ExtensionJsPath) {
			fmt.Printf(i18n.Get("status_js_patched")+"\n",
				inst.ExtensionJsPath, patcher.IsJSPatched(inst.ExtensionJsPath), patcher.HasBackup(inst.ExtensionJsPath))
		}
		if inst.AsarPath != "" {
			fmt.Printf(i18n.Get("status_asar_present")+"\n", inst.AsarPath)
		}
		if ruSt := rupack.InspectDesktop(inst); ruSt.Applicable {
			ver := ruSt.Version
			if ver == "" {
				ver = "unknown"
			}
			fmt.Printf(i18n.Get("status_ru_pack")+"\n", ruSt.Installed, ver, ruSt.HasBackup)
		}
	}

	proxyVar := env.GetProxyVar()
	if proxyVar == "" {
		proxyVar = i18n.Get("status_proxy_var_unset")
	}
	fmt.Printf(i18n.Get("status_proxy_var")+"\n", env.ProxyVar, proxyVar)

	if runningAddr, _, ok := daemon.FindActiveProxy(); ok {
		fmt.Printf(i18n.Get("status_proxy_running")+"\n", runningAddr)
	} else {
		fmt.Printf(i18n.Get("status_proxy_stopped")+"\n", proxy.ListenAddr)
	}
	return 0
}
