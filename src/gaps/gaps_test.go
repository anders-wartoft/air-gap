package gaps

import "testing"

func TestParseGapPayload_LegacyJSON(t *testing.T) {
	value := []byte(`{"partition":48,"topic":"export","window_min":0,"window_max":4999999,"gaps":[[2,4]]}`)
	pw, err := parseGapPayload([]byte("export_48:0"), value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pw.legacy || pw.topic != "export" || pw.partition != 48 || pw.windowMax != 4999999 || countMissing(pw.relGaps) != 3 {
		t.Errorf("unexpected window: %+v", pw)
	}
}

func TestGapState_TombstonesAndLegacyPrecedence(t *testing.T) {
	legacy := []byte(`{"partition":0,"topic":"t","window_min":0,"window_max":999,"gaps":[[9]]}`)
	state := newGapState()
	for _, r := range []struct{ key, value []byte }{
		{[]byte("t_0:0"), legacy},
		{[]byte("t:0:0"), nil},
		{[]byte("t_0:1000"), []byte(`{"partition":0,"topic":"t","window_min":1000,"window_max":1999,"gaps":[[1]]}`)},
		{[]byte("t_0:1000"), nil},
		{[]byte("t_0:2000"), []byte(`{"partition":0,"topic":"t","window_min":2000,"window_max":2999,"gaps":[[3]]}`)},
	} {
		if err := state.apply(r.key, r.value); err != nil {
			t.Fatalf("apply %s: %v", r.key, err)
		}
	}
	windows := state.windows()
	if len(windows) != 1 || !windows["t-0-2000"].legacy {
		t.Errorf("expected only legacy window 2000, got %+v", windows)
	}
	if state.stats.legacy != 3 || state.stats.tombstones != 2 || state.superseded() != 1 || state.stats.failed != 0 {
		t.Errorf("unexpected stats: %+v superseded=%d", state.stats, state.superseded())
	}
}
