//go:build windows

package daemon

import "syscall"

const (
	// The child gets no console of its own, and is excluded from Ctrl-C /
	// Ctrl-Break sent to the parent's process group.
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}
