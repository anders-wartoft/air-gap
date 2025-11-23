package resend

import (
	"context"
	"time"

	"sitia.nu/airgap/src/kafka"
)

// KafkaClient defines the interface used by RunResend().
// This allows both mocks (for tests) and real adapters.
type KafkaClient interface {
	Read(ctx context.Context, name string, offset int,
		bootstrapServers, topic, group, from string,
		handler kafka.KafkaHandler)
	SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error
	ReadToEndPartition(partition int, ctx context.Context, brokers string, topic string, group string, fromOffset int64,
		callbackFunction func(string, []byte, time.Time, []byte) bool) error
	GetLastOffset(bootstrapServers, topic string, partition int) (int64, error)
}

// UDPClient defines the interface for sending messages via UDP.
type UDPClient interface {
	SendMessage(msg []byte) error
	SendMessages(msgs [][]byte) error
	Close() error
}
