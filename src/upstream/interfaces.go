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

// TransportClient defines the interface for sending messages over a network transport (UDP, TCP, etc.)
type TransportClient interface {
	SendMessage(msg []byte) error
	SendMessages(msgs [][]byte) error
	Close() error
}

// UDPClient is deprecated, use TransportClient instead
type UDPClient = TransportClient
