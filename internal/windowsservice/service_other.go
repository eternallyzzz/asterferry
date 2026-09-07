//go:build !windows

// Package windowsservice runs daemons as console processes or Windows services.
package windowsservice

import "context"

// Run executes the daemon directly on platforms without the Windows Service
// Control Manager.
func Run(ctx context.Context, _ string, run func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return run(ctx)
}
