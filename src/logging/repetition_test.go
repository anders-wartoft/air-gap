package logging

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRepeatedMessagesCountAcrossInterleavedMessages(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventDiagnostic)
	for range 10001 {
		Logger.Warn(attrs, "connection failed")
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Fatalf("10,001 identical calls produced %d records, want 1", got)
	}
	Logger.Info(attrs, "recovered")
	Logger.Warn(attrs, "connection failed")
	Logger.FlushRepeated()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "recovered") ||
		!strings.Contains(lines[2], "[WARN] Message repeated 10001 times: connection failed") {
		t.Fatalf("unexpected transition output: %s", buf.String())
	}
}

func TestRepeatedMessagesPreserveSeverityAndAttributes(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	first := Event(EventDiagnostic, Str("client.address", "one"))
	second := Event(EventDiagnostic, Str("client.address", "two"))
	Logger.Warn(first, "failed")
	Logger.Error(first, "failed")
	Logger.Error(second, "failed")
	Logger.Error(Event(EventTLSHandshake, Str("client.address", "two")), "failed")
	if got := strings.Count(buf.String(), "\n"); got != 4 {
		t.Fatalf("different severities/attributes were suppressed: %s", buf.String())
	}
}

func TestRepeatSummaryTimerAndExplicitFlush(t *testing.T) {
	buf := resetLogging(t)
	interval := 40 * time.Millisecond
	if err := Logger.SetRepeatInterval(interval); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventDiagnostic)
	start := time.Now()
	Logger.Warn(attrs, "failed")
	Logger.Warn(attrs, "failed")
	deadline := time.Now().Add(2 * time.Second)
	for {
		emitMu.Lock()
		summary := strings.Contains(buf.String(), "Message repeated 1 times: failed")
		emitMu.Unlock()
		if summary {
			if time.Since(start) < interval {
				t.Fatal("summary emitted before the configured interval")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no summary emitted when logging became idle")
		}
		time.Sleep(time.Millisecond)
	}
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	Logger.Warn(attrs, "failed")
	Logger.Warn(attrs, "failed")
	Logger.FlushRepeated()
	emitMu.Lock()
	defer emitMu.Unlock()
	if got := strings.Count(buf.String(), "Message repeated 1 times: failed"); got != 2 {
		t.Fatalf("explicit flush did not emit the final repeat: %s", buf.String())
	}
}

func TestRepeatSummaryHasIntegrityAndConcurrentCount(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetHashAlgorithm("SHA256"); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetHashChainLength(2); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventTLSHandshake, Str("client.address", "127.0.0.1"),
		Error("certificate.missing"))
	Logger.Warn(attrs, "failed")
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				Logger.Warn(attrs, "failed")
			}
		}()
	}
	wg.Wait()
	Logger.FlushRepeated()
	records := decodeOTLPLines(t, buf.String())
	if len(records) != 3 {
		t.Fatalf("got %d records, want original, summary, anchor", len(records))
	}
	summary := records[1]
	if summary.eventName != EventTLSHandshake || summary.severity != "WARN" ||
		summary.body != "failed" ||
		summary.attributes["airgap.repeat.count"] != "1000" ||
		summary.attributes["client.address"] != "127.0.0.1" ||
		summary.attributes[AttrErrorType] != "certificate.missing" ||
		summary.attributes[AttrHash] == "" ||
		summary.attributes[AttrChainPrev] != records[0].attributes[AttrChain] ||
		records[2].attributes[AttrChainPrev] != summary.attributes[AttrChain] {
		t.Fatalf("invalid summary or chain: %+v", records)
	}
}

func TestRepeatIntervalParsingAndDisabledBehavior(t *testing.T) {
	buf := resetLogging(t)
	for _, value := range []string{"-1", "oops", "0.5", "9223372036855"} {
		if _, err := parseRepeatInterval(value); err == nil {
			t.Fatalf("invalid interval %q accepted", value)
		}
	}
	if interval, err := parseRepeatInterval("1234"); err != nil || interval != 1234*time.Millisecond {
		t.Fatalf("interval = %v, error = %v", interval, err)
	}
	buf.Reset()
	for range 3 {
		Logger.Warn(Event(EventDiagnostic), "failed")
	}
	if strings.Count(buf.String(), "\n") != 3 {
		t.Fatalf("zero did not disable suppression: %s", buf.String())
	}
}

func TestFatalIsNeverSuppressed(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		emit(FATAL, Event(EventAppFailure), "fatal")
	}
	if strings.Count(buf.String(), "[FATAL] fatal") != 2 {
		t.Fatalf("fatal record suppressed: %s", buf.String())
	}
}

func TestRepeatedMessagesFlushBeforeLogRotation(t *testing.T) {
	resetLogging(t)
	first := filepath.Join(t.TempDir(), "before.log")
	second := filepath.Join(t.TempDir(), "after.log")
	if err := Logger.SetLogFile(first); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventDiagnostic)
	Logger.Warn(attrs, "failed")
	Logger.Warn(attrs, "failed")
	if err := Logger.SetLogFile(second); err != nil {
		t.Fatal(err)
	}
	Logger.Warn(attrs, "failed")
	old, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(old), "Message repeated 1 times: failed") ||
		strings.Count(string(current), "\n") != 1 ||
		strings.Contains(string(current), "repeated") {
		t.Fatalf("summary crossed log files: before=%s after=%s", old, current)
	}
}

