package resend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resend.properties")
	contents := "# Configuration\n  # targetPort=invalid\n\n" +
		"id=tcp#1  # TCP output\ntargetPort=1234\t# destination\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readParameters(path, defaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if config.id != "tcp#1" || config.targetPort != 1234 {
		t.Fatalf("unexpected configuration: id=%q, targetPort=%d", config.id, config.targetPort)
	}
}
