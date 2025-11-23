package upstream

import (
	"context"

	"sitia.nu/airgap/src/kafka"
)

// KafkaClient defines the interface for reading from Kafka
type KafkaClient interface {
	Read(ctx context.Context, name string, offset int, bootstrapServers, topic, group, from string,
		handler kafka.KafkaHandler)
	SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error
}

// UDPClient defines the interface for UDP connection
type UDPClient interface {
	SendMessage(msg []byte) error
	SendMessages(msgs [][]byte) error
	Close() error
}
