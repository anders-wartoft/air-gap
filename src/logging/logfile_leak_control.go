//go:build airgap_logrotate_leak

package logging

import "os"

// This file is built only with the non-default airgap_logrotate_leak tag and
// must never be used for a release build. It restores the descriptor leak that
// TC-35 guards against, so the negative control can prove that the TC-35
// descriptor checks actually detect the defect rather than passing by chance.
func closeReplacedLogFile(previous *os.File) error {
	_ = previous
	return nil
}
