package create

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring"
)

func binaryGapPayload(t *testing.T, windowMin, windowMax int64, relMissing ...uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte(1)
	_ = binary.Write(&buf, binary.BigEndian, windowMin)
	_ = binary.Write(&buf, binary.BigEndian, windowMax)
	rb := roaring.BitmapOf(relMissing...)
	if _, err := rb.WriteTo(&buf); err != nil {
		t.Fatalf("failed to write bitmap: %v", err)
	}
	return buf.Bytes()
}

func TestParseGapPayload_LegacyJSON(t *testing.T) {
	key := []byte("export_48:1000")
	value := []byte(`{"partition":48,"topic":"export","window_min":1000,"window_max":1999,"gaps":[[3],[7,9]]}`)
	pw, err := parseGapPayload(key, value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pw.legacy || pw.topic != "export" || pw.partition != 48 || pw.windowMin != 1000 || pw.windowMax != 1999 {
		t.Fatalf("unexpected window: %+v", pw)
	}
	if fmt.Sprint(pw.relGaps) != "[[3] [7 9]]" {
		t.Errorf("unexpected gaps: %v", pw.relGaps)
	}
}

func TestParseGapPayload_LegacyJSONTopicWithUnderscore(t *testing.T) {
	value := []byte(`{"partition":2,"topic":"my_topic","window_min":0,"window_max":999,"gaps":[]}`)
	pw, err := parseGapPayload([]byte("my_topic_2:0"), value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw.topic != "my_topic" || pw.partition != 2 {
		t.Errorf("unexpected window: %+v", pw)
	}
}

func TestParseGapPayload_LegacyJSONMissingFields(t *testing.T) {
	if _, err := parseGapPayload([]byte("x_1:0"), []byte(`{"gaps":[]}`)); err == nil {
		t.Error("expected error for legacy payload without topic/partition/window_min")
	}
	if _, err := parseGapPayload([]byte("x_1:0"), []byte(`{"topic":"x","partition":1,"window_min":0,"gaps":[[1,2,3]]}`)); err == nil {
		t.Error("expected error for invalid gap range")
	}
}

type gapRecord struct{ key, value []byte }

func applyAll(t *testing.T, records ...gapRecord) *gapState {
	t.Helper()
	state := newGapState()
	for _, r := range records {
		if err := state.apply(r.key, r.value); err != nil {
			t.Fatalf("apply %s: %v", r.key, err)
		}
	}
	return state
}

func legacyRecord(topic string, partition int, windowMin, windowMax int64, gaps string) gapRecord {
	return gapRecord{
		key:   []byte(fmt.Sprintf("%s_%d:%d", topic, partition, windowMin)),
		value: []byte(fmt.Sprintf(`{"partition":%d,"topic":%q,"window_min":%d,"window_max":%d,"gaps":%s}`, partition, topic, windowMin, windowMax, gaps)),
	}
}

func TestGapState_BinaryWinsOverLegacyRegardlessOfOrder(t *testing.T) {
	legacy := legacyRecord("t", 0, 0, 999, "[[1]]")
	current := gapRecord{[]byte("t:0:0"), binaryGapPayload(t, 0, 999, 5)}

	for _, order := range [][]gapRecord{{legacy, current}, {current, legacy}} {
		state := applyAll(t, order...)
		got := state.windows()["t-0-0"]
		if got.legacy || fmt.Sprint(got.relGaps) != "[[5]]" {
			t.Errorf("binary record should win, got %+v", got)
		}
		if state.stats.legacy != 1 || state.superseded() != 1 {
			t.Errorf("unexpected stats: %+v superseded=%d", state.stats, state.superseded())
		}
	}
}

func TestGapState_TombstoneRemovesWindow(t *testing.T) {
	state := applyAll(t,
		gapRecord{[]byte("t:0:0"), binaryGapPayload(t, 0, 999, 5)},
		gapRecord{[]byte("t:0:1000"), binaryGapPayload(t, 1000, 1999, 7)},
		gapRecord{[]byte("t:0:0"), nil},
	)
	windows := state.windows()
	if _, ok := windows["t-0-0"]; ok || len(windows) != 1 {
		t.Errorf("tombstoned window still present: %+v", windows)
	}
	if state.stats.tombstones != 1 || state.stats.failed != 0 {
		t.Errorf("unexpected stats: %+v", state.stats)
	}
}

func TestGapState_BinaryTombstoneHidesLegacyRegardlessOfOrder(t *testing.T) {
	legacy := legacyRecord("my_topic", 2, 0, 999, "[[1]]")
	tombstone := gapRecord{[]byte("my_topic:2:0"), nil}
	for _, order := range [][]gapRecord{{legacy, tombstone}, {tombstone, legacy}} {
		state := applyAll(t, order...)
		if windows := state.windows(); len(windows) != 0 {
			t.Errorf("legacy window should be hidden by binary tombstone: %+v", windows)
		}
	}
}

func TestGapState_LegacyKeyTombstone(t *testing.T) {
	state := applyAll(t,
		legacyRecord("my_topic", 2, 0, 999, "[[1]]"),
		gapRecord{[]byte("my_topic_2:0"), nil},
	)
	if windows := state.windows(); len(windows) != 0 {
		t.Errorf("legacy tombstone should delete window: %+v", windows)
	}
	if state.stats.failed != 0 || state.superseded() != 0 {
		t.Errorf("unexpected stats: %+v", state.stats)
	}
}

func TestGapState_TombstoneWithInvalidKeyIsFailure(t *testing.T) {
	state := newGapState()
	if err := state.apply([]byte("nonsense"), nil); err == nil {
		t.Fatal("expected error for tombstone with invalid key")
	}
	if state.stats.failed != 1 || state.stats.tombstones != 0 {
		t.Errorf("unexpected stats: %+v", state.stats)
	}
}

func TestParseGapKey(t *testing.T) {
	cases := []struct {
		key       string
		topic     string
		partition int
		windowMin int64
		legacy    bool
	}{
		{"export:48:5000", "export", 48, 5000, false},
		{"my_topic:2:0", "my_topic", 2, 0, false},
		{"export_48:0", "export", 48, 0, true},
		{"my_topic_2:100", "my_topic", 2, 100, true},
	}
	for _, c := range cases {
		topic, partition, windowMin, legacy, err := parseGapKey([]byte(c.key))
		if err != nil || topic != c.topic || partition != c.partition || windowMin != c.windowMin || legacy != c.legacy {
			t.Errorf("parseGapKey(%q) = %q %d %d %t %v", c.key, topic, partition, windowMin, legacy, err)
		}
	}
	for _, bad := range []string{"", "x", "a:b:c:d", "topic:x:0", "topic_x:0", "_1:0", "t:1:abc"} {
		if _, _, _, _, err := parseGapKey([]byte(bad)); err == nil {
			t.Errorf("parseGapKey(%q): expected error", bad)
		}
	}
}

// TestLogParseSummary_OneLinePerCategory verifies that legacy and unparseable
// records are reported with one summary line each, not one line per record.
func TestLogParseSummary_OneLinePerCategory(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "create.log")
	if err := Logger.SetLogFile(logFile); err != nil {
		t.Fatalf("SetLogFile: %v", err)
	}
	t.Cleanup(func() { _ = Logger.SetLogFile("/dev/stderr") })

	logParseSummary(newGapState(), "gaps")

	records := []gapRecord{}
	for p := 0; p < 10000; p++ {
		records = append(records, legacyRecord("export", p, 0, 999, "[[1]]"))
	}
	for p := 0; p < 10; p++ {
		records = append(records, gapRecord{[]byte(fmt.Sprintf("export:%d:0", p)), nil})
	}
	state := applyAll(t, records...)
	for i := 0; i < 3; i++ {
		_ = state.apply([]byte("garbage"), []byte{1, 2})
	}
	state.stats.firstErr = errors.New("boom")
	logParseSummary(state, "gaps")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 summary lines, got %d:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], "Read 10000 gap records in the legacy JSON format") || !strings.Contains(lines[0], "10 were superseded") {
		t.Errorf("unexpected legacy summary: %s", lines[0])
	}
	if !strings.Contains(lines[1], "[ERROR]") || !strings.Contains(lines[1], "Skipped 3 gap records") || !strings.Contains(lines[1], "boom") {
		t.Errorf("unexpected error summary: %s", lines[1])
	}
}

