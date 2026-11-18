package upstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	contents := "# Configuration\n  # eps=invalid\n\n" +
		"id=tcp#1  # TCP output\neps=10\t# events per second\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	config, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if config.id != "tcp#1" || config.eps != 10 {
		t.Fatalf("unexpected configuration: id=%q, eps=%v", config.id, config.eps)
	}
}

func TestUDPPacingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte("transport=udp\neps=10\nudpPacingDatagramsPerSecond=12000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.validateUDPPacing(); err == nil {
		t.Fatal("accepted only one configured rate")
	}
	t.Setenv("AIRGAP_UPSTREAM_UDPPACINGBYTESPERSECOND", "14000000")
	config = overrideConfiguration(config)
	config = parseCommandLineOverrides([]string{"--udpPacingBurstDatagrams=16"}, config)
	if err := config.validateUDPPacing(); err != nil {
		t.Fatal(err)
	}
	if config.eps != 10 || config.udpPacingDatagramsPerSecond != 12000 ||
		config.udpPacingBytesPerSecond != 14000000 || config.udpPacingBurstDatagrams != 16 {
		t.Fatal("pacing settings not applied independently of eps")
	}
	config.transport = "tcp"
	if err := config.validateUDPPacing(); err == nil {
		t.Fatal("accepted UDP pacing for TCP")
	}
}

func TestUDPPacingEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte("transport=udp\nudpPacingDatagramsPerSecond=12000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := ReadParameters(path, DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIRGAP_UPSTREAM_UDPPACINGDATAGRAMSPERSECOND", "13000")
	t.Setenv("AIRGAP_UPSTREAM_UDP_PACING_DATAGRAMS_PER_SECOND", "29000")
	t.Setenv("AIRGAP_UPSTREAM_UDP_PACING_BYTES_PER_SECOND", "40600000")
	t.Setenv("AIRGAP_UPSTREAM_UDP_PACING_BURST_DATAGRAMS", "48")
	config = overrideConfiguration(config)
	if config.udpPacingDatagramsPerSecond != 29000 || config.udpPacingBytesPerSecond != 40600000 ||
		config.udpPacingBurstDatagrams != 48 {
		t.Fatal("canonical environment variables did not override property and legacy environment values")
	}
	config = parseCommandLineOverrides([]string{"--udpPacingDatagramsPerSecond=30000"}, config)
	if err := config.applyTestUDPPacing("20000,28000000,32"); err != nil {
		t.Fatal(err)
	}
	if config.udpPacingDatagramsPerSecond != 30000 || config.udpPacingBytesPerSecond != 40600000 ||
		config.udpPacingBurstDatagrams != 48 {
		t.Fatal("CLI did not take precedence or test defaults replaced environment values")
	}

	t.Setenv("AIRGAP_UPSTREAM_UDP_PACING_DATAGRAMS_PER_SECOND", "0")
	t.Setenv("AIRGAP_UPSTREAM_UDP_PACING_BYTES_PER_SECOND", "0")
	config = overrideConfiguration(config)
	if err := config.validateUDPPacing(); err != nil || config.udpPacingDatagramsPerSecond != 0 ||
		config.udpPacingBytesPerSecond != 0 {
		t.Fatalf("zero rates should disable pacing: %v", err)
	}
}
