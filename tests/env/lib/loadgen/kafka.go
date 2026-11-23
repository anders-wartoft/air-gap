package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/kversion"
)

func newKafkaClient(brokers, clientID string, options ...kgo.Opt) (*kgo.Client, error) {
	clientOptions := []kgo.Opt{
		kgo.SeedBrokers(strings.Split(brokers, ",")...),
		kgo.ClientID(clientID),
		kgo.MaxVersions(kversion.FromString("3.6.0")),
	}
	return kgo.NewClient(append(clientOptions, options...)...)
}

func kafkaTopicPartitions(ctx context.Context, client *kgo.Client, topic string) ([]int32, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &topic}}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		return nil, err
	}
	for _, item := range resp.Topics {
		if item.Topic != nil && *item.Topic == topic {
			if item.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(item.ErrorCode)
			}
			partitions := make([]int32, 0, len(item.Partitions))
			for _, partition := range item.Partitions {
				if partition.ErrorCode != 0 {
					return nil, kerr.TypedErrorForCode(partition.ErrorCode)
				}
				partitions = append(partitions, partition.Partition)
			}
			return partitions, nil
		}
	}
	return nil, fmt.Errorf("topic %q not found", topic)
}

func kafkaOffsets(ctx context.Context, client *kgo.Client, topic string, partitions []int32, timestamp int64) (map[int32]int64, error) {
	req := kmsg.NewPtrListOffsetsRequest()
	req.ReplicaID = -1
	entry := kmsg.NewListOffsetsRequestTopic()
	entry.Topic = topic
	for _, partition := range partitions {
		requestPartition := kmsg.NewListOffsetsRequestTopicPartition()
		requestPartition.Partition = partition
		requestPartition.Timestamp = timestamp
		entry.Partitions = append(entry.Partitions, requestPartition)
	}
	req.Topics = []kmsg.ListOffsetsRequestTopic{entry}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		return nil, err
	}
	offsets := make(map[int32]int64, len(partitions))
	for _, item := range resp.Topics {
		if item.Topic != topic {
			continue
		}
		for _, partition := range item.Partitions {
			if partition.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(partition.ErrorCode)
			}
			offsets[partition.Partition] = partition.Offset
		}
	}
	if len(offsets) != len(partitions) {
		return nil, fmt.Errorf("Kafka returned offsets for %d of %d partitions in %q", len(offsets), len(partitions), topic)
	}
	return offsets, nil
}

func kafkaCommittedOffsets(ctx context.Context, client *kgo.Client, group, topic string, partitions []int32) (map[int32]int64, error) {
	req := kmsg.NewPtrOffsetFetchRequest()
	req.Group = group
	req.Topics = []kmsg.OffsetFetchRequestTopic{{Topic: topic, Partitions: partitions}}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		return nil, err
	}
	if resp.ErrorCode != 0 {
		return nil, kerr.TypedErrorForCode(resp.ErrorCode)
	}
	offsets := make(map[int32]int64, len(partitions))
	for _, item := range resp.Topics {
		if item.Topic != topic {
			continue
		}
		for _, partition := range item.Partitions {
			if partition.ErrorCode != 0 {
				return nil, kerr.TypedErrorForCode(partition.ErrorCode)
			}
			offsets[partition.Partition] = partition.Offset
		}
	}
	if len(offsets) != len(partitions) {
		return nil, fmt.Errorf("Kafka returned committed offsets for %d of %d partitions in %q",
			len(offsets), len(partitions), topic)
	}
	return offsets, nil
}
