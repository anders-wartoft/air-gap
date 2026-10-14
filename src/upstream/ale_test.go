package upstream

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sitia.nu/airgap/src/protocol"
)

type captureTransport struct {
	messages [][]byte
	err      error
}

func (c *captureTransport) SendMessage(message []byte) error {
	c.messages = append(c.messages, append([]byte(nil), message...))
	return c.err
}

func (c *captureTransport) SendMessages(messages [][]byte) error {
	for _, message := range messages {
		c.messages = append(c.messages, append([]byte(nil), message...))
	}
	return c.err
}

func (c *captureTransport) Close() error { return nil }

func capturedStatusPayloads(t *testing.T, transport *captureTransport) []string {
	t.Helper()
	cache := protocol.CreateMessageCache()
	payloads := make([]string, 0)
	for _, message := range transport.messages {
		messageType, id, payload, err := protocol.ParseMessage(message, cache)
		if err != nil {
			t.Fatalf("ParseMessage() error = %v", err)
		}
		if payload == nil {
			continue
		}
		if !protocol.IsMessageType(messageType, protocol.TYPE_STATUS) {
			t.Errorf("message type = %d, want TYPE_STATUS", messageType)
		}
		if id != "STATUS" {
			t.Errorf("message id = %q, want STATUS", id)
		}
		payloads = append(payloads, string(payload))
	}
	return payloads
}

