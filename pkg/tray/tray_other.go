//go:build !windows

package tray

import "context"

func supported() bool { return false }

func hideConsole() {}

func run(ctx context.Context, opts Options) error {
	return ErrUnsupported
}
