//go:build !airgap_logrotate_leak

package logging

import "os"

// closeReplacedLogFile releases the descriptor that SetLogFile just replaced.
// Without it, every logrotate reload leaves an open descriptor to a rotated
// file, and the disk space of a deleted log is never returned.
func closeReplacedLogFile(previous *os.File) error {
	return previous.Close()
}
