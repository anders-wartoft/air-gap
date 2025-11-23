package test

import (
	"context"
	"testing"
	"time"

	"sitia.nu/airgap/src/kafka"
	"sitia.nu/airgap/src/protocol"
	"sitia.nu/airgap/src/upstream"
)

// MockKafkaClient signals when the handler has been called
type MockKafkaClient struct {
	HandlerCalled chan struct{}
}

func (m *MockKafkaClient) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	return nil
}

func (m *MockKafkaClient) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	go func() {
		// simulate a message
		// 		keepRunning = consumer.callback(id, message.Key, message.Timestamp, message.Value)
		handler("test_1_0", nil, time.Now(), []byte("hello-world"))
		// signal that the handler ran
		m.HandlerCalled <- struct{}{}
	}()
}

// MockUDPClient collects messages
type MockUDPClient struct {
	Sent [][]byte
}

func (m *MockUDPClient) SendMessage(msg []byte) error {
	m.Sent = append(m.Sent, msg)
	return nil
}

func (m *MockUDPClient) SendMessages(msgs [][]byte) error {
	m.Sent = append(m.Sent, msgs...)
	return nil
}

func (m *MockUDPClient) Close() error { return nil }

func TestRunUpstreamWithMockKafka(t *testing.T) {
	mockKafka := &MockKafkaClient{HandlerCalled: make(chan struct{}, 1)}
	mockUDP := &MockUDPClient{}

	// Run upstream (async)
	config, err := upstream.ReadParameters("../../config/upstream.properties", upstream.DefaultConfiguration())
	if err != nil {
		t.Fatalf("Error reading config: %v", err)
	}
	upstream.SetConfig(config)
	upstream.RunUpstream(mockKafka, mockUDP)

	// Wait for the Kafka handler to be called (with timeout)
	select {
	case <-mockKafka.HandlerCalled:
		// Handler ran, continue
	case <-time.After(1 * time.Second):
		t.Fatal("Kafka handler was not called within 1 second")
	}

	// Assertions: UDP messages
	if len(mockUDP.Sent) == 0 {
		t.Fatalf("Expected UDP messages, got none")
	}

	// Decode UDP messages and check for "hello-world"
	cache := protocol.CreateMessageCache()
	found := false
	for _, raw := range mockUDP.Sent {
		_, _, payload, err := protocol.ParseMessage(raw, cache)
		if err != nil {
			t.Errorf("Failed to parse UDP message: %v", err)
			continue
		}
		if string(payload) == "hello-world" {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Expected 'hello-world' in UDP sent messages, got %+v", mockUDP.Sent)
	}
}
