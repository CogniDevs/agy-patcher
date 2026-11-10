//go:build !windows

package tui

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// EnsureUTF8Console is a no-op on Unix terminals.
func EnsureUTF8Console() {}

type terminalSession struct {
	savedState string
}

func openTerminal() (*terminalSession, error) {
	s := &terminalSession{}
	if out, err := runStty("-g"); err == nil {
		s.savedState = strings.TrimSpace(out)
	}
	if _, err := runStty("raw", "-echo"); err != nil {
		return nil, err
	}
	_, _ = os.Stdout.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[2J\x1b[H")
	return s, nil
}

func (s *terminalSession) Close() {
	_, _ = os.Stdout.WriteString("\x1b[?7h\x1b[?25h\x1b[0m\x1b[?1049l")
	if s.savedState != "" {
		if _, err := runStty(s.savedState); err == nil {
			return
		}
	}
	_, _ = runStty("sane")
}

func (s *terminalSession) Size() (int, int) {
	if out, err := runStty("size"); err == nil {
		fields := strings.Fields(out)
		if len(fields) == 2 {
			rows, errR := strconv.Atoi(fields[0])
			cols, errC := strconv.Atoi(fields[1])
			if errR == nil && errC == nil && cols > 10 && rows > 5 {
				return cols, rows
			}
		}
	}
	return 100, 30
}

func watchResize(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}

func runStty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return string(out), err
}
