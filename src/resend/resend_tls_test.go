package resend

import (
	"context"
	"testing"
	"time"

	"sitia.nu/airgap/src/kafka"
)

type mockKafkaClient struct {
	tlsConfigured     bool
	setTLSCalls       int
	getOffsetTLSState []bool
	readCalls         int
}

func (m *mockKafkaClient) Read(ctx context.Context, name string, offset int,
	bootstrapServers, topic, group, from string,
	handler kafka.KafkaHandler) {
	// Not used in these tests.
}

func (m *mockKafkaClient) SetTLS(certFile, keyFile, caFile, keyPasswordFile string) error {
	m.setTLSCalls++
	m.tlsConfigured = true
	return nil
}

func (m *mockKafkaClient) ReadToEndPartition(partition int, ctx context.Context, brokers string, topic string, group string, fromOffset int64,
	callbackFunction func(string, []byte, time.Time, []byte) bool) error {
	m.readCalls++
	callbackFunction("topic_0_1", []byte("key"), time.Now(), []byte("value"))
	return nil
}

func (m *mockKafkaClient) GetLastOffset(bootstrapServers, topic string, partition int) (int64, error) {
	m.getOffsetTLSState = append(m.getOffsetTLSState, m.tlsConfigured)
	return 1, nil
}

type mockUDPClient struct {
	sent [][]byte
}

func (m *mockUDPClient) SendMessage(msg []byte) error {
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mockUDPClient) SendMessages(msgs [][]byte) error {
	m.sent = append(m.sent, msgs...)
	return nil
}

func (m *mockUDPClient) Close() error {
	return nil
}

func baseResendConfig() TransferConfiguration {
	config := defaultConfiguration()
	config.bootstrapServers = "broker:9092"
	config.topic = "topic"
	config.groupID = "group"
	config.partition = 0
	config.offsetFrom = 1
	config.offsetTo = 1
	config.payloadSize = 1400
	config.resendFileName = ""
	config.partitionStartValue = 0
	config.partitionStopValue = 0
	config.encryption = false
	config.compressWhenLengthExceeds = 0
	config.eps = -1
	return config
}

func runResendAndWait(t *testing.T, config TransferConfiguration, kafkaClient KafkaClient) {
	t.Helper()
	udpClient := &mockUDPClient{}
	done := make(chan struct{})
	go RunResend(kafkaClient, udpClient, config, done)

	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
		t.Fatalf("RunResend did not complete in time")
	}
}

func TestRunResend_TLSConfiguredBeforeOffsets(t *testing.T) {
	config := baseResendConfig()
	config.certFile = "/tmp/cert.pem"
	config.keyFile = "/tmp/key.pem"
	config.caFile = "/tmp/ca.pem"

	mockKafka := &mockKafkaClient{}
	runResendAndWait(t, config, mockKafka)

	if mockKafka.setTLSCalls == 0 {
		t.Fatalf("expected SetTLS to be called when TLS settings are present")
	}
	if len(mockKafka.getOffsetTLSState) == 0 {
		t.Fatalf("expected GetLastOffset to be called")
	}
	if !mockKafka.getOffsetTLSState[0] {
		t.Fatalf("expected TLS to be configured before GetLastOffset")
	}
}

func TestRunResend_NoTLSDoesNotCallSetTLS(t *testing.T) {
	config := baseResendConfig()

	mockKafka := &mockKafkaClient{}
	runResendAndWait(t, config, mockKafka)

	if mockKafka.setTLSCalls != 0 {
		t.Fatalf("expected SetTLS not to be called when TLS settings are absent")
	}
	if len(mockKafka.getOffsetTLSState) == 0 {
		t.Fatalf("expected GetLastOffset to be called")
	}
	if mockKafka.getOffsetTLSState[0] {
		t.Fatalf("expected TLS to be disabled for non-TLS configuration")
	}
}
