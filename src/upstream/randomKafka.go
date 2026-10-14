// random_kafka.go
package upstream

import (
	"context"
	"fmt"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/logging"
)

// RandomKafkaAdapter implements KafkaClient by generating synthetic messages.
type RandomKafkaAdapter struct {
}

func (r *RandomKafkaAdapter) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	// no-op for random
	return nil
}

// Read starts generating messages and calls the handler for each one.
// It respects ctx cancellation and stops when handler returns false.
func (r *RandomKafkaAdapter) Read(
	ctx context.Context,
	name string,
	offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler,
) {
	Logger.Debugf(logging.Event(logging.EventKafkaConsume,
		logging.Str("airgap.kafka.source", "random"),
		logging.Str("airgap.kafka.consumer.name", name),
		logging.Int("messaging.kafka.offset", int64(offset)),
		logging.Str("messaging.destination.name", topic),
		logging.Str("messaging.consumer.group.name", group),
		logging.Str("airgap.kafka.from", from),
	), "RandomKafkaAdapter Read() called: name=%s offset=%d topic=%q group=%q from=%q",
		name, offset, topic, group, from)

	func() {
		i := 0
		Logger.Debug(logging.Event(logging.EventKafkaConsume,
			logging.Str("airgap.kafka.source", "random"),
			logging.Str("airgap.kafka.consumer.name", name),
		), "RandomKafkaAdapter goroutine started")
		for {
			select {
			case <-ctx.Done():
				Logger.Debug(logging.Event(logging.EventAppShutdown,
					logging.Str("airgap.kafka.source", "random"),
					logging.Str("airgap.kafka.consumer.name", name),
					logging.Int("airgap.message.count", int64(i)),
				), "RandomKafkaAdapter context cancelled, stopping generator")
				return
			default:
				id := fmt.Sprintf("random_%d", i)
				msg := fmt.Sprintf("Random message %d", i)
				//				Logger.Debugf("RandomKafkaAdapter sending message id=%s", id)

				// Call the handler and respect its return value (false -> stop)
				cont := true
				// Protect against handler panics
				func() {
					defer func() {
						if r := recover(); r != nil {
							Logger.Errorf(logging.Event(logging.EventAppPanic,
								logging.Failure(),
								logging.Str("airgap.kafka.source", "random"),
								logging.Str("airgap.message.id", id),
							).Merge(logging.Exception(r)), "RandomKafkaAdapter handler panic: %v", r)
							cont = false
						}
					}()
					cont = handler(id, nil, time.Now(), []byte(msg))
				}()

				i++
				if !cont {
					Logger.Debugf(logging.Event(logging.EventAppShutdown,
						logging.Str("airgap.kafka.source", "random"),
						logging.Str("airgap.message.id", id),
						logging.Int("airgap.message.count", int64(i)),
					), "RandomKafkaAdapter handler requested stop for id=%s", id)
					return
				}
			}
		}
	}()
}
