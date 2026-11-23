package kafka

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

type dispatchTestProducer struct {
	records chan *kgo.Record
}

func (*dispatchTestProducer) Ping(context.Context) error { return nil }
func (p *dispatchTestProducer) Produce(_ context.Context, record *kgo.Record, promise func(*kgo.Record, error)) {
	p.records <- record
	if promise != nil {
		promise(record, nil)
	}
}
func (*dispatchTestProducer) Flush(context.Context) error { return nil }
func (*dispatchTestProducer) Close()                      {}

func testDispatcher(t *testing.T, size int) *dispatchTestProducer {
	t.Helper()
	p := &dispatchTestProducer{records: make(chan *kgo.Record)}
	SetProducer(p, size)
	t.Cleanup(func() {
		StopBackgroundThread()
		CloseProducer()
	})
	return p
}

func TestFullKafkaBatchesDrainWithoutTickerPacing(t *testing.T) {
	p := testDispatcher(t, 4)
	done := make(chan struct{})
	go func() {
		for range 1024 {
			WriteToKafka("key", "topic", 0, []byte("payload"))
		}
		FlushCache()
		close(done)
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range 1024 {
		select {
		case <-p.records:
		case <-timer.C:
			t.Fatal("full batches remained limited to one batch per 10 ms")
		}
	}
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("flush did not wait for dispatch completion")
	}
}

func TestKafkaDispatcherBackpressureAndPayloadOwnership(t *testing.T) {
	p := testDispatcher(t, 2)
	payload := []byte("original")
	WriteToKafka("0", "topic", 3, payload)
	copy(payload, "modified")
	WriteToKafka("1", "topic", 3, []byte("second"))
	WriteToKafka("2", "topic", 3, []byte("third"))
	WriteToKafka("3", "topic", 3, []byte("fourth"))
	written := make(chan struct{})
	go func() {
		WriteToKafka("4", "topic", 3, []byte("fifth"))
		close(written)
	}()
	select {
	case <-written:
		t.Fatal("full pending cache did not backpressure its writer")
	case <-time.After(20 * time.Millisecond):
	}
	cache.mu.Lock()
	pending, sending := len(cache.entries), cache.sending
	cache.mu.Unlock()
	if pending != 2 || sending != 2 {
		t.Fatalf("unbounded cache: pending=%d sending=%d", pending, sending)
	}
	flushed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-written
		FlushCache()
		close(flushed)
	}()
	for index := range 5 {
		select {
		case record := <-p.records:
			if string(record.Key) != string(rune('0'+index)) || record.Partition != 3 {
				t.Fatalf("changed order or routing: key=%s partition=%d", record.Key, record.Partition)
			}
			if index == 0 && !bytes.Equal(record.Value, []byte("original")) {
				t.Fatalf("queued payload aliased caller memory: %s", record.Value)
			}
		case <-time.After(time.Second):
			t.Fatal("producer recovery did not release backpressured writers")
		}
	}
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("final partial batch did not flush")
	}
	wg.Wait()
}
