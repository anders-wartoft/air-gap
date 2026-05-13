package create

import (
	"encoding/json"
	"os"
	"testing"
)

// buildGapMessage produces a JSON byte slice as emitted by the Java dedup app
// to the gap topic. The gap offsets are relative to windowMin, matching the
// format written by PartitionDedupApp.emitGapsForPartition.
func buildGapMessage(topic string, partition int, windowMin, windowMax float64, gaps [][]float64) json.RawMessage {
	gapsI := make([]interface{}, len(gaps))
	for i, g := range gaps {
		gi := make([]interface{}, len(g))
		for j, v := range g {
			gi[j] = v
		}
		gapsI[i] = gi
	}
	m := map[string]interface{}{
		"topic":      topic,
		"partition":  partition,
		"window_min": windowMin,
		"window_max": windowMax,
		"gaps":       gapsI,
	}
	b, _ := json.Marshal(m)
	return json.RawMessage(b)
}

// parseBundleFile reads the file written by writeResults and unmarshals it into
// a generic struct suitable for assertions.
type bundleResult struct {
	Topic     string      `json:"topic"`
	Partition float64     `json:"partition"`
	WindowMin float64     `json:"window_min"`
	WindowMax float64     `json:"window_max"`
	Gaps      [][]float64 `json:"gaps"`
}

type bundle struct {
	Type    string         `json:"type"`
	Results []bundleResult `json:"results"`
}

func readBundle(t *testing.T, path string) bundle {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	var b bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("failed to parse output JSON (%s): %v\nraw: %s", path, err, data)
	}
	return b
}

func findPartition(b bundle, partition int) *bundleResult {
	for i := range b.Results {
		if int(b.Results[i].Partition) == partition {
			return &b.Results[i]
		}
	}
	return nil
}

func assertSingleGap(t *testing.T, label string, gap []float64, want float64) {
	t.Helper()
	if len(gap) != 1 {
		t.Errorf("%s: expected single-element gap, got %v", label, gap)
		return
	}
	if gap[0] != want {
		t.Errorf("%s: expected offset %.0f, got %.0f", label, want, gap[0])
	}
}

func assertRangeGap(t *testing.T, label string, gap []float64, wantFrom, wantTo float64) {
	t.Helper()
	if len(gap) != 2 {
		t.Errorf("%s: expected two-element range, got %v", label, gap)
		return
	}
	if gap[0] != wantFrom || gap[1] != wantTo {
		t.Errorf("%s: expected [%.0f,%.0f], got %v", label, wantFrom, wantTo, gap)
	}
}

func tempFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "resend-test-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// TestWriteResults_MultipleWindowsMergedToAbsoluteOffsets verifies that when the
// gap topic contains multiple windows for the same partition, writeResults merges
// them into a single entry per partition and converts all relative offsets to
// absolute Kafka offsets.
//
// Setup:
//
//	Partition 0, window 1: window_min=0,    relative gaps: [3] and [7,9]
//	Partition 0, window 2: window_min=1000, relative gaps: [5] and [10,15]
//	Partition 1, window 1: window_min=0,    relative gap:  [2]
//
// Expected absolute gaps for partition 0: [3], [7,9], [1005], [1010,1015]
// Expected absolute gaps for partition 1: [2]
func TestWriteResults_MultipleWindowsMergedToAbsoluteOffsets(t *testing.T) {
	lastEntries := map[string]json.RawMessage{
		"transfer-0-0":    buildGapMessage("transfer", 0, 0, 999, [][]float64{{3}, {7, 9}}),
		"transfer-0-1000": buildGapMessage("transfer", 0, 1000, 1999, [][]float64{{5}, {10, 15}}),
		"transfer-1-0":    buildGapMessage("transfer", 1, 0, 999, [][]float64{{2}}),
	}
	out := tempFile(t)
	writeResults(lastEntries, TransferConfiguration{limit: "all", resendFileName: out})

	b := readBundle(t, out)

	if b.Type != "all" {
		t.Errorf("expected type 'all', got %q", b.Type)
	}
	if len(b.Results) != 2 {
		t.Fatalf("expected 2 results (one per partition), got %d", len(b.Results))
	}

	// --- Partition 0 ---
	p0 := findPartition(b, 0)
	if p0 == nil {
		t.Fatal("missing result for partition 0")
	}
	if p0.WindowMin != 0 {
		t.Errorf("partition 0: expected window_min=0 (normalised), got %.0f", p0.WindowMin)
	}
	if p0.WindowMax != 1999 {
		t.Errorf("partition 0: expected window_max=1999 (largest window), got %.0f", p0.WindowMax)
	}
	if len(p0.Gaps) != 4 {
		t.Fatalf("partition 0: expected 4 absolute gaps, got %d: %v", len(p0.Gaps), p0.Gaps)
	}
	assertSingleGap(t, "p0 gap[0]", p0.Gaps[0], 3)
	assertRangeGap(t, "p0 gap[1]", p0.Gaps[1], 7, 9)
	assertSingleGap(t, "p0 gap[2]", p0.Gaps[2], 1005)      // 1000 + 5
	assertRangeGap(t, "p0 gap[3]", p0.Gaps[3], 1010, 1015) // 1000+10, 1000+15

	// --- Partition 1 ---
	p1 := findPartition(b, 1)
	if p1 == nil {
		t.Fatal("missing result for partition 1")
	}
	if p1.WindowMin != 0 {
		t.Errorf("partition 1: expected window_min=0, got %.0f", p1.WindowMin)
	}
	if len(p1.Gaps) != 1 {
		t.Fatalf("partition 1: expected 1 gap, got %d: %v", len(p1.Gaps), p1.Gaps)
	}
	assertSingleGap(t, "p1 gap[0]", p1.Gaps[0], 2)
}

