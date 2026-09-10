package tui

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/vogler75/babel-gate/pkg/logger"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server"
)

// ANSI escape sequences
const (
	escClearScreen = "\033[2J"
	escCursorHome  = "\033[H"
	escAltScreen   = "\033[?1049h"
	escExitAlt     = "\033[?1049l"
	escHideCursor  = "\033[?25l"
	escShowCursor  = "\033[?25h"

	colorReset   = "\033[0m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorBlue    = "\033[34m"
	colorMagenta = "\033[35m"
	colorCyan    = "\033[36m"
	colorWhite   = "\033[37m"
)

type tuiPane int

const (
	paneProviders tuiPane = iota
	paneSessions
	paneLogs
	paneCount
)

// IsTerminal returns true if stdout is connected to a terminal.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// TUI represents the interactive terminal GUI.
type TUI struct {
	mu                sync.Mutex
	srv               *server.Server
	engine            *router.Engine
	ring              *logger.RingBuffer
	startTime         time.Time
	scrollPos         int // 0 means bottom (auto-scroll), >0 means scrolled up by N lines
	hScrollPos        int // 0 means left edge, >0 means scrolled right by N columns
	autoScroll        bool
	stopChan          chan struct{}
	origTerm          *term.State
	cols              int
	rows              int
	activePane        tuiPane
	providerSelection int
	sessionSelection  int
}

// New creates a new TUI instance.
func New(srv *server.Server, engine *router.Engine, ring *logger.RingBuffer) *TUI {
	return &TUI{
		srv:        srv,
		engine:     engine,
		ring:       ring,
		startTime:  time.Now(),
		autoScroll: true,
		stopChan:   make(chan struct{}),
	}
}

// Run enters raw mode, launches render & input loops, and blocks until quit or context cancelled.
func (t *TUI) Run(ctx context.Context) error {
	fd := int(os.Stdin.Fd())

	// Save original termios and set raw mode
	orig, err := term.MakeRaw(fd)
	if err == nil {
		t.origTerm = orig
	}

	// Enter alternate screen and hide cursor
	os.Stdout.WriteString(escAltScreen + escHideCursor + escClearScreen + escCursorHome)
	defer t.restoreTerminal()

	// Handle window resize signals (SIGWINCH)
	winch := make(chan os.Signal, 1)
	notifyWinch(winch)
	defer signal.Stop(winch)

	t.updateSize()

	// Input handling goroutine
	inputCh := make(chan string, 16)
	go t.readInput(inputCh)

	// Render ticker (5 frames per second for stats/logs)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	// Initial render
	t.render()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.stopChan:
			return nil
		case <-winch:
			t.updateSize()
			t.render()
		case <-ticker.C:
			t.render()
		case key := <-inputCh:
			if t.handleKey(key) {
				return nil
			}
			t.render()
		}
	}
}

func (t *TUI) restoreTerminal() {
	if t.origTerm != nil {
		_ = term.Restore(int(os.Stdin.Fd()), t.origTerm)
	}
	os.Stdout.WriteString(escShowCursor + escExitAlt)
}

func (t *TUI) updateSize() {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && cols > 0 && rows > 0 {
		t.cols = cols
		t.rows = rows
	} else {
		t.cols = 80
		t.rows = 24
	}
}

// readInput reads keystrokes including ANSI escape sequences (arrow keys, etc.)
func (t *TUI) readInput(ch chan<- string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}

		if b == 3 { // Ctrl+C
			ch <- "ctrl+c"
			return
		}

		if b == 27 { // Escape sequence
			if reader.Buffered() > 0 {
				b2, err := reader.ReadByte()
				if err != nil {
					ch <- string([]byte{b})
					continue
				}
				if b2 == '[' {
					b3, err := reader.ReadByte()
					if err != nil {
						ch <- string([]byte{b, b2})
						continue
					}
					switch b3 {
					case 'A':
						ch <- "up"
					case 'B':
						ch <- "down"
					case 'C':
						ch <- "right"
					case 'D':
						ch <- "left"
					case 'H':
						ch <- "home"
					case 'F':
						ch <- "end"
					case 'Z':
						ch <- "backtab"
					case '5': // Page Up (\033[5~)
						if reader.Buffered() > 0 {
							_, _ = reader.ReadByte() // '~'
						}
						ch <- "pageup"
					case '6': // Page Down (\033[6~)
						if reader.Buffered() > 0 {
							_, _ = reader.ReadByte() // '~'
						}
						ch <- "pagedown"
					default:
						ch <- fmt.Sprintf("esc[%c", b3)
					}
				} else {
					ch <- string([]byte{b, b2})
				}
			} else {
				ch <- "esc"
			}
		} else {
			ch <- string([]byte{b})
		}
	}
}