func TestRepeatIntervalEnvironmentHelper(t *testing.T) {
	if os.Getenv("AIRGAP_REPEAT_TEST_HELPER") != "1" {
		return
	}
	expected, err := parseRepeatInterval(os.Getenv("AIRGAP_LOG_REPEAT_INTERVAL"))
	if err != nil || repeatInterval != expected {
		t.Fatalf("interval = %v, expected %v, error = %v", repeatInterval, expected, err)
	}
}

func TestRepeatIntervalEnvironment(t *testing.T) {
	for _, value := range []string{"0", "1234", "-1", "oops", ""} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRepeatIntervalEnvironmentHelper$")
			t.Setenv("AIRGAP_LOG_REPEAT_INTERVAL", value)
			cmd.Env = append(os.Environ(), "AIRGAP_REPEAT_TEST_HELPER=1")
			output, err := cmd.CombinedOutput()
			_, parseErr := parseRepeatInterval(value)
			if parseErr == nil && err != nil {
				t.Fatalf("valid environment failed: %v\n%s", err, output)
			}
			if parseErr != nil && (err == nil || !strings.Contains(string(output), "Invalid AIRGAP_LOG_REPEAT_INTERVAL")) {
				t.Fatalf("invalid environment did not fail explicitly: %v\n%s", err, output)
			}
		})
	}
}

func TestRepeatCacheBoundAndEvictionFlush(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	attrs := Event(EventDiagnostic)
	Logger.Warn(attrs, "oldest")
	Logger.Warn(attrs, "oldest")
	for i := range maxRepeatEntries {
		Logger.Warnf(attrs, "unique %d", i)
	}
	emitMu.Lock()
	defer emitMu.Unlock()
	if len(repetitions) != maxRepeatEntries {
		t.Fatalf("cache has %d entries, want %d", len(repetitions), maxRepeatEntries)
	}
	if strings.Count(buf.String(), "Message repeated 1 times: oldest") != 1 {
		t.Fatalf("eviction lost pending count: %s", buf.String())
	}
}

func TestRepeatCacheExpiresIdleEntries(t *testing.T) {
	resetLogging(t)
	if err := Logger.SetRepeatInterval(20 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	Logger.Warn(Event(EventDiagnostic), "one-off")
	deadline := time.Now().Add(time.Second)
	for {
		emitMu.Lock()
		empty := len(repetitions) == 0 && repeatTimer == nil
		emitMu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle entries/timer were not released")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRepeatedMessagesFlushBeforeInstanceChange(t *testing.T) {
	buf := resetLogging(t)
	if err := Logger.SetFormat(FormatOTel); err != nil {
		t.Fatal(err)
	}
	if err := Logger.SetRepeatInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Logger.SetInstanceID("") })
	Logger.SetInstanceID("before")
	attrs := Event(EventDiagnostic)
	Logger.Warn(attrs, "failed")
	Logger.Warn(attrs, "failed")
	Logger.SetInstanceID("after")
	Logger.Warn(attrs, "failed")
	records := decodeOTLPLines(t, buf.String())
	if len(records) != 3 || records[1].eventName != EventDiagnostic ||
		records[1].resource["service.instance.id"] != "before" ||
		records[2].resource["service.instance.id"] != "after" {
		t.Fatalf("repeat state crossed service identities: %+v", records)
	}
}

func TestJSONRepeatCountOnlyPresentWhenNotOne(t *testing.T) {
	for _, count := range []int{1, 2, 5} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			buf := resetLogging(t)
			if err := Logger.SetFormat(FormatOTel); err != nil {
				t.Fatal(err)
			}
			if err := Logger.SetRepeatInterval(time.Hour); err != nil {
				t.Fatal(err)
			}
			attrs := Event(EventTLSHandshake, Str("client.address", "peer"),
				Error("certificate.missing"))
			Logger.Warn(attrs, "client sent no certificate")
			for range count {
				Logger.Warn(attrs, "client sent no certificate")
			}
			Logger.FlushRepeated()
			records := decodeOTLPLines(t, buf.String())
			if len(records) != 2 {
				t.Fatalf("got %d records, want original and repeat", len(records))
			}
			original, repeated := records[0], records[1]
			if _, present := original.attributes["airgap.repeat.count"]; present {
				t.Fatal("original record must not have a repeat count")
			}
			value, present := repeated.attributes["airgap.repeat.count"]
			if present != (count != 1) || (present && value != strconv.Itoa(count)) {
				t.Fatalf("count %d: attribute = %q, present = %v", count, value, present)
			}
			delete(repeated.attributes, "airgap.repeat.count")
			if repeated.body != original.body || repeated.eventName != original.eventName ||
				repeated.severity != original.severity || repeated.severityNo != original.severityNo ||
				!reflect.DeepEqual(repeated.attributes, original.attributes) ||
				!reflect.DeepEqual(repeated.resource, original.resource) {
				t.Fatalf("repeat changed original event: original=%+v repeated=%+v", original, repeated)
			}
			var payload otlpPayload
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if err := json.Unmarshal([]byte(lines[1]), &payload); err != nil {
				t.Fatal(err)
			}
			for _, attr := range payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
				if attr.Key == "airgap.repeat.count" && attr.Value.IntValue == nil {
					t.Fatal("repeat count must use OTLP intValue")
				}
			}
		})
	}
}
