package logging

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"sitia.nu/airgap/src/version"
)

// resetLogging returns the package to its defaults and captures output.
func resetLogging(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	setOutputForTest(buf)

	// Sequence numbers are unique for the lifetime of a process, so each test
	// has to start from a clean state to stand in for a fresh process.
	emitMu.Lock()
	integrity = integrityState{}
	signingConfig = signingConfiguration{}
	emitMu.Unlock()

	resourceMu.Lock()
	serviceName = ""
	serviceInstance = ""
	serviceVersion = version.GitVersion
	resourceMu.Unlock()

	Logger.SetLogLevel("TRACE")
	if err := Logger.SetFormat(FormatText); err != nil {
		t.Fatalf("SetFormat(text): %v", err)
	}
	if err := Logger.SetHashAlgorithm("none"); err != nil {
		t.Fatalf("SetHashAlgorithm(none): %v", err)
	}
	if err := Logger.SetHashChainLength(0); err != nil {
		t.Fatalf("SetHashChainLength(0): %v", err)
	}
	Logger.ClearSigningCertificate()

	t.Cleanup(func() {
		setOutputForTest(bytes.NewBuffer(nil))
		Logger.SetLogLevel("INFO")
		_ = Logger.SetFormat(FormatText)
		_ = Logger.SetHashAlgorithm("none")
		_ = Logger.SetHashChainLength(0)
		Logger.ClearSigningCertificate()
	})

	return buf
}

// decodedRecord is the flattened view of one OTLP/JSON line.
type decodedRecord struct {
	body       string
	eventName  string
	severity   string
	severityNo int
	timeNanos  string
	attributes map[string]string
	resource   map[string]string
}

// decodeOTLPLines parses captured OTLP/JSON output, one envelope per line.
func decodeOTLPLines(t *testing.T, raw string) []decodedRecord {
	t.Helper()

	var records []decodedRecord
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var payload otlpPayload
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("output line is not valid JSON: %v\nline: %s", err, line)
		}

		if len(payload.ResourceLogs) != 1 {
			t.Fatalf("expected exactly one resourceLogs entry, got %d", len(payload.ResourceLogs))
		}
		resourceLogs := payload.ResourceLogs[0]

		if len(resourceLogs.ScopeLogs) != 1 {
			t.Fatalf("expected exactly one scopeLogs entry, got %d", len(resourceLogs.ScopeLogs))
		}
		scopeLogs := resourceLogs.ScopeLogs[0]

		if len(scopeLogs.LogRecords) != 1 {
			t.Fatalf("expected exactly one logRecord, got %d", len(scopeLogs.LogRecords))
		}
		logRecord := scopeLogs.LogRecords[0]

		record := decodedRecord{
			eventName:  logRecord.EventName,
			severity:   logRecord.SeverityText,
			severityNo: logRecord.SeverityNumber,
			timeNanos:  logRecord.TimeUnixNano,
			attributes: map[string]string{},
			resource:   map[string]string{},
		}

		if logRecord.Body.StringValue != nil {
			record.body = *logRecord.Body.StringValue
		}

		for _, attr := range logRecord.Attributes {
			record.attributes[attr.Key] = flattenAnyValue(attr.Value)
		}
		for _, attr := range resourceLogs.Resource.Attributes {
			record.resource[attr.Key] = flattenAnyValue(attr.Value)
		}

		records = append(records, record)
	}

	return records
}

