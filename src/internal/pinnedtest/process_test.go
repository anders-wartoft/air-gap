package pinnedtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type applicationHarness struct {
	binaries string
	network  string
	address  string
}

type applicationProcess struct {
	cmd        *exec.Cmd
	done       chan struct{}
	err        error
	log        string
	configPath string
}

func newApplicationHarness(t *testing.T) *applicationHarness {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate project root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-o", dir, "./src/cmd/upstream", "./src/cmd/downstream")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build application binaries: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	nic := "lo"
	if runtime.GOOS == "darwin" {
		nic = "lo0"
	}
	return &applicationHarness{
		binaries: dir,
		network:  fmt.Sprintf("id=tc27\nnic=%s\ntargetIP=127.0.0.1\ntargetPort=%d\n", nic, port),
		address:  fmt.Sprintf("127.0.0.1:%d", port),
	}
}

func (h *applicationHarness) start(t *testing.T, role, configuration string, overrides ...string) *applicationProcess {
	t.Helper()
	return h.startWithEnvironment(t, role, configuration, nil, overrides...)
}

func (h *applicationHarness) startWithEnvironment(t *testing.T, role, configuration string, environment []string, overrides ...string) *applicationProcess {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, role+".properties")
	write(t, path, []byte(h.network+configuration))
	logPath := filepath.Join(dir, role+".log")
	output, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{path}
	args = append(args, overrides...)
	cmd := exec.Command(filepath.Join(h.binaries, role), args...)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "AIRGAP_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, environment...)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		output.Close()
		t.Fatal(err)
	}
	p := &applicationProcess{cmd: cmd, done: make(chan struct{}), log: logPath, configPath: path}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Errorf("terminate %s: %v", role, err)
			}
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				if err := cmd.Process.Kill(); err != nil {
					t.Errorf("kill %s: %v", role, err)
				}
				<-p.done
				t.Errorf("%s shutdown timed out", role)
			}
		}
		if err := output.Close(); err != nil {
			t.Errorf("close %s output: %v", role, err)
		}
		if p.err != nil {
			t.Errorf("%s exited unsuccessfully: %v\n%s", role, p.err, p.read(t))
		}
	})
	return p
}

func (p *applicationProcess) read(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(p.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (p *applicationProcess) wait(t *testing.T, marker string, offset int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		log := p.read(t)
		if strings.Contains(log[offset:], marker) {
			return log[offset:]
		}
		select {
		case <-p.done:
			t.Fatalf("process exited before %q: %v\n%s", marker, p.err, log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q\n%s", marker, log[offset:])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *applicationProcess) reload(t *testing.T) string {
	t.Helper()
	offset := len(p.read(t))
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	return p.wait(t, "SIGHUP handling completed", offset)
}

func requireReloadSuccess(t *testing.T, log string) {
	t.Helper()
	if strings.Contains(log, "TLS certificate reload failed") ||
		strings.Contains(log, "not implemented") || !strings.Contains(log, "Pinned TLS reload:") {
		t.Fatalf("valid pinned lifecycle reload rejected:\n%s", log)
	}
}
