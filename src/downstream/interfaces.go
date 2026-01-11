package downstream

// Interfaces for dependency injection / mocking

// TransportReceiver defines the interface for any transport protocol (UDP, TCP, etc.)
// The handler processes incoming messages regardless of transport type
// The protocol parsing remains the same for all transports
type TransportReceiver interface {
	Listen(ip string, port int, rcvBufSize int, handler func([]byte), mtu uint16, stopChan <-chan struct{}, numReceivers int)
	Close() error
	Setup(mtu uint16, numReceivers int, channelBufferSize int, readBufferMultiplier uint16)
}

// UDPReceiver is deprecated, use TransportReceiver instead
type UDPReceiver = TransportReceiver

type KafkaWriter interface {
	Write(key string, topic string, partition int32, message []byte) error
}