func flattenAnyValue(value otlpAnyValue) string {
	switch {
	case value.StringValue != nil:
		return *value.StringValue
	case value.IntValue != nil:
		return *value.IntValue
	case value.BoolValue != nil:
		if *value.BoolValue {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func TestTextFormatIsUnchangedWhenIntegrityDisabled(t *testing.T) {
	buf := resetLogging(t)

	Logger.Info(Event(EventDiagnostic), "hello world")

	got := buf.String()
	if !strings.Contains(got, "[INFO] hello world") {
		t.Fatalf("expected the historical text layout, got %q", got)
	}
	if strings.Contains(got, "airgap.log.") {
		t.Fatalf("integrity attributes must not appear when hashing is off, got %q", got)
	}
}

func TestHashIsDisabledByDefault(t *testing.T) {
	resetLogging(t)

	if integrity.enabled {
		t.Fatal("hashing must be opt-in")
	}
	if integrity.chainingEnabled() {
		t.Fatal("chaining must be opt-in")
	}
}

func TestHashCoversPayloadOnly(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	const body = "payload to be hashed"
	Logger.Info(Event(EventDiagnostic), body)

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	expected := sha256.Sum256([]byte(body))
	if got := records[0].attributes[AttrHash]; got != hex.EncodeToString(expected[:]) {
		t.Fatalf("hash must cover the payload only.\n got: %s\nwant: %s", got, hex.EncodeToString(expected[:]))
	}

	if got := records[0].attributes[AttrHashAlg]; got != "SHA256" {
		t.Fatalf("unexpected algorithm attribute %q", got)
	}
}

func TestIdenticalPayloadsShareAHashButNotAChainValue(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(100); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Warn(Event(EventDiagnostic), "Kafka connectivity degraded")
	Logger.Warn(Event(EventDiagnostic), "Kafka connectivity degraded")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	if records[0].attributes[AttrHash] != records[1].attributes[AttrHash] {
		t.Fatal("identical payloads must produce identical payload hashes")
	}
	if records[0].attributes[AttrChain] == records[1].attributes[AttrChain] {
		t.Fatal("repeated payloads must not produce identical chain values, or reordering would be undetectable")
	}
}

// TestChainMatchesDocumentedFormula recomputes the chain from the formula in
// the documentation, so the published verification recipe is tested rather than
// the implementation being compared against itself.
func TestChainMatchesDocumentedFormula(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(1000); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	for i := 0; i < 5; i++ {
		Logger.Infof(Event(EventDiagnostic), "record number %d", i)
	}

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 5 {
		t.Fatalf("expected 5 records, got %d", len(records))
	}

	var previousChain string
	for index, record := range records {
		seq, err := strconv.ParseUint(record.attributes[AttrSequence], 10, 64)
		if err != nil {
			t.Fatalf("record %d has an unparsable sequence number: %v", index, err)
		}
		if seq != uint64(index) {
			t.Fatalf("record %d carries sequence %d", index, seq)
		}

		if index > 0 && record.attributes[AttrChainPrev] != previousChain {
			t.Fatalf("record %d does not link to its predecessor.\n prev: %s\nwant: %s",
				index, record.attributes[AttrChainPrev], previousChain)
		}

		payloadHash, err := hex.DecodeString(record.attributes[AttrHash])
		if err != nil {
			t.Fatalf("record %d has an unparsable hash: %v", index, err)
		}
		chainPrev, err := hex.DecodeString(record.attributes[AttrChainPrev])
		if err != nil {
			t.Fatalf("record %d has an unparsable previous chain value: %v", index, err)
		}

		h := sha256.New()
		h.Write([]byte(chainDomain))
		h.Write([]byte{0})
		h.Write(chainPrev)

		seqBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(seqBytes, seq)
		h.Write(seqBytes)
		h.Write(payloadHash)

		want := hex.EncodeToString(h.Sum(nil))
		if got := record.attributes[AttrChain]; got != want {
			t.Fatalf("record %d chain value does not match the documented formula.\n got: %s\nwant: %s",
				index, got, want)
		}

		previousChain = record.attributes[AttrChain]
	}
}

func TestTamperedPayloadBreaksTheChain(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(1000); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "original payload")
	Logger.Info(Event(EventDiagnostic), "second payload")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	// Recomputing the first record's hash over an edited payload must no longer
	// match the stored hash, which is what makes the edit detectable.
	tampered := sha256.Sum256([]byte("edited payload"))
	if hex.EncodeToString(tampered[:]) == records[0].attributes[AttrHash] {
		t.Fatal("an edited payload produced the recorded hash")
	}

	// And the successor still commits to the original value.
	if records[1].attributes[AttrChainPrev] != records[0].attributes[AttrChain] {
		t.Fatal("the successor must commit to the original chain value")
	}
}

func TestAnchorIsEmittedEveryChainLengthRecords(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(10); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	for i := 0; i < 20; i++ {
		Logger.Infof(Event(EventDiagnostic), "record %d", i)
	}

	records := decodeOTLPLines(t, buf.String())

	// 20 application records plus one anchor after each window of 10.
	if len(records) != 22 {
		t.Fatalf("expected 22 records (20 application + 2 anchors), got %d", len(records))
	}

	var anchorIndexes []int
	for index, record := range records {
		if record.attributes[AttrAnchor] == "true" {
			anchorIndexes = append(anchorIndexes, index)
			if record.body != anchorBody {
				t.Fatalf("anchor at index %d has body %q", index, record.body)
			}
		}
	}

	if len(anchorIndexes) != 2 {
		t.Fatalf("expected 2 anchors, got %d at %v", len(anchorIndexes), anchorIndexes)
	}
	// The first anchor follows records 0-9, the second follows the next ten.
	if anchorIndexes[0] != 10 || anchorIndexes[1] != 21 {
		t.Fatalf("anchors are misplaced: %v", anchorIndexes)
	}

	first := records[anchorIndexes[0]]
	if first.attributes[AttrAnchorFrom] != "0" || first.attributes[AttrAnchorTo] != "10" {
		t.Fatalf("first anchor covers %s..%s, expected 0..10",
			first.attributes[AttrAnchorFrom], first.attributes[AttrAnchorTo])
	}

	second := records[anchorIndexes[1]]
	if second.attributes[AttrAnchorFrom] != "11" || second.attributes[AttrAnchorTo] != "21" {
		t.Fatalf("second anchor covers %s..%s, expected 11..21",
			second.attributes[AttrAnchorFrom], second.attributes[AttrAnchorTo])
	}
}

// TestAnchorDoesNotConsumeASlotInTheNextWindow guards the off-by-one where the
// anchor record itself counted towards the window it had just closed.
func TestAnchorDoesNotConsumeASlotInTheNextWindow(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(3); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	for i := 0; i < 9; i++ {
		Logger.Infof(Event(EventDiagnostic), "record %d", i)
	}

	records := decodeOTLPLines(t, buf.String())

	anchors := 0
	for _, record := range records {
		if record.attributes[AttrAnchor] == "true" {
			anchors++
		}
	}

	if anchors != 3 {
		t.Fatalf("9 records with chain length 3 must produce exactly 3 anchors, got %d", anchors)
	}
}

func TestAnchorIsItselfChained(t *testing.T) {
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
	if len(records) != 3 {
		t.Fatalf("expected 2 records plus an anchor, got %d", len(records))
	}

	anchor := records[2]
	if anchor.attributes[AttrAnchor] != "true" {
		t.Fatal("the third record should be the anchor")
	}

	// Signing the anchor must transitively cover the window, which only holds
	// if the anchor links to the last record of that window.
	if anchor.attributes[AttrChainPrev] != records[1].attributes[AttrChain] {
		t.Fatal("the anchor must link to the final record of the window it closes")
	}
	if anchor.attributes[AttrChain] == "" {
		t.Fatal("the anchor must carry its own chain value, since that is what gets signed")
	}
}

func TestChainIsDisabledWhenLengthIsZero(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "only hashed")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	if records[0].attributes[AttrHash] == "" {
		t.Fatal("the payload hash should still be present")
	}
	if _, found := records[0].attributes[AttrChain]; found {
		t.Fatal("no chain value should be emitted when hash-chain is 0")
	}
}

func TestSupportedHashAlgorithms(t *testing.T) {
	resetLogging(t)

	for _, name := range SupportedHashAlgorithms() {
		if err := Logger.SetHashAlgorithm(name); err != nil {
			t.Fatalf("SetHashAlgorithm(%s): %v", name, err)
		}
		if integrity.algorithm.name != name {
			t.Fatalf("SetHashAlgorithm(%s) selected %q", name, integrity.algorithm.name)
		}
	}
}

func TestHashAlgorithmSpellingIsNormalized(t *testing.T) {
	resetLogging(t)

	cases := map[string]string{
		"sha256":   "SHA256",
		"SHA-256":  "SHA256",
		"sha-512":  "SHA512",
		"sha3-256": "SHA3-256",
		"SHA3_512": "SHA3-512",
		" SHA384 ": "SHA384",
	}

	for input, want := range cases {
		if err := Logger.SetHashAlgorithm(input); err != nil {
			t.Fatalf("SetHashAlgorithm(%q): %v", input, err)
		}
		if integrity.algorithm.name != want {
			t.Fatalf("SetHashAlgorithm(%q) selected %q, want %q", input, integrity.algorithm.name, want)
		}
	}
}

func TestUnsupportedHashAlgorithmIsRejected(t *testing.T) {
	resetLogging(t)

	for _, name := range []string{"MD5", "SHA1", "rot13"} {
		if err := Logger.SetHashAlgorithm(name); err == nil {
			t.Fatalf("SetHashAlgorithm(%s) should have been rejected", name)
		}
	}

	// A rejected algorithm must not silently enable a different one.
	if integrity.enabled {
		t.Fatal("a rejected algorithm must leave hashing disabled")
	}
}

func TestSha512ProducesLongerDigest(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA512"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	const body = "digest length check"
	Logger.Info(Event(EventDiagnostic), body)

	records := decodeOTLPLines(t, buf.String())
	expected := sha512.Sum512([]byte(body))
	if records[0].attributes[AttrHash] != hex.EncodeToString(expected[:]) {
		t.Fatal("SHA512 digest does not match")
	}
}

func TestNegativeChainLengthIsRejected(t *testing.T) {
	resetLogging(t)

	if err := Logger.SetHashChainLength(-1); err == nil {
		t.Fatal("a negative chain length should have been rejected")
	}
}

func TestOTLPRecordShape(t *testing.T) {
	buf := resetLogging(t)

	SetServiceName("airgap-upstream")
	Logger.SetInstanceID("Upstream_1")
	t.Cleanup(func() {
		SetServiceName("")
		Logger.SetInstanceID("")
	})

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}

	Logger.Warn(Event(EventDiagnostic), "something to look at")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	record := records[0]

	if record.body != "something to look at" {
		t.Fatalf("unexpected body %q", record.body)
	}
	if record.severity != "WARN" || record.severityNo != severityNumberWarn {
		t.Fatalf("unexpected severity %s/%d", record.severity, record.severityNo)
	}
	if record.resource["service.name"] != "airgap-upstream" {
		t.Fatalf("unexpected service.name %q", record.resource["service.name"])
	}
	if record.resource["service.instance.id"] != "Upstream_1" {
		t.Fatalf("unexpected service.instance.id %q", record.resource["service.instance.id"])
	}
	if _, err := strconv.ParseInt(record.timeNanos, 10, 64); err != nil {
		t.Fatalf("timeUnixNano must be a numeric string, got %q", record.timeNanos)
	}
	// The text tag must not leak into a structured body.
	if strings.HasPrefix(record.body, "[WARN]") {
		t.Fatal("the severity tag belongs in severityText, not in the body")
	}
}