// TestLegacyAndBinaryRecords_IncludedInBundle reproduces the upgrade scenario:
// 10 000 legacy records and 10 binary records in the gap topic. All records must
// parse, and the gaps from the legacy records must end up in the bundle.
func TestLegacyAndBinaryRecords_IncludedInBundle(t *testing.T) {
	state := newGapState()
	add := func(key, value []byte) {
		_ = state.apply(key, value)
	}

	for p := 0; p < 10000; p++ {
		value := fmt.Sprintf(`{"partition":%d,"topic":"export","window_min":0,"window_max":999,"gaps":[[%d]]}`, p, p%1000)
		add([]byte(fmt.Sprintf("export_%d:0", p)), []byte(value))
	}
	// Binary records for partitions 0..9 window 1000 (new data after the upgrade).
	for p := 0; p < 10; p++ {
		add([]byte(fmt.Sprintf("export:%d:1000", p)), binaryGapPayload(t, 1000, 1999, 4))
	}

	if state.stats.failed != 0 || state.stats.legacy != 10000 || state.superseded() != 0 {
		t.Fatalf("unexpected stats: %+v", state.stats)
	}

	out := tempFile(t)
	writeResults(state.windows(), TransferConfiguration{limit: "all", resendFileName: out})
	b := readBundle(t, out)
	if len(b.Results) != 10000 {
		t.Fatalf("expected 10000 partitions in bundle, got %d", len(b.Results))
	}
	p3 := findPartition(b, 3)
	if p3 == nil || len(p3.Gaps) != 2 {
		t.Fatalf("partition 3: expected legacy and binary gap, got %+v", p3)
	}
	assertSingleGap(t, "legacy gap", p3.Gaps[0], 3)
	assertSingleGap(t, "binary gap", p3.Gaps[1], 1004)
	p4321 := findPartition(b, 4321)
	if p4321 == nil || len(p4321.Gaps) != 1 {
		t.Fatalf("partition 4321: expected one legacy gap, got %+v", p4321)
	}
	assertSingleGap(t, "legacy-only gap", p4321.Gaps[0], 321)
}
