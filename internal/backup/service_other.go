//go:build !windows

package backup

import "io"

// Main runs go-backup-tool. Windows service install/control (-service=...)
// and service-mode execution are only meaningful on Windows (see
// service_windows.go); on every other platform Main is just Run.
func Main(args []string, stderr io.Writer) int {
	return Run(args, stderr)
}
