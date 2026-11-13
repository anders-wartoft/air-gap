package kafka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

type statusTestClaim struct {
	sarama.ConsumerGroupClaim
	messages chan *sarama.ConsumerMessage
}

func (c statusTestClaim) Messages() <-chan *sarama.ConsumerMessage { return c.messages }

type statusTestSession struct{ sarama.ConsumerGroupSession }

func (statusTestSession) Context() context.Context { return context.Background() }

type canceledDelaySession struct {
	sarama.ConsumerGroupSession
	ctx    context.Context
	marked chan struct{}
}

func (s canceledDelaySession) Context() context.Context { return s.ctx }
func (s canceledDelaySession) MarkMessage(*sarama.ConsumerMessage, string) {
	s.marked <- struct{}{}
}

type delayedTestClaim struct {
	sarama.ConsumerGroupClaim
	messages chan *sarama.ConsumerMessage
}

func (c delayedTestClaim) Topic() string                            { return "topic" }
func (c delayedTestClaim) Partition() int32                         { return 0 }
func (c delayedTestClaim) Messages() <-chan *sarama.ConsumerMessage { return c.messages }

type delayedTestGroup struct {
	sarama.ConsumerGroup
	paused chan struct{}
}

func (g delayedTestGroup) Pause(map[string][]int32) { g.paused <- struct{}{} }

type failingDelayedFetcher struct {
	sarama.Consumer
	calls chan int64
}

func (f failingDelayedFetcher) ConsumePartition(_ string, _ int32, offset int64) (sarama.PartitionConsumer, error) {
	f.calls <- offset
	return nil, errors.New("fetch unavailable")
}

type testDelayedPartition struct {
	sarama.PartitionConsumer
	messages chan *sarama.ConsumerMessage
	closed   chan struct{}
}

func (p testDelayedPartition) Messages() <-chan *sarama.ConsumerMessage { return p.messages }
func (p testDelayedPartition) Close() error {
	close(p.closed)
	return nil
}

type testDelayedFetcher struct {
	sarama.Consumer
	partition testDelayedPartition
	calls     chan int64
}

func (f testDelayedFetcher) ConsumePartition(_ string, _ int32, offset int64) (sarama.PartitionConsumer, error) {
	f.calls <- offset
	return f.partition, nil
}

func TestDelayedFetchCancelsAfterReopeningPartition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages := make(chan *sarama.ConsumerMessage, 1)
	messages <- &sarama.ConsumerMessage{Topic: "topic", Timestamp: time.Now().Add(-24*time.Hour + 200*time.Millisecond)}
	partition := testDelayedPartition{messages: make(chan *sarama.ConsumerMessage, 1), closed: make(chan struct{})}
	partition.messages <- &sarama.ConsumerMessage{Topic: "topic", Offset: 1, Timestamp: time.Now()}
	fetcher := testDelayedFetcher{partition: partition, calls: make(chan int64, 1)}
	session := canceledDelaySession{ctx: ctx, marked: make(chan struct{}, 2)}
	delivered := make(chan struct{}, 2)
	consumer := Consumer{
		delay: -24 * time.Hour, group: delayedTestGroup{paused: make(chan struct{}, 1)},
		delayFetcher: fetcher, maxProcessingTime: 10 * time.Millisecond,
		callback: func(string, []byte, time.Time, []byte) bool {
			delivered <- struct{}{}
			return true
		},
	}
	done := make(chan error, 1)
	go func() { done <- consumer.ConsumeClaim(session, delayedTestClaim{messages: messages}) }()
	select {
	case <-partition.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed partition was not closed before its next long wait")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
	if len(delivered) != 1 || len(session.marked) != 1 {
		t.Fatalf("delivered=%d marked=%d, want one of each", len(delivered), len(session.marked))
	}
	select {
	case offset := <-fetcher.calls:
		if offset != 1 {
			t.Errorf("fetch offset = %d, want 1", offset)
		}
	default:
		t.Fatal("fetcher was not started")
	}
}