// handleKey processes a keystroke, returning true if the app should exit.
func (t *TUI) handleKey(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch key {
	case "q", "Q", "ctrl+c":
		close(t.stopChan)
		return true

	case "\t":
		t.activePane = (t.activePane + 1) % paneCount

	case "backtab":
		t.activePane = (t.activePane + paneCount - 1) % paneCount

	case "up", "k":
		t.moveSelection(-1)

	case "down", "j":
		t.moveSelection(1)

	case "right", "l":
		if t.activePane == paneLogs {
			t.hScrollPos += 8
		}

	case "left", "h":
		if t.activePane == paneLogs {
			t.hScrollPos -= 8
			if t.hScrollPos < 0 {
				t.hScrollPos = 0
			}
		}

	case "0":
		t.hScrollPos = 0

	case "pageup":
		if t.activePane == paneLogs {
			t.scrollPos += 10
			t.autoScroll = false
		}

	case "pagedown":
		if t.activePane == paneLogs {
			t.scrollPos -= 10
			if t.scrollPos <= 0 {
				t.scrollPos = 0
				t.autoScroll = true
			}
		}

	case "home":
		if t.activePane == paneProviders {
			t.providerSelection = 0
		} else if t.activePane == paneSessions {
			t.sessionSelection = 0
		} else if t.ring != nil {
			t.scrollPos = t.ring.Count()
			t.autoScroll = false
		}

	case "end":
		if t.activePane == paneProviders && t.engine != nil {
			t.providerSelection = maxInt(0, len(t.engine.GetProviderStates())-1)
		} else if t.activePane == paneSessions && t.srv != nil && t.srv.Sessions() != nil {
			t.sessionSelection = maxInt(0, len(t.srv.Sessions().ListSessions())-1)
		} else {
			t.scrollPos = 0
			t.autoScroll = true
		}

	case "c", "C":
		if t.activePane == paneLogs && t.ring != nil {
			t.ring.Clear()
			t.scrollPos = 0
			t.hScrollPos = 0
			t.autoScroll = true
		}

	case "r", "R":
		if t.engine != nil {
			if err := t.engine.ReloadRouting(); err != nil {
				log.Printf("[TUI] failed to reload routes: %v", err)
			} else {
				log.Printf("[TUI] routes reloaded from configuration")
			}
		}
		t.updateSize()

	case "p", "P":
		t.activePane = paneProviders

	case "s", "S":
		t.activePane = paneSessions

	case "g", "G":
		t.activePane = paneLogs

	case " ":
		if t.activePane == paneProviders && t.engine != nil {
			providers := t.engine.GetProviderStates()
			if len(providers) > 0 {
				if t.providerSelection >= len(providers) {
					t.providerSelection = 0
				}
				provider := providers[t.providerSelection]
				persisted, err := t.engine.SetProviderEnabled(provider.Name, !provider.Enabled)
				if err != nil {
					log.Printf("[TUI] failed to update provider %s: %v", provider.Name, err)
				} else {
					location := "for this process"
					if persisted {
						location = "and saved to config"
					}
					log.Printf("[TUI] provider %s %s %s", provider.Name, enabledWord(!provider.Enabled), location)
				}
			}
		}
	}

	return false
}

func (t *TUI) moveSelection(delta int) {
	switch t.activePane {
	case paneProviders:
		count := 0
		if t.engine != nil {
			count = len(t.engine.GetProviderStates())
		}
		t.providerSelection = clampInt(t.providerSelection+delta, 0, maxInt(0, count-1))
	case paneSessions:
		count := 0
		if t.srv != nil && t.srv.Sessions() != nil {
			count = len(t.srv.Sessions().ListSessions())
		}
		t.sessionSelection = clampInt(t.sessionSelection+delta, 0, maxInt(0, count-1))
	case paneLogs:
		if delta < 0 {
			t.scrollPos++
			t.autoScroll = false
		} else if t.scrollPos > 0 {
			t.scrollPos--
			if t.scrollPos == 0 {
				t.autoScroll = true
			}
		}
	}
}

