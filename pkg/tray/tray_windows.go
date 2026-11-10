//go:build windows

package tray

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"agy-patcher/pkg/assets"
	"agy-patcher/pkg/autostart"
	"agy-patcher/pkg/daemon"
	"agy-patcher/pkg/i18n"
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmCommand     = 0x0111
	wmTimer       = 0x0113
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmUser        = 0x0400
	wmTrayIcon    = wmUser + 42
	wmTrayRefresh = wmUser + 43
	wmTrayClose   = wmUser + 44

	nimAdd        = 0x00000000
	nimModify     = 0x00000001
	nimDelete     = 0x00000002
	nifMessage    = 0x00000001
	nifIcon       = 0x00000002
	nifTip        = 0x00000004
	mfString      = 0x00000000
	mfSeparator   = 0x00000800
	mfGrayed      = 0x00000001
	mfChecked     = 0x00000008
	tpmRightAlign = 0x0008
	tpmBottomAlgn = 0x0020
	tpmNonotify   = 0x0080
	tpmReturnCmd  = 0x0100

	swHide             = 0
	smCxSmIcon         = 49
	smCySmIcon         = 50
	lrDefaultColor     = 0x0000
	iconVersion        = 0x00030000
	createNewConsole   = 0x00000010
	createNoWindow     = 0x08000000
	detachedProcess    = 0x00000008
	timerRefreshID     = 1
	timerRefreshPeriod = 1000

	cmdStatus      = 1001
	cmdStats       = 1002
	cmdToggleProxy = 1003
	cmdOpenTUI     = 1004
	cmdToggleAuto  = 1005
	cmdExit        = 1006
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	user32                = syscall.NewLazyDLL("user32.dll")
	shell32               = syscall.NewLazyDLL("shell32.dll")
	procGetModuleHandleW  = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow  = kernel32.NewProc("GetConsoleWindow")
	procFreeConsole       = kernel32.NewProc("FreeConsole")
	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenuW       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procSetTimer          = user32.NewProc("SetTimer")
	procKillTimer         = user32.NewProc("KillTimer")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procCreateIconFromRes = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon       = user32.NewProc("DestroyIcon")
	procShowWindow        = user32.NewProc("ShowWindow")
	procShellNotifyIconW  = shell32.NewProc("Shell_NotifyIconW")
)

type point struct {
	X int32
	Y int32
}

type winMsg struct {
	Hwnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             syscall.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            syscall.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         winGUID
	hBalloonIcon     syscall.Handle
}

type trayApp struct {
	svc        *daemon.Service
	cancelRoot context.CancelFunc
	hwnd       syscall.Handle
	iconOn     syscall.Handle
	iconOff    syscall.Handle
	nidAdded   bool
}

var (
	activeMu   sync.Mutex
	activeTray *trayApp
)

// HideAndRelaunchDetached detaches from any flashing console window on Windows autostart/--tray,
// ensures the headless daemon/tray worker is running, and waits briefly for its IPC endpoint.
func HideAndRelaunchDetached(opts LaunchOptions) error {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swHide)
	}
	_, _, _ = procFreeConsole.Call()
	_, _, err := EnsureDaemon(opts)
	return err
}

// SpawnTrayBackground starts a detached console-free daemon/tray worker using the current executable.
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow | detachedProcess,
		HideWindow:    true,
	}
	return cmd.Start()
}

// SpawnTUIWindow opens a visible console window running the interactive TUI attached to this daemon.
func SpawnTUIWindow(opts LaunchOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"tui"}
	if opts.Lang != "" {
		args = append(args, "--lang", opts.Lang)
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewConsole,
	}
	return cmd.Start()
}

