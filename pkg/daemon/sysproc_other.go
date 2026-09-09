//go:build !windows

package daemon

import "syscall"

// Setsid detaches the child from the controlling terminal, so closing the
// terminal or hitting Ctrl-C does not take the gateway down with it.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