// render draws a unified, perfectly-aligned terminal frame.
func (t *TUI) render() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cols < 40 || t.rows < 10 {
		return
	}

	// Always use terminal width minus 1 so lines never wrap unexpectedly
	width := t.cols - 1
	if width < 40 {
		width = 40
	}

	var sb strings.Builder
	sb.WriteString(escCursorHome)

	// 1. Top Section: Header & Status
	uptime := time.Since(t.startTime).Truncate(time.Second)
	port := 8080
	if t.srv != nil && t.srv.Config() != nil {
		port = t.srv.Config().Server.Port
	}

	var reqCount, inTokens, outTokens int
	if t.srv != nil && t.srv.Sessions() != nil {
		sum := t.srv.Sessions().GetSummary()
		reqCount = sum.TotalRequests
		inTokens = sum.TotalInputTokens
		outTokens = sum.TotalOutputTokens
	}

	headerTitle := fmt.Sprintf(" BabelGate LLM Router :%d │ Uptime: %s ", port, formatDuration(uptime))
	sb.WriteString(t.renderBorderTop(headerTitle, width))
	sb.WriteString("\r\n")

	statusLine := fmt.Sprintf(" Status: %sRUNNING%s │ Requests: %s%d%s │ Tokens: In: %s%s%s / Out: %s%s%s / Total: %s%s%s",
		colorGreen+colorBold, colorReset,
		colorCyan+colorBold, reqCount, colorReset,
		colorYellow, formatTokens(int64(inTokens)), colorReset,
		colorYellow, formatTokens(int64(outTokens)), colorReset,
		colorBold+colorYellow, formatTokens(int64(inTokens+outTokens)), colorReset,
	)
	sb.WriteString(t.renderBoxLine(statusLine, width))
	sb.WriteString("\r\n")

	// 2. Focusable Providers and Sessions panes
	providerLines := t.getSortedProvidersInfo()
	providerCount := len(providerLines)
	if providerCount == 0 {
		providerLines = []string{" (No upstream providers configured)"}
	}
	sessionLines := t.getSessionsInfo(0)
	sessionCount := len(sessionLines)
	if sessionCount == 0 {
		sessionLines = []string{" (No sessions recorded yet)"}
	}

	// Reserve one row for each pane and distribute remaining space, keeping a
	// useful log area even on smaller terminals.
	availableContent := t.rows - 7
	providerHeight, sessionHeight := 1, 1
	remaining := availableContent - 5 // reserve at least three rows for logs when possible
	maxProviderHeight := minInt(maxInt(1, providerCount), 5)
	maxSessionHeight := minInt(maxInt(1, sessionCount), 6)
	for remaining > 0 && (providerHeight < maxProviderHeight || sessionHeight < maxSessionHeight) {
		if providerHeight < maxProviderHeight && remaining > 0 {
			providerHeight++
			remaining--
		}
		if sessionHeight < maxSessionHeight && remaining > 0 {
			sessionHeight++
			remaining--
		}
	}
	providerLines = selectedWindow(providerLines, t.providerSelection, providerHeight)
	sessionLines = selectedWindow(sessionLines, t.sessionSelection, sessionHeight)

	providerTitle := fmt.Sprintf("Providers [%d]", providerCount)
	if providerCount > 0 {
		providerTitle += fmt.Sprintf(" %d/%d", t.providerSelection+1, providerCount)
	}
	sb.WriteString(t.renderDivider(t.focusedPaneTitle(paneProviders, providerTitle), width))
	sb.WriteString("\r\n")
	for _, line := range providerLines {
		sb.WriteString(t.renderBoxLine(line, width))
		sb.WriteString("\r\n")
	}

	sessionTitle := fmt.Sprintf("Sessions [%d]", sessionCount)
	if sessionCount > 0 {
		sessionTitle += fmt.Sprintf(" %d/%d", t.sessionSelection+1, sessionCount)
	}
	sb.WriteString(t.renderDivider(t.focusedPaneTitle(paneSessions, sessionTitle), width))
	sb.WriteString("\r\n")
	for _, line := range sessionLines {
		sb.WriteString(t.renderBoxLine(line, width))
		sb.WriteString("\r\n")
	}

	// 3. Focusable Live Request Logs pane
	logHeaderTitle := "Live Request Logs"
	var scrollIndicators []string
	if !t.autoScroll && t.scrollPos > 0 {
		scrollIndicators = append(scrollIndicators, fmt.Sprintf("PAUSED: +%d lines", t.scrollPos))
	}
	if t.hScrollPos > 0 {
		scrollIndicators = append(scrollIndicators, fmt.Sprintf("Col +%d", t.hScrollPos))
	}
	if len(scrollIndicators) > 0 {
		logHeaderTitle = fmt.Sprintf("Live Request Logs [%s%s%s]", colorYellow+colorBold, strings.Join(scrollIndicators, ", "), colorReset)
	}
	sb.WriteString(t.renderDivider(t.focusedPaneTitle(paneLogs, logHeaderTitle), width))
	sb.WriteString("\r\n")

	// Calculate log box height dynamically:
	// Frame overhead: border/status + three dividers + provider/session rows + bottom/footer.
	fixedRows := 7 + len(providerLines) + len(sessionLines)
	logHeight := t.rows - fixedRows
	if logHeight < 1 {
		logHeight = 1
	}

	var allLines []string
	if t.ring != nil {
		allLines = t.ring.Lines()
	}

	inner := width - 2
	logSlice := t.computeVisibleLogs(allLines, logHeight)
	for i := 0; i < logHeight; i++ {
		lineContent := ""
		if i < len(logSlice) {
			colored := colorizeLogLine(logSlice[i])
			lineContent = sliceLogLine(colored, t.hScrollPos, inner)
		}
		sb.WriteString(t.renderBoxLine(lineContent, width))
		sb.WriteString("\r\n")
	}

	// Frame Bottom Border
	sb.WriteString(t.renderBorderBottom(width))
	sb.WriteString("\r\n")

	// 4. Footer Shortcuts
	footer := fmt.Sprintf(" %sq%s Quit │ %sTab/⇧Tab%s Pane │ %s↑/↓%s Select/Scroll │ %sSpace%s Toggle │ %sr%s Reload Routes │ %sc%s Clear Logs",
		colorBold, colorReset,
		colorBold, colorReset,
		colorBold, colorReset,
		colorBold, colorReset,
		colorBold, colorReset,
		colorBold, colorReset,
	)
	sb.WriteString(footer)

	os.Stdout.WriteString(sb.String())
}