// TestWriteResults_FirstMode_PicksGloballyFirstAbsoluteGap verifies that "first"
// mode selects the gap with the smallest absolute offset across all windows, not
// simply the first entry encountered. The windows arrive in non-sequential map
// iteration order, so the merge sort by windowMin is what enforces correctness.
//
// Setup:
//
//	Window with window_min=1000, relative gap: [5]  → absolute 1005
//	Window with window_min=0,    relative gap: [8]  → absolute 8
//
// Expected: only gap [8] emitted (absolute 8 < absolute 1005).
func TestWriteResults_FirstMode_PicksGloballyFirstAbsoluteGap(t *testing.T) {
	lastEntries := map[string]json.RawMessage{
		"transfer-0-1000": buildGapMessage("transfer", 0, 1000, 1999, [][]float64{{5}}),
		"transfer-0-0":    buildGapMessage("transfer", 0, 0, 999, [][]float64{{8}}),
	}
	out := tempFile(t)
	writeResults(lastEntries, TransferConfiguration{limit: "first", resendFileName: out})

	b := readBundle(t, out)

	if b.Type != "first" {
		t.Errorf("expected type 'first', got %q", b.Type)
	}
	if len(b.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(b.Results))
	}
	if len(b.Results[0].Gaps) != 1 {
		t.Fatalf("first mode: expected exactly 1 gap, got %d: %v", len(b.Results[0].Gaps), b.Results[0].Gaps)
	}
	assertSingleGap(t, "first mode gap", b.Results[0].Gaps[0], 8)
}

// TestWriteResults_ThreeWindows_AllAbsoluteOffsets exercises three consecutive
// windows for a single partition (simulating a long-running partition with many
// messages) and confirms that all gaps across all three windows are present in
// the output as absolute offsets.
//
// Setup (window_size = 1000):
//
//	Window 0–999:    relative gaps [2], [50, 55]
//	Window 1000–1999: relative gaps [100, 102]
//	Window 2000–2999: relative gaps [0], [500]
//
// Expected absolute gaps: [2], [50,55], [1100,1102], [2000], [2500]
func TestWriteResults_ThreeWindows_AllAbsoluteOffsets(t *testing.T) {
	lastEntries := map[string]json.RawMessage{
		"events-0-0":    buildGapMessage("events", 0, 0, 999, [][]float64{{2}, {50, 55}}),
		"events-0-1000": buildGapMessage("events", 0, 1000, 1999, [][]float64{{100, 102}}),
		"events-0-2000": buildGapMessage("events", 0, 2000, 2999, [][]float64{{0}, {500}}),
	}
	out := tempFile(t)
	writeResults(lastEntries, TransferConfiguration{limit: "all", resendFileName: out})

	b := readBundle(t, out)

	p0 := findPartition(b, 0)
	if p0 == nil {
		t.Fatal("missing result for partition 0")
	}
	if p0.WindowMax != 2999 {
		t.Errorf("expected window_max=2999, got %.0f", p0.WindowMax)
	}
	if len(p0.Gaps) != 5 {
		t.Fatalf("expected 5 absolute gaps, got %d: %v", len(p0.Gaps), p0.Gaps)
	}
	assertSingleGap(t, "gap[0]", p0.Gaps[0], 2)
	assertRangeGap(t, "gap[1]", p0.Gaps[1], 50, 55)
	assertRangeGap(t, "gap[2]", p0.Gaps[2], 1100, 1102) // 1000+100, 1000+102
	assertSingleGap(t, "gap[3]", p0.Gaps[3], 2000)      // 2000+0
	assertSingleGap(t, "gap[4]", p0.Gaps[4], 2500)      // 2000+500
}

// TestWriteResults_EmptyGaps verifies that a window with no gaps produces an
// empty gaps array in the output (no entries are dropped).
func TestWriteResults_EmptyGaps(t *testing.T) {
	lastEntries := map[string]json.RawMessage{
		"transfer-0-0": buildGapMessage("transfer", 0, 0, 999, [][]float64{}),
	}
	out := tempFile(t)
	writeResults(lastEntries, TransferConfiguration{limit: "all", resendFileName: out})

	b := readBundle(t, out)

	if len(b.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(b.Results))
	}
	if len(b.Results[0].Gaps) != 0 {
		t.Errorf("expected empty gaps, got %v", b.Results[0].Gaps)
	}
}

// TestWriteResults_OnlyEmptyWindowsMergedWithGappedWindow verifies that when one
// window has no gaps and another does, the merged result contains only the gaps
// from the gapped window at their correct absolute offsets.
func TestWriteResults_OnlyEmptyWindowsMergedWithGappedWindow(t *testing.T) {
	lastEntries := map[string]json.RawMessage{
		"transfer-0-0":    buildGapMessage("transfer", 0, 0, 999, [][]float64{}),
		"transfer-0-1000": buildGapMessage("transfer", 0, 1000, 1999, [][]float64{{7}, {20, 25}}),
	}
	out := tempFile(t)
	writeResults(lastEntries, TransferConfiguration{limit: "all", resendFileName: out})

	b := readBundle(t, out)

	p0 := findPartition(b, 0)
	if p0 == nil {
		t.Fatal("missing result for partition 0")
	}
	if len(p0.Gaps) != 2 {
		t.Fatalf("expected 2 gaps, got %d: %v", len(p0.Gaps), p0.Gaps)
	}
	assertSingleGap(t, "gap[0]", p0.Gaps[0], 1007)      // 1000+7
	assertRangeGap(t, "gap[1]", p0.Gaps[1], 1020, 1025) // 1000+20, 1000+25
}
