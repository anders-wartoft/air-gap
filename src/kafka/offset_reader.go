package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"sitia.nu/airgap/src/logging"
)

func ReadFromOffset(ctx context.Context, partition int, startOffset int64, brokers, topic, group, timestamp string, callback func(string, []byte, time.Time, []byte) bool) error {
	cl, err := newOffsetConsumer(brokers, "resend-readfromoffset", topic, int32(partition), startOffset)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer cl.Close()
	for {
		if ctx.Err() != nil {
			return nil
		}
		fetches := cl.PollRecords(ctx, 256)
		if err := fetchError(fetches); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("consume partition: %w", err)
		}
		stopped := false
		fetches.EachRecord(func(record *kgo.Record) {
			id := fmt.Sprintf("%s_%d_%d", record.Topic, record.Partition, record.Offset)
			if !callback(id, record.Key, record.Timestamp, record.Value) {
				stopped = true
			}
		})
		if stopped {
			return nil
		}
	}
}

func ReadSingleOffset(ctx context.Context, partition int, offset int64, brokers, topic, group, timestamp string, callback func(string, []byte, time.Time, []byte) bool) error {
	start := max(int64(0), offset-5)
	cl, err := newOffsetConsumer(brokers, "resend-readsingle", topic, int32(partition), start)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer cl.Close()
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fetches := cl.PollRecords(ctx, 256)
		if err := fetchError(fetches); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("consume partition: %w", err)
		}
		found := false
		fetches.EachRecord(func(record *kgo.Record) {
			if record.Offset == offset {
				id := fmt.Sprintf("%s_%d_%d", record.Topic, record.Partition, record.Offset)
				callback(id, record.Key, record.Timestamp, record.Value)
				found = true
			}
		})
		if found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timeout waiting for offset %d", offset)
		default:
		}
	}
}

func TailTopic(ctx context.Context, brokers, topic string, callback func(string, []byte, time.Time, []byte) bool) error {
	opts, err := FranzClientOptions(brokers, "gaps-tail")
	if err != nil {
		return err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}
	defer cl.Close()
	partitions, err := topicPartitions(ctx, cl, topic)
	if err != nil {
		return err
	}
	offsets, err := listOffsets(ctx, cl, topic, partitions, -1)
	if err != nil {
		return err
	}
	starts := make(map[int32]kgo.Offset, len(partitions))
	for _, partition := range partitions {
		starts[partition] = kgo.NewOffset().At(offsets[partition])
	}
	cl.AddConsumePartitions(map[string]map[int32]kgo.Offset{topic: starts})
	for ctx.Err() == nil {
		fetches := cl.PollRecords(ctx, 256)
		if err := fetchError(fetches); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		fetches.EachRecord(func(record *kgo.Record) {
			id := fmt.Sprintf("%s_%d_%d", record.Topic, record.Partition, record.Offset)
			if !callback(id, record.Key, record.Timestamp, record.Value) {
				cl.PauseFetchPartitions(map[string][]int32{record.Topic: {record.Partition}})
			}
		})
	}
	return nil
}

func ReadToEnd(ctx context.Context, brokers, topic, group string, callback func(string, []byte, time.Time, []byte) bool) error {
	return ReadToEndPartition(-1, ctx, brokers, topic, group, 0, callback)
}

