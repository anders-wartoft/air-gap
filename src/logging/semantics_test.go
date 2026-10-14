package logging

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"sitia.nu/airgap/src/version"
)

// rawAttributes returns the attributes of a single OTLP line in wire order,
// without collapsing duplicates. The uniqueness rule cannot be tested through a
// map, because a map hides exactly the defect being looked for.
func rawAttributes(t *testing.T, line string) []otlpKeyValue {
	t.Helper()

	var payload otlpPayload
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("output line is not valid JSON: %v\nline: %s", err, line)
	}

	return payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
}

// firstLine returns the first non-empty line of captured output.
func firstLine(t *testing.T, raw string) string {
	t.Helper()

	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}

	t.Fatal("no output was produced")
	return ""
}

// TestEventNameIsPresentOnEveryLevel is the central promise of the semantic
// attributes: a consumer can classify any record without parsing the body.
func TestEventNameIsPresentOnEveryLevel(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Trace(Event(EventAppStart), "trace body")
	Logger.Debug(Event(EventConfigParse), "debug body")
	Logger.Info(Event(EventAppReady), "info body")
	Logger.Warn(Event(EventKafkaConnect), "warn body")
	Logger.Error(Event(EventKafkaConsume), "error body")
	Logger.Tracef(Event(EventUDPSend), "formatted %d", 1)
	Logger.Debugf(Event(EventUDPReceive), "formatted %d", 2)
	Logger.Infof(Event(EventGapDetected), "formatted %d", 3)
	Logger.Warnf(Event(EventGapClosed), "formatted %d", 4)
	Logger.Errorf(Event(EventProtocolDecode), "formatted %d", 5)

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 10 {
		t.Fatalf("expected 10 records, got %d", len(records))
	}

	want := []string{
		EventAppStart, EventConfigParse, EventAppReady, EventKafkaConnect, EventKafkaConsume,
		EventUDPSend, EventUDPReceive, EventGapDetected, EventGapClosed, EventProtocolDecode,
	}

	for i, record := range records {
		got := record.eventName
		if got != want[i] {
			t.Errorf("record %d: %s = %q, want %q", i, AttrEventName, got, want[i])
		}
	}
}

// TestEventNameIsNotDuplicatedAsAttribute keeps EventName in the dedicated
// LogRecord field rather than the attributes array.
func TestEventNameIsNotDuplicatedAsAttribute(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventAppStart, Str("file.path", "/etc/airgap/upstream.properties")), "started")

	attrs := rawAttributes(t, firstLine(t, buf.String()))
	for _, attr := range attrs {
		if attr.Key == AttrEventName {
			t.Fatalf("%s was duplicated as an attribute: %+v", AttrEventName, attrs)
		}
	}
}

// TestTextModeIgnoresSemanticAttributes is the compatibility guarantee. Adding
// attributes to every call site must not change what an existing installation
// writes to its log file.
func TestTextModeIgnoresSemanticAttributes(t *testing.T) {
	buf := resetLogging(t)

	Logger.Info(Event(EventAppStart,
		Str("file.path", "/etc/airgap/upstream.properties"),
		Int("server.port", 9092),
	), "Starting upstream")

	got := buf.String()
	if !strings.HasSuffix(got, "[INFO] Starting upstream\n") {
		t.Fatalf("text output carried semantic attributes: %q", got)
	}
}

// TestTextModeStillCarriesIntegrityAttributes guards the other half of the
// rule: suppressing semantic attributes must not suppress the hash.
func TestTextModeStillCarriesIntegrityAttributes(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	Logger.Info(Event(EventAppStart, Str("file.path", "/etc/airgap")), "Starting upstream")

	got := buf.String()
	if !strings.Contains(got, AttrHash+"=") {
		t.Fatalf("text output lost its integrity attributes: %q", got)
	}
	if strings.Contains(got, AttrEventName) {
		t.Fatalf("text output carried semantic attributes: %q", got)
	}
}

// TestReservedAttributePrefixIsDropped stops a call site from forging integrity
// data. A record must not be able to claim a hash, a sequence number or a chain
// value that the framework did not compute.
func TestReservedAttributePrefixIsDropped(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventDiagnostic,
		Str(AttrHash, "deadbeef"),
		Str(AttrSequence, "0"),
		Str(AttrChain, "forged"),
		Str("airgap.log.sig", "forged"),
		Str("airgap.message.id", "kept"),
	), "forgery attempt")

	for _, attr := range rawAttributes(t, firstLine(t, buf.String())) {
		if strings.HasPrefix(attr.Key, reservedAttrPrefix) {
			t.Errorf("call site was allowed to set reserved attribute %q", attr.Key)
		}
	}

	records := decodeOTLPLines(t, buf.String())
	if records[0].attributes["airgap.message.id"] != "kept" {
		t.Error("a non-reserved attribute was dropped along with the reserved ones")
	}
}

// TestDuplicateAttributeKeysAreDropped enforces the OTel data model rule that
// keys are unique within a record. The first occurrence is the one the call
// site meant.
func TestDuplicateAttributeKeysAreDropped(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventDiagnostic,
		Str("file.path", "first"),
		Str("file.path", "second"),
		Str(AttrEventName, "attempted.override"),
	), "duplicates")

	attrs := rawAttributes(t, firstLine(t, buf.String()))

	seen := map[string]int{}
	for _, attr := range attrs {
		seen[attr.Key]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("attribute %q appears %d times, want 1", key, count)
		}
	}

	records := decodeOTLPLines(t, buf.String())
	if got := records[0].attributes["file.path"]; got != "first" {
		t.Errorf("file.path = %q, want the first occurrence %q", got, "first")
	}
	if got := records[0].eventName; got != EventDiagnostic {
		t.Errorf("%s = %q, want %q", AttrEventName, got, EventDiagnostic)
	}
}

