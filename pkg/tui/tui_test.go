package tui

import (
	"strings"
	"testing"
)

func TestStripANSI(t *testing.T) {
	colored := "\033[32mhello\033[0m \033[1mworld\033[0m"
	plain := stripANSI(colored)
	if plain != "hello world" {
		t.Errorf("expected 'hello world', got %q", plain)
	}
}

func TestVisualWidth(t *testing.T) {
	ascii := "hello"
	if visualWidth(ascii) != 5 {
		t.Errorf("expected width 5, got %d", visualWidth(ascii))
	}

	colored := "\033[32mhello\033[0m"
	if visualWidth(colored) != 5 {
		t.Errorf("expected colored width 5, got %d", visualWidth(colored))
	}

	emoji := "🚀"
	if visualWidth(emoji) != 2 {
		t.Errorf("expected emoji width 2, got %d", visualWidth(emoji))
	}
}

func TestTruncateToVisualWidth(t *testing.T) {
	colored := "\033[32mhello world this is a very long line\033[0m"
	truncated := truncateToVisualWidth(colored, 10)
	w := visualWidth(truncated)
	if w > 10 {
		t.Errorf("expected visual width <= 10, got %d: %q", w, truncated)
	}
	plain := stripANSI(truncated)
	if !strings.HasSuffix(plain, "…") {
		t.Errorf("expected ellipsis at end, got %q", plain)
	}
}

func TestComputeVisibleLogs(t *testing.T) {
	tui := &TUI{
		autoScroll: true,
		scrollPos:  0,
	}

	lines := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}

	visible := tui.computeVisibleLogs(lines, 4)
	if len(visible) != 4 || visible[0] != "7" || visible[3] != "10" {
		t.Errorf("expected [7 8 9 10], got %v", visible)
	}

	tui.autoScroll = false
	tui.scrollPos = 2
	visible = tui.computeVisibleLogs(lines, 4)
	if len(visible) != 4 || visible[0] != "5" || visible[3] != "8" {
		t.Errorf("expected [5 6 7 8], got %v", visible)
	}
}

func TestColorizeLogLine(t *testing.T) {
	line200 := "2026/09/08 13:00:00 [GET] /api/sessions -> 200 [::1]:1234 took 1ms"
	colorized := colorizeLogLine(line200)
	if !strings.Contains(colorized, colorGreen) {
		t.Errorf("expected colorGreen in colorized line, got %q", colorized)
	}

	line500 := "2026/09/08 13:00:00 [POST] /v1/messages -> 500 [::1]:1234 took 1ms"
	colorized500 := colorizeLogLine(line500)
	if !strings.Contains(colorized500, colorRed) {
		t.Errorf("expected colorRed in colorized line, got %q", colorized500)
	}
}

func TestSliceLogLine(t *testing.T) {
	plain := "1234567890abcdef"
	
	// hScroll = 0
	res0 := sliceLogLine(plain, 0, 10)
	if visualWidth(res0) > 10 {
		t.Errorf("expected width <= 10, got %d", visualWidth(res0))
	}
	if !strings.HasPrefix(stripANSI(res0), "123456789") {
		t.Errorf("expected prefix '123456789', got %q", stripANSI(res0))
	}

	// hScroll = 5
	res5 := sliceLogLine(plain, 5, 10)
	if visualWidth(res5) > 10 {
		t.Errorf("expected width <= 10, got %d", visualWidth(res5))
	}
	if !strings.HasPrefix(stripANSI(res5), "67890") {
		t.Errorf("expected prefix '67890', got %q", stripANSI(res5))
	}

	// Slicing with ANSI colors
	colored := "\033[32mhello world\033[0m this is a test"
	resColored := sliceLogLine(colored, 6, 20)
	if !strings.Contains(resColored, colorGreen) {
		t.Errorf("expected active green color preserved, got %q", resColored)
	}
	if !strings.HasPrefix(stripANSI(resColored), "world this") {
		t.Errorf("expected 'world this', got %q", stripANSI(resColored))
	}

	// hScroll beyond line length
	resBeyond := sliceLogLine(plain, 50, 10)
	if stripANSI(resBeyond) != "" {
		t.Errorf("expected empty string when scrolled beyond line length, got %q", stripANSI(resBeyond))
	}
}

func TestHorizontalScrollKeyHandling(t *testing.T) {
	app := &TUI{
		autoScroll: true,
		scrollPos:  0,
		hScrollPos: 0,
		stopChan:   make(chan struct{}),
	}

	// Scroll right with right arrow
	app.handleKey("right")
	if app.hScrollPos != 8 {
		t.Errorf("expected hScrollPos 8, got %d", app.hScrollPos)
	}

	// Scroll right with vim 'l'
	app.handleKey("l")
	if app.hScrollPos != 16 {
		t.Errorf("expected hScrollPos 16, got %d", app.hScrollPos)
	}

	// Scroll left with left arrow
	app.handleKey("left")
	if app.hScrollPos != 8 {
		t.Errorf("expected hScrollPos 8, got %d", app.hScrollPos)
	}

	// Scroll left with vim 'h'
	app.handleKey("h")
	if app.hScrollPos != 0 {
		t.Errorf("expected hScrollPos 0, got %d", app.hScrollPos)
	}

	// Scroll left below 0 clamps to 0
	app.handleKey("left")
	if app.hScrollPos != 0 {
		t.Errorf("expected clamped hScrollPos 0, got %d", app.hScrollPos)
	}

	// Reset to 0 with '0' key
	app.handleKey("right")
	app.handleKey("right")
	if app.hScrollPos != 16 {
		t.Fatalf("expected hScrollPos 16, got %d", app.hScrollPos)
	}
	app.handleKey("0")
	if app.hScrollPos != 0 {
		t.Errorf("expected hScrollPos reset to 0, got %d", app.hScrollPos)
	}
}