func TestDelayedFetchDoesNotMarkRejectedMessage(t *testing.T) {
	messages := make(chan *sarama.ConsumerMessage, 1)
	messages <- &sarama.ConsumerMessage{Topic: "topic", Offset: 4, Timestamp: time.Now().Add(-time.Second + 200*time.Millisecond)}
	session := canceledDelaySession{ctx: context.Background(), marked: make(chan struct{}, 1)}
	group := delayedTestGroup{paused: make(chan struct{}, 1)}
	fetcher := failingDelayedFetcher{calls: make(chan int64, 1)}
	consumer := Consumer{
		delay: -time.Second, group: group, delayFetcher: fetcher, maxProcessingTime: 10 * time.Millisecond,
		callback: func(string, []byte, time.Time, []byte) bool { return false },
	}
	if err := consumer.ConsumeClaim(session, delayedTestClaim{messages: messages}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-group.paused:
	default:
		t.Fatal("delayed partition was not paused")
	}
	select {
	case <-session.marked:
		t.Fatal("rejected message was marked")
	case <-fetcher.calls:
		t.Fatal("fetcher started after a rejected message")
	default:
	}
}

func TestDelayedFetchFailureReportsStatus(t *testing.T) {
	messages := make(chan *sarama.ConsumerMessage, 1)
	messages <- &sarama.ConsumerMessage{Topic: "topic", Offset: 4, Timestamp: time.Now().Add(-time.Second + 200*time.Millisecond)}
	session := canceledDelaySession{ctx: context.Background(), marked: make(chan struct{}, 1)}
	group := delayedTestGroup{paused: make(chan struct{}, 1)}
	fetcher := failingDelayedFetcher{calls: make(chan int64, 1)}
	status := make(chan string, 1)
	consumer := Consumer{
		delay: -time.Second, group: group, delayFetcher: fetcher, maxProcessingTime: 10 * time.Millisecond,
		callback:  func(string, []byte, time.Time, []byte) bool { return true },
		onFailure: func(message string) { status <- message },
	}
	err := consumer.ConsumeClaim(session, delayedTestClaim{messages: messages})
	if err == nil || !strings.Contains(err.Error(), "fetch unavailable") {
		t.Fatalf("fetch error = %v", err)
	}
	select {
	case offset := <-fetcher.calls:
		if offset != 5 {
			t.Errorf("fetch offset = %d, want 5", offset)
		}
	default:
		t.Fatal("fetcher was not started")
	}
	select {
	case <-session.marked:
	default:
		t.Fatal("successfully delivered message was not marked")
	}
	select {
	case message := <-status:
		if !strings.Contains(message, "fetch unavailable") {
			t.Errorf("status = %q", message)
		}
	default:
		t.Fatal("fetch failure was not reported")
	}
}

func TestDelayedClaimStopsOnSessionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages := make(chan *sarama.ConsumerMessage, 1)
	messages <- &sarama.ConsumerMessage{Topic: "topic", Timestamp: time.Now()}
	delivered := make(chan struct{}, 1)
	session := canceledDelaySession{ctx: ctx, marked: make(chan struct{}, 1)}
	consumer := Consumer{
		delay: -24 * time.Hour,
		callback: func(string, []byte, time.Time, []byte) bool {
			delivered <- struct{}{}
			return true
		},
	}
	done := make(chan error, 1)
	go func() { done <- consumer.ConsumeClaim(session, statusTestClaim{messages: messages}) }()
	// Wait until the record has been taken from the claim before canceling.
	deadline := time.After(time.Second)
	for len(messages) != 0 {
		select {
		case <-deadline:
			t.Fatal("consumer did not read the delayed record")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delayed claim did not stop after session cancellation")
	}
	select {
	case <-delivered:
		t.Fatal("canceled delayed record was delivered")
	case <-session.marked:
		t.Fatal("canceled delayed record was marked")
	default:
	}
}

func TestConsumerStatusRecoversOnSessionAndMessage(t *testing.T) {
	status := "unavailable at startup"
	c := Consumer{
		ready:     make(chan bool),
		onHealthy: func() { status = "running" },
		callback: func(string, []byte, time.Time, []byte) bool {
			if status != "running" {
				t.Errorf("status at delivery = %q", status)
			}
			return false
		},
	}
	if err := c.Setup(nil); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("status after setup = %q", status)
	}
	status = "consumer error during session"
	messages := make(chan *sarama.ConsumerMessage, 1)
	messages <- &sarama.ConsumerMessage{Topic: "topic"}
	if err := c.ConsumeClaim(statusTestSession{}, statusTestClaim{messages: messages}); err != nil {
		t.Fatal(err)
	}
}
