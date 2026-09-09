// Package daemon re-launches the current executable as a detached background
// process, so a flag like -tray can return the terminal to the user instead of
// occupying it for the lifetime of the gateway.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
)

// envMarker is set on the detached child so it knows not to detach again.
const envMarker = "BABELGATE_DAEMON"

// IsChild reports whether this process is the detached child spawned by Detach.
func IsChild() bool {
	return os.Getenv(envMarker) == "1"
}

// Detach starts a copy of this process with the same arguments and working
// directory, fully disconnected from the current terminal, and returns its PID.
// The caller is expected to exit immediately afterwards.
//
// Standard streams are pointed at the null device, so the child must already
// log to a file: anything written to stdout or stderr after this is discarded.
func Detach() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locating executable: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envMarker+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = sysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting background process: %w", err)
	}

	pid := cmd.Process.Pid
	// Release so the child is never left as a zombie waiting to be reaped.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("releasing background process: %w", err)
	}
	return pid, nil
}
