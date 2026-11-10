package patcher

import (
	"bytes"
	"io"
)

const streamChunkSize = 1024 * 1024 // 1 MiB sliding read window

type byteRule struct {
	from []byte
	to   []byte
}

func maxRuleWidth(rules []byteRule) int {
	maxW := 0
	for _, r := range rules {
		if len(r.from) > maxW {
			maxW = len(r.from)
		}
	}
	return maxW
}

// streamScanNeedles scans r in 1 MiB overlapping windows and stops early once earlyStop() returns true.
func streamScanNeedles(r io.Reader, needles [][]byte, onHit func(idx int) (stop bool)) error {
	maxW := 0
	for _, nd := range needles {
		if len(nd) > maxW {
			maxW = len(nd)
		}
	}
	if maxW == 0 {
		return nil
	}
	overlapLen := maxW - 1
	buf := make([]byte, streamChunkSize)
	carry := 0

	for {
		n, err := io.ReadFull(r, buf[carry:])
		valid := carry + n
		if valid > 0 {
			window := buf[:valid]
			for idx, nd := range needles {
				if len(nd) > 0 && bytes.Contains(window, nd) {
					if onHit(idx) {
						return nil
					}
				}
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
		carry = overlapLen
		if valid < carry {
			carry = valid
		}
		copy(buf[:carry], buf[valid-carry:valid])
	}
}

// streamRewriteRules applies same-length byte substitutions over r using a bounded 1 MiB
// sliding window (never loading large binaries wholly into memory) and writes the result to w.
func streamRewriteRules(w io.Writer, r io.Reader, rules []byteRule) ([]int, error) {
	counts := make([]int, len(rules))
	maxW := maxRuleWidth(rules)
	if maxW == 0 {
		_, err := io.Copy(w, r)
		return counts, err
	}

	tailReserve := maxW - 1
	winSize := streamChunkSize
	if winSize < maxW*4 {
		winSize = maxW * 4
	}
	buf := make([]byte, winSize)
	carry := 0

	for {
		n, err := io.ReadFull(r, buf[carry:])
		eof := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !eof {
			return counts, err
		}

		valid := carry + n
		if valid == 0 {
			break
		}

		commitLimit := valid
		if !eof {
			commitLimit = valid - tailReserve
			if commitLimit < 0 {
				commitLimit = 0
			}
		}

		emitEnd := commitLimit
		pos := 0
		for pos < commitLimit {
			relIdx, ruleIdx := findEarliestMatch(buf[pos:valid], rules)
			if ruleIdx < 0 {
				break
			}
			absPos := pos + relIdx
			if absPos >= commitLimit {
				break
			}
			rule := rules[ruleIdx]
			copy(buf[absPos:absPos+len(rule.to)], rule.to)
			counts[ruleIdx]++

			endPos := absPos + len(rule.to)
			if endPos > emitEnd {
				emitEnd = endPos
			}
			pos = endPos
		}

		if emitEnd > 0 {
			if _, wErr := w.Write(buf[:emitEnd]); wErr != nil {
				return counts, wErr
			}
		}

		if eof {
			break
		}
		carry = valid - emitEnd
		if carry > 0 {
			copy(buf[:carry], buf[emitEnd:valid])
		}
	}
	return counts, nil
}

func findEarliestMatch(data []byte, rules []byteRule) (pos int, ruleIdx int) {
	bestPos := -1
	bestRule := -1
	for i, r := range rules {
		if len(r.from) == 0 || len(r.from) != len(r.to) || len(data) < len(r.from) {
			continue
		}
		idx := bytes.Index(data, r.from)
		if idx >= 0 && (bestPos < 0 || idx < bestPos) {
			bestPos = idx
			bestRule = i
		}
	}
	return bestPos, bestRule
}
