package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests trace directly to REQ-52. Hash-chain and signature behavior have
// their own focused tests because integrity is optional and independent of the
// OTLP format contract.

func TestREQ5201TextRemainsTheDefault(t *testing.T) {
	buf := resetLogging(t)

	Logger.Info(Event(EventDiagnostic), "legacy default")

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, "[INFO] legacy default") {
		t.Fatalf("default output no longer uses the historical text layout: %q", line)
	}
	if json.Valid([]byte(line)) {
		t.Fatalf("default output unexpectedly became JSON: %q", line)
	}
	if Logger.GetFormat() != FormatText {
		t.Fatalf("default format = %q, want %q", Logger.GetFormat(), FormatText)
	}
}

func TestREQ5201EmptyFormatMeansTextAndAliasesSelectOTLP(t *testing.T) {
	resetLogging(t)

	for _, value := range []string{"", "text", " TEXT "} {
		if err := Logger.SetFormat(value); err != nil {
			t.Fatalf("SetFormat(%q): %v", value, err)
		}
		if got := Logger.GetFormat(); got != FormatText {
			t.Fatalf("SetFormat(%q) selected %q, want text", value, got)
		}
	}

	for _, value := range []string{"otel", "OTLP", "otlp/json"} {
		if err := Logger.SetFormat(value); err != nil {
			t.Fatalf("SetFormat(%q): %v", value, err)
		}
		if got := Logger.GetFormat(); got != FormatOTel {
			t.Fatalf("SetFormat(%q) selected %q, want otel", value, got)
		}
	}
}

func TestREQ5201InvalidFormatFailsWithoutChangingTheActiveFormat(t *testing.T) {
	resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetFormat("yaml"); err == nil {
		t.Fatal("unsupported format was accepted")
	}
	if got := Logger.GetFormat(); got != FormatOTel {
		t.Fatalf("rejected value changed active format to %q", got)
	}
}

func TestREQ5201EnvironmentOverridesTheConfiguredFormat(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatText); err != nil {
		t.Fatalf("SetFormat(text): %v", err)
	}
	t.Setenv("AIRGAP_UPSTREAM_LOG_FORMAT", "otel")

	if err := ApplyEnvironmentOptions("AIRGAP_UPSTREAM_"); err != nil {
		t.Fatalf("ApplyEnvironmentOptions: %v", err)
	}
	Logger.Info(Event(EventDiagnostic), "after override")

	lines := nonEmptyLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want override diagnostic and OTLP record", len(lines))
	}
	if json.Valid([]byte(lines[0])) {
		t.Fatalf("override diagnostic should use the previously active text format: %q", lines[0])
	}
	if !json.Valid([]byte(lines[1])) {
		t.Fatalf("record after environment override is not JSON: %q", lines[1])
	}
}

func TestREQ5202OneIndependentEnvelopePerPhysicalLine(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)

	messages := []string{
		`quote=" backslash=\ tab=` + "\t",
		"first line\nsecond line\rthird line",
		"Unicode: åäö 日本語 \U0001F512",
	}
	for _, message := range messages {
		Logger.Info(Event(EventDiagnostic), message)
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != len(messages) {
		t.Fatalf("got %d physical lines for %d events\n%s", len(lines), len(messages), buf.String())
	}

	for index, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not independent valid JSON: %q", index, line)
		}
		records := decodeOTLPLines(t, line)
		if len(records) != 1 || records[0].body != messages[index] {
			t.Fatalf("line %d body = %q, want %q", index, records[0].body, messages[index])
		}
	}
}

func TestREQ5202EnvelopeHasExactlyOneEntryAtEveryOTLPLayer(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)

	Logger.Info(Event(EventDiagnostic), "shape")

	var payload otlpPayload
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(payload.ResourceLogs) != 1 {
		t.Fatalf("resourceLogs count = %d, want 1", len(payload.ResourceLogs))
	}
	if len(payload.ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("scopeLogs count = %d, want 1", len(payload.ResourceLogs[0].ScopeLogs))
	}
	if len(payload.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("logRecords count = %d, want 1", len(payload.ResourceLogs[0].ScopeLogs[0].LogRecords))
	}
}

func TestREQ5203ResourceAndScopeIdentity(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	SetServiceName(`upstream "primary"`)
	Logger.SetInstanceID("upstream-å")

	Logger.Info(Event(EventDiagnostic), "identity")

	record := onlyDecodedRecord(t, buf.String())
	if got := record.resource["service.name"]; got != `upstream "primary"` {
		t.Fatalf("service.name = %q", got)
	}
	if got := record.resource["service.instance.id"]; got != "upstream-å" {
		t.Fatalf("service.instance.id = %q", got)
	}

	var payload otlpPayload
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got := payload.ResourceLogs[0].ScopeLogs[0].Scope.Name; got == "" {
		t.Fatal("instrumentation scope name is empty")
	}
}

func TestREQ5203HostnameFallbackIsNonEmpty(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	SetServiceName("create")

	Logger.Info(Event(EventDiagnostic), "fallback")

	record := onlyDecodedRecord(t, buf.String())
	if record.resource["service.instance.id"] == "" ||
		record.resource["service.instance.id"] == "unknown" {
		t.Fatalf("service.instance.id fallback = %q", record.resource["service.instance.id"])
	}
}

