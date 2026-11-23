package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"sitia.nu/airgap/src/logging"
)

const version = "2.1.0"

var assignor = "roundrobin"

type franzPartition struct {
	topic string
	id    int32
}

type franzDelivery struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func ReadFromKafkaWithContext(ctx context.Context, name string, offsetSeconds int, brokers, topics, group, timestamp string,
	deliver func(string, []byte, time.Time, []byte) bool, errorCallback ...func(string),
) {
	ReadFromKafkaWithContextSettings(ctx, name, offsetSeconds, brokers, topics, group, timestamp, deliver, ConsumerSettings{}, errorCallback...)
}

func ReadFromKafkaWithContextSettings(ctx context.Context, name string, offsetSeconds int, brokers, topics, group, timestamp string,
	deliver func(string, []byte, time.Time, []byte) bool, settings ConsumerSettings, errorCallback ...func(string),
) {
	if timestamp != "" {
		if _, err := time.Parse("2006-01-02T15:04:05-07:00", timestamp); err != nil {
			Logger.Panicf(logging.Event(logging.EventConfigInvalid,
				logging.Failure(),
				logging.Str("airgap.config.setting", "timestamp"),
				logging.Str("airgap.config.value", timestamp),
				logging.ErrorType(err),
			), "Error parsing from time: %v", err)
		}
	}
	ReadUpstreamWithFranz(ctx, name, offsetSeconds, brokers, topics, group, timestamp, deliver, settings, errorCallback...)
}

func ReadUpstreamWithFranz(ctx context.Context, name string, offsetSeconds int, brokers, topics, group, _ string,
	deliver func(string, []byte, time.Time, []byte) bool, settings ConsumerSettings, status ...func(string),
) {
	event := logging.Event(logging.EventKafkaConsume, logging.Str("airgap.kafka.consumer.name", name),
		logging.Str("messaging.consumer.group.name", group))
	var ready sync.Once
	fail := func(err error, retryIn time.Duration) {
		errorAttrs := event.Merge(logging.Err(err)).With(logging.ErrMessage(err))
		switch {
		case errors.Is(err, errKafkaRecordDelivery):
			if retryIn > 0 {
				Logger.WarnT(errorAttrs, "Kafka record delivery failed: {error.message}. Retrying in {airgap.kafka.retry.interval}",
					logging.Str("airgap.kafka.retry.interval", retryIn.String()))
			} else {
				Logger.WarnT(errorAttrs, "Kafka record delivery failed: {error.message}")
			}
		case RetryableStartupError(err):
			if retryIn > 0 {
				Logger.WarnT(errorAttrs, "Kafka consumer group client unavailable: {error.message}. Retrying in {airgap.kafka.retry.interval}",
					logging.Str("airgap.kafka.retry.interval", retryIn.String()))
			} else {
				Logger.WarnT(errorAttrs, "Kafka consumer group client unavailable: {error.message}")
			}
		default:
			Logger.Errorf(event.Merge(logging.Err(err)), "franz-go consumer error: %v", err)
		}
		if len(status) > 0 && status[0] != nil {
			status[0](fmt.Sprintf("Kafka error: %v", err))
		}
	}
	healthy := func() {
		ready.Do(func() {
			Logger.Infof(event, "Franz-go consumer (WithContext) %s up and running", name)
		})
		if len(status) > 0 && status[0] != nil {
			status[0]("running")
		}
	}
	assigned := func() {
		Logger.Infof(event, "Franz-go consumer (WithContext) %s assigned partitions", name)
		healthy()
	}
	balancers := map[string]kgo.GroupBalancer{
		"roundrobin": kgo.RoundRobinBalancer(), "range": kgo.RangeBalancer(), "sticky": kgo.StickyBalancer(),
	}
	balancer, ok := balancers[assignor]
	if !ok {
		fail(fmt.Errorf("unsupported Kafka group assignor %q", assignor), 0)
		return
	}
	opts, err := FranzClientOptions(brokers, name)
	if err != nil {
		fail(err, 0)
		return
	}
	opts = append(opts, []kgo.Opt{
		kgo.ConsumeTopics(strings.Split(topics, ",")...),
		kgo.ConsumerGroup(group), kgo.Balancers(balancer),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.AutoCommitMarks(), kgo.AutoCommitInterval(time.Second),
		kgo.BlockRebalanceOnPoll(),
	}...)
	if settings.FetchDefaultBytes > 0 {
		opts = append(opts, kgo.FetchMaxBytes(settings.FetchDefaultBytes))
	}
	if settings.FetchMaxBytes > 0 {
		opts = append(opts,
			kgo.FetchMaxPartitionBytes(settings.FetchMaxBytes),
			kgo.BrokerMaxReadBytes(settings.FetchMaxBytes+int32(min(1<<20, (1<<31-1)-int(settings.FetchMaxBytes)))),
		)
	}
	Logger.Infof(event, "Starting upstream franz-go consumer: %s", name)
	delay := -time.Duration(offsetSeconds) * time.Second
	for ctx.Err() == nil {
		err := runFranzConsumer(ctx, opts, delay, settings.ChannelBufferSize, deliver, settings.BeforeMessage,
			func(err error) { fail(err, 0) }, healthy, assigned)
		if ctx.Err() != nil {
			return
		}
		retry := settings.StartupRetryInterval
		if retry <= 0 {
			retry = DefaultStartupRetryInterval
		}
		retryable := RetryableStartupError(err) || errors.Is(err, errKafkaRecordDelivery)
		if retryable {
			fail(err, retry)
		} else {
			fail(err, 0)
		}
		if (!retryable) || errors.Is(err, kerr.TopicAuthorizationFailed) ||
			errors.Is(err, kerr.GroupAuthorizationFailed) || errors.Is(err, kerr.ClusterAuthorizationFailed) {
			return
		}
		if !waitForFranzDelivery(ctx, time.Now().Add(retry)) {
			return
		}
	}
}

