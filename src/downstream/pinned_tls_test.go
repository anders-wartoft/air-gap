package downstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sitia.nu/airgap/src/internal/pinnedtest"
)

func TestPinnedTLSGenerationHelper(t *testing.T) {
	if dir := os.Getenv("AIRGAP_PINNED_GENERATE_DIR"); dir != "" {
		os.Args = []string{"downstream", "--generate-tls-key", "--tls-key-output-dir=" + dir,
			"--tls-key-name=identity", "--tls-key-role=server",
			"--json-file-dir=" + filepath.Join(dir, "public-entries")}
		Main("test")
		os.Exit(0)
	}
}

func TestPinnedTLSGeneration(t *testing.T) {
	pinnedtest.Generation(t, "TestPinnedTLSGenerationHelper", "server")
}

func TestPinnedTLSAuthentication(t *testing.T) {
	pinnedtest.Authentication(t, pinnedTLSFixture, true)
}

func TestPinnedTLSCACompatibility(t *testing.T) {
	pinnedtest.CACompatibility(t, pinnedTLSFixture, true)
}

func pinnedTLSFixture(t *testing.T, properties string) any {
	t.Helper()
	defaults := defaultConfiguration()
	if _, ok := any(&defaults).(pinnedtest.ConfigBuilder); !ok {
		t.Fatal("REQ-51 pending: configuration must implement BuildTLSConfig() (*tls.Config, error)")
	}

	path := filepath.Join(t.TempDir(), "downstream.properties")
	if err := os.WriteFile(path, []byte(properties), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readConfiguration(path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	return &config
}

func TestPinnedTLSConfigurationPrecedence(t *testing.T) {
	c := defaultConfiguration()
	if c.tcpTLSAuthMode != "ca" {
		t.Fatal("CA mode must remain the default")
	}

	t.Setenv("AIRGAP_DOWNSTREAM_TCP_TLS_AUTH_MODE", "pinned")
	t.Setenv("AIRGAP_DOWNSTREAM_TCP_TLS_TRUSTED_KEYS_DIR", "/environment")
	c = overrideConfiguration(c)
	if c.tcpTLSAuthMode != "pinned" || c.tcpTLSTrustedKeysDir != "/environment" {
		t.Fatal("pinned TLS environment settings were ignored")
	}
	c = parseCommandLineOverrides([]string{"--tcpTLSAuthMode=ca", "--tcpTLSTrustedKeysDir="}, c)
	if c.tcpTLSAuthMode != "ca" || c.tcpTLSTrustedKeysDir != "" {
		t.Fatal("command-line settings did not override environment")
	}
}

func TestConfigurationComments(t *testing.T) {
	for _, strict := range []bool{false, true} {
		mode := "startup"
		if strict {
			mode = "reload"
		}
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "downstream.properties")
			contents := "# Managed configuration\n\n  \t\n# privateKeyFiles=\n" +
				"  # tcpTLSAuthMode=invalid\n\t# unknownSetting=value\n" +
				"  id=tcp#1  # TCP output\n tcpTLSAuthMode=pinned\t# TLS mode\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := readConfigurationFile(path, defaultConfiguration(), strict)
			if err != nil {
				t.Fatal(err)
			}
			if config.id != "tcp#1" || config.tcpTLSAuthMode != "pinned" {
				t.Fatalf("active settings not parsed: id=%q, mode=%q", config.id, config.tcpTLSAuthMode)
			}
		})
	}
	t.Run("reload_rejects_uncommented_unknown_key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "downstream.properties")
		if err := os.WriteFile(path, []byte("unknownSetting=value\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readConfigurationFile(path, defaultConfiguration(), true)
		if err == nil || !strings.Contains(err.Error(), "unknown configuration key: unknownSetting") {
			t.Fatalf("expected unknown-key error, got %v", err)
		}
	})
}
