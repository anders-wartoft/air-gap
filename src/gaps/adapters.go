package gaps

import (
	"context"
	"time"

	"sitia.nu/airgap/src/kafka"
)

// KafkaAdapter implements the KafkaReader interface.
type KafkaAdapter struct{}

func NewKafkaReader() *KafkaAdapter {
	return &KafkaAdapter{}
}

func (r *KafkaAdapter) ReadToEnd(ctx context.Context, brokers string, topic string, group string, callbackFunction func(string, []byte, time.Time, []byte) bool) error {
	return kafka.ReadToEnd(ctx, brokers, topic, group, callbackFunction)
}

func (r *KafkaAdapter) Tail(ctx context.Context, brokers string, topic string, callbackFunction func(string, []byte, time.Time, []byte) bool) error {
	return kafka.TailTopic(ctx, brokers, topic, callbackFunction)
}
