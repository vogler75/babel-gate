//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyWinch(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}
