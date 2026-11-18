package kafka

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

var errPauseBufferFull = errors.New("experimental pause buffer is full")

type pauseDelivery struct {
	message *sarama.ConsumerMessage
	at      time.Time
}

type observedPartitionFetcher struct {
	sarama.Consumer
	opens atomic.Int64
}

func (f *observedPartitionFetcher) ConsumePartition(topic string, partition int32, offset int64) (sarama.PartitionConsumer, error) {
	reader, err := f.Consumer.ConsumePartition(topic, partition, offset)
	if err == nil {
		f.opens.Add(1)
	}
	return reader, err
}

// Test-only alternative to closing delayed readers. The byte limit counts
// retained keys and values, not Sarama fetch buffers or Go object overhead.
type pauseExperiment struct {
	group      sarama.ConsumerGroup
	delay      time.Duration
	drain      bool
	limit      int
	mu         sync.Mutex
	peakBytes  int
	pauses     int
	deliveries chan pauseDelivery
	failures   chan error
}

func (*pauseExperiment) Setup(sarama.ConsumerGroupSession) error   { return nil }
func (*pauseExperiment) Cleanup(sarama.ConsumerGroupSession) error { return nil }

func (h *pauseExperiment) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	err := h.consume(session, claim)
	if err != nil {
		select {
		case h.failures <- err:
		case <-session.Context().Done():
		}
	}
	return err
}

func (h *pauseExperiment) consume(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	partitions := map[string][]int32{claim.Topic(): {claim.Partition()}}
	defer h.group.Resume(partitions)
	var pending []*sarama.ConsumerMessage
	retained := 0
	appendPending := func(message *sarama.ConsumerMessage) error {
		size := len(message.Key) + len(message.Value)
		if size > h.limit-retained {
			return errPauseBufferFull
		}
		pending = append(pending, message)
		retained += size
		h.mu.Lock()
		if retained > h.peakBytes {
			h.peakBytes = retained
		}
		h.mu.Unlock()
		return nil
	}
	for {
		if len(pending) == 0 {
			select {
			case <-session.Context().Done():
				return nil
			case message, ok := <-claim.Messages():
				if !ok {
					return nil
				}
				if err := appendPending(message); err != nil {
					return err
				}
			}
		}
		message := pending[0]
		if wait := time.Until(message.Timestamp.Add(h.delay)); wait > 0 {
			h.group.Pause(partitions)
			h.mu.Lock()
			h.pauses++
			h.mu.Unlock()
			var inFlight <-chan *sarama.ConsumerMessage
			if h.drain {
				inFlight = claim.Messages()
			}
			timer := time.NewTimer(wait)
		waiting:
			for {
				select {
				case <-session.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
					break waiting
				case next, ok := <-inFlight:
					if !ok {
						timer.Stop()
						return fmt.Errorf("partition channel closed while paused")
					}
					if err := appendPending(next); err != nil {
						timer.Stop()
						return err
					}
				}
			}
			h.group.Resume(partitions)
		}
		select {
		case <-session.Context().Done():
			return nil
		case h.deliveries <- pauseDelivery{message: message, at: time.Now()}:
			session.MarkMessage(message, "")
		}
		retained -= len(message.Key) + len(message.Value)
		pending[0] = nil
		pending = pending[1:]
	}
}

// Uses the same disposable broker as the delayed-subscription regressions.
// No production handler is changed by this experiment.
func TestPauseResumeWithKafka(t *testing.T) {
	testPauseResumeWithKafka(t, false)
}

func TestBoundedDelayedReaderWithKafka(t *testing.T) {
	testPauseResumeWithKafka(t, true)
}

