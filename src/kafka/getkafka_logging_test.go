package kafka

import (
	"bytes"
	"encoding/pem"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sitia.nu/airgap/src/logging"
)

func TestLoadTLSCertificateRedactsPasswordFilePath(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "encrypted-key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "ENCRYPTED PRIVATE KEY",
		Bytes: []byte("not-needed-for-password-file-read"),
	})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(t.TempDir(), "sensitive-password-file-name")

	_, err := LoadTLSCertificate("unused-cert.pem", keyFile, passwordFile)
	if err == nil {
		t.Fatal("LoadTLSCertificate() succeeded with a missing password file")
	}
	if strings.Contains(err.Error(), passwordFile) {
		t.Fatalf("LoadTLSCertificate() error exposes password file path: %q", err)
	}
	if !strings.Contains(err.Error(), "file does not exist") {
		t.Fatalf("LoadTLSCertificate() error = %q, want redacted not-found classification", err)
	}
}

func TestSetTLSConfigParametersReturnsRedactedPasswordFileError(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "encrypted-key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "ENCRYPTED PRIVATE KEY",
		Bytes: []byte("not-needed-for-password-file-read"),
	})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(t.TempDir(), "sensitive-password-file-name")

	_, err := SetTLSConfigParameters("unused-cert.pem", keyFile, "unused-ca.pem", passwordFile)
	if err == nil {
		t.Fatal("SetTLSConfigParameters() succeeded with a missing password file")
	}
	if strings.Contains(err.Error(), passwordFile) {
		t.Fatalf("SetTLSConfigParameters() error exposes password file path: %q", err)
	}
	if !strings.Contains(err.Error(), "file does not exist") {
		t.Fatalf("SetTLSConfigParameters() error = %q, want redacted not-found classification", err)
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

	writer := &saramaLogWriter{originalWriter: logging.StdLogger.Writer()}
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

	writer := &saramaLogWriter{originalWriter: &output}
	const historical = "[SARAMA] 2026/10/13 11:00:00 connected\n"
	if _, err := writer.Write([]byte(historical)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := output.String(); got != historical {
		t.Fatalf("text-mode Sarama output changed:\n got %q\nwant %q", got, historical)
	}
}
