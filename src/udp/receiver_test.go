package udp

import (
	"math"
	"testing"
)

func TestRecordRxqOverflow(t *testing.T) {
	RxqOvflDrops.Store(0)
	RxqOvflDropsTotal.Store(0)
	t.Cleanup(func() {
		RxqOvflDrops.Store(0)
		RxqOvflDropsTotal.Store(0)
	})

	var first, second uint32
	recordRxqOverflow(&first, 7)
	recordRxqOverflow(&first, 7)
	recordRxqOverflow(&second, 3)
	recordRxqOverflow(&first, 10)
	if got := RxqOvflDrops.Swap(0); got != 13 {
		t.Fatalf("first interval: got %d drops, want 13", got)
	}

	recordRxqOverflow(&second, 5)
	if got := RxqOvflDrops.Load(); got != 2 {
		t.Errorf("second interval: got %d drops, want 2", got)
	}
	if got := RxqOvflDropsTotal.Load(); got != 15 {
		t.Errorf("total: got %d drops, want 15", got)
	}

	first = math.MaxUint32 - 2
	recordRxqOverflow(&first, 2)
	if got := RxqOvflDrops.Load(); got != 7 {
		t.Errorf("interval after wrap: got %d drops, want 7", got)
	}
	if got := RxqOvflDropsTotal.Load(); got != 20 {
		t.Errorf("total after wrap: got %d drops, want 20", got)
	}
}
