package test

import (
	"sync"

	"sitia.nu/airgap/src/downstream"
)

var Logger = downstream.Logger

// Make sure our mocks implement the interfaces
var _ downstream.UDPReceiver = (*MockUDP)(nil)
var _ downstream.KafkaWriter = (*MockKafka)(nil)

// --- Mock UDP Receiver ---
type MockUDP struct {
	Messages [][]byte
	mu       sync.Mutex
	stop     chan struct{}
}

func (m *MockUDP) Listen(targetIP string, targetPort int, rcvBufSize int, handleUdpMessage func(msg []byte), mtu uint16, stopChan <-chan struct{}, numReceivers int) {
	Logger.Info("MockUDP Listen called")
	m.stop = make(chan struct{})
	go func() {
		<-stopChan
		close(m.stop)
	}()
	// Simulate delivering all messages
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.Messages {
		Logger.Printf("MockUDP delivering message of length %d", len(msg))
		handleUdpMessage(msg)
	}
}

func (m *MockUDP) SendMessage(msg []byte) error {
	Logger.Info("MockUDP SendMessage called")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Messages = append(m.Messages, msg)
	return nil
}

func (m *MockUDP) Close() error {
	Logger.Info("MockUDP Close called")
	close(m.stop)
	return nil
}

func (a *MockUDP) Setup(
	mtu uint16,
	numReceivers int,
	channelBufferSize int,
	readBufferMultiplier uint16,
	enableRxqOvfl bool,
) {
}

// --- Mock Kafka Writer ---
type MockKafka struct {
	Written [][]byte
	mu      sync.Mutex
}

func (m *MockKafka) Write(key, topic string, partition int32, msg []byte) error {
	Logger.Info("MockKafka Write called")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Written = append(m.Written, msg)
	return nil
}

func (m *MockKafka) Close() error {
	return nil
}

func (m *MockKafka) Flush() {}

// --- Test Downstream ---
// func TestRunDownstreamWithMockUDPAndKafka(t *testing.T) {
// 	config := downstream.NewTransferConfiguration()

// 	mockUDP := &MockUDP{}
// 	mockKafka := &MockKafka{}

// 	// We are trying to get the mtu value from the NIC, so we will need ip and port
// 	config.SetMtu(1400) // Default MTU if NIC lookup fails")

// 	// Simulated UDP message (cleartext type)
// 	message := []byte{byte(1)} // TYPE_CLEARTEXT = 1
// 	mockUDP.Messages = append(mockUDP.Messages, message)
// 	downstream.SetConfig(*config)

// 	// Run downstream in a goroutine
// 	stop := make(chan struct{})
// 	go func() {
// 		Logger.Info("Starting RunDownstream with mocks")
// 		downstream.RunDownstream(mockKafka, mockUDP, stop)
// 		close(stop)
// 	}()

// 	// Let it process
// 	time.Sleep(200 * time.Millisecond)

// 	// Assertions
// 	mockKafka.mu.Lock()
// 	defer mockKafka.mu.Unlock()
// 	if len(mockKafka.Written) == 0 {
// 		t.Fatalf("Expected Kafka to have received at least one message, but got none")
// 	}

// 	if string(mockKafka.Written[0]) == "" {
// 		t.Errorf("Expected Kafka to receive non-empty message, got empty string")
// 	}
// }