func TestSeverityMapping(t *testing.T) {
	cases := map[LogLevel]int{
		TRACE: severityNumberTrace,
		DEBUG: severityNumberDebug,
		INFO:  severityNumberInfo,
		WARN:  severityNumberWarn,
		ERROR: severityNumberError,
		FATAL: severityNumberFatal,
	}

	for level, want := range cases {
		if got := severityNumber(level); got != want {
			t.Fatalf("severityNumber(%v) = %d, want %d", level, got, want)
		}
	}
}

func TestTextFormatCarriesIntegrityAttributes(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(5); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "text mode payload")

	got := buf.String()
	for _, key := range []string{AttrHashAlg, AttrHash, AttrSequence, AttrChain, AttrChainPrev} {
		if !strings.Contains(got, key+"=") {
			t.Fatalf("expected %s in text output, got %q", key, got)
		}
	}
}

func TestLevelFilteringStillApplies(t *testing.T) {
	buf := resetLogging(t)
	Logger.SetLogLevel("WARN")

	Logger.Debug(Event(EventDiagnostic), "should not appear")
	Logger.Warn(Event(EventDiagnostic), "should appear")

	got := buf.String()
	if strings.Contains(got, "should not appear") {
		t.Fatal("a record below the threshold was emitted")
	}
	if !strings.Contains(got, "should appear") {
		t.Fatal("a record at the threshold was dropped")
	}
}