// RunTray runs the persistent Win32 tray UI + daemon IPC server + optional bypass proxy.
func RunTray(opts LaunchOptions) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	lock, err := daemon.AcquireLock()
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return ErrAlreadyRunning
		}
		return err
	}
	defer lock.Release()

	ctx, cancelRoot := context.WithCancel(context.Background())
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

	app := &trayApp{
		svc:        svc,
		cancelRoot: cancelRoot,
	}

	activeMu.Lock()
	activeTray = app
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		if activeTray == app {
			activeTray = nil
		}
		activeMu.Unlock()
	}()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	cx := int32(16)
	cy := int32(16)
	if r, _, _ := procGetSystemMetrics.Call(smCxSmIcon); r > 0 {
		cx = int32(r)
	}
	if r, _, _ := procGetSystemMetrics.Call(smCySmIcon); r > 0 {
		cy = int32(r)
	}
	app.iconOn = loadIconFromICO(assets.TrayOnICO, cx, cy)
	app.iconOff = loadIconFromICO(assets.TrayOffICO, cx, cy)
	defer func() {
		if app.iconOn != 0 {
			_, _, _ = procDestroyIcon.Call(uintptr(app.iconOn))
		}
		if app.iconOff != 0 {
			_, _, _ = procDestroyIcon.Call(uintptr(app.iconOff))
		}
	}()

	className, err := syscall.UTF16PtrFromString("AGPatcherTrayWindow")
	if err != nil {
		return err
	}
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     syscall.Handle(hInst),
		hIcon:         app.iconOn,
		lpszClassName: className,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	titlePtr, _ := syscall.UTF16PtrFromString("agy-patcher Tray")
	hwnd, _, createErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(titlePtr)),
		0, 0, 0, 0, 0,
		0, 0,
		hInst,
		0,
	)
	if hwnd == 0 {
		return createErr
	}
	app.hwnd = syscall.Handle(hwnd)

	svc.SetOnChange(func() {
		if app.hwnd != 0 {
			_, _, _ = procPostMessageW.Call(uintptr(app.hwnd), wmTrayRefresh, 0, 0)
		}
	})

	if _, err := svc.StartHTTP(ctx); err != nil {
		_, _, _ = procDestroyWindow.Call(uintptr(app.hwnd))
		return err
	}

	app.addOrUpdateIcon(nimAdd)
	_, _, _ = procSetTimer.Call(uintptr(app.hwnd), timerRefreshID, timerRefreshPeriod, 0)

	go func() {
		<-ctx.Done()
		if app.hwnd != 0 {
			_, _, _ = procPostMessageW.Call(uintptr(app.hwnd), wmTrayClose, 0, 0)
		}
	}()

	if opts.StartProxy {
		go svc.StartProxy()
	}

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	_, _, _ = procKillTimer.Call(uintptr(app.hwnd), timerRefreshID)
	svc.SetOnChange(nil)
	app.deleteIcon()
	svc.Shutdown()
	return nil
}

func (a *trayApp) t(key string, args ...any) string {
	return i18n.TFor(a.svc.Lang(), key, args...)
}

func (a *trayApp) currentIcon(snap daemon.StateSnapshot) syscall.Handle {
	if snap.ProxyOwned && a.iconOn != 0 {
		return a.iconOn
	}
	if !snap.ProxyOwned && a.iconOff != 0 {
		return a.iconOff
	}
	return a.iconOn
}

func (a *trayApp) tooltipText(snap daemon.StateSnapshot) string {
	if snap.ProxyOwned {
		return a.t("tray_tooltip_on", snap.ProxyAddr, snap.Stats.Active, snap.Stats.Total)
	}
	if snap.ProxyNote != "" {
		return "agy-patcher — " + snap.ProxyNote
	}
	if snap.ExternalProxy != "" {
		return a.t("tray_tooltip_ext", snap.ExternalProxy)
	}
	return a.t("tray_tooltip_off")
}

func (a *trayApp) addOrUpdateIcon(msgType uint32) {
	if a.hwnd == 0 {
		return
	}
	if msgType == nimModify && !a.nidAdded {
		return
	}
	snap := a.svc.Snapshot(0)
	nid := notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             a.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayIcon,
		hIcon:            a.currentIcon(snap),
	}
	tipRunes := syscall.StringToUTF16(a.tooltipText(snap))
	if len(tipRunes) > len(nid.szTip) {
		tipRunes = tipRunes[:len(nid.szTip)-1]
		tipRunes = append(tipRunes, 0)
	}
	copy(nid.szTip[:], tipRunes)

	r, _, _ := procShellNotifyIconW.Call(uintptr(msgType), uintptr(unsafe.Pointer(&nid)))
	if r != 0 && msgType == nimAdd {
		a.nidAdded = true
	}
}

func (a *trayApp) deleteIcon() {
	if !a.nidAdded || a.hwnd == 0 {
		return
	}
	nid := notifyIconDataW{
		cbSize: uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:   a.hwnd,
		uID:    1,
	}
	_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	a.nidAdded = false
}

func (a *trayApp) openTUIWindow() {
	_ = SpawnTUIWindow(LaunchOptions{Lang: a.svc.Lang()})
}