func testPauseResumeWithKafka(t *testing.T, production bool) {
	t.Helper()
	brokers := os.Getenv("AIRGAP_DELAY_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("AIRGAP_DELAY_KAFKA_BROKERS is not set")
	}
	ConfigureSaramaLogger()
	for _, scenario := range []struct {
		name     string
		drain    bool
		limit    int
		overflow bool
		cancel   bool
		spacing  time.Duration
	}{
		{name: "pause-only", limit: 64 * 1024},
		{name: "pause-and-drain", drain: true, limit: 64 * 1024},
		{name: "paced-delivery", drain: true, limit: 64 * 1024, spacing: 750 * time.Millisecond},
		{name: "bounded-overflow", drain: true, limit: 1200, overflow: true},
		{name: "cancel-while-paused", drain: true, limit: 64 * 1024, cancel: true},
	} {
		if production && !scenario.drain {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			const partitions = 30
			payloadBytes := 600
			perPartition := 64
			if production && scenario.overflow {
				payloadBytes = 2048
			}
			if scenario.spacing > 0 {
				perPartition = 8
			}
			topic := fmt.Sprintf("pause-%s-%d", scenario.name, time.Now().UnixNano())
			config := sarama.NewConfig()
			config.Version = sarama.V3_6_0_0
			config.Consumer.Offsets.Initial = sarama.OffsetOldest
			config.Consumer.MaxProcessingTime = 100 * time.Millisecond
			config.ChannelBufferSize = 1
			if scenario.spacing > 0 {
				config.Consumer.MaxProcessingTime = 30 * time.Second
				config.ChannelBufferSize = 4
			}
			config.Producer.Return.Successes = true
			config.Producer.Partitioner = sarama.NewManualPartitioner

			var logs synchronizedLog
			oldOutput := logging.StdLogger.Writer()
			logging.StdLogger.SetOutput(&logs)
			defer logging.StdLogger.SetOutput(oldOutput)
			defer func() {
				if t.Failed() {
					t.Logf("Sarama log:\n%s", logs.String())
				}
			}()
			client, err := sarama.NewClient(strings.Split(brokers, ","), config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			admin, err := sarama.NewClusterAdminFromClient(client)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			if err := admin.CreateTopic(topic, &sarama.TopicDetail{
				NumPartitions: partitions, ReplicationFactor: 1,
			}, false); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := admin.DeleteTopic(topic); err != nil {
					t.Errorf("delete experimental topic: %v", err)
				}
			}()
			producer, err := sarama.NewSyncProducerFromClient(client)
			if err != nil {
				t.Fatal(err)
			}
			due := time.Now().Add(10 * time.Second).Truncate(time.Millisecond)
			if scenario.spacing > 0 {
				due = time.Now().Add(120 * time.Second).Truncate(time.Millisecond)
			}
			batch := make([]*sarama.ProducerMessage, 0, partitions*perPartition)
			for offset := 0; offset < perPartition; offset++ {
				for partition := int32(0); partition < partitions; partition++ {
					timestamp := due.Add(-120 * time.Second).Add(time.Duration(offset) * scenario.spacing)
					if production && scenario.overflow && offset > 0 {
						timestamp = timestamp.Add(2 * time.Second)
					}
					batch = append(batch, &sarama.ProducerMessage{
						Topic: topic, Partition: partition,
						Timestamp: timestamp,
						Value:     sarama.StringEncoder(strings.Repeat("x", payloadBytes)),
					})
				}
			}
			produceErr := producer.SendMessages(batch)
			closeErr := producer.Close()
			if produceErr != nil || closeErr != nil {
				t.Fatalf("produce: %v; close: %v", produceErr, closeErr)
			}
			group, err := sarama.NewConsumerGroup(strings.Split(brokers, ","), topic, config)
			if err != nil {
				t.Fatal(err)
			}
			h := &pauseExperiment{
				group: group, delay: 120 * time.Second,
				drain: scenario.drain, limit: scenario.limit,
				deliveries: make(chan pauseDelivery, partitions*perPartition),
				failures:   make(chan error, partitions),
			}
			var handler sarama.ConsumerGroupHandler = h
			var fetcher sarama.Consumer
			var observedFetcher *observedPartitionFetcher
			observed := &observedConsumerGroup{
				ConsumerGroup: group, paused: make(chan int32, partitions*perPartition*2),
			}
			if production {
				fetcher, err = sarama.NewConsumer(strings.Split(brokers, ","), config)
				if err != nil {
					group.Close()
					t.Fatal(err)
				}
				observedFetcher = &observedPartitionFetcher{Consumer: fetcher}
				handler = &Consumer{
					name: topic, ready: make(chan bool), delay: -120 * time.Second,
					group: observed, delayFetcher: observedFetcher, maxProcessingTime: config.Consumer.MaxProcessingTime,
					onFailure: func(message string) { h.failures <- errors.New(message) },
					callback: func(id string, key []byte, timestamp time.Time, value []byte) bool {
						var partition int32
						var offset int64
						if _, err := fmt.Sscanf(id, topic+"_%d_%d", &partition, &offset); err != nil {
							h.failures <- err
							return false
						}
						h.deliveries <- pauseDelivery{
							message: &sarama.ConsumerMessage{
								Topic: topic, Partition: partition, Offset: offset,
								Timestamp: timestamp, Key: key, Value: value,
							},
							at: time.Now(),
						}
						return true
					},
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- group.Consume(ctx, []string{topic}, handler) }()
			stopped := false
			stop := func() {
				t.Helper()
				if stopped {
					return
				}
				stopped = true
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("consume: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("consumer did not cancel within 5 seconds")
				}
				if err := group.Close(); err != nil {
					t.Errorf("close group: %v", err)
				}
				if fetcher != nil {
					if err := fetcher.Close(); err != nil {
						t.Errorf("close fetcher: %v", err)
					}
				}
			}
			defer stop()
			timer := time.NewTimer(time.Until(due) + 20*time.Second)
			defer timer.Stop()
			if scenario.overflow && !production {
				select {
				case err := <-h.failures:
					if !errors.Is(err, errPauseBufferFull) {
						t.Fatal(err)
					}
				case <-timer.C:
					t.Fatal("buffer limit was not exercised")
				}
			} else if scenario.cancel {
				// Wait for every partition to pause, then cancel before eligibility.
				for {
					h.mu.Lock()
					paused := h.pauses
					h.mu.Unlock()
					if production {
						paused = len(observed.paused)
					}
					if paused >= partitions {
						break
					}
					select {
					case err := <-h.failures:
						t.Fatal(err)
					case <-timer.C:
						t.Fatal("partitions did not pause")
					case <-time.After(10 * time.Millisecond):
					}
				}
				if time.Until(due) < time.Second {
					t.Fatal("not enough time remains to test cancellation during the wait")
				}
			} else {
				counts := make([]int, partitions)
				for delivered := 0; delivered < partitions*perPartition; delivered++ {
					select {
					case delivery := <-h.deliveries:
						message := delivery.message
						if delivery.at.Before(message.Timestamp.Add(h.delay)) {
							t.Fatal("delivered before the configured delay")
						}
						if message.Offset != int64(counts[message.Partition]) {
							t.Fatalf("partition %d: offset %d, want %d",
								message.Partition, message.Offset, counts[message.Partition])
						}
						counts[message.Partition]++
					case err := <-h.failures:
						t.Fatal(err)
					case <-timer.C:
						t.Fatalf("only delivered %d records", delivered)
					}
				}
			}
			closesBeforeShutdown := strings.Count(logs.String(), "closed dead subscription")
			stop()
			if ((scenario.overflow && !production) || scenario.cancel) && len(h.deliveries) != 0 {
				t.Error("delivered records while they were still ineligible")
			}
			h.mu.Lock()
			pauses, peak := h.pauses, h.peakBytes
			h.mu.Unlock()
			if production {
				pauses = len(observed.paused)
			}
			if pauses == 0 || peak > scenario.limit {
				t.Fatalf("pause/bound check failed: pauses=%d peak=%d limit=%d", pauses, peak, scenario.limit)
			}
			if scenario.spacing > 0 && !production && pauses <= partitions {
				t.Fatalf("paced delivery did not exercise repeated waits: %d pauses", pauses)
			}
			ids := make([]int32, partitions)
			for i := range ids {
				ids[i] = int32(i)
			}
			coordinator, err := client.Coordinator(topic)
			if err != nil {
				t.Fatal(err)
			}
			offsets, err := coordinator.FetchOffset(sarama.NewOffsetFetchRequest(
				config.Version, topic, map[string][]int32{topic: ids}))
			if err != nil {
				t.Fatal(err)
			}
			for _, partition := range ids {
				block := offsets.GetBlock(topic, partition)
				if block == nil || block.Err != sarama.ErrNoError {
					t.Fatalf("partition %d: invalid committed offset: %+v", partition, block)
				}
				if (scenario.overflow && !production) || scenario.cancel {
					if block.Offset > 0 {
						t.Errorf("partition %d committed undelivered records: %d", partition, block.Offset)
					}
				} else if block.Offset != int64(perPartition) {
					t.Errorf("partition %d committed %d, want %d", partition, block.Offset, perPartition)
				}
			}
			timeouts := strings.Count(logs.String(), "abandoned subscription")
			closes := strings.Count(logs.String(), "closed dead subscription")
			if !scenario.drain && timeouts == 0 {
				t.Error("pause-only did not reproduce the in-flight delivery timeout")
			}
			if scenario.drain && timeouts != 0 {
				t.Errorf("draining while paused produced %d subscription timeouts", timeouts)
			}
			if scenario.drain && !scenario.overflow && closesBeforeShutdown != 0 {
				t.Errorf("draining while paused closed %d subscriptions before shutdown", closesBeforeShutdown)
			}
			if production {
				refetches := observedFetcher.opens.Load()
				if scenario.overflow && refetches <= partitions {
					t.Errorf("group and dedicated-reader overflow not both exercised: %d refetches", refetches)
				} else if !scenario.overflow && refetches != 0 {
					t.Errorf("retained reader was reopened %d times", refetches)
				}
				t.Logf("production pauses=%d refetches=%d timeouts=%d closes before shutdown=%d including shutdown=%d overflow=%t cancel=%t",
					pauses, refetches, timeouts, closesBeforeShutdown, closes, scenario.overflow, scenario.cancel)
			} else {
				t.Logf("pauses=%d peak retained payload bytes/partition=%d timeouts=%d closes before shutdown=%d including shutdown=%d overflow=%t cancel=%t",
					pauses, peak, timeouts, closesBeforeShutdown, closes, scenario.overflow, scenario.cancel)
			}
		})
	}
}