// TestFilteredRecordsDoNotAdvanceTheChain confirms the chain covers what was
// written, not what was offered, so a verifier sees no gaps in sequence numbers.
func TestFilteredRecordsDoNotAdvanceTheChain(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(100); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}
	Logger.SetLogLevel("WARN")

	Logger.Debug(Event(EventDiagnostic), "dropped")
	Logger.Warn(Event(EventDiagnostic), "kept one")
	Logger.Debug(Event(EventDiagnostic), "dropped")
	Logger.Warn(Event(EventDiagnostic), "kept two")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	if records[0].attributes[AttrSequence] != "0" || records[1].attributes[AttrSequence] != "1" {
		t.Fatalf("sequence numbers must be contiguous across filtered records, got %s and %s",
			records[0].attributes[AttrSequence], records[1].attributes[AttrSequence])
	}
}

// TestConcurrentEmissionKeepsTheChainConsistent exercises the lock that makes
// sequence numbers follow write order.
func TestConcurrentEmissionKeepsTheChainConsistent(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(1000); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	const writers = 8
	const perWriter = 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				Logger.Infof(Event(EventDiagnostic), "writer %d record %d", id, i)
			}
		}(w)
	}
	wg.Wait()

	records := decodeOTLPLines(t, buf.String())
	if len(records) != writers*perWriter {
		t.Fatalf("expected %d records, got %d", writers*perWriter, len(records))
	}

	var previousChain string
	for index, record := range records {
		if got := record.attributes[AttrSequence]; got != strconv.Itoa(index) {
			t.Fatalf("record at position %d carries sequence %s", index, got)
		}
		if index > 0 && record.attributes[AttrChainPrev] != previousChain {
			t.Fatalf("chain broken at record %d", index)
		}
		previousChain = record.attributes[AttrChain]
	}
}

