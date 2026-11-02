package downstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

type statusTestProducer struct {
	sarama.AsyncProducer
	errors    chan *sarama.ProducerError
	successes chan *sarama.ProducerMessage
}

func (p *statusTestProducer) Errors() <-chan *sarama.ProducerError { return p.errors }
func (p *statusTestProducer) Successes() <-chan *sarama.ProducerMessage {
	return p.successes
}

func waitForKafkaStatus(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		kafkaStatusMu.Lock()
		got := kafkaStatus
		kafkaStatusMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Kafka status = %q, want %q", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKafkaProducerStatusRecoversAfterDelivery(t *testing.T) {
	p := &statusTestProducer{
		errors:    make(chan *sarama.ProducerError),
		successes: make(chan *sarama.ProducerMessage),
	}
	defer close(p.errors)
	defer close(p.successes)
	UpdateKafkaStatus("running")
	monitorKafkaProducer(p)
	for range 3 {
		p.errors <- &sarama.ProducerError{Err: sarama.ErrOutOfBrokers}
		waitForKafkaStatus(t, "Kafka producer error: "+sarama.ErrOutOfBrokers.Error())
		p.successes <- &sarama.ProducerMessage{
			Topic: "transfer", Key: sarama.StringEncoder("key"), Value: sarama.StringEncoder("value"),
		}
		waitForKafkaStatus(t, "running")
	}
}

func TestKafkaProducerStartupStatusRecovers(t *testing.T) {
	UpdateKafkaStatus("running")
	attempts := 0
	expected := &startupTestProducer{}
	p, err := newKafkaProducerWithRetry(context.Background(), "test", time.Millisecond,
		func() (sarama.AsyncProducer, error) {
			attempts++
			if attempts == 1 {
				return nil, sarama.ErrOutOfBrokers
			}
			kafkaStatusMu.Lock()
			got := kafkaStatus
			kafkaStatusMu.Unlock()
			if !strings.HasPrefix(got, "Kafka producer unavailable:") {
				t.Errorf("retry status = %q", got)
			}
			return expected, nil
		})
	if err != nil || p != expected {
		t.Fatalf("producer = %v, error = %v", p, err)
	}
	waitForKafkaStatus(t, "running")
}
