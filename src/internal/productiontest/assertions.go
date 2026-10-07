// Package productiontest provides test-only assertions for REQ-50.
package productiontest

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"sitia.nu/airgap/src/logging"
)

type warner interface {
	WarnProductionConfiguration(phase string)
}

// Lifecycle runs a helper in an isolated process so global application state
// and termination signals cannot affect the test runner.
func Lifecycle(t *testing.T, helper, configuration string, signal os.Signal, fileLog ...bool) {
	t.Helper()
	dir := t.TempDir()
	configPath := dir + "/application.properties"
	if runtime.GOOS == "linux" {
		configuration = strings.ReplaceAll(configuration, "nic=lo0", "nic=lo")
	}
	if err := os.WriteFile(configPath, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := dir + "/application.log"
	if len(fileLog) > 0 && fileLog[0] {
		configuration += "logFileName=" + logPath + "\n"
		if err := os.WriteFile(configPath, []byte(configuration), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outputPath := logPath
	if len(fileLog) > 0 && fileLog[0] {
		outputPath = dir + "/console.log"
	}
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^"+helper+"$")
	cmd.Env = append(os.Environ(), "AIRGAP_PRODUCTION_HELPER="+configPath)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if signal != nil {
		deadline := time.Now().Add(10 * time.Second)
		for {
			data, err := os.ReadFile(logPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "phase=startup ") {
				// Allow the daemon to install its signal handler and start workers.
				time.Sleep(300 * time.Millisecond)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("startup warning timeout: %s", data)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := cmd.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			data, _ := os.ReadFile(logPath)
			t.Fatalf("lifecycle failed: %v\n%s", err, data)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("orderly shutdown did not finish")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var startup, shutdown []string
	inShutdown := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "[PRODUCTION-CONFIG] phase=startup ") {
			startup = append(startup, strings.SplitN(line, " setting=", 2)[1])
		}
		if strings.Contains(line, "[PRODUCTION-CONFIG] phase=shutdown ") {
			inShutdown = true
			shutdown = append(shutdown, strings.SplitN(line, " setting=", 2)[1])
		} else if inShutdown {
			t.Errorf("application log after shutdown warning block: %s", line)
		}
	}
	if len(startup) == 0 || !reflect.DeepEqual(startup, shutdown) {
		t.Fatalf("startup/shutdown warning mismatch: startup=%v shutdown=%v\n%s", startup, shutdown, data)
	}
}

func Check(t *testing.T, config any, expected map[string]string) {
	t.Helper()
	w, ok := config.(warner)
	if !ok {
		t.Fatal("REQ-50 pending: configuration must implement WarnProductionConfiguration(phase string)")
	}
	before := reflect.ValueOf(config).Elem().Interface()
	for _, phase := range []string{"startup", "shutdown"} {
		for _, level := range []string{"INFO", "ERROR", "FATAL"} {
			t.Run(phase+"/"+level, func(t *testing.T) {
				output := Capture(t, level, func() { w.WarnProductionConfiguration(phase) })
				AssertOutput(t, output, phase, expected)
				if !reflect.DeepEqual(before, reflect.ValueOf(config).Elem().Interface()) {
					t.Error("production warning evaluation changed configuration")
				}
			})
		}
	}
}

func Capture(t *testing.T, level string, run func()) string {
	t.Helper()
	oldOutput := logging.StdLogger.Writer()
	oldLevel := logging.Logger.GetLogLevel()
	defer logging.StdLogger.SetOutput(oldOutput)
	defer logging.Logger.SetLogLevel(oldLevel)
	var output bytes.Buffer
	logging.StdLogger.SetOutput(&output)
	logging.Logger.SetLogLevel(level)
	run()
	if got := logging.Logger.GetLogLevel(); got != level {
		t.Errorf("warning emission changed logLevel from %s to %s", level, got)
	}
	return output.String()
}

func AssertOutput(t *testing.T, output, phase string, expected map[string]string) {
	t.Helper()
	seen := make(map[string]int)
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		marker := "[WARN] [PRODUCTION-CONFIG] phase=" + phase + " setting="
		_, message, ok := strings.Cut(line, marker)
		if !ok {
			t.Errorf("unexpected warning event: %s", line)
			continue
		}
		setting, valueAndRisk, ok := strings.Cut(message, " value=")
		if !ok {
			t.Errorf("missing setting/value envelope: %s", line)
			continue
		}
		value, risk, ok := strings.Cut(valueAndRisk, " risk=")
		if !ok || len(strings.TrimSpace(risk)) < 10 {
			t.Errorf("missing actionable risk explanation: %s", line)
		}
		want, exists := expected[setting]
		if !exists || value != want {
			t.Errorf("unexpected setting/value %s=%s (expected %v)", setting, value, expected)
		}
		seen[setting]++
	}
	for setting := range expected {
		if seen[setting] != 1 {
			t.Errorf("expected exactly one %s warning, got %d; output: %s", setting, seen[setting], output)
		}
	}
}

func CheckStartup(t *testing.T, validate func(), expected map[string]string) {
	t.Helper()
	output := Capture(t, "INFO", validate)
	first := strings.Index(output, "[WARN] [PRODUCTION-CONFIG]")
	if first < 0 {
		t.Fatalf("REQ-50 pending: checkConfiguration emitted no production warnings; output: %s", output)
	}
	lineStart := strings.LastIndex(output[:first], "\n") + 1
	AssertOutput(t, output[lineStart:], "startup", expected)
}