func (t *TUI) computeVisibleLogs(lines []string, visibleRows int) []string {
	total := len(lines)
	if total == 0 {
		return []string{"  Waiting for requests..."}
	}

	if t.autoScroll || t.scrollPos == 0 {
		if total <= visibleRows {
			return lines
		}
		return lines[total-visibleRows:]
	}

	endIdx := total - t.scrollPos
	if endIdx < visibleRows {
		endIdx = visibleRows
	}
	if endIdx > total {
		endIdx = total
	}

	startIdx := endIdx - visibleRows
	if startIdx < 0 {
		startIdx = 0
	}

	return lines[startIdx:endIdx]
}

func (t *TUI) getSortedProvidersInfo() []string {
	if t.engine == nil {
		return nil
	}

	states := t.engine.GetProviderStates()
	if len(states) == 0 {
		return nil
	}
	if t.providerSelection >= len(states) {
		t.providerSelection = 0
	}

	var res []string
	for index, state := range states {
		modelsCount := len(t.engine.GetProviderModels(state.Name))

		url := ""
		if t.srv != nil && t.srv.Config() != nil {
			if pc, ok := t.srv.Config().Providers[state.Name]; ok {
				url = pc.BaseURL
			}
		}
		if url == "" {
			url = "(default cloud API)"
		}

		selector := " "
		if index == t.providerSelection {
			selector = "▶"
		}
		statusColor := colorDim
		status := "○ Disabled"
		if state.Enabled {
			statusColor = colorGreen
			status = "● Enabled "
		}
		line := fmt.Sprintf(" %s %s[Prio %d]%s %s%-9s%s (%s) %s%s%s (%d models) %s%s%s",
			selector,
			colorDim, state.Priority, colorReset,
			colorBold+colorCyan, strings.ToUpper(state.Name), colorReset,
			state.Type,
			statusColor, status, colorReset,
			modelsCount,
			colorDim, url, colorReset,
		)
		res = append(res, line)
	}

	return res
}

