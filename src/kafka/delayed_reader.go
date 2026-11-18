package kafka

import (
	"context"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"sitia.nu/airgap/src/logging"
)

const (
	delayedBufferBytes    = 64 * 1024
	delayedBufferMessages = 64
)

// Limits apply to queued records, in addition to the current record and
// Sarama's fetch buffers. The record limit also bounds queued message objects.
type delayedClaimReader struct {
	consumer    *Consumer
	claim       sarama.ConsumerGroupClaim
	partition   sarama.PartitionConsumer
	pending     []*sarama.ConsumerMessage
	pendingSize int
	paused      bool
	groupPaused bool
	stopDrain   chan struct{}
	drained     chan struct{}
}

func (r *delayedClaimReader) messages() <-chan *sarama.ConsumerMessage {
	if r.partition != nil {
		return r.partition.Messages()
	}
	return r.claim.Messages()
}

func (r *delayedClaimReader) takeBuffered() *sarama.ConsumerMessage {
	if len(r.pending) == 0 {
		return nil
	}
	message := r.pending[0]
	r.pending[0] = nil
	r.pending = r.pending[1:]
	r.pendingSize -= delayedMessageBytes(message)
	return message
}

func delayedMessageBytes(message *sarama.ConsumerMessage) int {
	size := len(message.Key) + len(message.Value)
	for _, header := range message.Headers {
		size += len(header.Key) + len(header.Value)
	}
	return size
}

func (r *delayedClaimReader) partitionMap() map[string][]int32 {
	return map[string][]int32{r.claim.Topic(): {r.claim.Partition()}}
}

func (r *delayedClaimReader) fallback() error {
	if r.partition != nil {
		if err := r.partition.Close(); err != nil {
			return fmt.Errorf("close delayed Kafka partition: %w", err)
		}
		r.partition = nil
	} else if r.stopDrain == nil {
		// Pause does not retract in-flight records. Discard these without
		// marking them, then reread from the first undelivered offset.
		r.stopDrain, r.drained = make(chan struct{}), make(chan struct{})
		go func() {
			defer close(r.drained)
			for {
				select {
				case _, ok := <-r.claim.Messages():
					if !ok {
						return
					}
				case <-r.stopDrain:
					return
				}
			}
		}()
	}
	r.pending, r.pendingSize = nil, 0
	r.paused = false
	Logger.Debugf(logging.Event(logging.EventKafkaConsume,
		logging.Str("airgap.kafka.consumer.name", r.consumer.name),
		logging.Str("messaging.destination.name", r.claim.Topic()),
		logging.Decimal("messaging.destination.partition.id", int64(r.claim.Partition())),
		logging.Str("airgap.kafka.stage", "delay.buffer.full"),
	), "Delayed Kafka buffer limit reached; rereading partition %s/%d after delivery",
		r.claim.Topic(), r.claim.Partition())
	return nil
}

func (r *delayedClaimReader) wait(ctx context.Context, message *sarama.ConsumerMessage) error {
	for remaining := time.Until(message.Timestamp.Add(-r.consumer.delay)); remaining > 0; remaining = time.Until(message.Timestamp.Add(-r.consumer.delay)) {
		var inFlight <-chan *sarama.ConsumerMessage
		if r.consumer.delayFetcher != nil &&
			(r.paused || remaining >= r.consumer.maxProcessingTime/2) &&
			(r.stopDrain == nil || r.partition != nil) {
			if !r.paused {
				if r.partition != nil {
					r.partition.Pause()
				} else {
					r.consumer.group.Pause(r.partitionMap())
					r.groupPaused = true
				}
				r.paused = true
			}
			inFlight = r.messages()
		}
		timer := time.NewTimer(remaining)
	waiting:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				break waiting
			case next, ok := <-inFlight:
				if !ok {
					timer.Stop()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return fmt.Errorf("delayed Kafka partition channel closed while waiting: %s/%d",
						r.claim.Topic(), r.claim.Partition())
				}
				size := delayedMessageBytes(next)
				if len(r.pending) >= delayedBufferMessages || size > delayedBufferBytes-r.pendingSize {
					if err := r.fallback(); err != nil {
						timer.Stop()
						return err
					}
					inFlight = nil
				} else {
					r.pending = append(r.pending, next)
					r.pendingSize += size
				}
			}
		}
	}
	return ctx.Err()
}

func (r *delayedClaimReader) delivered(ctx context.Context, offset int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.stopDrain != nil && r.partition == nil {
		partition, err := r.consumer.delayFetcher.ConsumePartition(r.claim.Topic(), r.claim.Partition(), offset+1)
		if err != nil {
			return fmt.Errorf("resume delayed Kafka partition at offset %d: %w", offset+1, err)
		}
		r.partition = partition
	}
	if r.paused && len(r.pending) == 0 {
		if r.partition != nil {
			r.partition.Resume()
		} else {
			r.consumer.group.Resume(r.partitionMap())
			r.groupPaused = false
		}
		r.paused = false
	}
	return nil
}

func (r *delayedClaimReader) close() {
	if r.partition != nil {
		if err := r.partition.Close(); err != nil {
			Logger.Errorf(logging.Event(logging.EventKafkaConsume).Merge(logging.Err(err)),
				"Error closing delayed partition consumer: %v", err)
		}
	}
	if r.stopDrain != nil {
		close(r.stopDrain)
		<-r.drained
	}
	if r.groupPaused {
		r.consumer.group.Resume(r.partitionMap())
	}
}
