package upstream

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestCalibrationDoesNotRecommendOnInvalidInput(t *testing.T) {
	tests := []struct {
		name, topic, extra, want string
	}{
		{"production topic", "events", "--rates=1000", "--topic=airgap-calibration"},
		{"ambiguous wire ID", "airgap-calibration_qa", "--rates=1000", "--topic=airgap-calibration"},
		{"rates out of order", "airgap-calibration-qa", "--rates=2000,1000", "strictly increasing"},
		{"client cert without key", "airgap-calibration-qa", "--kafka-cert-file=cert.pem", "certificate/key pair"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			err := calibrateUDP(context.Background(), []string{
				"--address=127.0.0.1:1234", "--brokers=127.0.0.1:9092", "--topic=" + tt.topic, tt.extra,
			}, &out, &diagnostics)
			if err == nil || !strings.Contains(err.Error(), tt.want) || out.Len() != 0 {
				t.Fatalf("err=%v stdout=%q", err, out.String())
			}
		})
	}
}

func TestCalibrationHelpKeepsStdoutEmpty(t *testing.T) {
	var out, diagnostics bytes.Buffer
	err := calibrateUDP(context.Background(), []string{"--help"}, &out, &diagnostics)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(diagnostics.String(), "--calibrate") || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, out.String(), diagnostics.String())
	}
}

func TestBatchPacingIsOnlyAUDPDefault(t *testing.T) {
	tests := []struct {
		name, transport, fallback string
		args                      []string
		want                      [3]int
		wantError                 bool
	}{
		{name: "default UDP", fallback: "23000,32200000,24", want: [3]int{23000, 32200000, 24}},
		{name: "TCP unchanged", transport: "tcp", fallback: "23000,32200000,24", want: [3]int{0, 0, 32}},
		{name: "explicit off", transport: "udp", fallback: "23000,32200000,24",
			args: []string{"--udpPacingDatagramsPerSecond=0", "--udpPacingBytesPerSecond=0"}, want: [3]int{0, 0, 32}},
		{name: "fixed caps", transport: "udp", fallback: "23000,32200000,24",
			args: []string{"--udpPacingDatagramsPerSecond=12000", "--udpPacingBytesPerSecond=14000000"}, want: [3]int{12000, 14000000, 32}},
		{name: "invalid default", transport: "udp", fallback: "23000,32200000,257", want: [3]int{0, 0, 32}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := DefaultConfiguration()
			config.transport = tt.transport
			config = parseCommandLineOverrides(tt.args, config)
			err := config.applyTestUDPPacing(tt.fallback)
			got := [3]int{config.udpPacingDatagramsPerSecond, config.udpPacingBytesPerSecond, config.udpPacingBurstDatagrams}
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("pacing=%v, err=%v; want %v, error=%v", got, err, tt.want, tt.wantError)
			}
		})
	}
}
