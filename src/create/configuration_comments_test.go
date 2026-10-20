package create

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "create.properties")
	contents := "# Configuration\n  # topic=ignored\n\n" +
		"topic=tcp#1  # source topic\nlogLevel=INFO\t# verbosity\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readParameters(path, defaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if config.topic != "tcp#1" || config.logLevel != "INFO" {
		t.Fatalf("unexpected configuration: topic=%q, logLevel=%q", config.topic, config.logLevel)
	}
}
