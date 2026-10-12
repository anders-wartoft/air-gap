package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestTransportServicesReloadMainProcess(t *testing.T) {
	for _, name := range []string{"airgap-upstream@.service", "airgap-downstream@.service"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			section := ""
			var reloads []string
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					section = line
					continue
				}
				if section == "[Service]" && strings.HasPrefix(line, "ExecReload=") {
					reloads = append(reloads, strings.TrimPrefix(line, "ExecReload="))
				}
			}
			if len(reloads) != 1 || reloads[0] != "/bin/kill -HUP $MAINPID" {
				t.Fatalf("reload must send SIGHUP to the main process: got %q", reloads)
			}
		})
	}
}
