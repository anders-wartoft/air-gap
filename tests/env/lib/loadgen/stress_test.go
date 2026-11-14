package main

import (
	"context"
	"testing"
	"time"
)

func TestStressPayloadValidation(t *testing.T) {
	for _, seq := range []uint64{0, 1200, 2402} {
		payload := stressPayload(seq, 123456789, 400, 1600)
		gotSeq, gotSent, err := validateStressPayload(payload, 400, 1600)
		if err != nil || gotSeq != seq || gotSent != 123456789 {
			t.Fatalf("round trip sequence %d: %d/%d, %v", seq, gotSeq, gotSent, err)
		}
		payload[len(payload)-1] ^= 1
		if _, _, err := validateStressPayload(payload, 400, 1600); err == nil {
			t.Fatal("corrupted payload accepted")
		}
		if _, _, err := validateStressPayload(payload[:23], 400, 1600); err == nil {
			t.Fatal("truncated header accepted")
		}
	}
}

func TestStressWindowBoundsAndDuplicateDetection(t *testing.T) {
	w := newStressWindow(2)
	ctx := context.Background()
	if !w.reserve(ctx, 1, 11) || !w.reserve(ctx, 2, 22) {
		t.Fatal("initial reservations failed")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if w.reserve(canceled, 3, 33) {
		t.Fatal("full window accepted another record")
	}
	if err := w.complete(2, 23); err == nil || w.size() != 2 {
		t.Fatal("corrupt timestamp released a credit")
	}
	if err := w.complete(2, 22); err != nil {
		t.Fatal(err)
	}
	if err := w.complete(2, 22); err == nil {
		t.Fatal("duplicate delivery accepted")
	}
	if !w.reserve(ctx, 3, 33) {
		t.Fatal("completed delivery did not release a credit")
	}
	w.abandon(3)
	if err := w.complete(1, 11); err != nil || w.size() != 0 {
		t.Fatalf("remaining delivery: %v, pending=%d", err, w.size())
	}
}

func TestStressWindowCancellationUnblocksProducer(t *testing.T) {
	w := newStressWindow(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.reserve(ctx, 1, 11)
	result := make(chan bool, 1)
	go func() { result <- w.reserve(ctx, 2, 22) }()
	cancel()
	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("canceled producer added a record")
		}

	case <-time.After(time.Second):
		t.Fatal("producer remained blocked on a full window")
	}
}

func TestStressWindowTracksLateAndMissingRecords(t *testing.T) {
	w := newStressWindow(1)
	ctx := context.Background()
	sent := time.Now().Add(-6 * time.Second).UnixNano()
	w.reserve(ctx, 1, sent)
	if err := w.expire(time.Now()); err != nil {
		t.Fatal(err)
	}
	if w.size() != 1 || len(w.missingSample()) != 1 {
		t.Fatal("expiration concealed a missing record")
	}
	if !w.reserve(ctx, 2, time.Now().UnixNano()) {
		t.Fatal("loss stopped load generation")
	}
	if err := w.complete(1, sent); err != nil || w.size() != 1 {
		t.Fatalf("late delivery: %v, pending=%d", err, w.size())
	}
	if err := w.complete(1, sent); err == nil {
		t.Fatal("late duplicate accepted")
	}
}