func TestREQ5204SeverityTimestampAndFormattedBodyMapping(t *testing.T) {
	resetLogging(t)

	tests := []struct {
		level  LogLevel
		text   string
		number int
	}{
		{TRACE, "TRACE", 1},
		{DEBUG, "DEBUG", 5},
		{INFO, "INFO", 9},
		{WARN, "WARN", 13},
		{ERROR, "ERROR", 17},
		{FATAL, "FATAL", 21},
	}

	now := time.Now()
	for _, testCase := range tests {
		line, err := encodeOTLP(now, testCase.level, "value=42", nil)
		if err != nil {
			t.Fatalf("encodeOTLP(%s): %v", testCase.text, err)
		}
		record := onlyDecodedRecord(t, string(line))
		if record.severity != testCase.text || record.severityNo != testCase.number {
			t.Fatalf("%s mapped to %s/%d", testCase.text, record.severity, record.severityNo)
		}
		if record.body != "value=42" {
			t.Fatalf("%s body = %q", testCase.text, record.body)
		}
		nanos, err := strconv.ParseInt(record.timeNanos, 10, 64)
		if err != nil || nanos != now.UnixNano() || nanos <= 0 {
			t.Fatalf("%s timeUnixNano = %q, err=%v", testCase.text, record.timeNanos, err)
		}
	}
}

func TestREQ5204PublicFormattedMethodsResolveArgumentsBeforeEncoding(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)

	Logger.Infof(Event(EventDiagnostic), "partition=%d state=%s", 7, "RUNNING")
	Logger.Errorf(Event(EventDiagnostic), "failure: %v", errors.New("disk full"))

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	if records[0].body != "partition=7 state=RUNNING" {
		t.Fatalf("Infof body = %q", records[0].body)
	}
	if records[1].body != "failure: disk full" {
		t.Fatalf("Errorf body = %q", records[1].body)
	}
}

func TestREQ5205OTLPAttributeTypeFidelityAndUniqueKeys(t *testing.T) {
	resetLogging(t)

	line, err := encodeOTLP(time.Now(), INFO, "typed", []Attr{
		{Key: "string", Value: "value"},
		{Key: "integer", Value: "9007199254740993", IsInt: true},
		{Key: "boolean", Value: "true", IsBool: true},
	})
	if err != nil {
		t.Fatalf("encodeOTLP: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	attributes := raw["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)["attributes"].([]any)

	seen := map[string]bool{}
	for _, item := range attributes {
		attribute := item.(map[string]any)
		key := attribute["key"].(string)
		if seen[key] {
			t.Fatalf("duplicate attribute key %q", key)
		}
		seen[key] = true
		value := attribute["value"].(map[string]any)
		switch key {
		case "string":
			if got := value["stringValue"]; got != "value" {
				t.Fatalf("stringValue = %#v", got)
			}
		case "integer":
			if got := value["intValue"]; got != "9007199254740993" {
				t.Fatalf("intValue = %#v; protobuf JSON int64 must remain a decimal string", got)
			}
		case "boolean":
			if got := value["boolValue"]; got != true {
				t.Fatalf("boolValue = %#v", got)
			}
		}
	}
}

func TestREQ5205OTelAloneDoesNotAddIntegrityOrSignatureAttributes(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)

	Logger.Info(Event(EventDiagnostic), "format only")

	record := onlyDecodedRecord(t, buf.String())
	for key := range record.attributes {
		if strings.HasPrefix(key, "airgap.log.") {
			t.Fatalf("OTel format alone added integrity/signature attribute %q", key)
		}
	}
}

func TestREQ5206OTelUsesTheSelectedWriter(t *testing.T) {
	resetLogging(t)
	mustSetOTel(t)

	var destination bytes.Buffer
	setOutputForTest(&destination)
	Logger.Info(Event(EventDiagnostic), "selected destination")

	record := onlyDecodedRecord(t, destination.String())
	if record.body != "selected destination" {
		t.Fatalf("selected writer received body %q", record.body)
	}
}

func TestREQ5207FilteringHappensBeforeEnvelopeEncoding(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)
	Logger.SetLogLevel("WARN")

	Logger.Debug(Event(EventDiagnostic), "filtered")
	Logger.Warn(Event(EventDiagnostic), "retained")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 1 || records[0].body != "retained" {
		t.Fatalf("filtered output = %#v", records)
	}
}

func TestREQ5207ConcurrentEventsRemainCompleteAndUninterleaved(t *testing.T) {
	buf := resetLogging(t)
	mustSetOTel(t)

	const writers = 12
	const recordsPerWriter = 30

	var wait sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			for record := 0; record < recordsPerWriter; record++ {
				Logger.Infof(Event(EventDiagnostic), "writer=%d record=%d", writer, record)
			}
		}(writer)
	}
	wait.Wait()

	lines := nonEmptyLines(buf.String())
	if len(lines) != writers*recordsPerWriter {
		t.Fatalf("physical line count = %d, want %d", len(lines), writers*recordsPerWriter)
	}

	seen := make(map[string]bool, len(lines))
	for index, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("concurrent line %d is interleaved or invalid: %q", index, line)
		}
		body := onlyDecodedRecord(t, line).body
		if seen[body] {
			t.Fatalf("duplicate event %q", body)
		}
		seen[body] = true
	}
}

func mustSetOTel(t *testing.T) {
	t.Helper()
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
}

func nonEmptyLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func onlyDecodedRecord(t *testing.T, raw string) decodedRecord {
	t.Helper()
	records := decodeOTLPLines(t, raw)
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	return records[0]
}
