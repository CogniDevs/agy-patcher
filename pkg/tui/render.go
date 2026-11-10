package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"agy-patcher/pkg/env"
	"agy-patcher/pkg/proxy"
)

const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"

	fgPrimary   = "\x1b[38;2;205;214;244m"
	fgMuted     = "\x1b[38;2;127;132;156m"
	fgAccent    = "\x1b[38;2;137;180;250m"
	fgCyan      = "\x1b[38;2;125;207;255m"
	fgGreen     = "\x1b[38;2;166;227;161m"
	fgYellow    = "\x1b[38;2;249;226;175m"
	fgRed       = "\x1b[38;2;243;139;168m"
	fgBorder    = "\x1b[38;2;69;71;90m"
	fgBorderAct = "\x1b[38;2;137;180;250m"

	bgSelect = "\x1b[48;2;42;46;63m"
	bgBadge  = "\x1b[48;2;30;32;48m"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// visibleLen counts visible display columns, ignoring CSI ANSI escape sequences.
func visibleLen(s string) int {
	n := 0
	inEsc := false
	for i := 0; i < len(s); {
		b := s[i]
		if inEsc {
			if b >= 0x40 && b <= 0x7E && i > 0 {
				inEsc = false
			}
			i++
			continue
		}
		if b == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i += 2
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// truncateANSI clips s to at most maxCols visible cells without cutting ANSI codes mid-sequence.
func truncateANSI(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if visibleLen(s) <= maxCols {
		return s
	}

	ellipsis := "…"
	limit := maxCols - 1
	if maxCols == 1 {
		ellipsis = ""
		limit = 1
	}

	var out strings.Builder
	cols := 0
	inEsc := false
	hasANSI := false

	for i := 0; i < len(s); {
		b := s[i]
		if inEsc {
			out.WriteByte(b)
			if b >= 0x40 && b <= 0x7E {
				inEsc = false
			}
			i++
			continue
		}
		if b == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			hasANSI = true
			out.WriteString("\x1b[")
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if cols >= limit {
			break
		}
		out.WriteRune(r)
		cols++
		i += size
	}

	out.WriteString(ellipsis)
	if hasANSI {
		out.WriteString(ansiReset)
	}
	return out.String()
}

func padRightANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := truncateANSI(s, width)
	v := visibleLen(clipped)
	if v >= width {
		return clipped
	}
	return clipped + strings.Repeat(" ", width-v)
}

func truncateMiddle(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxCols {
		return s
	}
	if maxCols <= 3 {
		return string(runes[:maxCols])
	}
	keepRight := (maxCols - 1) * 2 / 3
	if keepRight < 1 {
		keepRight = 1
	}
	keepLeft := maxCols - 1 - keepRight
	if keepLeft < 1 {
		keepLeft = 1
		keepRight = maxCols - 2
	}
	return string(runes[:keepLeft]) + "…" + string(runes[len(runes)-keepRight:])
}

func wrapPlain(s string, maxCols int) []string {
	if maxCols <= 1 {
		if s == "" {
			return []string{""}
		}
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) <= maxCols {
		return []string{s}
	}
	var out []string
	for len(runes) > maxCols {
		split := -1
		for i := maxCols; i >= maxCols/2; i-- {
			if runes[i-1] == ' ' {
				split = i - 1
				break
			}
		}
		if split <= 0 {
			out = append(out, string(runes[:maxCols]))
			runes = runes[maxCols:]
			continue
		}
		out = append(out, strings.TrimRight(string(runes[:split]), " "))
		runes = []rune(strings.TrimLeft(string(runes[split+1:]), " "))
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func drawBox(title string, body []string, width, height int, active bool) []string {
	if width < 6 {
		width = 6
	}
	if height < 3 {
		height = 3
	}

	bCol := fgBorder
	tCol := fgMuted
	if active {
		bCol = fgBorderAct
		tCol = fgAccent
	}

	innerW := width - 2
	var lines []string

	// Top border with embedded title
	titleLabel := ""
	if title != "" {
		titleLabel = " " + title + " "
		if visibleLen(titleLabel) > innerW-2 {
			titleLabel = truncateANSI(titleLabel, innerW-2)
		}
	}
	titleCols := visibleLen(titleLabel)
	leftDash := 1
	if innerW < 3 {
		leftDash = 0
	}
	rightDash := innerW - leftDash - titleCols
	if rightDash < 0 {
		rightDash = 0
	}

	top := bCol + "╭" + strings.Repeat("─", leftDash) + ansiReset +
		ansiBold + tCol + titleLabel + ansiReset +
		bCol + strings.Repeat("─", rightDash) + "╮" + ansiReset
	lines = append(lines, top)

	// Content rows
	contentRows := height - 2
	contentW := innerW - 2
	if contentW < 1 {
		contentW = 1
	}
	for i := 0; i < contentRows; i++ {
		raw := ""
		if i < len(body) {
			raw = body[i]
		}
		row := bCol + "│" + ansiReset + " " + padRightANSI(raw, contentW) + " " + bCol + "│" + ansiReset
		lines = append(lines, row)
	}

	// Bottom border
	bottom := bCol + "╰" + strings.Repeat("─", innerW) + "╯" + ansiReset
	lines = append(lines, bottom)
	return lines
}

func mergeColumns(left, right []string, leftW, rightW int) []string {
	rows := len(left)
	if len(right) > rows {
		rows = len(right)
	}
	out := make([]string, rows)
	emptyL := strings.Repeat(" ", leftW)
	emptyR := strings.Repeat(" ", rightW)
	for i := 0; i < rows; i++ {
		l := emptyL
		if i < len(left) {
			l = left[i]
		}
		r := emptyR
		if i < len(right) {
			r = right[i]
		}
		out[i] = l + " " + r
	}
	return out
}

func formatLogTag(level logLevel) string {
	switch level {
	case logOK:
		return fgGreen + "[OK]   " + ansiReset
	case logWarn:
		return fgYellow + "[WARN] " + ansiReset
	case logErr:
		return fgRed + "[ERR]  " + ansiReset
	case logProxy:
		return fgCyan + "[PROXY]" + ansiReset
	case logDNS:
		return fgAccent + "[DNS]  " + ansiReset
	default:
		return fgMuted + "[INFO] " + ansiReset
	}
}

func renderFrame(m *model, w, h int) string {
	if w < 50 {
		w = 50
	}
	if h < 18 {
		h = 18
	}

	var b strings.Builder
	row := 1
	writeRow := func(s string) {
		fmt.Fprintf(&b, "\x1b[%d;1H", row)
		b.WriteString(s)
		b.WriteString("\x1b[K")
		row++
	}

	// Header bar (2 lines)
	writeRow(renderHeader(m, w))
	writeRow(fgBorder + strings.Repeat("─", w) + ansiReset)

	footerH := 2
	if m.inputMode {
		footerH = 4
	}
	bodyH := h - 2 - footerH
	if bodyH < 10 {
		bodyH = 10
	}

	var bodyLines []string
	detailTitle, headerLines, detailList := m.detailItems()

	if w >= 94 {
		leftW := w * 44 / 100
		if leftW < 42 {
			leftW = 42
		}
		if leftW > 68 {
			leftW = 68
		}
		rightW := w - leftW - 1

		menuH := 8
		detailH := bodyH - menuH
		if detailH < 8 {
			detailH = 8
			menuH = bodyH - detailH
			if menuH < 5 {
				menuH = 5
			}
		}

		leftTop := drawBox(m.t("tui_box_menu"), m.renderSections(leftW-4, menuH-2), leftW, menuH, m.focus == focusSections && !m.inputMode)
		leftBot := drawBox(detailTitle, m.renderDetail(headerLines, detailList, leftW-4, detailH-2), leftW, detailH, m.focus == focusDetail && !m.inputMode)
		leftCol := append(leftTop, leftBot...)

		telemetryH := 6
		logsH := bodyH - telemetryH
		if logsH < 6 {
			logsH = 6
			telemetryH = bodyH - logsH
		}
		rightTop := drawBox(m.t("tui_box_telemetry"), m.renderTelemetry(rightW-4), rightW, telemetryH, false)
		rightBot := drawBox(m.t("tui_box_logs_wide"), m.renderLogs(rightW-4, logsH-2), rightW, logsH, m.focus == focusLogs && !m.inputMode)
		rightCol := append(rightTop, rightBot...)

		bodyLines = mergeColumns(leftCol, rightCol, leftW, rightW)
	} else {
		menuH := 8
		detailH := 7
		if bodyH < menuH+detailH+4 {
			menuH = 5
			detailH = 6
		}
		logsH := bodyH - menuH - detailH
		if logsH < 4 {
			logsH = 4
		}
		bodyLines = append(bodyLines, drawBox(m.t("tui_box_menu"), m.renderSections(w-4, menuH-2), w, menuH, m.focus == focusSections && !m.inputMode)...)
		bodyLines = append(bodyLines, drawBox(detailTitle, m.renderDetail(headerLines, detailList, w-4, detailH-2), w, detailH, m.focus == focusDetail && !m.inputMode)...)
		bodyLines = append(bodyLines, drawBox(m.t("tui_box_logs_narrow"), m.renderLogs(w-4, logsH-2), w, logsH, m.focus == focusLogs && !m.inputMode)...)
	}

	for i := 0; i < bodyH; i++ {
		line := ""
		if i < len(bodyLines) {
			line = bodyLines[i]
		}
		writeRow(padRightANSI(line, w))
	}

	if m.inputMode {
		promptTitle, hintText := m.inputPromptMeta()
		promptLine := fmt.Sprintf("%s❯ %s%s%s█%s", fgAccent+ansiBold, ansiReset+fgPrimary, m.inputBuf, fgAccent, ansiReset)
		hintLine := fgMuted + hintText + ansiReset
		promptBox := drawBox(promptTitle, []string{promptLine, hintLine}, w, 4, true)
		for _, pl := range promptBox {
			writeRow(padRightANSI(pl, w))
		}
	} else {
		writeRow(fgBorder + strings.Repeat("─", w) + ansiReset)
		footer := m.renderFooter()
		writeRow(padRightANSI(footer, w))
	}

	b.WriteString("\x1b[J")
	return b.String()
}

func renderHeader(m *model, w int) string {
	left := fmt.Sprintf(" %s◆%s %sagpatcher%s %sv%s%s", fgAccent+ansiBold, ansiReset, ansiBold+fgPrimary, ansiReset, fgMuted, m.version, ansiReset)
	if m.busy != "" {
		spin := spinnerFrames[m.spinIdx%len(spinnerFrames)]
		left += fmt.Sprintf("  %s%s %s%s", fgYellow, spin, m.busy, ansiReset)
	}

	var proxyBadge string
	if m.proxyOwned {
		proxyBadge = fmt.Sprintf("%s%s ● PROXY %s %s", bgBadge, fgGreen+ansiBold, m.proxyAddr, ansiReset)
	} else if m.externalProxy != "" {
		proxyBadge = fmt.Sprintf("%s%s ● EXT %s %s", bgBadge, fgCyan+ansiBold, m.externalProxy, ansiReset)
	} else {
		proxyBadge = fmt.Sprintf("%s%s ○ PROXY OFF %s", bgBadge, fgMuted, ansiReset)
	}

	langBadge := fmt.Sprintf("%s%s [%s] %s", bgBadge, fgAccent+ansiBold, strings.ToUpper(m.lang), ansiReset)
	right := proxyBadge + " " + langBadge + " "

	lCols := visibleLen(left)
	rCols := visibleLen(right)
	gap := w - lCols - rCols
	if gap < 1 {
		return padRightANSI(left+" "+right, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) renderSections(maxW, maxRows int) []string {
	items := m.sectionItems()
	lines := make([]string, 0, len(items))
	activeIdx := int(m.sectionIdx)
	for i, it := range items {
		isActive := i == activeIdx
		isFocused := isActive && m.focus == focusSections
		lines = append(lines, renderMenuRow(it, isActive, isFocused, maxW))
	}
	if maxRows > 0 && len(lines) > maxRows {
		start := 0
		if activeIdx >= maxRows {
			start = activeIdx - maxRows + 1
		}
		if start+maxRows > len(lines) {
			start = len(lines) - maxRows
		}
		if start < 0 {
			start = 0
		}
		lines = lines[start : start+maxRows]
	}
	return lines
}

func renderMenuRow(it actionItem, active, focused bool, maxW int) string {
	badge := ""
	if it.badge != "" {
		if focused {
			badge = " " + strings.ReplaceAll(it.badge, ansiReset, "")
		} else {
			badge = " " + it.badge
		}
	}
	keySuffix := ""
	if it.key != "" {
		if focused {
			keySuffix = fmt.Sprintf("  %s[%s]", fgCyan, it.key)
		} else {
			keySuffix = fmt.Sprintf("  %s[%s]%s", fgMuted, it.key, ansiReset)
		}
	}

	if focused {
		raw := fmt.Sprintf("%s%s▸ %s%s%s%s",
			bgSelect, fgAccent+ansiBold, ansiBold+fgPrimary, it.label, badge, keySuffix)
		vis := visibleLen(raw)
		if maxW > 0 {
			if vis > maxW {
				raw = truncateANSI(raw, maxW)
				raw = strings.TrimSuffix(raw, ansiReset)
			} else if vis < maxW {
				raw += strings.Repeat(" ", maxW-vis)
			}
		}
		return raw + ansiReset
	}

	prefix := "  "
	labelCol := fgPrimary
	if active {
		prefix = fgAccent + ansiBold + "▸ " + ansiReset
		labelCol = ansiBold + fgPrimary
	}
	return fmt.Sprintf("%s%s%s%s%s%s",
		prefix, labelCol, it.label, ansiReset, badge, keySuffix)
}

func (m *model) renderDetail(headerLines []string, items []detailItem, maxW, maxRows int) []string {
	var lines []string
	for _, hl := range headerLines {
		lines = append(lines, truncateANSI(hl, maxW))
	}
	if len(headerLines) > 0 && len(items) > 0 {
		lines = append(lines, fgBorder+strings.Repeat("─", maxW)+ansiReset)
	}

	selStart, selEnd := 0, 0
	for i, it := range items {
		focused := m.focus == focusDetail && i == m.detailIdx
		itemStart := len(lines)

		switch it.kind {
		case detailInstall:
			if it.installIdx >= 0 && it.installIdx < len(m.installs) {
				lines = append(lines, m.renderInstallEntry(m.installs[it.installIdx], focused, maxW)...)
			}
		case detailDNS:
			if it.dnsIdx >= 0 && it.dnsIdx < len(m.dnsProviders) {
				sp := m.dnsProviders[it.dnsIdx]
				var probe *proxy.DNSProbeResult
				if res, ok := m.dnsProbes[proxy.ProviderSyncKey(sp)]; ok {
					probe = &res
				}
				lines = append(lines, renderDNSEntry(it.dnsIdx, sp, probe, focused, maxW)...)
			}
		default:
			rowItem := actionItem{
				key:   it.key,
				label: it.label,
				badge: it.badge,
			}
			lines = append(lines, renderMenuRow(rowItem, focused, focused, maxW))
			if it.desc != "" && maxW > 10 {
				for _, dl := range wrapPlain(it.desc, maxW-6) {
					lines = append(lines, fmt.Sprintf("      %s%s%s", fgMuted, dl, ansiReset))
				}
			}
		}

		if i == m.detailIdx {
			selStart = itemStart
			selEnd = len(lines) - 1
			if selEnd < selStart {
				selEnd = selStart
			}
		}
	}

	if maxRows > 0 && len(lines) > maxRows {
		start := 0
		if selEnd >= maxRows {
			start = selEnd - maxRows + 1
		}
		if start > selStart {
			start = selStart
		}
		if start+maxRows > len(lines) {
			start = len(lines) - maxRows
		}
		if start < 0 {
			start = 0
		}
		lines = lines[start : start+maxRows]
	}
	return lines
}

func renderDNSEntry(idx int, sp proxy.DNSProviderSpec, probe *proxy.DNSProbeResult, focused bool, maxW int) []string {
	checkBox := "[ ]"
	checkCol := fgMuted
	if sp.Enabled {
		checkBox = "[✓]"
		checkCol = fgGreen
	}
	kindLabel := "DoH"
	kindCol := fgCyan
	switch strings.ToLower(sp.Kind) {
	case proxy.DNSKindUDP:
		kindLabel = "UDP"
		kindCol = fgYellow
	case proxy.DNSKindDoT:
		kindLabel = "DoT"
		kindCol = fgAccent
	}

	probeCol := ""
	probeText := ""
	if probe != nil {
		switch probe.Status {
		case proxy.ProbeSub:
			probeCol = fgGreen
			probeText = fmt.Sprintf("✓%dms", probe.LatencyMs)
		case proxy.ProbeDirect:
			probeCol = fgYellow
			probeText = fmt.Sprintf("○dir %dms", probe.LatencyMs)
		case proxy.ProbeTLSErr:
			probeCol = fgRed
			probeText = "✗tls"
		case proxy.ProbeErr:
			probeCol = fgRed
			probeText = "✗err"
		}
	}

	prefix := fmt.Sprintf("%d. %s [%s] ", idx+1, sp.Name, kindLabel)
	extraCols := 0
	if sp.Source == proxy.DNSSourceUser {
		extraCols += 7
	}
	if probeText != "" {
		extraCols += visibleLen(probeText) + 1
	}
	availTarget := maxW - 9 - visibleLen(prefix) - extraCols
	if availTarget < 12 {
		availTarget = 12
	}
	targetDisp := truncateMiddle(sp.Target, availTarget)

	var mainLine string
	if focused {
		userSeg := ""
		if sp.Source == proxy.DNSSourceUser {
			userSeg = fmt.Sprintf(" %s[user]", fgAccent)
		}
		probeSeg := ""
		if probeText != "" {
			probeSeg = fmt.Sprintf(" %s%s", probeCol, probeText)
		}
		raw := fmt.Sprintf("%s%s▸ %s%s %s%d. %s%s %s[%s]%s%s%s — %s",
			bgSelect, fgAccent+ansiBold, checkCol, checkBox,
			ansiBold+fgPrimary, idx+1, sp.Name, bgSelect,
			kindCol, kindLabel, userSeg, probeSeg, bgSelect+fgMuted, targetDisp)
		vis := visibleLen(raw)
		if maxW > 0 {
			if vis > maxW {
				raw = truncateANSI(raw, maxW)
				raw = strings.TrimSuffix(raw, ansiReset)
			} else if vis < maxW {
				raw += strings.Repeat(" ", maxW-vis)
			}
		}
		mainLine = raw + ansiReset
	} else {
		nameCol := ansiBold + fgPrimary
		if !sp.Enabled {
			nameCol = fgMuted
		}
		userSeg := ""
		if sp.Source == proxy.DNSSourceUser {
			userSeg = fmt.Sprintf(" %s[user]%s", fgAccent, ansiReset)
		}
		probeSeg := ""
		if probeText != "" {
			probeSeg = fmt.Sprintf(" %s%s%s", probeCol, probeText, ansiReset)
		}
		mainLine = fmt.Sprintf("  %s%s%s %s%d. %s%s %s[%s]%s%s%s %s— %s%s",
			checkCol, checkBox, ansiReset,
			nameCol, idx+1, sp.Name, ansiReset,
			kindCol, kindLabel, ansiReset, userSeg, probeSeg,
			fgMuted, targetDisp, ansiReset)
	}

	out := []string{mainLine}
	if len(sp.FallbackIPs) > 0 {
		subLine := fmt.Sprintf("      %ssub: %s%s", fgMuted, strings.Join(sp.FallbackIPs, ", "), ansiReset)
		out = append(out, truncateANSI(subLine, maxW))
	}
	return out
}

func (m *model) renderInstallEntry(iv installView, focused bool, maxW int) []string {
	var lines []string
	checked := m.selected[iv.installKey]

	icon := "○"
	stateCol := fgMuted
	stateLabel := m.t("tui_state_stock")
	if iv.missing {
		icon = "✗"
		stateCol = fgRed
		stateLabel = m.t("tui_state_missing")
	} else if iv.noPatchNeeded {
		icon = "✓"
		stateCol = fgGreen
		stateLabel = m.t("tui_state_no_patch")
	} else if iv.binaryPatched && iv.proxyVarPatched && (!iv.hasJS || iv.jsPatched) {
		icon = "✓"
		stateCol = fgGreen
		stateLabel = m.t("tui_state_patched")
	} else if iv.binaryPatched || iv.jsPatched {
		icon = "◐"
		stateCol = fgYellow
		stateLabel = m.t("tui_state_partial")
	}

	checkBox := "[ ]"
	checkCol := fgMuted
	if checked {
		checkBox = "[✓]"
		checkCol = fgGreen
	}

	manualTag := ""
	if iv.overrideKey != "" {
		manualTag = m.t("tui_tag_custom")
	} else if iv.install.Manual {
		manualTag = m.t("tui_tag_manual")
	}

	var header string
	if focused {
		raw := fmt.Sprintf("%s%s▸ %s%s %s%s %s%s%s (%s) — %s%s%s%s",
			bgSelect, fgAccent+ansiBold, checkCol, checkBox, stateCol, icon,
			ansiBold+fgPrimary, iv.install.Label, fgMuted, iv.install.Type,
			stateCol, stateLabel, fgCyan, manualTag)
		vis := visibleLen(raw)
		if maxW > 0 {
			if vis > maxW {
				raw = truncateANSI(raw, maxW)
				raw = strings.TrimSuffix(raw, ansiReset)
			} else if vis < maxW {
				raw += strings.Repeat(" ", maxW-vis)
			}
		}
		header = raw + ansiReset
	} else {
		header = fmt.Sprintf("  %s%s%s %s%s%s %s%s%s %s(%s)%s — %s%s%s%s%s%s",
			checkCol, checkBox, ansiReset, stateCol, icon, ansiReset,
			ansiBold+fgPrimary, iv.install.Label, ansiReset,
			fgMuted, iv.install.Type, ansiReset,
			stateCol, stateLabel, ansiReset, fgCyan, manualTag, ansiReset)
	}
	lines = append(lines, header)

	pathLine := fmt.Sprintf("      %s%s%s", fgMuted, truncateMiddle(iv.install.Path, maxW-6), ansiReset)
	lines = append(lines, pathLine)

	var statusBadges string
	if iv.noPatchNeeded {
		statusBadges = fgGreen + m.t("tui_badge_patch_optional") + ansiReset
	} else {
		statusBadges = fmt.Sprintf("%s%s:%s%s %sproxy:%s%s",
			fgMuted, m.t("tui_badge_patch"), ansiReset, boolBadge(iv.binaryPatched),
			fgMuted, ansiReset, boolBadge(iv.proxyVarPatched))
		if iv.hasJS {
			statusBadges += fmt.Sprintf(" %sjs:%s%s", fgMuted, ansiReset, boolBadge(iv.jsPatched))
		}
		if iv.ruApplicable {
			statusBadges += fmt.Sprintf(" %sru:%s%s", fgMuted, ansiReset, boolBadge(iv.ruInstalled))
		} else if iv.install.AsarPath != "" {
			statusBadges += fmt.Sprintf(" %sasar%s", fgMuted, ansiReset)
		}
	}
	targetPrefix := m.t("tui_label_targets")
	prefixCols := 6 + visibleLen(targetPrefix) + 1
	badgeCols := visibleLen(statusBadges)
	availTargets := maxW - prefixCols - badgeCols - 2
	if availTargets >= 10 {
		targetsPart := iv.targetsLine
		if visibleLen(targetsPart) > availTargets {
			targetsPart = truncateMiddle(targetsPart, availTargets)
		}
		targetText := fmt.Sprintf("      %s%s%s %s  %s",
			fgCyan, targetPrefix, ansiReset, targetsPart, statusBadges)
		lines = append(lines, truncateANSI(targetText, maxW))
	} else {
		targetText := fmt.Sprintf("      %s%s%s %s",
			fgCyan, targetPrefix, ansiReset, iv.targetsLine)
		lines = append(lines, truncateANSI(targetText, maxW))
		lines = append(lines, truncateANSI("      "+statusBadges, maxW))
	}
	return lines
}

func boolBadge(ok bool) string {
	if ok {
		return fgGreen + "✓" + ansiReset
	}
	return fgMuted + "✗" + ansiReset
}

func (m *model) renderTelemetry(maxW int) []string {
	proxyEnvVal := m.proxyEnv
	if proxyEnvVal == "" {
		proxyEnvVal = fgMuted + m.t("tui_not_set") + ansiReset
	} else {
		proxyEnvVal = fgGreen + proxyEnvVal + ansiReset
	}

	routeStr := fgMuted + m.t("tui_state_stopped") + ansiReset
	if m.proxyOwned {
		srcTag := m.t("tui_src_embedded")
		if m.daemonClient != nil {
			srcTag = m.t("tui_src_daemon")
		}
		routeStr = fmt.Sprintf("%s● %s%s  %s(%s)%s", fgGreen, m.proxyAddr, ansiReset, fgMuted, srcTag, ansiReset)
	} else if m.externalProxy != "" {
		routeStr = fmt.Sprintf("%s● %s%s  %s(%s)%s", fgCyan, m.externalProxy, ansiReset, fgMuted, m.t("tui_src_system_proxy"), ansiReset)
	} else if m.proxyNote != "" {
		routeStr = fgYellow + m.proxyNote + ansiReset
	}

	var statsLine string
	if m.proxyOwned {
		st := m.proxyStats
		if m.proxyServer != nil {
			st = m.proxyServer.Stats()
		}
		statsLine = fmt.Sprintf("%s%s:%s %d  %s%s:%s %d  %s%s:%s %d",
			fgMuted, m.t("tui_stat_active"), ansiReset+fgCyan, st.Active,
			fgMuted, m.t("tui_stat_total"), ansiReset+fgPrimary, st.Total,
			fgMuted, m.t("tui_stat_gate"), ansiReset+fgGreen, st.Gate) + ansiReset
	} else {
		statsLine = fgMuted + m.t("tui_gates_list") + ansiReset
	}

	return []string{
		fmt.Sprintf("%s%s:%s %s", fgMuted, env.ProxyVar, ansiReset, truncateANSI(proxyEnvVal, maxW-13)),
		fmt.Sprintf("%s%s:%s %s", fgMuted, m.t("tui_label_proxy"), ansiReset, routeStr),
		statsLine,
		truncateANSI(fgMuted+m.t("tui_label_os")+m.osLabel+"  •  "+m.formatDNSSummary()+ansiReset, maxW),
	}
}

func (m *model) renderLogs(maxW, maxRows int) []string {
	if maxRows <= 0 {
		return nil
	}
	if len(m.logs) == 0 {
		return []string{fgMuted + m.t("tui_logs_empty") + ansiReset}
	}

	const prefixCols = 17 // "15:04:05 [INFO]  " or "15:04:05 [PROXY] "
	msgW := maxW - prefixCols
	var wrapped []string
	if msgW < 12 {
		for _, entry := range m.logs {
			line := fmt.Sprintf("%s%s%s %s %s%s%s",
				fgMuted, entry.ts, ansiReset,
				formatLogTag(entry.level),
				fgPrimary, entry.msg, ansiReset)
			wrapped = append(wrapped, truncateANSI(line, maxW))
		}
	} else {
		indent := strings.Repeat(" ", prefixCols)
		for _, entry := range m.logs {
			parts := wrapPlain(entry.msg, msgW)
			for i, p := range parts {
				if i == 0 {
					line := fmt.Sprintf("%s%s%s %s %s%s%s",
						fgMuted, entry.ts, ansiReset,
						formatLogTag(entry.level),
						fgPrimary, p, ansiReset)
					wrapped = append(wrapped, line)
				} else {
					wrapped = append(wrapped, fmt.Sprintf("%s%s%s%s", indent, fgPrimary, p, ansiReset))
				}
			}
		}
	}

	total := len(wrapped)
	end := total - m.logScroll
	if end < maxRows && total >= maxRows {
		end = maxRows
	}
	if end > total {
		end = total
	}
	if end < 0 {
		end = 0
	}
	start := end - maxRows
	if start < 0 {
		start = 0
	}
	return wrapped[start:end]
}

func (m *model) renderFooter() string {
	if m.toastText != "" && !m.now().After(m.toastUntil) {
		col := fgGreen
		switch m.toastLevel {
		case logErr:
			col = fgRed
		case logWarn:
			col = fgYellow
		}
		return fmt.Sprintf(" %s%s%s %s", bgBadge, col+ansiBold, m.toastText, ansiReset)
	}
	switch m.focus {
	case focusLogs:
		return fgMuted + m.t("tui_footer_logs") + ansiReset
	case focusDetail:
		switch m.sectionIdx {
		case sectionPatch:
			if m.isLinux() && m.linuxInstallView {
				return fgMuted + m.t("tui_footer_detail_linux_install") + ansiReset
			}
			return fgMuted + m.t("tui_footer_detail_patch") + ansiReset
		case sectionDNS:
			return fgMuted + m.t("tui_footer_detail_dns") + ansiReset
		default:
			return fgMuted + m.t("tui_footer_detail_other") + ansiReset
		}
	default:
		return fgMuted + m.t("tui_footer_menu") + ansiReset
	}
}
