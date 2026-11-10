package tui

import (
	"context"
	"os"
	"time"
	"unicode/utf8"
)

type KeyKind int

const (
	KeyNone KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyTab
	KeyCtrlC
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDown
	KeyDelete
	KeyRune
)

type KeyEvent struct {
	Kind KeyKind
	Rune rune
}

// IsInteractive reports whether both stdin and stdout are attached to a terminal device.
func IsInteractive() bool {
	inInfo, err := os.Stdin.Stat()
	if err != nil || (inInfo.Mode()&os.ModeCharDevice) == 0 {
		return false
	}
	outInfo, err := os.Stdout.Stat()
	if err != nil || (outInfo.Mode()&os.ModeCharDevice) == 0 {
		return false
	}
	return true
}

func startKeyReader(ctx context.Context) <-chan KeyEvent {
	out := make(chan KeyEvent, 32)
	rawBytes := make(chan byte, 64)

	go func() {
		var buf [64]byte
		for {
			n, err := os.Stdin.Read(buf[:])
			if n > 0 {
				for i := 0; i < n; i++ {
					select {
					case rawBytes <- buf[i]:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				close(rawBytes)
				return
			}
		}
	}()

	go func() {
		defer close(out)
		var pending []byte
		escTimer := time.NewTimer(time.Hour)
		escTimer.Stop()
		defer escTimer.Stop()

		emit := func(ev KeyEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		flushEsc := func() bool {
			wasSingle := len(pending) == 1 && pending[0] == 0x1b
			pending = pending[:0]
			if wasSingle {
				return emit(KeyEvent{Kind: KeyEsc})
			}
			return true
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-escTimer.C:
				if len(pending) > 0 && pending[0] == 0x1b {
					if !flushEsc() {
						return
					}
				}
			case b, ok := <-rawBytes:
				if !ok {
					if len(pending) > 0 && pending[0] == 0x1b {
						_ = flushEsc()
					}
					return
				}

				pending = append(pending, b)

				// Escape sequence state machine
				if pending[0] == 0x1b {
					if len(pending) == 1 {
						escTimer.Reset(45 * time.Millisecond)
						continue
					}
					escTimer.Stop()
					if ev, consumed := parseEscapeSeq(pending); consumed > 0 {
						pending = pending[consumed:]
						if ev.Kind != KeyNone {
							if !emit(ev) {
								return
							}
						}
						continue
					}
					if len(pending) >= 64 {
						pending = pending[:0]
					} else {
						escTimer.Reset(45 * time.Millisecond)
					}
					continue
				}

				// Control characters
				if len(pending) == 1 && (b < 0x20 || b == 0x7f) {
					pending = pending[:0]
					switch b {
					case 0x03:
						if !emit(KeyEvent{Kind: KeyCtrlC}) {
							return
						}
					case '\r', '\n':
						if !emit(KeyEvent{Kind: KeyEnter}) {
							return
						}
					case '\t':
						if !emit(KeyEvent{Kind: KeyTab}) {
							return
						}
					case 0x7f, '\b':
						if !emit(KeyEvent{Kind: KeyBackspace}) {
							return
						}
					}
					continue
				}

				// UTF-8 rune decoding
				if utf8.FullRune(pending) {
					r, size := utf8.DecodeRune(pending)
					pending = pending[size:]
					if r != utf8.RuneError {
						if !emit(KeyEvent{Kind: KeyRune, Rune: r}) {
							return
						}
					}
				} else if len(pending) >= utf8.UTFMax {
					pending = pending[:0]
				}
			}
		}
	}()

	return out
}

// parseEscapeSeq tries to parse an ANSI escape sequence at the start of buf.
// Returns (event, bytesConsumed). bytesConsumed == 0 means incomplete sequence.
func parseEscapeSeq(buf []byte) (KeyEvent, int) {
	if len(buf) < 2 || buf[0] != 0x1b {
		return KeyEvent{}, 0
	}

	switch buf[1] {
	case '[':
		if len(buf) < 3 {
			return KeyEvent{}, 0
		}
		// X10/VT200 mouse report: ESC [ M cb cx cy (always 6 bytes)
		if buf[2] == 'M' {
			if len(buf) < 6 {
				return KeyEvent{}, 0
			}
			return KeyEvent{Kind: KeyNone}, 6
		}
		// Final byte of CSI sequence is in 0x40..0x7E
		for i := 2; i < len(buf); i++ {
			c := buf[i]
			if c >= 0x40 && c <= 0x7E {
				seq := string(buf[2 : i+1])
				return mapCSI(seq), i + 1
			}
		}
		return KeyEvent{}, 0
	case 'O':
		if len(buf) < 3 {
			return KeyEvent{}, 0
		}
		switch buf[2] {
		case 'A':
			return KeyEvent{Kind: KeyUp}, 3
		case 'B':
			return KeyEvent{Kind: KeyDown}, 3
		case 'C':
			return KeyEvent{Kind: KeyRight}, 3
		case 'D':
			return KeyEvent{Kind: KeyLeft}, 3
		case 'H':
			return KeyEvent{Kind: KeyHome}, 3
		case 'F':
			return KeyEvent{Kind: KeyEnd}, 3
		default:
			return KeyEvent{Kind: KeyNone}, 3
		}
	default:
		// Alt+key or unrecognized 2-byte sequence
		return KeyEvent{Kind: KeyEsc}, 1
	}
}

func mapCSI(seq string) KeyEvent {
	switch seq {
	case "A":
		return KeyEvent{Kind: KeyUp}
	case "B":
		return KeyEvent{Kind: KeyDown}
	case "C":
		return KeyEvent{Kind: KeyRight}
	case "D":
		return KeyEvent{Kind: KeyLeft}
	case "H", "1~", "7~":
		return KeyEvent{Kind: KeyHome}
	case "F", "4~", "8~":
		return KeyEvent{Kind: KeyEnd}
	case "5~":
		return KeyEvent{Kind: KeyPgUp}
	case "6~":
		return KeyEvent{Kind: KeyPgDown}
	case "3~":
		return KeyEvent{Kind: KeyDelete}
	default:
		return KeyEvent{Kind: KeyNone}
	}
}