// TestSequenceNumbersSurviveReconfiguration guards the case where enabling the
// chain after startup renumbered the stream from zero, which put duplicate
// sequence numbers in one file and made a legitimate restart look like tampering.
func TestSequenceNumbersSurviveReconfiguration(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	// Mirrors a properties file where hash is applied before hash-chain, so a
	// record is emitted in between.
	Logger.Info(Event(EventDiagnostic), "hashed but not yet chained")

	if err := Logger.SetHashChainLength(10); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "first chained record")
	Logger.Info(Event(EventDiagnostic), "second chained record")

	records := decodeOTLPLines(t, buf.String())
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}

	for index, record := range records {
		if got := record.attributes[AttrSequence]; got != strconv.Itoa(index) {
			t.Fatalf("record %d carries sequence %s; sequence numbers must stay unique and monotonic",
				index, got)
		}
	}

	if _, found := records[0].attributes[AttrChain]; found {
		t.Fatal("the record emitted before hash-chain was set should carry no chain value")
	}
	if records[1].attributes[AttrChainRestart] != "true" {
		t.Fatal("the first chained record must declare that it opens a new chain segment")
	}
	if records[2].attributes[AttrChainRestart] == "true" {
		t.Fatal("only the first record of a segment may declare a restart")
	}
	if records[2].attributes[AttrChainPrev] != records[1].attributes[AttrChain] {
		t.Fatal("the chain must be continuous after the restart")
	}
}

