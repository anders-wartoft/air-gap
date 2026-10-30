package downstream

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/IBM/sarama"
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

func retryableKafkaStartupError(err error) bool {
	return kafka.RetryableStartupError(err)
}

func kafkaProducerAttempt(ctx context.Context, create func() (sarama.AsyncProducer, error)) (sarama.AsyncProducer, error) {
	type result struct {
		producer sarama.AsyncProducer
		err      error
	}
	completed := make(chan result)
	// Sarama's constructor has no context. Let shutdown proceed immediately;
	// an in-flight constructor finishes on its own timeouts and releases any
	// producer it created if the caller has already canceled.
	go func() {
		producer, err := create()
		select {
		case completed <- result{producer, err}:
		case <-ctx.Done():
			if producer != nil {
				if err := producer.Close(); err != nil {
					Logger.Errorf(logging.Event(logging.EventKafkaConnect).Merge(logging.Err(err)),
						"Closing Kafka producer after startup cancellation: %v", err)
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-completed:
		return result.producer, result.err
	}
}

func newKafkaProducerWithRetry(ctx context.Context, brokers string, interval time.Duration,
	create func() (sarama.AsyncProducer, error),
) (sarama.AsyncProducer, error) {
	for attempt := int64(1); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		producer, err := kafkaProducerAttempt(ctx, create)
		if err == nil {
			return producer, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableKafkaStartupError(err) {
			return nil, err
		}
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
