package kafka

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

type dispatchTestProducer struct {
	sarama.AsyncProducer
	input   chan *sarama.ProducerMessage
	workers sync.WaitGroup
}

func (p *dispatchTestProducer) Input() chan<- *sarama.ProducerMessage {
	return p.input
}

func testDispatcher(t *testing.T, size int) *dispatchTestProducer {
	t.Helper()
	p := &dispatchTestProducer{input: make(chan *sarama.ProducerMessage)}
	SetProducer(p, size)
	t.Cleanup(func() {
		go func() {
			for range p.input {
			}
		}()
		StopBackgroundThread()
		p.workers.Wait()
		close(p.input)
	})
	return p
}

func TestFullKafkaBatchesDrainWithoutTickerPacing(t *testing.T) {
	p := testDispatcher(t, 4)
	done := make(chan struct{})
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
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
		case <-p.input:
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
	// The first batch is blocked on the producer; only one more batch fits.
	WriteToKafka("2", "topic", 3, []byte("third"))
	WriteToKafka("3", "topic", 3, []byte("fourth"))
	written := make(chan struct{})
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
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
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		<-written
		FlushCache()
		close(flushed)
	}()
	for index := range 5 {
		select {
		case msg := <-p.input:
			value, err := msg.Value.Encode()
			if err != nil {
				t.Fatal(err)
			}
			key, err := msg.Key.Encode()
			if err != nil || string(key) != string(rune('0'+index)) || msg.Partition != 3 {
				t.Fatalf("changed order or routing: key=%s partition=%d err=%v", key, msg.Partition, err)
			}
			if index == 0 && !bytes.Equal(value, []byte("original")) {
				t.Fatalf("queued payload aliased caller memory: %s", value)
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
}
