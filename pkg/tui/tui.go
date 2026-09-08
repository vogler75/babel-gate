package tui

import (
	"bufio"
	"context"
	"fmt"
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

// IsTerminal returns true if stdout is connected to a terminal.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// TUI represents the interactive terminal GUI.
type TUI struct {
	mu         sync.Mutex
	srv        *server.Server
	engine     *router.Engine
	ring       *logger.RingBuffer
	startTime  time.Time
	scrollPos  int // 0 means bottom (auto-scroll), >0 means scrolled up by N lines
	autoScroll bool
	stopChan   chan struct{}
	origTerm   *term.State
	cols       int
	rows       int
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

	case "up", "k":
		t.scrollPos++
		t.autoScroll = false

	case "down", "j":
		if t.scrollPos > 0 {
			t.scrollPos--
		}
		if t.scrollPos == 0 {
			t.autoScroll = true
		}

	case "pageup":
		t.scrollPos += 10
		t.autoScroll = false

	case "pagedown":
		t.scrollPos -= 10
		if t.scrollPos <= 0 {
			t.scrollPos = 0
			t.autoScroll = true
		}

	case "home":
		if t.ring != nil {
			t.scrollPos = t.ring.Count()
			t.autoScroll = false
		}

	case "end", "G":
		t.scrollPos = 0
		t.autoScroll = true

	case "c", "C":
		if t.ring != nil {
			t.ring.Clear()
			t.scrollPos = 0
			t.autoScroll = true
		}

	case "r", "R":
		t.updateSize()
	}

	return false
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

	// 2. Middle Section: Connected Upstream Providers
	sb.WriteString(t.renderDivider(" Connected Upstream Providers ", width))
	sb.WriteString("\r\n")

	providersList := t.getSortedProvidersInfo()
	provLines := len(providersList)
	if provLines == 0 {
		sb.WriteString(t.renderBoxLine(" (No upstream providers registered)", width))
		sb.WriteString("\r\n")
		provLines = 1
	} else {
		for _, p := range providersList {
			sb.WriteString(t.renderBoxLine(p, width))
			sb.WriteString("\r\n")
		}
	}

	// 3. Lower Section: Live Request Logs
	logHeaderTitle := " Live Request Logs "
	if !t.autoScroll && t.scrollPos > 0 {
		logHeaderTitle = fmt.Sprintf(" Live Request Logs [%sPAUSED: Scrolled +%d%s] ", colorYellow+colorBold, t.scrollPos, colorReset)
	}
	sb.WriteString(t.renderDivider(logHeaderTitle, width))
	sb.WriteString("\r\n")

	// Calculate log box height dynamically:
	// Frame overhead:
	// Top border (1) + Status row (1) + Providers divider (1) + Providers rows (provLines) + Logs divider (1) + Bottom border (1) + Footer row (1)
	fixedRows := 1 + 1 + 1 + provLines + 1 + 1 + 1
	logHeight := t.rows - fixedRows
	if logHeight < 3 {
		logHeight = 3
	}

	var allLines []string
	if t.ring != nil {
		allLines = t.ring.Lines()
	}

	logSlice := t.computeVisibleLogs(allLines, logHeight)
	for i := 0; i < logHeight; i++ {
		lineContent := ""
		if i < len(logSlice) {
			lineContent = colorizeLogLine(logSlice[i])
		}
		sb.WriteString(t.renderBoxLine(lineContent, width))
		sb.WriteString("\r\n")
	}

	// Frame Bottom Border
	sb.WriteString(t.renderBorderBottom(width))
	sb.WriteString("\r\n")

	// 4. Footer Shortcuts
	footer := fmt.Sprintf(" %sq%s: Quit │ %s↑/↓/PgUp/PgDn%s: Scroll │ %sEnd%s: Auto-Scroll │ %sc%s: Clear │ %sr%s: Redraw",
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

	all := t.engine.GetProviders()
	if len(all) == 0 {
		return nil
	}

	type provItem struct {
		name     string
		priority int
		typeStr  string
		url      string
		models   int
	}

	var items []provItem
	for name, p := range all {
		prio := t.engine.GetProviderPriority(name)
		modelsCount := len(t.engine.GetProviderModels(name))

		url := ""
		if t.srv != nil && t.srv.Config() != nil {
			if pc, ok := t.srv.Config().Providers[name]; ok {
				url = pc.BaseURL
			}
		}
		if url == "" {
			url = "(default cloud API)"
		}

		items = append(items, provItem{
			name:     name,
			priority: prio,
			typeStr:  p.Type(),
			url:      url,
			models:   modelsCount,
		})
	}

	// Sort by priority ascending, then name
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if items[i].priority > items[j].priority || (items[i].priority == items[j].priority && items[i].name > items[j].name) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}

	var res []string
	for _, it := range items {
		line := fmt.Sprintf(" %s[Prio %d]%s %s%-9s%s (%s) %s● Connected%s (%d models) %s%s%s",
			colorDim, it.priority, colorReset,
			colorBold+colorCyan, strings.ToUpper(it.name), colorReset,
			it.typeStr,
			colorGreen, colorReset,
			it.models,
			colorDim, it.url, colorReset,
		)
		res = append(res, line)
	}

	return res
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
