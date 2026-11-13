package kafka

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

type synchronizedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type observedConsumerGroup struct {
	sarama.ConsumerGroup
	paused chan int32
}

func (g *observedConsumerGroup) Pause(partitions map[string][]int32) {
	g.ConsumerGroup.Pause(partitions)
	for _, ids := range partitions {
		for _, id := range ids {
			g.paused <- id
		}
	}
}

type observedHandler struct {
	*Consumer
	member   string
	sessions chan string
}

func (h *observedHandler) Setup(session sarama.ConsumerGroupSession) error {
	h.sessions <- fmt.Sprintf("%s:setup:%d", h.member, len(session.Claims()["delay-rebalance-test"]))
	return h.Consumer.Setup(session)
}

func (h *observedHandler) Cleanup(sarama.ConsumerGroupSession) error {
	h.sessions <- h.member + ":cleanup"
	return nil
}

func (l *synchronizedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *synchronizedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func consumeTestGroup(ctx context.Context, group sarama.ConsumerGroup, topic string, handler *Consumer) error {
	for ctx.Err() == nil {
		if err := group.Consume(ctx, []string{topic}, handler); err != nil {
			return err
		}
		if ctx.Err() == nil {
			handler.ready = make(chan bool)
		}
	}
	return nil
}

type restartingTestGroup struct {
	sarama.ConsumerGroup
	cancel context.CancelFunc
	calls  int
}

func (g *restartingTestGroup) Consume(_ context.Context, _ []string, handler sarama.ConsumerGroupHandler) error {
	if err := handler.Setup(nil); err != nil {
		return err
	}
	g.calls++
	if g.calls == 2 {
		g.cancel()
	}
	return nil
}

func TestConsumeTestGroupRestartsEndedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	group := &restartingTestGroup{cancel: cancel}
	handler := &Consumer{ready: make(chan bool)}
	ready := handler.ready
	if err := consumeTestGroup(ctx, group, "topic", handler); err != nil {
		t.Fatal(err)
	}
	if group.calls != 2 {
		t.Fatalf("started %d sessions, want 2", group.calls)
	}
	select {
	case <-ready:
	default:
		t.Fatal("first session did not signal readiness")
	}
}

// Run with a disposable Kafka broker; TC-49 provides one.
func TestDelayedSubscriptionWithKafka(t *testing.T) {
	brokers := os.Getenv("AIRGAP_DELAY_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("AIRGAP_DELAY_KAFKA_BROKERS is not set")
	}
	ConfigureSaramaLogger()
	t.Run("24h", func(t *testing.T) { testDelayedSubscription(t, brokers, "delay-24h-test", 24*time.Hour) })
	t.Run("3m", func(t *testing.T) { testDelayedSubscription(t, brokers, "delay-3m-test", 3*time.Minute) })
	t.Run("rebalance", func(t *testing.T) { testDelayedSubscriptionRebalance(t, brokers) })
	t.Run("reader", func(t *testing.T) { testDelayedReader(t, brokers) })
	t.Run("burst", func(t *testing.T) { testDelayedBurst(t, brokers) })
}