func TestSplitMessageID(t *testing.T) {
	tests := []struct {
		id        string
		topic     string
		partition string
		offset    string
	}{
		{"transfer_3_42", "transfer", "3", "42"},
		{"my_topic_name_0_7", "my_topic_name", "0", "7"},
		{"topic_12", "topic", "", "12"},
		{"topic", "topic", "", ""},
	}
	for _, tc := range tests {
		topic, partition, offset := splitMessageID(tc.id)
		if topic != tc.topic || partition != tc.partition || offset != tc.offset {
			t.Errorf("splitMessageID(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tc.id, topic, partition, offset, tc.topic, tc.partition, tc.offset)
		}
	}
}

func TestLostEventPayloadUsesDefaultTemplate(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_1"
	config.lostEventDirectory = "/var/lib/airgap/lost"
	config.kafkaMaxMessageBytes = 6291456

	payload, err := lostEventPayload(lostEventContext("transfer_3_42",
		"message size 10 bytes exceeds maxEventBytes 5 bytes",
		"/var/lib/airgap/lost/transfer_3_42-1.bin", 10))
	if err != nil {
		t.Fatalf("lostEventPayload() error = %v", err)
	}
	got := string(payload)
	want := "AIRGAP LOST EVENT: Event transfer 3:42 couldn't be delivered due to " +
		"message size 10 bytes exceeds maxEventBytes 5 bytes. " +
		"The message can be found in its entirety on upstream machine Upstream_1, in /var/lib/airgap/lost. " +
		"Max message length is 6291456."
	if got != want {
		t.Errorf("lostEventPayload() = %q, want %q", got, want)
	}
}

func TestLostEventPayloadUsesConfiguredTemplate(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_7"
	config.lostEventDirectory = "/lost"
	config.airgapLostEventMessage = "ALE {topic_name}/{partition}/{row} {reason} {upstream_id} {lost_event_file}"

	payload, err := lostEventPayload(lostEventContext("transfer_1_2", "too big", "/lost/transfer_1_2-9.bin", 3))
	if err != nil {
		t.Fatalf("lostEventPayload() error = %v", err)
	}
	got := string(payload)
	want := "ALE transfer/1/2 too big Upstream_7 /lost/transfer_1_2-9.bin"
	if got != want {
		t.Errorf("lostEventPayload() = %q, want %q", got, want)
	}
}

func TestLostEventPayloadUsesJSONFormat(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = `Upstream "north"\one`
	config.airgapLostEventMessageFormat = messageFormatJSON
	config.airgapLostEventMessage = `{
		"eventName":"message.lost",
		"body":{"stringValue":"AIRGAP LOST EVENT: {reason}"},
		"attributes":[
			{"key":"service.instance.id","value":{"stringValue":"{upstreamId}"}},
			{"key":"messaging.kafka.offset","value":{"intValue":"{row}"}}
		]
	}`

	payload, err := lostEventPayload(lostEventContext(
		"transfer_1_2", "can't open \"payload\"\nnext line", "/lost/file.bin", 3,
	))
	if err != nil {
		t.Fatalf("lostEventPayload() error = %v", err)
	}
	if !json.Valid(payload) {
		t.Fatalf("lostEventPayload() produced invalid JSON: %s", payload)
	}
	var record struct {
		EventName string `json:"eventName"`
		Body      struct {
			StringValue string `json:"stringValue"`
		} `json:"body"`
		Attributes []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string `json:"stringValue"`
				IntValue    string `json:"intValue"`
			} `json:"value"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if record.EventName != "message.lost" {
		t.Errorf("eventName = %q", record.EventName)
	}
	if record.Body.StringValue != "AIRGAP LOST EVENT: can't open \"payload\"\nnext line" {
		t.Errorf("body.stringValue = %q", record.Body.StringValue)
	}
	if record.Attributes[0].Value.StringValue != config.id {
		t.Errorf("service.instance.id = %q, want %q", record.Attributes[0].Value.StringValue, config.id)
	}
	if record.Attributes[1].Value.IntValue != "2" {
		t.Errorf("messaging.kafka.offset = %q, want 2", record.Attributes[1].Value.IntValue)
	}
}

func TestLostEventPayloadWithoutDirectoryIsExplicit(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_1"
	config.lostEventDirectory = ""

	payload, err := lostEventPayload(lostEventContext("transfer_3_42", "too big", "", 3))
	if err != nil {
		t.Fatalf("lostEventPayload() error = %v", err)
	}
	got := string(payload)
	if !strings.Contains(got, lostEventDirectoryDisabled) {
		t.Errorf("guard should state that no directory is configured, got %q", got)
	}
}

func TestLostEventTemplateKeepsUnknownPlaceholders(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	got := renderTemplate("{topic_name} {not_a_placeholder}",
		lostEventContext("transfer_0_1", "reason", "", 0))
	if got != "transfer {not_a_placeholder}" {
		t.Errorf("renderTemplate() = %q", got)
	}
}

func TestLostEventFileNameSanitizesSeparators(t *testing.T) {
	got := lostEventFileName("a/b\\c:d transfer_1_2")
	if strings.ContainsAny(got, `/\: `) {
		t.Errorf("lostEventFileName() left unsafe characters: %q", got)
	}
	if got != "a_b_c_d_transfer_1_2" {
		t.Errorf("lostEventFileName() = %q", got)
	}
}

func TestLostMessageTruncatedTemplateUsesConfiguredPlaceholders(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.id = "Upstream_9"
	config.lostEventDirectory = "/airgap/lost_events/"
	config.lostMessageTruncatedMessage = "File {fileName} from {fileDate} at {diskFreeBytes} bytes for {fileSize}: {action}"

	got := renderTemplate(config.lostMessageTruncatedMessage, lostMessageTruncatedContext("/airgap/lost_events/transfer_1_2-3.bin", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), 2048, 512, "deleted"))
	want := "File transfer_1_2-3.bin from 2025-01-02T03:04:05Z at 2048 bytes for 512: deleted"
	if got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}
}

func TestReclaimLostEventDirectoryAppliesRetentionAndSendsStatus(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	dir := t.TempDir()
	config = DefaultConfiguration()
	config.lostEventDirectory = dir
	config.lostEventDirectoryCompressAfterDays = 7
	config.lostEventDirectoryDeleteAfterDays = 60
	config.lostMessageTruncatedMessage = "TRUNCATED {fileName} {fileDate} {diskFreeBytes} {fileSize} {action}"
	config.payloadSize = 65535
	transport := &captureTransport{}
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

	oldCompress := filepath.Join(dir, "compress-me.bin")
	oldDelete := filepath.Join(dir, "delete-me.bin")
	fresh := filepath.Join(dir, "fresh.bin")
	compressTime := now.Add(-10 * 24 * time.Hour)
	deleteTime := now.Add(-70 * 24 * time.Hour)
	freshTime := now.Add(-24 * time.Hour)
	if err := os.WriteFile(oldCompress, []byte("payload"), 0o600); err != nil {
		t.Fatalf("creating compress-file: %v", err)
	}
	if err := os.Chtimes(oldCompress, compressTime, compressTime); err != nil {
		t.Fatalf("setting compress-file mtime: %v", err)
	}
	if err := os.WriteFile(oldDelete, []byte("payload"), 0o600); err != nil {
		t.Fatalf("creating delete-file: %v", err)
	}
	if err := os.Chtimes(oldDelete, deleteTime, deleteTime); err != nil {
		t.Fatalf("setting delete-file mtime: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("payload"), 0o600); err != nil {
		t.Fatalf("creating fresh file: %v", err)
	}
	if err := os.Chtimes(fresh, freshTime, freshTime); err != nil {
		t.Fatalf("setting fresh-file mtime: %v", err)
	}

	reclaimLostEventDirectorySpace(transport, dir, 1, now)

	if _, err := os.Stat(oldDelete); !os.IsNotExist(err) {
		t.Fatalf("70-day file should be deleted before compression: %v", err)
	}
	if _, err := os.Stat(oldDelete + ".zip"); !os.IsNotExist(err) {
		t.Fatalf("70-day file must not escape deletion as a new archive: %v", err)
	}
	if _, err := os.Stat(oldCompress); !os.IsNotExist(err) {
		t.Fatalf("10-day source should be removed after compression: %v", err)
	}
	archiveInfo, err := os.Stat(oldCompress + ".zip")
	if err != nil {
		t.Fatalf("10-day archive should exist: %v", err)
	}
	if !archiveInfo.ModTime().Equal(compressTime) {
		t.Fatalf("archive mtime = %s, want source mtime %s", archiveInfo.ModTime(), compressTime)
	}
	archive, err := zip.OpenReader(oldCompress + ".zip")
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	if len(archive.File) != 1 {
		archive.Close()
		t.Fatalf("archive entries = %d, want 1", len(archive.File))
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		archive.Close()
		t.Fatalf("opening archive entry: %v", err)
	}
	content, err := io.ReadAll(entry)
	entry.Close()
	if err != nil {
		archive.Close()
		t.Fatalf("reading archive entry: %v", err)
	}
	if string(content) != "payload" {
		archive.Close()
		t.Fatalf("archive content = %q, want payload", content)
	}
	if !archive.File[0].Modified.Equal(compressTime) {
		archive.Close()
		t.Fatalf("archive entry mtime = %s, want %s", archive.File[0].Modified, compressTime)
	}
	archive.Close()
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("1-day file should be retained: %v", err)
	}

	payloads := capturedStatusPayloads(t, transport)
	if len(payloads) != 2 {
		t.Fatalf("status payload count = %d, want 2: %q", len(payloads), payloads)
	}
	joined := strings.Join(payloads, "\n")
	for _, expected := range []string{"delete-me.bin", "deleted", "compress-me.bin", "compressed"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("status payloads do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "fresh.bin") {
		t.Errorf("fresh file unexpectedly produced a status: %s", joined)
	}
}

func TestReclaimLostEventDirectoryDisabledRetentionIsNoOp(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	dir := t.TempDir()
	path := filepath.Join(dir, "old.bin")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	config = DefaultConfiguration()
	config.lostEventDirectory = dir
	config.lostEventDirectoryCompressAfterDays = 0
	config.lostEventDirectoryDeleteAfterDays = 0
	config.payloadSize = 65535
	transport := &captureTransport{}

	reclaimLostEventDirectorySpace(transport, dir, 1, time.Now())

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("disabled retention removed old file: %v", err)
	}
	if len(transport.messages) != 0 {
		t.Fatalf("disabled retention sent %d status frames", len(transport.messages))
	}
}

func TestReclaimLostEventDirectoryEmergencyEvictsOldest(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	dir := t.TempDir()
	now := time.Now()
	oldest := filepath.Join(dir, "oldest.bin")
	newest := filepath.Join(dir, "newest.bin")
	for path, age := range map[string]time.Duration{
		oldest: 48 * time.Hour,
		newest: 24 * time.Hour,
	} {
		if err := os.WriteFile(path, make([]byte, 1024*1024), 0o600); err != nil {
			t.Fatal(err)
		}
		modTime := now.Add(-age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	config = DefaultConfiguration()
	config.lostEventDirectory = dir
	config.lostEventDirectoryCompressAfterDays = 0
	config.lostEventDirectoryDeleteAfterDays = 0
	config.lostMessageTruncatedMessage = "TRUNCATED {fileName} {action}"
	config.payloadSize = 65535
	transport := &captureTransport{}
	freeChecks := 0
	freeBytes := func(string) (int64, error) {
		freeChecks++
		if freeChecks == 1 {
			return 0, nil
		}
		return 1, nil
	}

	reclaimLostEventDirectorySpaceWithFree(transport, dir, 1, now, freeBytes)

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest file should be evicted first: %v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest file should remain after enough space is reclaimed: %v", err)
	}
	payloads := capturedStatusPayloads(t, transport)
	if len(payloads) != 1 || !strings.Contains(payloads[0], "oldest.bin evicted") {
		t.Fatalf("emergency status = %q, want oldest.bin evicted", payloads)
	}
}

func TestOldestLostEventFilesExcludesNonRegularEntries(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(regular, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(regular, filepath.Join(dir, "payload-link")); err != nil {
		t.Fatal(err)
	}

	candidates, err := oldestLostEventFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].path != regular {
		t.Fatalf("candidates = %#v, want only %s", candidates, regular)
	}
}

func TestLostMessageTruncatedJSONStatusIsValidAndEscaped(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.payloadSize = 65535
	config.lostMessageTruncatedMessageFormat = messageFormatJSON
	config.lostMessageTruncatedMessage = `{"eventName":"message.lost.truncated","body":{"stringValue":"AIRGAP LOST MESSAGE TRUNCATED"},"attributes":[{"key":"file.name","value":{"stringValue":"{fileName}"}},{"key":"airgap.event.lost.action","value":{"stringValue":"{action}"}}]}`
	transport := &captureTransport{}

	emitLostMessageTruncated(transport, `/lost/value "quoted"\backslash.bin`,
		time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), 2048, 512, "deleted")

	payloads := capturedStatusPayloads(t, transport)
	if len(payloads) != 1 {
		t.Fatalf("status payload count = %d, want 1", len(payloads))
	}
	if !json.Valid([]byte(payloads[0])) {
		t.Fatalf("status payload is invalid JSON: %s", payloads[0])
	}
	if !strings.Contains(payloads[0], `value \"quoted\"\\backslash.bin`) {
		t.Fatalf("status payload did not JSON-escape file name: %s", payloads[0])
	}
}

