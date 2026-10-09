package upstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedTLSAgingConfiguration(t *testing.T) {
	if DefaultConfiguration().tcpTLSPinIdleSeconds != 0 {
		t.Fatal("aging must be disabled by default")
	}
	path := filepath.Join(t.TempDir(), "upstream.properties")
	for _, value := range []string{"-1", "1.5", "invalid", "2147483648"} {
		if err := os.WriteFile(path, []byte("tcpTLSPinIdleSeconds="+value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadParameters(path, DefaultConfiguration()); err == nil {
			t.Fatalf("invalid idle seconds accepted: %s", value)
		}
	}
	if err := os.WriteFile(path, []byte("tcpTLSPinIdleSeconds=30"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadParameters(path, DefaultConfiguration())
	if err != nil || c.tcpTLSPinIdleSeconds != 30 {
		t.Fatalf("file aging setting ignored: %v", err)
	}
	if err := c.validateTLSMode(); err == nil {
		t.Fatal("aging accepted in CA mode")
	}
	t.Setenv("AIRGAP_UPSTREAM_TCP_TLS_PIN_IDLE_SECONDS", "60")
	c = overrideConfiguration(c)
	if c.tcpTLSPinIdleSeconds != 60 {
		t.Fatal("environment aging setting ignored")
	}
	c = parseCommandLineOverrides([]string{"--tcpTLSPinIdleSeconds=0"}, c)
	if c.tcpTLSPinIdleSeconds != 0 {
		t.Fatal("CLI could not disable aging")
	}
}