func (t *TUI) getSessionsInfo(limit int) []string {
	if t.srv == nil || t.srv.Sessions() == nil {
		return nil
	}
	sessions := t.srv.Sessions().ListSessions()
	if len(sessions) == 0 {
		return nil
	}
	total := len(sessions)
	t.sessionSelection = clampInt(t.sessionSelection, 0, maxInt(0, total-1))
	if limit > 1 && len(sessions) > limit {
		start := clampInt(t.sessionSelection-limit/2, 0, len(sessions)-limit)
		sessions = sessions[start : start+limit]
	}
	lines := make([]string, 0, len(sessions)+1)
	for index, sess := range sessions {
		age := time.Since(sess.LastActive).Truncate(time.Second)
		if age < 0 {
			age = 0
		}
		model := "-"
		if sess.LastModel != "" {
			model = sess.LastModel
		} else if len(sess.RecentRequests) > 0 && sess.RecentRequests[0].Model != "" {
			model = sess.RecentRequests[0].Model
		} else if len(sess.Models) > 0 {
			model = sess.Models[len(sess.Models)-1]
		}
		rate := "—"
		if sess.TokensPerSecond > 0 {
			rate = fmt.Sprintf("%.1f tok/s", sess.TokensPerSecond)
		}
		selector := " "
		absoluteIndex := index
		if limit > 1 && total > limit {
			absoluteIndex = clampInt(t.sessionSelection-limit/2, 0, total-limit) + index
		}
		if absoluteIndex == t.sessionSelection {
			selector = "▶"
		}
		contextTokens := formatTokens(int64(sess.ContextTokens))
		if sess.ContextTokensEstimated {
			contextTokens = "~" + contextTokens
		}
		lines = append(lines, fmt.Sprintf(" %s %-18s Req:%-4d Ctx:%-7s Out:%-7s Speed:%s%-11s%s Active:%-8s %s%s%s ID:%s%s%s",
			selector,
			truncatePlain(sess.Client, 18), sess.RequestCount, contextTokens, formatTokens(int64(sess.OutputTokens)),
			colorGreen, rate, colorReset, formatShortAge(age),
			colorDim, model, colorReset,
			colorBold+colorCyan, sess.ID, colorReset))
	}
	return lines
}

func (t *TUI) focusedPaneTitle(pane tuiPane, title string) string {
	if t.activePane == pane {
		return fmt.Sprintf(" %s▶ %s%s ", colorCyan+colorBold, title, colorReset)
	}
	return " " + title + " "
}

