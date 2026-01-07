//go:build !fuzz
package protocol

func shouldValidateChecksum() bool {
	return true
}