func GetLastOffset(ctx context.Context, brokers, topic string, partition int32) (int64, error) {
	opts, err := FranzClientOptions(brokers, "offset-reader")
	if err != nil {
		return 0, err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	offsets, err := listOffsets(ctx, cl, topic, []int32{partition}, -1)
	if err != nil {
		return 0, err
	}
	return offsets[partition], nil
}

func ReadToEndPartition(partition int, ctx context.Context, brokers, topic, group string, fromOffset int64,
	callback func(string, []byte, time.Time, []byte) bool,
) error {
	Logger.Infof(logging.Event(logging.EventResendReplay,
		logging.Str("server.address", brokers),
		logging.Str("messaging.destination.name", topic),
		logging.Decimal("messaging.destination.partition.id", int64(partition)),
		logging.Int("messaging.kafka.offset", fromOffset),
		logging.Str("airgap.kafka.stage", "starting"),
	), "Starting ReadToEnd for topic %s and partition %d", topic, partition)

	opts, err := FranzClientOptions(brokers, "readtoend")
	if err != nil {
		return err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}
	defer cl.Close()
	partitions, err := topicPartitions(ctx, cl, topic)
	if err != nil {
		return err
	}
	if partition >= 0 {
		found := false
		for _, candidate := range partitions {
			if int(candidate) == partition {
				partitions = []int32{candidate}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("partition %d not found in topic %s", partition, topic)
		}
	}
	oldest, err := listOffsets(ctx, cl, topic, partitions, -2)
	if err != nil {
		return err
	}
	newest, err := listOffsets(ctx, cl, topic, partitions, -1)
	if err != nil {
		return err
	}
	for _, id := range partitions {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := max(fromOffset, oldest[id])
		if start >= newest[id] {
			continue
		}
		if err := readPartitionRange(ctx, brokers, topic, id, start, newest[id]-1, callback); err != nil {
			return err
		}
	}
	return nil
}

func readPartitionRange(ctx context.Context, brokers, topic string, partition int32, start, end int64,
	callback func(string, []byte, time.Time, []byte) bool,
) error {
	cl, err := newOffsetConsumer(brokers, "readtoend", topic, partition, start)
	if err != nil {
		return err
	}
	defer cl.Close()
	lastRecordAt := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pollCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		fetches := cl.PollRecords(pollCtx, 256)
		cancel()
		if err := fetchError(fetches); err != nil {
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
		done := false
		fetches.EachRecord(func(record *kgo.Record) {
			lastRecordAt = time.Now()
			id := fmt.Sprintf("%s_%d_%d", topic, partition, record.Offset)
			callback(id, record.Key, record.Timestamp, record.Value)
			if record.Offset >= end {
				done = true
			}
		})
		if done {
			return nil
		}
		if time.Since(lastRecordAt) >= 2*time.Second {
			Logger.Debugf(logging.Event(logging.EventResendReplay,
				logging.Str("messaging.destination.name", topic),
				logging.Decimal("messaging.destination.partition.id", int64(partition)),
				logging.Int("airgap.kafka.stall_timeout_ms", 2000),
				logging.Str("airgap.kafka.stage", "partition.stalled"),
			), "No progress for 2s on partition %d, assuming done", partition)
			return nil
		}
	}
}

func newOffsetConsumer(brokers, clientID, topic string, partition int32, offset int64) (*kgo.Client, error) {
	opts, err := FranzClientOptions(brokers, clientID)
	if err != nil {
		return nil, err
	}
	opts = append(opts, kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
		topic: {partition: kgo.NewOffset().At(offset)},
	}))
	return kgo.NewClient(opts...)
}

func topicPartitions(ctx context.Context, cl *kgo.Client, topic string) ([]int32, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &topic}}
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		return nil, err
	}
	for _, item := range resp.Topics {
		if item.Topic != nil && *item.Topic == topic {
			if item.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(item.ErrorCode)
			}
			partitions := make([]int32, 0, len(item.Partitions))
			for _, part := range item.Partitions {
				if part.ErrorCode != 0 {
					return nil, kerr.TypedErrorForCode(part.ErrorCode)
				}
				partitions = append(partitions, part.Partition)
			}
			return partitions, nil
		}
	}
	return nil, fmt.Errorf("topic %q not found", topic)
}

func listOffsets(ctx context.Context, cl *kgo.Client, topic string, partitions []int32, timestamp int64) (map[int32]int64, error) {
	req := kmsg.NewPtrListOffsetsRequest()
	req.ReplicaID = -1
	entry := kmsg.NewListOffsetsRequestTopic()
	entry.Topic = topic
	for _, partition := range partitions {
		part := kmsg.NewListOffsetsRequestTopicPartition()
		part.Partition = partition
		part.Timestamp = timestamp
		entry.Partitions = append(entry.Partitions, part)
	}
	req.Topics = []kmsg.ListOffsetsRequestTopic{entry}
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		return nil, err
	}
	offsets := make(map[int32]int64, len(partitions))
	for _, item := range resp.Topics {
		if item.Topic != topic {
			continue
		}
		for _, part := range item.Partitions {
			if part.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(part.ErrorCode)
			}
			offsets[part.Partition] = part.Offset
		}
	}
	if len(offsets) != len(partitions) {
		return nil, fmt.Errorf("Kafka returned offsets for %d of %d partitions in %q", len(offsets), len(partitions), topic)
	}
	return offsets, nil
}

func fetchError(fetches kgo.Fetches) error {
	var errs []error
	for _, failure := range fetches.Errors() {
		errs = append(errs, failure.Err)
	}
	return errors.Join(errs...)
}