func selectedWindow(lines []string, selection, height int) []string {
	if height <= 0 || len(lines) <= height {
		return lines
	}
	start := clampInt(selection-height/2, 0, len(lines)-height)
	return lines[start : start+height]
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncatePlain(value string, width int) string {
	if visualWidth(value) <= width {
		return value
	}
	return stripANSI(truncateToVisualWidth(value, width))
}

func formatShortAge(age time.Duration) string {
	if age < time.Minute {
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(age.Hours()))
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (t *TUI) renderBorderTop(title string, width int) string {
	plainTitleLen := visualWidth(title)
	remain := width - 2 - plainTitleLen
	if remain < 0 {
		remain = 0
	}
	left := 2
	right := remain - left
	if right < 0 {
		right = 0
	}
	return colorDim + "┌" + strings.Repeat("─", left) + colorReset + colorBold + title + colorReset + colorDim + strings.Repeat("─", right) + "┐" + colorReset
}

func (t *TUI) renderDivider(title string, width int) string {
	plainTitleLen := visualWidth(title)
	remain := width - 2 - plainTitleLen
	if remain < 0 {
		remain = 0
	}
	left := 2
	right := remain - left
	if right < 0 {
		right = 0
	}
	return colorDim + "├" + strings.Repeat("─", left) + colorReset + colorBold + title + colorReset + colorDim + strings.Repeat("─", right) + "┤" + colorReset
}

func (t *TUI) renderBorderBottom(width int) string {
	inner := width - 2
	if inner < 0 {
		inner = 0
	}
	return colorDim + "└" + strings.Repeat("─", inner) + "┘" + colorReset
}

func (t *TUI) renderBoxLine(content string, width int) string {
	inner := width - 2
	visLen := visualWidth(content)
	if visLen > inner {
		content = truncateToVisualWidth(content, inner)
		visLen = visualWidth(content)
	}
	pad := inner - visLen
	if pad < 0 {
		pad = 0
	}
	return colorDim + "│" + colorReset + content + strings.Repeat(" ", pad) + colorDim + "│" + colorReset
}

func colorizeLogLine(line string) string {
	if strings.Contains(line, " -> 2") {
		line = strings.Replace(line, " -> 2", " -> "+colorGreen+"2", 1)
		idx := strings.Index(line, " -> "+colorGreen+"2")
		if idx != -1 && len(line) >= idx+12 {
			line = line[:idx+11] + colorReset + line[idx+11:]
		}
	} else if strings.Contains(line, " -> 4") {
		line = strings.Replace(line, " -> 4", " -> "+colorYellow+"4", 1)
		idx := strings.Index(line, " -> "+colorYellow+"4")
		if idx != -1 && len(line) >= idx+12 {
			line = line[:idx+11] + colorReset + line[idx+11:]
		}
	} else if strings.Contains(line, " -> 5") {
		line = strings.Replace(line, " -> 5", " -> "+colorRed+"5", 1)
		idx := strings.Index(line, " -> "+colorRed+"5")
		if idx != -1 && len(line) >= idx+12 {
			line = line[:idx+11] + colorReset + line[idx+11:]
		}
	}

	if len(line) >= 19 && line[4] == '/' && line[7] == '/' && line[10] == ' ' && line[13] == ':' {
		line = colorDim + line[:19] + colorReset + line[19:]
	}

	return line
}

// stripANSI removes ANSI color and escape sequences from a string.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEsc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// visualWidth computes terminal cell width taking wide runes into account.
func visualWidth(s string) int {
	clean := stripANSI(s)
	width := 0
	for _, r := range clean {
		width += runeWidth(r)
	}
	return width
}

func runeWidth(r rune) int {
	if r == 0 {
		return 0
	}
	// Zero-width characters / combining marks
	if r < 32 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	// Common wide characters and emojis
	if r >= 0x1100 &&
		(r <= 0x115f ||
			r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
			(r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) ||
			(r >= 0xfe10 && r <= 0xfe19) ||
			(r >= 0xfe30 && r <= 0xfe6f) ||
			(r >= 0xff00 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6) ||
			(r >= 0x1f000 && r <= 0x1f9ff) ||
			(r >= 0x20000 && r <= 0x2fffd) ||
			(r >= 0x30000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// truncateToVisualWidth truncates string to max visual cell width, preserving ANSI sequences.
func truncateToVisualWidth(s string, maxCells int) string {
	var b strings.Builder
	inEsc := false
	curWidth := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\033' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if inEsc {
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}

		w := runeWidth(r)
		if curWidth+w > maxCells-1 {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		curWidth += w
	}
	b.WriteString(colorReset)
	return b.String()
}

// sliceLogLine slices s horizontally starting at visual column hScroll up to maxCells visual width,
// preserving active ANSI color sequences and wide characters.
func sliceLogLine(s string, hScroll int, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	if hScroll < 0 {
		hScroll = 0
	}

	var b strings.Builder
	inEsc := false
	curCol := 0
	visibleCells := 0
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\033' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if inEsc {
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}

		w := runeWidth(r)

		// Character completely before horizontal scroll offset
		if curCol+w <= hScroll {
			curCol += w
			continue
		}

		// Character straddles the left scroll boundary (e.g. wide rune)
		if curCol < hScroll {
			if visibleCells < maxCells {
				b.WriteByte(' ')
				visibleCells++
			}
			curCol += w
			continue
		}

		curCol += w

		// Would adding this character exceed maxCells?
		if visibleCells+w > maxCells {
			if visibleCells < maxCells {
				b.WriteString("…")
				visibleCells++
			}
			break
		}

		// If this character reaches maxCells, check if there's more visible text after
		if visibleCells+w == maxCells && i+1 < len(runes) {
			hasMore := false
			for j := i + 1; j < len(runes); j++ {
				if runes[j] != '\033' && runeWidth(runes[j]) > 0 {
					hasMore = true
					break
				}
			}
			if hasMore {
				b.WriteString("…")
				visibleCells++
				break
			}
		}

		b.WriteRune(r)
		visibleCells += w
	}

	b.WriteString(colorReset)
	return b.String()
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000.0)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	}
	return fmt.Sprintf("%d", n)
}
