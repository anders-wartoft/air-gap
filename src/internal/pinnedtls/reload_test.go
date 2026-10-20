package pinnedtls

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateReloadFileComments(t *testing.T) {
	for _, test := range []struct {
		name, contents string
		wantError      bool
	}{
		{"comments", "  # privateKeyFiles=\nid=tcp#1 # output\n", false},
		{"equals_only_in_comment", "invalid # ignored=value\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.properties")
			if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
				t.Fatal(err)
			}
			err := ValidateReloadFile(path)
			if (err != nil) != test.wantError {
				t.Fatalf("ValidateReloadFile() = %v, want error=%t", err, test.wantError)
			}
		})
	}
}
