// random_kafka.go
package upstream

import (
	"context"
	"fmt"
	"time"

	"sitia.nu/airgap/src/kafka"
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
	_ context.Context,
	name string,
	offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler,
) {
	Logger.Debugf("RandomKafkaAdapter Read() called: name=%s offset=%d topic=%q group=%q from=%q",
		name, offset, topic, group, from)

	// Create a background context we can cancel
	ctx := context.Background()

	// Launch background goroutine
	go func() {
		i := 0
		Logger.Debugf("RandomKafkaAdapter goroutine started")
		for {
			select {
			case <-ctx.Done():
				Logger.Debugf("RandomKafkaAdapter context cancelled, stopping generator")
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
							Logger.Errorf("RandomKafkaAdapter handler panic: %v", r)
							cont = false
						}
					}()
					cont = handler(id, nil, time.Now(), []byte(msg))
				}()

				i++
				if !cont {
					Logger.Debugf("RandomKafkaAdapter handler requested stop for id=%s", id)
					return
				}
			}
		}
	}()
}
