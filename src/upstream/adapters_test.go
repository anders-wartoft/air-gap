package upstream

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"strconv"
	"testing"
	"time"
)

type writeOnlyTestConn struct {
	net.Conn
	written  bytes.Buffer
	reads    int
	writeErr error
}

func (c *writeOnlyTestConn) Read([]byte) (int, error) {
	c.reads++
	return 0, errors.New("unexpected read on one-way data connection")
}

func (c *writeOnlyTestConn) Write(data []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.written.Write(data)
}

func (c *writeOnlyTestConn) SetReadDeadline(time.Time) error { return nil }
func (c *writeOnlyTestConn) Close() error                    { return nil }

func TestTCPSendDoesNotProbeHealthyConnection(t *testing.T) {
	conn := &writeOnlyTestConn{}
	adapter := &TCPAdapter{conn: conn, addr: "127.0.0.1:0"}
	defer adapter.Close()
	messages := [][]byte{[]byte("first"), []byte("second")}
	err := adapter.SendMessages(messages)
	if conn.reads != 0 {
		t.Fatalf("healthy send performed %d reads", conn.reads)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.written.String(); got != "firstsecond" {
		t.Fatalf("written data = %q", got)
	}
}

func TestTCPWriteFailureReconnectsAndResends(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		data := make([]byte, len("resend"))
		_, err = io.ReadFull(conn, data)
		if err == nil && string(data) != "resend" {
			err = errors.New("unexpected resent payload")
		}
		received <- err
	}()
	adapter := &TCPAdapter{
		conn: &writeOnlyTestConn{writeErr: net.ErrClosed},
		addr: listener.Addr().String(),
	}
	defer adapter.Close()
	if err := adapter.SendMessage([]byte("resend")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message was not resent after the write failure")
	}
}

func TestKafkaConsumerSettingsAcceptInt32Limits(t *testing.T) {
	config := DefaultConfiguration()
	config.kafkaFetchDefaultBytes = math.MaxInt32
	config.kafkaMaxMessageBytes = math.MaxInt32

	settings, err := kafkaConsumerSettings(config)
	if err != nil {
		t.Fatalf("kafkaConsumerSettings() error = %v", err)
	}
	if settings.FetchDefaultBytes != math.MaxInt32 || settings.FetchMaxBytes != math.MaxInt32 {
		t.Fatalf("fetch settings = %d/%d, want %d/%d",
			settings.FetchDefaultBytes, settings.FetchMaxBytes, math.MaxInt32, math.MaxInt32)
	}
	if settings.MaxProcessingTime != 30*time.Second {
		t.Fatalf("MaxProcessingTime = %s, want 30s", settings.MaxProcessingTime)
	}
}

func TestKafkaConsumerSettingsRejectInvalidProcessingTime(t *testing.T) {
	config := DefaultConfiguration()
	config.kafkaMaxProcessingTimeMs = 0

	if _, err := kafkaConsumerSettings(config); err == nil {
		t.Fatal("kafkaConsumerSettings() accepted a zero processing timeout")
	}
}

func TestKafkaConsumerSettingsRejectInt32Overflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot represent a value larger than int32")
	}

	tests := []struct {
		name      string
		configure func(*TransferConfiguration)
	}{
		{
			name: "fetch default",
			configure: func(config *TransferConfiguration) {
				config.kafkaFetchDefaultBytes = int(math.MaxInt32) + 1
				config.kafkaMaxMessageBytes = int(math.MaxInt32) + 1
			},
		},
		{
			name: "maximum message",
			configure: func(config *TransferConfiguration) {
				config.kafkaMaxMessageBytes = int(math.MaxInt32) + 1
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			config := DefaultConfiguration()
			testCase.configure(&config)

			if _, err := kafkaConsumerSettings(config); err == nil {
				t.Fatal("kafkaConsumerSettings() accepted an int32-overflowing value")
			}
		})
	}
}

func TestKafkaRetryIntervalDefaultAndSettings(t *testing.T) {
	config := DefaultConfiguration()
	settings, err := kafkaConsumerSettings(config)
	if err != nil {
		t.Fatalf("kafkaConsumerSettings() error = %v", err)
	}
	if settings.StartupRetryInterval != 5*time.Second {
		t.Fatalf("StartupRetryInterval = %s, want 5s", settings.StartupRetryInterval)
	}

	config.kafkaRetryInterval = 250
	if settings, err = kafkaConsumerSettings(config); err != nil || settings.StartupRetryInterval != 250*time.Millisecond {
		t.Fatalf("StartupRetryInterval = %s (err %v), want 250ms", settings.StartupRetryInterval, err)
	}

	config.kafkaRetryInterval = 0
	if _, err := kafkaConsumerSettings(config); err == nil {
		t.Fatal("kafkaConsumerSettings() accepted a zero kafkaRetryInterval")
	}
}
