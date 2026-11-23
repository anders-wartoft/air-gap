package downstream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
)

func parseKafkaRetryInterval(value string) (int, error) {
	ms, err := strconv.Atoi(value)
	if err != nil || ms <= 0 || int64(ms) > int64((1<<63-1)/time.Millisecond) {
		return 0, fmt.Errorf("invalid kafkaRetryInterval %q: expected positive milliseconds within time.Duration range", value)
	}
	return ms, nil
}

// parseProducerCompression validates the Kafka producer compression codec name.
// "none" keeps records uncompressed so consumers (such as the Java deduplicator)
// never need native codec libraries, which require an executable temp directory.
func parseProducerCompression(value string) (string, error) {
	codec := strings.ToLower(value)
	switch codec {
	case "none", "gzip", "snappy", "lz4", "zstd":
		return codec, nil
	}
	return "", fmt.Errorf("invalid producerCompression %q: legal values are none, gzip, snappy, lz4, zstd", value)
}

func retryableKafkaStartupError(err error) bool {
	return kafka.RetryableStartupError(err)
}

func kafkaProducerAttempt(ctx context.Context, create func() (kafka.Producer, error)) (kafka.Producer, error) {
	type result struct {
		producer kafka.Producer
		err      error
	}
	completed := make(chan result)
	go func() {
		producer, err := create()
		if err != nil {
			producer = nil
		} else if producer == nil {
			err = errors.New("Kafka producer factory returned nil")
		} else {
			err = producer.Ping(ctx)
		}
		select {
		case completed <- result{producer, err}:
		case <-ctx.Done():
			if producer != nil {
				producer.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-completed:
		if result.err != nil && result.producer != nil {
			result.producer.Close()
		}
		return result.producer, result.err
	}
}

func newKafkaProducerWithRetry(ctx context.Context, brokers string, interval time.Duration,
	create func() (kafka.Producer, error),
) (kafka.Producer, error) {
	for attempt := int64(1); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		producer, err := kafkaProducerAttempt(ctx, create)
		if err == nil {
			UpdateKafkaStatus("running")
			return producer, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableKafkaStartupError(err) {
			return nil, err
		}
		UpdateKafkaStatus(fmt.Sprintf("Kafka producer unavailable: %v", err))
		Logger.Warnf(logging.Event(logging.EventKafkaConnect,
			logging.Failure(),
			logging.Str("server.address", brokers),
			logging.Int("airgap.retry.count", attempt),
			logging.Int("airgap.retry.delay_ms", interval.Milliseconds()),
		).Merge(logging.Err(err)), "Kafka producer unavailable: %v. Retrying in %s...", err, interval)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