// TestAttributeValueTypes checks that the helpers produce the OTLP types a
// consumer expects, rather than stringifying everything.
func TestAttributeValueTypes(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventKafkaConsume,
		Str("messaging.destination.name", "upstream"),
		Decimal("messaging.destination.partition.id", 7),
		Bool("airgap.message.denied", true),
		Failure(),
	), "typed attributes")

	byKey := map[string]otlpAnyValue{}
	for _, attr := range rawAttributes(t, firstLine(t, buf.String())) {
		byKey[attr.Key] = attr.Value
	}

	if v := byKey["messaging.destination.name"]; v.StringValue == nil || *v.StringValue != "upstream" {
		t.Errorf("string attribute was not encoded as stringValue: %+v", v)
	}
	if v := byKey["messaging.destination.partition.id"]; v.StringValue == nil || *v.StringValue != "7" {
		t.Errorf("partition ID was not encoded as stringValue: %+v", v)
	}
	if v := byKey["airgap.message.denied"]; v.BoolValue == nil || !*v.BoolValue {
		t.Errorf("bool attribute was not encoded as boolValue: %+v", v)
	}
	if v := byKey[AttrErrorType]; v.StringValue == nil || *v.StringValue != "_OTHER" {
		t.Errorf("%s was not encoded as _OTHER: %+v", AttrErrorType, v)
	}
}

// TestErrorTypeIsAbsentOnSuccess ensures successful records are not marked as
// failures.
func TestErrorTypeIsAbsentOnSuccess(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventKafkaConsume), "ordinary record")

	records := decodeOTLPLines(t, buf.String())
	if _, present := records[0].attributes[AttrErrorType]; present {
		t.Errorf("%s was set on a successful record", AttrErrorType)
	}
}

// TestErrHelper covers the shorthand used at failure sites.
func TestErrHelper(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Errorf(Event(EventKafkaConnect).Merge(Err(errors.New("connection refused"))),
		"Failed to connect: %v", errors.New("connection refused"))

	records := decodeOTLPLines(t, buf.String())
	if got := records[0].attributes[AttrErrorType]; got != "_OTHER" {
		t.Errorf("%s = %q, want _OTHER", AttrErrorType, got)
	}
}

// TestServiceVersionIsAResourceAttribute follows the OTel convention, where the
// build identity belongs to the resource rather than to each record.
func TestServiceVersionIsAResourceAttribute(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventAppReady), "running")

	records := decodeOTLPLines(t, buf.String())
	if got := records[0].resource["service.version"]; got != version.GitVersion {
		t.Errorf("resource service.version = %q, want %q", got, version.GitVersion)
	}
}

// TestServiceVersionIsNotDuplicatedOnStartupRecords keeps build identity on
// the Resource only.
func TestServiceVersionIsNotDuplicatedOnStartupRecords(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	SetServiceVersion("1.2.3-TEST")
	t.Cleanup(func() { SetServiceVersion(version.GitVersion) })

	Logger.Info(Event(EventAppStart), "Starting upstream")

	records := decodeOTLPLines(t, buf.String())
	if _, present := records[0].attributes["service.version"]; present {
		t.Error("service.version was duplicated on the LogRecord")
	}
	if got := records[0].resource["service.version"]; got != "1.2.3-TEST" {
		t.Errorf("resource service.version = %q, want %q", got, "1.2.3-TEST")
	}
}

// TestAnchorRecordCarriesEventName makes the generated anchor classifiable the
// same way as any other record.
func TestAnchorRecordCarriesEventName(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(2); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "one")
	Logger.Info(Event(EventDiagnostic), "two")

	records := decodeOTLPLines(t, buf.String())

	var anchors int
	for _, record := range records {
		if record.body != anchorBody {
			continue
		}
		anchors++
		if got := record.eventName; got != EventLogAnchor {
			t.Errorf("anchor %s = %q, want %q", AttrEventName, got, EventLogAnchor)
		}
	}

	if anchors == 0 {
		t.Fatal("no anchor record was written")
	}
}

// TestAttrsWithDoesNotMutate protects the shared-base-list pattern used at call
// sites, where one Attrs value is reused for several records.
func TestAttrsWithDoesNotMutate(t *testing.T) {
	base := Event(EventKafkaConsume, Str("messaging.destination.name", "upstream"))

	first := base.With(Decimal("messaging.destination.partition.id", 1))
	second := base.With(Decimal("messaging.destination.partition.id", 2))

	if len(base) != 2 {
		t.Fatalf("base list was mutated, len = %d", len(base))
	}
	if first[2].Value != "1" || second[2].Value != "2" {
		t.Fatalf("derived lists share storage: %q and %q", first[2].Value, second[2].Value)
	}
}

// TestEmptyAttributeKeysAreDropped keeps malformed attributes out of the
// output, where they would be rejected by a strict OTLP consumer.
func TestEmptyAttributeKeysAreDropped(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Info(Event(EventDiagnostic, Str("", "orphan")), "empty key")

	for _, attr := range rawAttributes(t, firstLine(t, buf.String())) {
		if attr.Key == "" {
			t.Error("an attribute with an empty key reached the output")
		}
	}
}

// TestLiteralBodyIsNotFormatted covers the formatted variants being called
// without arguments, which happens wherever a message contains a percent sign.
func TestLiteralBodyIsNotFormatted(t *testing.T) {
	buf := resetLogging(t)

	Logger.Infof(Event(EventStatsReport), "buffer is 90% full")

	if !strings.HasSuffix(buf.String(), "[INFO] buffer is 90% full\n") {
		t.Fatalf("literal body was mangled by the formatter: %q", buf.String())
	}
}