func TestLostMessageTruncatedNoneSendsNoStatus(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.payloadSize = 65535
	config.lostMessageTruncatedMessage = "NONE"
	transport := &captureTransport{}

	emitLostMessageTruncated(transport, "/lost/file.bin", time.Now(), 2048, 512, "deleted")

	if len(transport.messages) != 0 {
		t.Fatalf("NONE sent %d status frames", len(transport.messages))
	}
}

func TestRecordLostEventReturnsGuardAndWritesOriginal(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	dir := t.TempDir()
	config = DefaultConfiguration()
	config.lostEventGuard = true
	config.lostEventDirectory = dir

	atomic.StoreInt64(&lostEvents, 0)
	atomic.StoreInt64(&totalLostEvents, 0)

	guard := recordLostEvent(nil, "transfer_0_9", "too large", []byte("original payload"))
	if !strings.HasPrefix(string(guard), lostEventPrefix) {
		t.Fatalf("expected a guard payload, got %q", guard)
	}
	if atomic.LoadInt64(&lostEvents) != 1 || atomic.LoadInt64(&totalLostEvents) != 1 {
		t.Errorf("lost event counters not incremented: %d / %d",
			atomic.LoadInt64(&lostEvents), atomic.LoadInt64(&totalLostEvents))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading lost-event directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 lost-event file, got %d", len(entries))
	}
	content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("reading lost-event file: %v", err)
	}
	if string(content) != "original payload" {
		t.Errorf("lost-event file content = %q, want %q", content, "original payload")
	}
}

