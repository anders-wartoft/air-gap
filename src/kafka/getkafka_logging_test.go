package kafka

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sitia.nu/airgap/src/logging"
)

func TestKeyPasswordFileReadErrorIsStatic(t *testing.T) {
	err := keyPasswordFileReadError(os.ErrNotExist)
	if err.Error() != "failed to read key password file: file does not exist" {
		t.Fatal("key password file error lacks the expected static classification")
	}
}

func TestREQ5207SaramaUsesOTLPApplicationLogger(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sarama.log")
	if err := logging.Logger.SetLogFile(logPath); err != nil {
		t.Fatalf("SetLogFile: %v", err)
	}
	t.Cleanup(func() {
		_ = logging.Logger.SetFormat(logging.FormatText)
	})

	if err := logging.Logger.SetFormat(logging.FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	writer := &saramaLogWriter{}
	saramaLogger := log.New(writer, "[SARAMA] ", 0)
	saramaLogger.Print("connected")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, `{"resourceLogs":`) {
		t.Fatalf("Sarama record did not use OTLP application logger: %q", line)
	}
	if !strings.Contains(line, `"stringValue":"[SARAMA] connected"`) {
		t.Fatalf("Sarama message was not preserved: %q", line)
	}
}

func TestREQ5201SaramaTextOutputRemainsUnchangedByDefault(t *testing.T) {
	var output bytes.Buffer
	t.Cleanup(func() {
		_ = logging.Logger.SetFormat(logging.FormatText)
	})

	if err := logging.Logger.SetFormat(logging.FormatText); err != nil {
		t.Fatalf("SetFormat(text): %v", err)
	}

	logging.StdLogger.SetOutput(&output)
	t.Cleanup(func() {
		logging.StdLogger.SetOutput(os.Stderr)
	})

	writer := &saramaLogWriter{}
	const historical = "[SARAMA] 2026/10/13 11:00:00 connected\n"
	if _, err := writer.Write([]byte(historical)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := output.String(); got != historical {
		t.Fatalf("text-mode Sarama output changed:\n got %q\nwant %q", got, historical)
	}
}

func TestSaramaTextOutputFollowsLogRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sarama.log")
	rotatedPath := filepath.Join(dir, "sarama.log.1")

	if err := logging.Logger.SetFormat(logging.FormatText); err != nil {
		t.Fatalf("SetFormat(text): %v", err)
	}
	if err := logging.Logger.SetLogFile(logPath); err != nil {
		t.Fatalf("SetLogFile(first): %v", err)
	}

	writer := &saramaLogWriter{}
	if err := os.Rename(logPath, rotatedPath); err != nil {
		t.Fatalf("rotate log file: %v", err)
	}
	if err := logging.Logger.SetLogFile(logPath); err != nil {
		t.Fatalf("SetLogFile(second): %v", err)
	}

	const message = "[SARAMA] after rotation\n"
	if _, err := writer.Write([]byte(message)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(new log): %v", err)
	}
	if string(raw) != message {
		t.Fatalf("Sarama output was not redirected to the new log:\n got %q\nwant %q", raw, message)
	}
	rotated, err := os.ReadFile(rotatedPath)
	if err != nil {
		t.Fatalf("ReadFile(rotated log): %v", err)
	}
	if string(rotated) != "" {
		t.Fatalf("Sarama output was written to the rotated log: %q", rotated)
	}
}
