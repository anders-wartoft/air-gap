package kafka

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

func testDelayedBurst(t *testing.T, brokers string) {
	t.Helper()
	const topic, groupID, partitions, perPartition = "delay-burst-test", "delay-burst", 30, 1000
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Consumer.MaxProcessingTime = 200 * time.Millisecond
	config.ChannelBufferSize = 1
	config.Producer.Return.Successes = true
	config.Producer.Partitioner = sarama.NewManualPartitioner
	config.Producer.Flush.Messages = 500
	config.Producer.Flush.Frequency = 10 * time.Millisecond

	var logBuffer synchronizedLog
	oldOutput := logging.StdLogger.Writer()
	logging.StdLogger.SetOutput(&logBuffer)
	defer logging.StdLogger.SetOutput(oldOutput)
	defer func() {
		if t.Failed() {
			t.Logf("Sarama log:\n%s", logBuffer.String())
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
	if err := client.RefreshMetadata(topic); err != nil {
		t.Fatal(err)
	}

	producer, err := sarama.NewAsyncProducerFromClient(client)
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(25 * time.Second)
	go func() {
		for i := 0; i < perPartition; i++ {
			for partition := int32(0); partition < partitions; partition++ {
				producer.Input() <- &sarama.ProducerMessage{
					Topic: topic, Partition: partition, Timestamp: due.Add(-24 * time.Hour),
					Value: sarama.StringEncoder("burst"),
				}
			}
		}
		producer.AsyncClose()
	}()
	successes, failures, produced := producer.Successes(), producer.Errors(), 0
	for successes != nil || failures != nil {
		select {
		case _, ok := <-successes:
			if ok {
				produced++
			} else {
				successes = nil
			}
		case failure, ok := <-failures:
			if ok {
				t.Fatalf("produce burst: %v", failure.Err)
			}
			failures = nil
		}
	}
	if produced != partitions*perPartition || time.Until(due) < 5*time.Second {
		t.Fatalf("burst produced=%d, time until due=%s", produced, time.Until(due))
	}

	group, err := sarama.NewConsumerGroup(strings.Split(brokers, ","), groupID, config)
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := sarama.NewConsumer(strings.Split(brokers, ","), config)
	if err != nil {
		group.Close()
		t.Fatal(err)
	}
	paused := make(chan int32, 2*partitions)
	observed := &observedConsumerGroup{ConsumerGroup: group, paused: paused}
	var mu sync.Mutex
	counts := make([]int, partitions)
	total := 0
	var firstDelivery time.Time
	complete := make(chan struct{})
	handler := &Consumer{
		name: groupID, ready: make(chan bool), delay: -24 * time.Hour,
		group: observed, delayFetcher: fetcher, maxProcessingTime: config.Consumer.MaxProcessingTime,
		callback: func(id string, _ []byte, timestamp time.Time, _ []byte) bool {
			mu.Lock()
			defer mu.Unlock()
			if firstDelivery.IsZero() {
				firstDelivery = time.Now()
			}
			if remaining := time.Until(timestamp.Add(24 * time.Hour)); remaining > 0 {
				t.Errorf("delivered %s %s early", id, remaining)
			}
			var partition, offset int
			if _, err := fmt.Sscanf(id, topic+"_%d_%d", &partition, &offset); err != nil || partition < 0 || partition >= partitions {
				t.Errorf("invalid ID %s: %v", id, err)
				return false
			}
			if offset != counts[partition] {
				t.Errorf("partition %d: offset %d, want %d", partition, offset, counts[partition])
			}
			counts[partition]++
			total++
			if total == partitions*perPartition {
				close(complete)
			}
			return true
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var consumeErr error
	go func() {
		defer close(done)
		consumeErr = group.Consume(ctx, []string{topic}, handler)
		if closeErr := group.Close(); consumeErr == nil {
			consumeErr = closeErr
		}
		if closeErr := fetcher.Close(); consumeErr == nil {
			consumeErr = closeErr
		}
	}()
	defer func() { cancel(); <-done }()
	parked := make(map[int32]bool)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for len(parked) < partitions {
		select {
		case partition := <-paused:
			parked[partition] = true
		case <-done:
			t.Fatalf("burst consumer stopped before pausing all partitions: %v", consumeErr)
		case <-timer.C:
			t.Fatalf("only %d partitions were paused", len(parked))
		}
	}
	if !time.Now().Before(due) {
		t.Fatal("burst became eligible before all partitions were paused")
	}
	select {
	case <-complete:
	case <-done:
		t.Fatalf("burst consumer stopped before delivery completed: %v", consumeErr)
	case <-time.After(60 * time.Second):
		mu.Lock()
		delivered := total
		perPartitionCounts := append([]int(nil), counts...)
		mu.Unlock()
		t.Fatalf("burst was not delivered: total=%d partitions=%v", delivered, perPartitionCounts)
	}
	elapsed := time.Since(firstDelivery)
	cancel()
	select {
	case <-done:
		if consumeErr != nil {
			t.Fatal(consumeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("burst consumer did not stop promptly")
	}
	mu.Lock()
	t.Logf("delivered %d records across %d partitions in %s after the first delivery", total, partitions, elapsed)
	for partition, count := range counts {
		if count != perPartition {
			t.Errorf("partition %d delivered %d records, want %d", partition, count, perPartition)
		}
	}
	mu.Unlock()
	partitionIDs := make([]int32, partitions)
	for partition := range partitionIDs {
		partitionIDs[partition] = int32(partition)
	}
	coordinator, err := client.Coordinator(groupID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := coordinator.FetchOffset(sarama.NewOffsetFetchRequest(
		config.Version, groupID, map[string][]int32{topic: partitionIDs}))
	if err != nil {
		t.Fatal(err)
	}
	for _, partition := range partitionIDs {
		block := response.GetBlock(topic, partition)
		if block == nil || block.Err != sarama.ErrNoError || block.Offset != perPartition {
			t.Errorf("partition %d: unexpected committed offset: %+v", partition, block)
		}
	}
	if strings.Contains(logBuffer.String(), "abandoned subscription") {
		t.Errorf("burst abandoned a subscription; log:\n%s", logBuffer.String())
	}
}
