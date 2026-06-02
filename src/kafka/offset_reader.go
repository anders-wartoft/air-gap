// src/kafka/offset_reader.go
package kafka

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
)

// ReadFromOffset starts a partition consumer at startOffset (inclusive) and forwards messages to callback until ctx is done.
func ReadFromOffset(ctx context.Context, partition int, startOffset int64, brokers, topic, group, timestamp string, callback func(string, []byte, time.Time, []byte) bool) error {
	ConfigureSaramaLogger()

	version, err := sarama.ParseKafkaVersion(sarama.DefaultVersion.String())
	if err != nil {
		return fmt.Errorf("parse kafka version: %w", err)
	}
	cfg := sarama.NewConfig()
	cfg.Version = version
	cfg.ClientID = "resend-readfromoffset"
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest

	if tlsConfigParameters != nil {
		tlsCfg, err := createTLSConfig()
		if err != nil {
			return fmt.Errorf("create tls config: %w", err)
		}
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsCfg
	}

	client, err := sarama.NewClient(strings.Split(brokers, ","), cfg)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer client.Close()

	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return fmt.Errorf("new consumer: %w", err)
	}
	defer consumer.Close()

	pc, err := consumer.ConsumePartition(topic, int32(partition), startOffset)
	if err != nil {
		return fmt.Errorf("consume partition: %w", err)
	}
	defer pc.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-pc.Messages():
			if !ok {
				return nil
			}
			id := fmt.Sprintf("%s_%d_%d", msg.Topic, msg.Partition, msg.Offset)
			keep := callback(id, msg.Key, msg.Timestamp, msg.Value)
			if !keep {
				return nil
			}
		case <-time.After(100 * time.Millisecond):
			// loop, check ctx
		}
	}
}

// ReadSingleOffset consumes a partition starting at offset-5 (safety) and stops once the requested offset is returned.
// It times out after 15 seconds to avoid blocking forever.
func ReadSingleOffset(ctx context.Context, partition int, offset int64, brokers, topic, group, timestamp string, callback func(string, []byte, time.Time, []byte) bool) error {
	ConfigureSaramaLogger()

	// Start a bit earlier in case message removed or to be sure we catch it; however ensure not negative
	start := offset - 5
	if start < 0 {
		start = offset
	}
	version, err := sarama.ParseKafkaVersion(sarama.DefaultVersion.String())
	if err != nil {
		return fmt.Errorf("parse kafka version: %w", err)
	}
	cfg := sarama.NewConfig()
	cfg.Version = version
	cfg.ClientID = "resend-readsingle"
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest

	if tlsConfigParameters != nil {
		tlsCfg, err := createTLSConfig()
		if err != nil {
			return fmt.Errorf("create tls config: %w", err)
		}
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsCfg
	}

	client, err := sarama.NewClient(strings.Split(brokers, ","), cfg)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer client.Close()

	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return fmt.Errorf("new consumer: %w", err)
	}
	defer consumer.Close()

	pc, err := consumer.ConsumePartition(topic, int32(partition), start)
	if err != nil {
		return fmt.Errorf("consume partition: %w", err)
	}
	defer pc.Close()

	timeout := time.After(15 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for offset %d", offset)
		case msg, ok := <-pc.Messages():
			if !ok {
				return fmt.Errorf("partition consumer closed")
			}
			if msg.Offset == offset {
				id := fmt.Sprintf("%s_%d_%d", msg.Topic, msg.Partition, msg.Offset)
				callback(id, msg.Key, msg.Timestamp, msg.Value)
				return nil
			}
		}
	}
}

// TailTopic starts a consumer at the current newest offset for every partition
// in topic and forwards arriving messages to callback until ctx is done.
// One goroutine is launched per partition; TailTopic blocks until all finish.
func TailTopic(ctx context.Context, brokers, topic string, callback func(string, []byte, time.Time, []byte) bool) error {
	ConfigureSaramaLogger()

	version, err := sarama.ParseKafkaVersion(sarama.DefaultVersion.String())
	if err != nil {
		return fmt.Errorf("parse kafka version: %w", err)
	}
	cfg := sarama.NewConfig()
	cfg.Version = version
	cfg.ClientID = "gaps-tail"
	if tlsConfigParameters != nil {
		tlsCfg, err := createTLSConfig()
		if err != nil {
			return fmt.Errorf("create tls config: %w", err)
		}
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsCfg
	}

	client, err := sarama.NewClient(strings.Split(brokers, ","), cfg)
	if err != nil {
		return fmt.Errorf("new sarama client: %w", err)
	}
	defer client.Close()

	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return fmt.Errorf("new consumer: %w", err)
	}
	defer consumer.Close()

	partitions, err := consumer.Partitions(topic)
	if err != nil {
		return fmt.Errorf("get partitions for %s: %w", topic, err)
	}

	var wg sync.WaitGroup
	for _, p := range partitions {
		wg.Add(1)
		go func(partition int32) {
			defer wg.Done()
			pc, err := consumer.ConsumePartition(topic, partition, sarama.OffsetNewest)
			if err != nil {
				Logger.Errorf("TailTopic: consume partition %d: %v", partition, err)
				return
			}
			defer pc.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-pc.Messages():
					if !ok {
						return
					}
					id := fmt.Sprintf("%s_%d_%d", msg.Topic, msg.Partition, msg.Offset)
					if !callback(id, msg.Key, msg.Timestamp, msg.Value) {
						return
					}
				case <-time.After(100 * time.Millisecond):
					// re-check ctx
				}
			}
		}(p)
	}
	wg.Wait()
	return nil
}