func testDelayedSubscriptionRebalance(t *testing.T, brokers string) {
	t.Helper()
	const topic, groupID, partitions, perPartition = "delay-rebalance-test", "delay-rebalance", 3, 8
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Consumer.MaxProcessingTime = 200 * time.Millisecond
	config.ChannelBufferSize = 1
	config.Producer.Return.Successes = true
	config.Producer.Partitioner = sarama.NewManualPartitioner

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

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	sessions := make(chan string, 32)
	paused := make(chan int32, 32)
	deliveries := make(chan string, partitions*perPartition*2)
	errors := make(chan error, 8)
	startMember := func(name string) {
		group, err := sarama.NewConsumerGroup(strings.Split(brokers, ","), groupID, config)
		if err != nil {
			t.Fatal(err)
		}
		fetcher, err := sarama.NewConsumer(strings.Split(brokers, ","), config)
		if err != nil {
			t.Fatal(err)
		}
		observed := &observedConsumerGroup{ConsumerGroup: group, paused: paused}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer group.Close()
			defer fetcher.Close()
			for ctx.Err() == nil {
				handler := &observedHandler{
					member: name, sessions: sessions,
					Consumer: &Consumer{
						name: name, delay: -24 * time.Hour, ready: make(chan bool),
						group: observed, delayFetcher: fetcher, maxProcessingTime: config.Consumer.MaxProcessingTime,
						callback: func(id string, _ []byte, timestamp time.Time, _ []byte) bool {
							if time.Now().Before(timestamp.Add(24 * time.Hour)) {
								errors <- fmt.Errorf("%s delivered %s early", name, id)
							}
							deliveries <- id
							return true
						},
					},
				}
				if err := group.Consume(ctx, []string{topic}, handler); err != nil && ctx.Err() == nil {
					errors <- fmt.Errorf("%s consume: %w", name, err)
					return
				}
			}
		}()
	}
	startMember("first")
	select {
	case event := <-sessions:
		if event != "first:setup:3" {
			t.Fatalf("unexpected first session: %s", event)
		}
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(30 * time.Second):
		t.Fatal("first consumer group member did not start")
	}

	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(20 * time.Second)
	for partition := int32(0); partition < partitions; partition++ {
		for i := 0; i < perPartition; i++ {
			if _, _, err := producer.SendMessage(&sarama.ProducerMessage{
				Topic: topic, Partition: partition, Timestamp: due.Add(-24 * time.Hour),
				Value: sarama.StringEncoder("test"),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	parked := make(map[int32]bool)
	for len(parked) < partitions {
		select {
		case partition := <-paused:
			parked[partition] = true
		case err := <-errors:
			t.Fatal(err)
		case <-time.After(10 * time.Second):
			t.Fatal("first member did not pause all delayed partitions")
		}
	}
	startMember("second")
	firstReleased, secondAssigned := false, false
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for !firstReleased || !secondAssigned {
		select {
		case event := <-sessions:
			if event == "first:cleanup" {
				firstReleased = true
			}
			if strings.HasPrefix(event, "second:setup:") && event != "second:setup:0" {
				secondAssigned = true
			}
		case err := <-errors:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatalf("rebalance did not complete: first released=%v second assigned=%v", firstReleased, secondAssigned)
		}
	}
	if !time.Now().Before(due) {
		t.Fatal("rebalance did not complete before records became eligible")
	}
	seen := make(map[string]bool)
	for len(seen) < partitions*perPartition {
		select {
		case id := <-deliveries:
			if seen[id] {
				t.Fatalf("duplicate delivery after rebalance: %s", id)
			}
			seen[id] = true
		case err := <-errors:
			t.Fatal(err)
		case <-time.After(30 * time.Second):
			t.Fatalf("received %d records after rebalance", len(seen))
		}
	}
	cancel()
	wg.Wait()
	select {
	case id := <-deliveries:
		t.Errorf("additional delivery after rebalance: %s", id)
	default:
	}
	select {
	case err := <-errors:
		t.Error(err)
	default:
	}
	coordinator, err := client.Coordinator(groupID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := coordinator.FetchOffset(sarama.NewOffsetFetchRequest(
		config.Version, groupID, map[string][]int32{topic: {0, 1, 2}}))
	if err != nil {
		t.Fatal(err)
	}
	for partition := int32(0); partition < partitions; partition++ {
		block := response.GetBlock(topic, partition)
		if block == nil || block.Err != sarama.ErrNoError || block.Offset != perPartition {
			t.Errorf("partition %d: unexpected committed offset: %+v", partition, block)
		}
	}
	if strings.Contains(logBuffer.String(), "abandoned subscription") {
		t.Errorf("subscription was abandoned during rebalance; Sarama log:\n%s", logBuffer.String())
	}
}

func testDelayedSubscription(t *testing.T, brokers, topic string, testedDelay time.Duration) {
	t.Helper()
	const (
		partitionCount = 3
		perPartition   = 12
	)
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Consumer.Offsets.AutoCommit.Interval = 200 * time.Millisecond
	config.Consumer.MaxProcessingTime = 200 * time.Millisecond
	config.ChannelBufferSize = 1
	config.Producer.Return.Successes = true
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Partitioner = sarama.NewManualPartitioner

	var logBuffer synchronizedLog
	oldOutput := logging.StdLogger.Writer()
	logging.StdLogger.SetOutput(&logBuffer)
	defer logging.StdLogger.SetOutput(oldOutput)

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
		NumPartitions: partitionCount, ReplicationFactor: 1,
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := client.RefreshMetadata(topic); err != nil {
		t.Fatal(err)
	}

	type delivery struct {
		group     string
		id        string
		timestamp time.Time
		at        time.Time
	}
	deliveries := make(chan delivery, 3*partitionCount*perPartition)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	groups := []struct {
		name  string
		delay time.Duration
	}{
		{"delay-now", 0},
		{"delay-3m", -3 * time.Minute},
		{"delay-24h", -24 * time.Hour},
	}
	defer func() {
		cancel()
		wg.Wait()
	}()
	for _, spec := range groups {
		group, err := sarama.NewConsumerGroup(strings.Split(brokers, ","), spec.name, config)
		if err != nil {
			t.Fatal(err)
		}
		var fetcher sarama.Consumer
		if spec.delay < 0 {
			fetcher, err = sarama.NewConsumer(strings.Split(brokers, ","), config)
			if err != nil {
				t.Fatal(err)
			}
		}
		handler := &Consumer{
			name: spec.name, delay: spec.delay, ready: make(chan bool), group: group,
			delayFetcher: fetcher, maxProcessingTime: config.Consumer.MaxProcessingTime,
			callback: func(id string, _ []byte, timestamp time.Time, _ []byte) bool {
				deliveries <- delivery{spec.name, id, timestamp, time.Now()}
				return true
			},
		}
		ready := handler.ready
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := consumeTestGroup(ctx, group, topic, handler)
			if closeErr := group.Close(); err == nil {
				err = closeErr
			}
			if fetcher != nil {
				if closeErr := fetcher.Close(); err == nil {
					err = closeErr
				}
			}
			if err != nil && ctx.Err() == nil {
				errs <- fmt.Errorf("%s: %w", spec.name, err)
			}
		}()
		select {
		case <-ready:
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not become ready", spec.name)
		}
	}

	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		t.Fatal(err)
	}
	// Each first record is due in ten seconds for the group under test.
	// Later records include older timestamps to exercise partition ordering.
	due := time.Now().Add(10 * time.Second)
	for partition := int32(0); partition < partitionCount; partition++ {
		for i := 0; i < perPartition; i++ {
			timestamp := due.Add(-testedDelay)
			if partition == 2 || (i > 0 && i%2 == 0) {
				timestamp = due.Add(-testedDelay - time.Hour)
			}
			if partition != 2 && i == 5 {
				timestamp = due.Add(2*time.Second - testedDelay)
			}
			_, _, err := producer.SendMessage(&sarama.ProducerMessage{
				Topic: topic, Partition: partition,
				Key:       sarama.StringEncoder(fmt.Sprintf("%d-%d", partition, i)),
				Value:     sarama.StringEncoder("test"),
				Timestamp: timestamp,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]map[string]bool)
	nextOffset := make(map[string]map[int]int)
	independentProgress := false
	expectedGroups := len(groups)
	if testedDelay == 3*time.Minute {
		expectedGroups-- // 24-hour records are still ineligible at shutdown.
	}
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	for received := 0; received < expectedGroups*partitionCount*perPartition; received++ {
		select {
		case d := <-deliveries:
			if seen[d.group] == nil {
				seen[d.group] = make(map[string]bool)
				nextOffset[d.group] = make(map[int]int)
			}
			if seen[d.group][d.id] {
				t.Errorf("%s received duplicate %s", d.group, d.id)
			}
			seen[d.group][d.id] = true
			var partition, offset int
			if _, err := fmt.Sscanf(d.id, topic+"_%d_%d", &partition, &offset); err != nil {
				t.Fatalf("invalid record ID %q: %v", d.id, err)
			}
			if offset != nextOffset[d.group][partition] {
				t.Errorf("%s partition %d: offset %d, want %d", d.group, partition, offset, nextOffset[d.group][partition])
			}
			nextOffset[d.group][partition]++
			for _, spec := range groups {
				if d.group == spec.name && d.at.Before(d.timestamp.Add(-spec.delay)) {
					t.Errorf("%s delivered %s before it was eligible", d.group, d.id)
				}
				if spec.delay == -testedDelay && d.group == spec.name &&
					partition == 2 && d.at.Before(due.Add(-2*time.Second)) {
					independentProgress = true
				}
			}
		case err := <-errs:
			t.Fatal(err)
		case <-timer.C:
			t.Fatalf("timed out after receiving %d records; Sarama log:\n%s", received, logBuffer.String())
		}
	}
	if !independentProgress {
		t.Error("eligible partition did not make progress while other partitions were waiting")
	}
	cancel()
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer groups did not stop promptly after cancellation")
	}

	for _, spec := range groups {
		expectedOffset := int64(perPartition)
		if testedDelay == 3*time.Minute && spec.delay == -24*time.Hour {
			expectedOffset = -1
		}
		expectedCount := partitionCount * perPartition
		if expectedOffset == -1 {
			expectedCount = 0
		}
		if len(seen[spec.name]) != expectedCount {
			t.Errorf("%s received %d distinct records, want %d", spec.name, len(seen[spec.name]), expectedCount)
		}
		coordinator, err := client.Coordinator(spec.name)
		if err != nil {
			t.Fatal(err)
		}
		request := sarama.NewOffsetFetchRequest(config.Version, spec.name, map[string][]int32{topic: {0, 1, 2}})
		response, err := coordinator.FetchOffset(request)
		if err != nil {
			t.Fatal(err)
		}
		for partition := int32(0); partition < partitionCount; partition++ {
			block := response.GetBlock(topic, partition)
			if block == nil || block.Err != sarama.ErrNoError || block.Offset != expectedOffset {
				t.Errorf("%s partition %d: unexpected committed offset: %+v", spec.name, partition, block)
			}
		}
	}
	if strings.Contains(logBuffer.String(), "abandoned subscription") {
		t.Errorf("delayed subscription was abandoned; Sarama log:\n%s", logBuffer.String())
	}
}

func testDelayedReader(t *testing.T, brokers string) {
	t.Helper()
	const topic, groupID, recordCount = "delay-reader-test", "delay-reader", 12
	config := sarama.NewConfig()
	config.Version = sarama.V3_6_0_0
	config.Producer.Return.Successes = true
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
		NumPartitions: 1, ReplicationFactor: 1,
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := client.RefreshMetadata(topic); err != nil {
		t.Fatal(err)
	}

	var logBuffer synchronizedLog
	oldOutput := logging.StdLogger.Writer()
	logging.StdLogger.SetOutput(&logBuffer)
	defer logging.StdLogger.SetOutput(oldOutput)
	ctx, cancel := context.WithCancel(context.Background())
	deliveries := make(chan string, recordCount*2)
	early := make(chan string, recordCount)
	var wg sync.WaitGroup
	startReader := func(name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ReadFromKafkaWithContextSettings(ctx, name, -86400, brokers, topic, groupID, "",
				func(id string, _ []byte, timestamp time.Time, _ []byte) bool {
					if time.Now().Before(timestamp.Add(24 * time.Hour)) {
						early <- id
					}
					deliveries <- id
					return true
				}, ConsumerSettings{ChannelBufferSize: 1, MaxProcessingTime: 200 * time.Millisecond})
		}()
	}
	defer func() { cancel(); wg.Wait() }()
	waitMembers := func(count int) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			groups, err := admin.DescribeConsumerGroups([]string{groupID})
			if err == nil && len(groups) == 1 && groups[0].State == "Stable" && len(groups[0].Members) == count {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("reader group did not reach %d members: %v", count, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	startReader("delay-reader-first")
	waitMembers(1)

	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(15 * time.Second)
	for i := 0; i < recordCount; i++ {
		if _, _, err := producer.SendMessage(&sarama.ProducerMessage{
			Topic: topic, Timestamp: due.Add(-24 * time.Hour),
			Value: sarama.StringEncoder("test"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	startReader("delay-reader-second")
	waitMembers(2)
	if !time.Now().Before(due) {
		t.Fatal("reader rebalance did not complete before records became eligible")
	}
	for i := 0; i < recordCount; i++ {
		select {
		case id := <-deliveries:
			if expected := fmt.Sprintf("%s_0_%d", topic, i); id != expected {
				t.Errorf("delivery %d = %s, want %s", i, id, expected)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("reader received only %d records", i)
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not shut down promptly")
	}
	select {
	case id := <-early:
		t.Errorf("reader delivered %s early", id)
	default:
	}
	select {
	case id := <-deliveries:
		t.Errorf("reader delivered an extra record: %s", id)
	default:
	}
	coordinator, err := client.Coordinator(groupID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := coordinator.FetchOffset(sarama.NewOffsetFetchRequest(
		config.Version, groupID, map[string][]int32{topic: {0}}))
	if err != nil {
		t.Fatal(err)
	}
	if block := response.GetBlock(topic, 0); block == nil || block.Err != sarama.ErrNoError || block.Offset != recordCount {
		t.Errorf("unexpected reader offset: %+v", block)
	}
	if strings.Contains(logBuffer.String(), "abandoned subscription") {
		t.Errorf("reader abandoned a subscription; log:\n%s", logBuffer.String())
	}
}
