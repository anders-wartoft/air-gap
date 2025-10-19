package create

import (
	"context"
	"time"
)

// KafkaReader interface for mockable Kafka reading
type KafkaReader interface {
	ReadToEnd(ctx context.Context, brokers string, topic string, group string, callbackFunction func(string, []byte, time.Time, []byte) bool) error
}