func TestRecordLostEventWithoutGuardReturnsNil(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.lostEventGuard = false
	config.lostEventDirectory = ""

	if guard := recordLostEvent(nil, "transfer_0_9", "too large", []byte("payload")); guard != nil {
		t.Errorf("expected nil guard when lostEventGuard is disabled, got %q", guard)
	}
}

func TestRecordLostEventWithoutLostEventDirectory(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.lostEventGuard = true
	config.lostEventDirectory = ""

	guard := recordLostEvent(nil, "transfer_0_9", "too large", []byte("payload"))
	if !strings.Contains(string(guard), "transfer 0:9") {
		t.Errorf("guard does not identify the event: %q", guard)
	}
}

func TestEncodeForTransportCleartext(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.encryption = false
	config.payloadSize = 1400

	messages, err := encodeForTransport("transfer_0_1", []byte("hello"), false)
	if err != nil {
		t.Fatalf("encodeForTransport returned error: %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("expected at least one wire message")
	}
}

func TestEncodeForTransportEncryptionErrorIsReturned(t *testing.T) {
	saved := config
	defer func() { config = saved }()

	config = DefaultConfiguration()
	config.encryption = true
	config.key = []byte("too-short-key")
	config.payloadSize = 1400

	if _, err := encodeForTransport("transfer_0_1", []byte("hello"), false); err == nil {
		t.Fatal("expected an error for an invalid symmetric key")
	}
}
