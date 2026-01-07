package downstream

import (
	"testing"
)

// OBS: Before running the fuzzer, disable the checksum test in parser.go ParseMessage
func FuzzUdpMessageHandler(f *testing.F) {
	f.Fuzz(func(t *testing.T, packet []byte){
		kafkaWriter = NewNullAdapter()
		handleUdpMessage(packet)
	})
}

