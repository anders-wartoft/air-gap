package downstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"sitia.nu/airgap/src/kafka"
)

type statusTestProducer struct {
	results chan error
}

func (*statusTestProducer) Ping(context.Context) error { return nil }
func (p *statusTestProducer) Produce(_ context.Context, record *kgo.Record, promise func(*kgo.Record, error)) {
	promise(record, <-p.results)
}
func (*statusTestProducer) Flush(context.Context) error { return nil }
func (*statusTestProducer) Close()                      {}

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
	p := &statusTestProducer{results: make(chan error, 1)}
	kafka.SetProducer(p, 1, UpdateKafkaStatus)
	t.Cleanup(kafka.CloseProducer)
	UpdateKafkaStatus("running")
	for range 3 {
		p.results <- kerr.BrokerNotAvailable
		kafka.WriteToKafka("key", "transfer", 0, []byte("value"))
		waitForKafkaStatus(t, "Kafka producer error: "+kerr.BrokerNotAvailable.Error())
		p.results <- nil
		kafka.WriteToKafka("key", "transfer", 0, []byte("value"))
		waitForKafkaStatus(t, "running")
	}
}

func TestKafkaProducerStartupStatusRecovers(t *testing.T) {
	UpdateKafkaStatus("running")
	attempts := 0
	expected := &startupTestProducer{}
	p, err := newKafkaProducerWithRetry(context.Background(), "test", time.Millisecond,
		func() (kafka.Producer, error) {
			attempts++
			if attempts == 1 {
				return nil, kerr.BrokerNotAvailable
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
