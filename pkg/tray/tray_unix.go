//go:build !windows

package tray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"agy-patcher/pkg/assets"
	"agy-patcher/pkg/daemon"
	"agy-patcher/pkg/i18n"
)

// HideAndRelaunchDetached ensures the background tray/proxy daemon is running detached on Unix.
func HideAndRelaunchDetached(opts LaunchOptions) error {
	_, _, err := EnsureDaemon(opts)
	return err
}

// SpawnTrayBackground launches the background tray/proxy daemon detached from the terminal.
func SpawnTrayBackground(opts LaunchOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--tray-worker"}
	if opts.Lang != "" {
		args = append(args, "--lang", opts.Lang)
	}
	if opts.StartProxy {
		args = append(args, "--with-proxy")
	} else {
		args = append(args, "--no-proxy")
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// SpawnTUIWindow opens a terminal window running the TUI if a supported terminal emulator is present.
func SpawnTUIWindow(opts LaunchOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"tui"}
	if opts.Lang != "" {
		args = append(args, "--lang", opts.Lang)
	}

	if runtime.GOOS == "darwin" {
		full := fmt.Sprintf(`"%s" %s`, exe, strings.Join(args, " "))
		return exec.Command("osascript", "-e", fmt.Sprintf(`tell application "Terminal" to do script %q`, full)).Start()
	}

	type termSpec struct {
		bin  string
		args []string
	}
	var candidates []termSpec
	if custom := strings.TrimSpace(os.Getenv("TERMINAL")); custom != "" {
		candidates = append(candidates, termSpec{bin: custom, args: []string{"-e"}})
	}
	candidates = append(candidates,
		termSpec{bin: "x-terminal-emulator", args: []string{"-e"}},
		termSpec{bin: "gnome-terminal", args: []string{"--"}},
		termSpec{bin: "konsole", args: []string{"-e"}},
		termSpec{bin: "xfce4-terminal", args: []string{"-e"}},
		termSpec{bin: "wezterm", args: []string{"start", "--"}},
		termSpec{bin: "kitty"},
		termSpec{bin: "alacritty", args: []string{"-e"}},
		termSpec{bin: "foot"},
		termSpec{bin: "tilix", args: []string{"-e"}},
		termSpec{bin: "mate-terminal", args: []string{"-e"}},
		termSpec{bin: "lxterminal", args: []string{"-e"}},
		termSpec{bin: "xterm", args: []string{"-e"}},
	)
	for _, t := range candidates {
		if _, lookErr := exec.LookPath(t.bin); lookErr != nil {
			continue
		}
		var cmdArgs []string
		cmdArgs = append(cmdArgs, t.args...)
		cmdArgs = append(cmdArgs, exe)
		cmdArgs = append(cmdArgs, args...)
		cmd := exec.Command(t.bin, cmdArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd.Start()
	}
	return errors.New("no supported terminal emulator found")
}

// RunTray runs the persistent Unix background bypass proxy daemon + IPC service
// (and optional yad desktop tray indicator when running under X11/Wayland).
func RunTray(opts LaunchOptions) error {
	lock, err := daemon.AcquireLock()
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return ErrAlreadyRunning
		}
		return err
	}
	defer lock.Release()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancelRoot := context.WithCancel(sigCtx)
	defer cancelRoot()

	svc, err := daemon.NewService(daemon.ServiceOptions{
		Version:    opts.Version,
		Lang:       opts.Lang,
		CancelRoot: cancelRoot,
	})
	if err != nil {
		return err
	}
	defer svc.Shutdown()

	if _, err := svc.StartHTTP(ctx); err != nil {
		return err
	}

	if opts.StartProxy {
		go svc.StartProxy()
	}

	// Best-effort Linux tray indicator via yad if running inside a desktop session.
	if runtime.GOOS == "linux" && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") {
		if yadPath, lookErr := exec.LookPath("yad"); lookErr == nil {
			startLinuxYadTray(ctx, yadPath, svc)
		}
	}

	<-ctx.Done()
	svc.Shutdown()
	return nil
}

func startLinuxYadTray(ctx context.Context, yadPath string, svc *daemon.Service) {
	dir, err := daemon.StateDir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0700)
	iconPath := filepath.Join(dir, "agpatcher.png")
	if err := os.WriteFile(iconPath, assets.IconPNG, 0644); err != nil {
		return
	}

	exe, _ := os.Executable()
	cmdArgs := []string{
		"--notification",
		"--listen",
		"--image=" + iconPath,
		"--text=agy-patcher Proxy",
	}
	if exe != "" {
		cmdArgs = append(cmdArgs, fmt.Sprintf(`--command=%q open-tui`, exe))
	}

	yadCmd := exec.CommandContext(ctx, yadPath, cmdArgs...)
	stdin, err := yadCmd.StdinPipe()
	if err != nil {
		return
	}
	if err := yadCmd.Start(); err != nil {
		_ = stdin.Close()
		return
	}

	updateCh := make(chan struct{}, 1)
	svc.SetOnChange(func() {
		select {
		case updateCh <- struct{}{}:
		default:
		}
	})

	writeTooltip := func() {
		snap := svc.Snapshot(0)
		var tip string
		if snap.ProxyOwned {
			tip = i18n.TFor(snap.Lang, "tray_tooltip_on", snap.ProxyAddr, snap.Stats.Active, snap.Stats.Total)
		} else if snap.ExternalProxy != "" {
			tip = i18n.TFor(snap.Lang, "tray_tooltip_ext", snap.ExternalProxy)
		} else {
			tip = i18n.TFor(snap.Lang, "tray_tooltip_off")
		}
		_, _ = io.WriteString(stdin, "tooltip:"+tip+"\n")
	}

	go func() {
		defer stdin.Close()
		writeTooltip()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		doneWait := make(chan error, 1)
		go func() {
			doneWait <- yadCmd.Wait()
		}()
		for {
			select {
			case <-ctx.Done():
				_, _ = io.WriteString(stdin, "quit\n")
				return
			case <-doneWait:
				// Never shut down the proxy daemon just because a desktop tray bar restarted.
				svc.SetOnChange(nil)
				return
			case <-updateCh:
				writeTooltip()
			case <-ticker.C:
				writeTooltip()
			}
		}
	}()
}
