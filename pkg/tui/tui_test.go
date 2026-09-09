package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server"
	"github.com/vogler75/babel-gate/pkg/session"
)

func TestStripANSI(t *testing.T) {
	colored := "\033[32mhello\033[0m \033[1mworld\033[0m"
	plain := stripANSI(colored)
	if plain != "hello world" {
		t.Errorf("expected 'hello world', got %q", plain)
	}
}

func TestProviderSelectionAndToggle(t *testing.T) {
	engine, err := router.NewEngine(&config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {Type: "openai"},
		"google": {Type: "google"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	app := &TUI{engine: engine, stopChan: make(chan struct{})}
	states := engine.GetProviderStates()
	if len(states) != 2 || !states[0].Enabled {
		t.Fatalf("unexpected initial provider states: %+v", states)
	}

	app.handleKey(" ")
	states = engine.GetProviderStates()
	if states[0].Enabled {
		t.Fatalf("space should disable selected provider: %+v", states)
	}
	lines := app.getSortedProvidersInfo()
	if len(lines) != 2 || !strings.Contains(stripANSI(lines[0]), "Disabled") {
		t.Fatalf("disabled provider not shown in TUI: %q", lines)
	}

	app.handleKey("down")
	if app.providerSelection != 1 {
		t.Fatalf("down should select next provider, got %d", app.providerSelection)
	}
	app.handleKey("\t")
	if app.activePane != paneSessions {
		t.Fatalf("tab should focus sessions, got pane %d", app.activePane)
	}
	app.handleKey("\t")
	if app.activePane != paneLogs {
		t.Fatalf("second tab should focus logs, got pane %d", app.activePane)
	}
	app.handleKey("backtab")
	if app.activePane != paneSessions {
		t.Fatalf("shift-tab should focus previous pane, got pane %d", app.activePane)
	}
}

func TestReloadRoutesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := "providers:\n  openai:\n    type: openai\nrouting:\n  routes:\n    fast: openai/gpt-4o-mini\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updated := "providers:\n  openai:\n    type: openai\nrouting:\n  routes:\n    fast: openai/gpt-4.1\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	app := &TUI{engine: engine, stopChan: make(chan struct{})}
	app.handleKey("r")
	route, err := engine.ResolveModel("fast")
	if err != nil {
		t.Fatal(err)
	}
	if route.TargetModel != "gpt-4.1" {
		t.Fatalf("R did not reload the updated route: got %q", route.TargetModel)
	}
}

func TestSessionsViewShowsTokenSpeed(t *testing.T) {
	cfg := &config.Config{Database: config.DatabaseConfig{Path: t.TempDir() + "/metrics.db"}}
	engine, err := router.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(cfg, engine)
	defer srv.Metrics().Close()
	sess := srv.Sessions().GetOrCreate("session-1", "127.0.0.1", "test", "Test Client")
	srv.Sessions().RecordRequest(sess.ID, session.RequestRecord{
		Model: "test-model", DurationMs: 2000, GenerationDurationMs: 1000,
		InputTokens: 42, OutputTokens: 25, TotalTokens: 67, Status: "success",
	})
	second := srv.Sessions().GetOrCreate("session-2", "127.0.0.2", "test", "Other Client")
	srv.Sessions().RecordRequest(second.ID, session.RequestRecord{
		Model: "other-model", DurationMs: 1000, OutputTokens: 10, TotalTokens: 10, Status: "success",
	})

	app := New(srv, engine, nil)
	app.handleKey("\t")
	if app.activePane != paneSessions {
		t.Fatal("tab should focus the sessions pane")
	}
	lines := app.getSessionsInfo(8)
	plainLines := stripANSI(strings.Join(lines, "\n"))
	if len(lines) != 2 || !strings.Contains(plainLines, "25.0 tok/s") || !strings.Contains(plainLines, "Ctx:42") {
		t.Fatalf("session token telemetry not rendered: %q", lines)
	}
	var sessionOneLine string
	for _, line := range lines {
		plain := strings.TrimSpace(stripANSI(line))
		if strings.Contains(plain, "ID:session-1") {
			sessionOneLine = plain
			break
		}
	}
	if !strings.HasSuffix(sessionOneLine, "ID:session-1") || strings.Index(sessionOneLine, "Test Client") > strings.Index(sessionOneLine, "ID:session-1") {
		t.Fatalf("session ID should be the last column: %q", sessionOneLine)
	}
	app.handleKey("down")
	if app.sessionSelection != 1 {
		t.Fatalf("down should select the next session, got %d", app.sessionSelection)
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
		activePane: paneLogs,
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

	app.handleKey("up")
	if app.scrollPos != 1 || app.autoScroll {
		t.Fatalf("up in logs should pause and scroll, pos=%d auto=%v", app.scrollPos, app.autoScroll)
	}
	app.handleKey("down")
	if app.scrollPos != 0 || !app.autoScroll {
		t.Fatalf("down at log bottom should restore auto-scroll, pos=%d auto=%v", app.scrollPos, app.autoScroll)
	}
}
