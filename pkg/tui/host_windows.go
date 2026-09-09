//go:build windows

package tui

import (
	"strings"
	"syscall"
	"unsafe"
)

// ansiCapableHosts are the console hosts known to render the TUI's ANSI escape
// sequences correctly. The legacy cmd.exe console is deliberately absent: it
// mangles the output, so callers fall back to plain console logging there.
var ansiCapableHosts = map[string]bool{
	"powershell.exe":      true,
	"pwsh.exe":            true,
	"windowsterminal.exe": true,
	"wt.exe":              true,
	"bash.exe":            true,
	"sh.exe":              true,
	"zsh.exe":             true,
	"fish.exe":            true,
	"alacritty.exe":       true,
	"wezterm-gui.exe":     true,
}

// HostSupportsTUI reports whether the shell that launched this process can
// render the TUI, along with the detected host executable name for logging.
//
// Detection is by parent process name rather than by environment variable:
// PSModulePath is a machine-wide variable that cmd.exe inherits too, so it
// cannot tell the two apart. An unknown or undetectable parent is treated as
// unsupported, matching the request that only PowerShell-class shells get the
// TUI.
func HostSupportsTUI() (bool, string) {
	name := parentProcessName()
	if name == "" {
		return false, "unknown"
	}
	return ansiCapableHosts[strings.ToLower(name)], name
}

// parentProcessName returns the executable file name of this process's parent,
// or "" if it cannot be determined. Parent PIDs can be recycled by Windows, so
// this is a best-effort heuristic used only to pick a UI mode.
func parentProcessName() string {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(snapshot)

	self := uint32(syscall.Getpid())
	parents := make(map[uint32]uint32) // pid -> ppid
	names := make(map[uint32]string)   // pid -> exe name

	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
		names[entry.ProcessID] = syscall.UTF16ToString(entry.ExeFile[:])
	}

	return names[parents[self]]
}
