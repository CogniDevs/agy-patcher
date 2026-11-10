//go:build windows

package tui

import (
	"context"
	"os"
	"syscall"
	"unsafe"
)

const (
	enableProcessedInput        = 0x0001
	enableLineInput             = 0x0002
	enableEchoInput             = 0x0004
	enableWindowInput           = 0x0008
	enableMouseInput            = 0x0010
	enableQuickEditMode         = 0x0040
	enableExtendedFlags         = 0x0080
	enableVirtualTerminalInput  = 0x0200
	enableVirtualTerminalOutput = 0x0004
	disableNewlineAutoReturn    = 0x0008
	cpUTF8                      = 65001
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenBufInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleOutputCP      = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP            = kernel32.NewProc("SetConsoleCP")
	procGetConsoleWindow        = kernel32.NewProc("GetConsoleWindow")
	procAllocConsole            = kernel32.NewProc("AllocConsole")
	procSetStdHandle            = kernel32.NewProc("SetStdHandle")
)

type coord struct {
	X int16
	Y int16
}

type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// EnsureUTF8Console switches the Windows console codepage to UTF-8 so Cyrillic and box glyphs render cleanly.
func EnsureUTF8Console() {
	_, _, _ = procSetConsoleOutputCP.Call(uintptr(cpUTF8))
	_, _, _ = procSetConsoleCP.Call(uintptr(cpUTF8))
}

type terminalSession struct {
	inHandle  syscall.Handle
	outHandle syscall.Handle
	origIn    uint32
	origOut   uint32
	hasIn     bool
	hasOut    bool
}

func openConsoleDevice(name string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	return syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
}

// ensureConsoleHandles rebinds os.Stdin/os.Stdout to CONIN$/CONOUT$ when launched
// from a detached/tray process where inherited standard handles point to NUL.
func ensureConsoleHandles() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd == 0 {
		_, _, _ = procAllocConsole.Call()
	}

	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(os.Stdin.Fd()), &mode); err != nil {
		if hIn, openErr := openConsoleDevice("CONIN$"); openErr == nil && hIn != syscall.InvalidHandle {
			const stdInput = uint32(0xFFFFFFF6) // STD_INPUT_HANDLE (-10)
			_, _, _ = procSetStdHandle.Call(uintptr(stdInput), uintptr(hIn))
			os.Stdin = os.NewFile(uintptr(hIn), "CONIN$")
		}
	}

	if err := syscall.GetConsoleMode(syscall.Handle(os.Stdout.Fd()), &mode); err != nil {
		if hOut, openErr := openConsoleDevice("CONOUT$"); openErr == nil && hOut != syscall.InvalidHandle {
			const (
				stdOutput = uint32(0xFFFFFFF5) // STD_OUTPUT_HANDLE (-11)
				stdError  = uint32(0xFFFFFFF4) // STD_ERROR_HANDLE (-12)
			)
			_, _, _ = procSetStdHandle.Call(uintptr(stdOutput), uintptr(hOut))
			_, _, _ = procSetStdHandle.Call(uintptr(stdError), uintptr(hOut))
			f := os.NewFile(uintptr(hOut), "CONOUT$")
			os.Stdout = f
			os.Stderr = f
		}
	}
}

func openTerminal() (*terminalSession, error) {
	ensureConsoleHandles()
	EnsureUTF8Console()
	s := &terminalSession{
		inHandle:  syscall.Handle(os.Stdin.Fd()),
		outHandle: syscall.Handle(os.Stdout.Fd()),
	}

	var inMode uint32
	if err := syscall.GetConsoleMode(s.inHandle, &inMode); err == nil {
		s.origIn = inMode
		s.hasIn = true
		rawIn := (inMode &^ (enableLineInput | enableEchoInput | enableProcessedInput | enableWindowInput | enableMouseInput | enableQuickEditMode)) | enableExtendedFlags | enableVirtualTerminalInput
		_ = setConsoleMode(s.inHandle, rawIn)
	}

	var outMode uint32
	if err := syscall.GetConsoleMode(s.outHandle, &outMode); err == nil {
		s.origOut = outMode
		s.hasOut = true
		rawOut := outMode | enableVirtualTerminalOutput | disableNewlineAutoReturn
		_ = setConsoleMode(s.outHandle, rawOut)
	}

	_, _ = os.Stdout.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[2J\x1b[H")
	return s, nil
}

func (s *terminalSession) Close() {
	_, _ = os.Stdout.WriteString("\x1b[?7h\x1b[?25h\x1b[0m\x1b[?1049l")
	if s.hasIn {
		_ = setConsoleMode(s.inHandle, s.origIn)
	}
	if s.hasOut {
		_ = setConsoleMode(s.outHandle, s.origOut)
	}
}

func (s *terminalSession) Size() (int, int) {
	var info consoleScreenBufferInfo
	r1, _, _ := procGetConsoleScreenBufInfo.Call(uintptr(s.outHandle), uintptr(unsafe.Pointer(&info)))
	if r1 != 0 {
		w := int(info.Window.Right-info.Window.Left) + 1
		h := int(info.Window.Bottom-info.Window.Top) + 1
		if w > 10 && h > 5 {
			return w, h
		}
	}
	return 100, 30
}

func watchResize(ctx context.Context) <-chan struct{} {
	return nil
}

var procSetConsoleMode = kernel32.NewProc("SetConsoleMode")

func setConsoleMode(h syscall.Handle, mode uint32) error {
	r1, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r1 == 0 {
		return err
	}
	return nil
}
