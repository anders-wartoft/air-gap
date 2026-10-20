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
