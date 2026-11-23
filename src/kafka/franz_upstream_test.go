package kafka

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestFranzBatchDoesNotMarkFailedDelivery(t *testing.T) {
	records := []*kgo.Record{
		{Topic: "export", Partition: 2, Offset: 5},
		{Topic: "export", Partition: 2, Offset: 6},
		{Topic: "export", Partition: 2, Offset: 7},
	}
	var sent []string
	var marked []*kgo.Record
	err := deliverFranzBatch(context.Background(), 0, records, nil,
		func(id string, _ []byte, _ time.Time, _ []byte) bool {
			sent = append(sent, id)
			return id != "export_2_6"
		}, func(records ...*kgo.Record) { marked = append(marked, records...) })
	if err == nil || len(sent) != 2 || len(marked) != 1 || marked[0] != records[0] {
		t.Fatalf("error=%v sent=%v marked=%v", err, sent, marked)
	}
}

func TestFranzBatchCancelsBeforeDelayedDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := deliverFranzBatch(ctx, time.Hour, []*kgo.Record{{Timestamp: time.Now()}}, nil,
		func(string, []byte, time.Time, []byte) bool {
			t.Error("record delivered before its deadline")
			return true
		}, func(...*kgo.Record) { t.Error("undelivered record marked") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}

func TestFranzUpstreamWithKafka(t *testing.T) {
	broker := os.Getenv("AIRGAP_FRANZ_KAFKA_BROKER")
	if broker == "" {
		t.Skip("AIRGAP_FRANZ_KAFKA_BROKER is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	cl, err := kgo.NewClient(kgo.SeedBrokers(broker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	topic := fmt.Sprintf("airgap-franz-%d", time.Now().UnixNano())
	req := kmsg.NewPtrCreateTopicsRequest()
	req.Topics = []kmsg.CreateTopicsRequestTopic{{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}}
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Topics) != 1 || resp.Topics[0].ErrorCode != 0 {
		t.Fatalf("create test topic: %+v", resp.Topics)
	}
	const records = 100
	batch := make([]*kgo.Record, 0, records)
	for i := 0; i < records; i++ {
		batch = append(batch, &kgo.Record{Topic: topic, Value: []byte("payload")})
	}
	if err := cl.ProduceSync(ctx, batch...).FirstErr(); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		thread string
		delay  int
	}{{"sec0", 0}, {"sec-3", -3}} {
		t.Run(spec.thread, func(t *testing.T) {
			t.Parallel()
			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			type delivery struct {
				id        string
				timestamp time.Time
				at        time.Time
			}
			got := make(chan delivery, records+1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				ReadUpstreamWithFranz(runCtx, spec.thread, spec.delay, broker, topic, topic+"-"+spec.thread, "",
					func(id string, _ []byte, timestamp time.Time, _ []byte) bool {
						got <- delivery{id, timestamp, time.Now()}
						return true
					}, ConsumerSettings{})
			}()
			defer func() {
				stop()
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					t.Error("consumer did not shut down")
				}
			}()
			for i := 0; i < records; i++ {
				select {
				case record := <-got:
					if record.id != fmt.Sprintf("%s_0_%d", topic, i) ||
						record.at.Before(record.timestamp.Add(-time.Duration(spec.delay)*time.Second)) {
						t.Fatalf("unexpected delivery %d: %+v", i, record)
					}
				case <-runCtx.Done():
					t.Fatalf("received %d/%d records: %v", i, records, runCtx.Err())
				}
			}
		})
	}
}