// TestAnchorWindowStartsAtTheRestart confirms an anchor cannot claim to cover
// records that were written before the chain existed.
func TestAnchorWindowStartsAtTheRestart(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "before the chain exists")

	if err := Logger.SetHashChainLength(2); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.Info(Event(EventDiagnostic), "one")
	Logger.Info(Event(EventDiagnostic), "two")

	records := decodeOTLPLines(t, buf.String())

	var anchor *decodedRecord
	for index := range records {
		if records[index].attributes[AttrAnchor] == "true" {
			anchor = &records[index]
			break
		}
	}

	if anchor == nil {
		t.Fatal("expected an anchor")
	}
	// Record 0 predates the chain, so the window must open at sequence 1.
	if anchor.attributes[AttrAnchorFrom] != "1" {
		t.Fatalf("anchor window starts at %s, expected 1", anchor.attributes[AttrAnchorFrom])
	}
}

func TestApplyOption(t *testing.T) {
	resetLogging(t)

	handled, err := ApplyOption("hash", "SHA3-256")
	if !handled || err != nil {
		t.Fatalf("ApplyOption(hash) handled=%v err=%v", handled, err)
	}
	if integrity.algorithm.name != "SHA3-256" {
		t.Fatalf("unexpected algorithm %q", integrity.algorithm.name)
	}

	handled, err = ApplyOption("hash-chain", "10")
	if !handled || err != nil {
		t.Fatalf("ApplyOption(hash-chain) handled=%v err=%v", handled, err)
	}
	if integrity.chainLen != 10 {
		t.Fatalf("unexpected chain length %d", integrity.chainLen)
	}

	handled, err = ApplyOption("logFormat", "otel")
	if !handled || err != nil {
		t.Fatalf("ApplyOption(logFormat) handled=%v err=%v", handled, err)
	}
	if Logger.GetFormat() != FormatOTel {
		t.Fatalf("unexpected format %q", Logger.GetFormat())
	}

	handled, err = ApplyOption("bootstrapServers", "kafka:9092")
	if handled || err != nil {
		t.Fatalf("an unrelated key must be left to the caller, handled=%v err=%v", handled, err)
	}
}

func TestApplyOptionRejectsBadValues(t *testing.T) {
	resetLogging(t)

	for _, testCase := range []struct{ key, value string }{
		{"hash", "MD5"},
		{"hash-chain", "ten"},
		{"hash-chain", "-5"},
		{"logFormat", "yaml"},
	} {
		handled, err := ApplyOption(testCase.key, testCase.value)
		if !handled {
			t.Fatalf("%s=%s should be recognised as a logging option", testCase.key, testCase.value)
		}
		if err == nil {
			t.Fatalf("%s=%s should have been rejected", testCase.key, testCase.value)
		}
	}
}

func TestIsOption(t *testing.T) {
	for _, key := range []string{
		OptionFormat, OptionHash, OptionHashChain,
		OptionSigningCertificate, OptionSigningKey, OptionSignerID, OptionSigningEKU,
	} {
		if !IsOption(key) {
			t.Fatalf("%s should be a logging option", key)
		}
	}
	if IsOption("topic") {
		t.Fatal("topic is not a logging option")
	}
}

// TestChainRestartsOnInstanceChange documents that the genesis value is bound to
// the service identity, so segments from different instances cannot be spliced.
func TestChainRestartsOnInstanceChange(t *testing.T) {
	buf := resetLogging(t)

	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatalf("SetFormat(otel): %v", err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatalf("SetHashAlgorithm: %v", err)
	}
	if err := Logger.SetHashChainLength(100); err != nil {
		t.Fatalf("SetHashChainLength: %v", err)
	}

	Logger.SetInstanceID("instance-a")
	Logger.Info(Event(EventDiagnostic), "first")
	firstGenesis := decodeOTLPLines(t, buf.String())[0].attributes[AttrChainPrev]

	buf.Reset()
	Logger.SetInstanceID("instance-b")
	Logger.Info(Event(EventDiagnostic), "second")
	secondGenesis := decodeOTLPLines(t, buf.String())[0].attributes[AttrChainPrev]

	t.Cleanup(func() { Logger.SetInstanceID("") })

	if firstGenesis == secondGenesis {
		t.Fatal("a different instance must start from a different genesis value")
	}
}
