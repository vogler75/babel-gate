//go:build windows

package tui

import (
	"os"
)

func notifyWinch(ch chan<- os.Signal) {
	// Windows does not support SIGWINCH signals
}
