package kafka

import (
	"context"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestDelayedReaderRetainsAndResumesInOrder(t *testing.T) {
	messages := make(chan *sarama.ConsumerMessage, 2)
	first := &sarama.ConsumerMessage{Offset: 11, Value: []byte("first")}
	second := &sarama.ConsumerMessage{Offset: 12, Value: []byte("second")}
	messages <- first
	messages <- second
	group := delayedTestGroup{paused: make(chan struct{}, 2), resumed: make(chan struct{}, 1)}
	fetcher := failingDelayedFetcher{calls: make(chan int64, 1)}
	reader := delayedClaimReader{
		consumer: &Consumer{group: group, delayFetcher: fetcher, maxProcessingTime: time.Millisecond},
		claim:    delayedTestClaim{messages: messages},
	}
	defer reader.close()
	current := &sarama.ConsumerMessage{Offset: 10, Timestamp: time.Now().Add(100 * time.Millisecond)}
	if err := reader.wait(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if reader.pendingSize != len(first.Value)+len(second.Value) {
		t.Fatalf("retained bytes = %d", reader.pendingSize)
	}
	if err := reader.delivered(context.Background(), current.Offset); err != nil {
		t.Fatal(err)
	}
	select {
	case <-group.resumed:
		t.Fatal("resumed fetching while buffered records remained")
	default:
	}
	for _, expected := range []*sarama.ConsumerMessage{first, second} {
		if got := reader.takeBuffered(); got != expected {
			t.Fatalf("buffered message = %v, want %v", got, expected)
		}
		if err := reader.delivered(context.Background(), expected.Offset); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-group.resumed:
	default:
		t.Fatal("fetching did not resume after draining retained records")
	}
	if reader.pendingSize != 0 || reader.takeBuffered() != nil {
		t.Fatal("drained records are still retained")
	}
	current = &sarama.ConsumerMessage{Offset: 13, Timestamp: time.Now().Add(25 * time.Millisecond)}
	if err := reader.wait(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := reader.delivered(context.Background(), current.Offset); err != nil {
		t.Fatal(err)
	}
	if len(group.paused) != 2 || len(group.resumed) != 1 {
		t.Fatal("second wait did not pause and resume the same reader")
	}
	select {
	case <-fetcher.calls:
		t.Fatal("retained reader was reopened")
	default:
	}
}

func TestDelayedReaderOverflowRereadsAfterCurrentOffset(t *testing.T) {
	for _, limit := range []string{"bytes", "records", "headers"} {
		t.Run(limit, func(t *testing.T) {
			messages := make(chan *sarama.ConsumerMessage, delayedBufferMessages+1)
			switch limit {
			case "records":
				for i := 0; i <= delayedBufferMessages; i++ {
					messages <- &sarama.ConsumerMessage{Offset: int64(i + 11)}
				}
			case "bytes":
				messages <- &sarama.ConsumerMessage{Value: make([]byte, delayedBufferBytes+1)}
			case "headers":
				messages <- &sarama.ConsumerMessage{Headers: []*sarama.RecordHeader{
					{Value: make([]byte, delayedBufferBytes+1)},
				}}
			}
			partition := testDelayedPartition{
				messages: make(chan *sarama.ConsumerMessage), closed: make(chan struct{}),
			}
			fetcher := testDelayedFetcher{partition: partition, calls: make(chan int64, 1)}
			group := delayedTestGroup{paused: make(chan struct{}, 1), resumed: make(chan struct{}, 1)}
			reader := delayedClaimReader{
				consumer: &Consumer{group: group, delayFetcher: fetcher, maxProcessingTime: time.Millisecond},
				claim:    delayedTestClaim{messages: messages},
			}
			defer reader.close()
			current := &sarama.ConsumerMessage{Offset: 10, Timestamp: time.Now().Add(100 * time.Millisecond)}
			if err := reader.wait(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if reader.stopDrain == nil || reader.pendingSize != 0 || len(reader.pending) != 0 {
				t.Fatal("overflow did not discard the uncommitted queue for rereading")
			}
			select {
			case <-fetcher.calls:
				t.Fatal("reread started before current record was delivered")
			default:
			}
			if err := reader.delivered(context.Background(), current.Offset); err != nil {
				t.Fatal(err)
			}
			select {
			case offset := <-fetcher.calls:
				if offset != 11 {
					t.Fatalf("reread offset = %d, want 11", offset)
				}
			default:
				t.Fatal("overflow did not reopen the partition")
			}
		})
	}
}