func ReadFromKafka(name string, offsetSeconds int, brokers, topics, group, timestamp string,
	callbackFunction func(string, []byte, time.Time, []byte) bool,
) {
	ReadFromKafkaWithContext(context.Background(), name, offsetSeconds, brokers, topics, group, timestamp, callbackFunction)
}

func runFranzConsumer(ctx context.Context, opts []kgo.Opt, delay time.Duration, pollLimit int,
	deliver func(string, []byte, time.Time, []byte) bool, admit func(context.Context) error,
	reportError func(error), healthy, assigned func(),
) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(map[franzPartition]franzDelivery)
	stop := func(partitions map[string][]int32) {
		for topic, ids := range partitions {
			for _, id := range ids {
				if job, ok := jobs[franzPartition{topic, id}]; ok {
					job.cancel()
				}
			}
		}
		for topic, ids := range partitions {
			for _, id := range ids {
				key := franzPartition{topic, id}
				if job, ok := jobs[key]; ok {
					<-job.done
					delete(jobs, key)
				}
			}
		}
	}
	opts = append(opts[:len(opts):len(opts)],
		kgo.OnPartitionsAssigned(func(context.Context, *kgo.Client, map[string][]int32) { assigned() }),
		kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, partitions map[string][]int32) {
			stop(partitions)
			commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := cl.CommitMarkedOffsets(commitCtx); err != nil {
				reportError(fmt.Errorf("commit delivered Kafka offsets on revoke: %w", err))
			}
		}),
		kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, partitions map[string][]int32) { stop(partitions) }),
	)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}
	defer cl.CloseAllowingRebalance()
	if err := cl.Ping(ctx); err != nil {
		return fmt.Errorf("connect Kafka consumer: %w", err)
	}
	if pollLimit <= 0 {
		pollLimit = 256
	}
	for ctx.Err() == nil {
		fetches := cl.PollRecords(ctx, pollLimit)
		failures := fetches.Errors()
		for _, failure := range failures {
			if ctx.Err() == nil {
				reportError(failure.Err)
				if errors.Is(failure.Err, kerr.TopicAuthorizationFailed) || errors.Is(failure.Err, kerr.GroupAuthorizationFailed) ||
					errors.Is(failure.Err, kerr.ClusterAuthorizationFailed) || !RetryableStartupError(failure.Err) {
					cancel(failure.Err)
				}
			}
		}
		var completed []<-chan struct{}
		if ctx.Err() == nil && fetches.NumRecords() > 0 {
			if len(failures) == 0 {
				healthy()
			}
			fetches.EachPartition(func(part kgo.FetchTopicPartition) {
				if len(part.Records) == 0 {
					return
				}
				key := franzPartition{part.Topic, part.Partition}
				jobCtx, jobCancel := context.WithCancel(ctx)
				done := make(chan struct{})
				completed = append(completed, done)
				jobs[key] = franzDelivery{jobCancel, done}
				go func(records []*kgo.Record) {
					defer close(done)
					defer jobCancel()
					if err := deliverFranzBatch(jobCtx, delay, records, admit, deliver, cl.MarkCommitRecords); err != nil {
						if jobCtx.Err() == nil {
							cancel(err)
						}
					}
				}(part.Records)
			})
		}
		cl.AllowRebalance()
		// Avoid polling paused partitions: franz-go can discard and refetch
		// their buffered data. Keep one bounded poll batch in delivery per group.
		for _, done := range completed {
			<-done
		}
	}
	return context.Cause(ctx)
}

func deliverFranzBatch(ctx context.Context, delay time.Duration, records []*kgo.Record,
	admit func(context.Context) error, deliver func(string, []byte, time.Time, []byte) bool, mark func(...*kgo.Record),
) error {
	var lastDelivered *kgo.Record
	defer func() {
		if lastDelivered != nil {
			mark(lastDelivered)
		}
	}()
	for _, record := range records {
		if !waitForFranzDelivery(ctx, record.Timestamp.Add(delay)) {
			return ctx.Err()
		}
		if admit != nil {
			if err := admit(ctx); err != nil {
				return fmt.Errorf("before consuming Kafka record: %w", err)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := fmt.Sprintf("%s_%d_%d", record.Topic, record.Partition, record.Offset)
		if !deliver(id, record.Key, record.Timestamp, record.Value) {
			return fmt.Errorf("%w: Kafka record %s was not delivered; retrying from committed offset", errKafkaRecordDelivery, id)
		}
		lastDelivered = record
	}
	return nil
}

var errKafkaRecordDelivery = errors.New("Kafka record delivery failed")

func waitForFranzDelivery(ctx context.Context, deadline time.Time) bool {
	if wait := time.Until(deadline); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}
