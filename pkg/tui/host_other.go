//go:build !windows

package tui

// HostSupportsTUI reports whether the shell that launched this process can
// render the TUI. Everywhere except Windows the answer is always yes, so the
// returned host name is empty.
func HostSupportsTUI() (bool, string) {
	return true, ""
}