func (a *trayApp) showContextMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	snap := a.svc.Snapshot(0)
	statusLabel := a.t("tray_menu_status_off")
	if snap.ProxyOwned {
		statusLabel = a.t("tray_menu_status_on", snap.ProxyAddr)
	} else if snap.ProxyNote != "" {
		statusLabel = "● " + snap.ProxyNote
	} else if snap.ExternalProxy != "" {
		statusLabel = a.t("tray_menu_status_ext", snap.ExternalProxy)
	}
	appendMenuText(syscall.Handle(hMenu), mfString|mfGrayed, cmdStatus, statusLabel)

	if snap.ProxyOwned {
		statsLabel := a.t("tray_menu_stats", snap.Stats.Active, snap.Stats.Total, snap.Stats.Gate)
		appendMenuText(syscall.Handle(hMenu), mfString|mfGrayed, cmdStats, statsLabel)
	}
	_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)

	toggleLabel := a.t("tray_menu_start_proxy")
	if snap.ProxyOwned {
		toggleLabel = a.t("tray_menu_stop_proxy")
	}
	appendMenuText(syscall.Handle(hMenu), mfString, cmdToggleProxy, toggleLabel)
	appendMenuText(syscall.Handle(hMenu), mfString, cmdOpenTUI, a.t("tray_menu_open_tui"))

	autoOn, _ := autostart.IsEnabled()
	autoFlags := uintptr(mfString)
	if autoOn {
		autoFlags |= mfChecked
	}
	appendMenuText(syscall.Handle(hMenu), autoFlags, cmdToggleAuto, a.t("tray_menu_autostart_win"))

	_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	appendMenuText(syscall.Handle(hMenu), mfString, cmdExit, a.t("tray_menu_exit"))

	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWin.Call(uintptr(a.hwnd))

	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightAlign|tpmBottomAlgn|tpmReturnCmd|tpmNonotify,
		uintptr(pt.X),
		uintptr(pt.Y),
		0,
		uintptr(a.hwnd),
		0,
	)
	_, _, _ = procPostMessageW.Call(uintptr(a.hwnd), wmNull, 0, 0)
	if cmd != 0 {
		a.handleCommand(uint32(cmd))
	}
}

func (a *trayApp) handleCommand(id uint32) {
	switch id {
	case cmdToggleProxy:
		snap := a.svc.Snapshot(0)
		if snap.ProxyOwned {
			go a.svc.StopProxy(false)
		} else {
			go a.svc.StartProxy()
		}
	case cmdOpenTUI:
		a.openTUIWindow()
	case cmdToggleAuto:
		if enabled, _ := autostart.IsEnabled(); enabled {
			_ = autostart.Disable()
		} else {
			_, _ = autostart.Enable()
		}
		a.svc.RefreshAutostart()
	case cmdExit:
		a.cancelRoot()
		_, _, _ = procDestroyWindow.Call(uintptr(a.hwnd))
		_, _, _ = procPostQuitMessage.Call(0)
	}
}

func appendMenuText(hMenu syscall.Handle, flags uintptr, id uint32, text string) {
	ptr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	_, _, _ = procAppendMenuW.Call(uintptr(hMenu), flags, uintptr(id), uintptr(unsafe.Pointer(ptr)))
}

func trayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	activeMu.Lock()
	app := activeTray
	activeMu.Unlock()

	if app != nil {
		switch uint32(msg) {
		case wmTimer, wmTrayRefresh:
			app.addOrUpdateIcon(nimModify)
			return 0
		case wmTrayClose:
			_, _, _ = procDestroyWindow.Call(uintptr(app.hwnd))
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		case wmTrayIcon:
			switch uint32(lParam) {
			case wmRButtonUp, wmLButtonUp:
				app.showContextMenu()
				return 0
			case wmLButtonDbl:
				app.openTUIWindow()
				return 0
			}
		case wmCommand:
			app.handleCommand(uint32(wParam & 0xFFFF))
			return 0
		case wmDestroy:
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func loadIconFromICO(icoBytes []byte, cx, cy int32) syscall.Handle {
	if len(icoBytes) < 6 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(icoBytes[4:6]))
	if count <= 0 || len(icoBytes) < 6+count*16 {
		return 0
	}

	bestIdx := 0
	bestDiff := int32(1 << 29)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		w := int32(icoBytes[off])
		if w == 0 {
			w = 256
		}
		diff := w - cx
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			bestIdx = i
		}
	}

	entryOff := 6 + bestIdx*16
	resSize := binary.LittleEndian.Uint32(icoBytes[entryOff+8 : entryOff+12])
	resOffset := binary.LittleEndian.Uint32(icoBytes[entryOff+12 : entryOff+16])
	if int(resOffset)+int(resSize) > len(icoBytes) || resSize == 0 {
		return 0
	}

	frame := icoBytes[resOffset : resOffset+resSize]
	h, _, _ := procCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&frame[0])),
		uintptr(len(frame)),
		1,
		iconVersion,
		uintptr(cx),
		uintptr(cy),
		lrDefaultColor,
	)
	return syscall.Handle(h)
}
