package kafka

import (
	"context"
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
