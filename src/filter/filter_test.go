package filter

import "testing"

// Guards the fix for the Go negative-modulo bug: before this fix,
// Check(0) returned false for BOTH the "1,3,5" and "2,4,6" filters,
// silently dropping Kafka offset 0 of every partition on every chain.
// After the fix:
//   * "1,3,5" (groups[0]=[1], k=2) delivers numbers with pos=1 — i.e.
//     odd numbers {…, -1, 1, 3, 5, …}. Zero has pos=2, so it is NOT
//     delivered by this chain.
//   * "2,4,6" (groups[0]=[2], k=2) delivers numbers with pos=2 — i.e.
//     even numbers {…, -2, 0, 2, 4, 6, …}. Zero IS delivered by this
//     chain, closing the offset-0 gap for TC-12 chain-run.
func TestCheckCoversOffsetZeroForEvenChain(t *testing.T) {
	odd, err := NewFilter("1,3,5")
	if err != nil {
		t.Fatalf("NewFilter(1,3,5) failed: %v", err)
	}
	even, err := NewFilter("2,4,6")
	if err != nil {
		t.Fatalf("NewFilter(2,4,6) failed: %v", err)
	}

	if odd.Check(0) {
		t.Error("odd (1,3,5) chain should NOT deliver offset 0 (pos=2)")
	}
	if !even.Check(0) {
		t.Error("even (2,4,6) chain SHOULD deliver offset 0 (pos=2); " +
			"regression of the Go negative-modulo bug — pre-fix both chains dropped 0")
	}

	for _, n := range []int64{1, 3, 5, 7, 9, 11} {
		if !odd.Check(n) {
			t.Errorf("odd chain should deliver %d", n)
		}
		if even.Check(n) {
			t.Errorf("even chain should NOT deliver %d", n)
		}
	}
	for _, n := range []int64{2, 4, 6, 8, 10} {
		if odd.Check(n) {
			t.Errorf("odd chain should NOT deliver %d", n)
		}
		if !even.Check(n) {
			t.Errorf("even chain should deliver %d", n)
		}
	}
}

func TestCombinedCoverageZeroThroughN(t *testing.T) {
	odd, _ := NewFilter("1,3,5")
	even, _ := NewFilter("2,4,6")
	for n := int64(0); n < 20; n++ {
		a, b := odd.Check(n), even.Check(n)
		if a == b {
			t.Errorf("number %d: both chains agree (odd=%v even=%v); expected exactly one", n, a, b)
		}
	}
}

func TestPosModNonNegative(t *testing.T) {
	cases := []struct {
		a, m, want int64
	}{
		{-1, 2, 1},
		{-2, 2, 0},
		{-3, 2, 1},
		{0, 2, 0},
		{1, 2, 1},
		{2, 2, 0},
		{-1, 3, 2},
		{-2, 3, 1},
		{-3, 3, 0},
	}
	for _, c := range cases {
		if got := posMod(c.a, c.m); got != c.want {
			t.Errorf("posMod(%d, %d) = %d; want %d", c.a, c.m, got, c.want)
		}
	}
}
