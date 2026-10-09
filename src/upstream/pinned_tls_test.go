package upstream

import (
	"os"
	"path/filepath"
	"testing"

	"sitia.nu/airgap/src/internal/pinnedtest"
)

func TestPinnedTLSGenerationHelper(t *testing.T) {
	if dir := os.Getenv("AIRGAP_PINNED_GENERATE_DIR"); dir != "" {
		os.Args = []string{"upstream", "--generate-tls-keysets=2", "--tls-key-output-dir=" + dir,
			"--tls-key-name=identity", "--tls-key-role=client"}
		Main("test")
		os.Exit(0)
	}
}

func TestPinnedTLSGeneration(t *testing.T) {
	pinnedtest.Generation(t, "TestPinnedTLSGenerationHelper", "client")
}

func TestPinnedTLSAuthentication(t *testing.T) {
	pinnedtest.Authentication(t, pinnedTLSFixture, false)
}

func TestPinnedTLSCACompatibility(t *testing.T) {
	pinnedtest.CACompatibility(t, pinnedTLSFixture, false)
}

func pinnedTLSFixture(t *testing.T, properties string) any {
	t.Helper()
	defaults := DefaultConfiguration()
	if _, ok := any(&defaults).(pinnedtest.ConfigBuilder); !ok {
		t.Fatal("REQ-51 pending: configuration must implement BuildTLSConfig() (*tls.Config, error)")
	}

	path := filepath.Join(t.TempDir(), "upstream.properties")
	if err := os.WriteFile(path, []byte(properties), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := ReadParameters(path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	return &config
}

func TestPinnedTLSConfigurationPrecedence(t *testing.T) {
	c := DefaultConfiguration()
	if c.tcpTLSAuthMode != "ca" {
		t.Fatal("CA mode must remain the default")
	}
	t.Setenv("AIRGAP_UPSTREAM_TCP_TLS_AUTH_MODE", "pinned")
	t.Setenv("AIRGAP_UPSTREAM_TCP_TLS_TRUSTED_KEYS_DIR", "/environment")
	c = overrideConfiguration(c)
	if c.tcpTLSAuthMode != "pinned" || c.tcpTLSTrustedKeysDir != "/environment" {
		t.Fatal("pinned TLS environment settings were ignored")
	}
	c = parseCommandLineOverrides([]string{"--tcpTLSAuthMode=ca", "--tcpTLSTrustedKeysDir="}, c)
	if c.tcpTLSAuthMode != "ca" || c.tcpTLSTrustedKeysDir != "" {
		t.Fatal("command-line settings did not override environment")
	}
}
