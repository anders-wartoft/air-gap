package udp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestUDPPacingSharesBurstAcrossSenders(t *testing.T) {
	for _, tc := range []struct {
		name           string
		packets, bytes int
		burst, size    int
	}{
		{"datagrams", 1, 1000000, 2, 1},
		{"bytes", 1000000, 1, 32, pacingByteBurst / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var conn UDPConn
			if err := conn.ConfigurePacing(ctx, tc.packets, tc.bytes, tc.burst); err != nil {
				t.Fatal(err)
			}
			msg := make([]byte, tc.size)
			writes := make(chan struct{}, 4)
			var senders sync.WaitGroup
			results := make(chan error, 2)
			for range 2 {
				senders.Add(1)
				go func() {
					defer senders.Done()
					results <- conn.pacer.send(ctx, [][]byte{msg, msg}, func([]byte) error {
						writes <- struct{}{}
						return nil
					})
				}()
			}
			for range 2 {
				select {
				case <-writes:
				case <-time.After(time.Second):
					cancel()
					t.Fatal("initial burst did not send")
				}
			}
			select {
			case <-writes:
				t.Error("senders exceeded the shared burst")
			case <-time.After(15 * time.Millisecond):
			}
			cancel()
			senders.Wait()
			successes := 0
			for range 2 {
				if err := <-results; err == nil {
					successes++
				} else if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if successes != 1 {
				t.Fatalf("completed %d sends, want one", successes)
			}
		})
	}
}

func TestUDPPacingReturnsWriteErrorWithoutSendingRest(t *testing.T) {
	ctx := context.Background()
	var conn UDPConn
	if err := conn.ConfigurePacing(ctx, 10, 1000, 2); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("write failed")
	calls := 0
	err := conn.pacer.send(ctx, [][]byte{[]byte("first"), []byte("second")}, func([]byte) error {
		calls++
		return failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("send made %d writes and returned %v", calls, err)
	}
}
