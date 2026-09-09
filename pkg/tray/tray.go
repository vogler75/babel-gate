// Package tray provides an optional system tray / notification area icon so a
// background BabelGate instance can be reached without hunting for its PID.
//
// Only Windows has a real implementation; every other platform compiles to a
// no-op that reports Supported() == false. The implementation is pure Go
// (syscall against user32/shell32) so CGO_ENABLED=0 cross-builds keep working.
package tray

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by Run on platforms without a tray implementation.
var ErrUnsupported = errors.New("tray: not supported on this platform")

// Options configures the tray icon and its menu.
type Options struct {
	// Tooltip is shown when hovering the icon, e.g. "BabelGate - :8080".
	Tooltip string
	// DashboardURL is opened by the "Open Dashboard" menu item.
	DashboardURL string
	// OnQuit is invoked when the user picks "Quit". Run returns afterwards.
	OnQuit func()
}

// Run displays the tray icon and blocks until the user quits via the menu or
// ctx is cancelled. It returns ErrUnsupported where no implementation exists.
func Run(ctx context.Context, opts Options) error {
	return run(ctx, opts)
}

// Supported reports whether this build has a working tray implementation.
func Supported() bool {
	return supported()
}

// HideConsole detaches from the console window when this process is its sole
// owner, which is the case when launched from Explorer or a shortcut. Running
// from an existing terminal leaves that terminal untouched. It is a no-op on
// platforms without a tray. Call it only once logging has been redirected to a
// file, since afterwards stdout and stderr go nowhere.
func HideConsole() {
	hideConsole()
}
