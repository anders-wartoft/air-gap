package upstream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTCPRetryAndTransportStatus(t *testing.T) {
	savedConfig := config
	savedStatus, savedPrevious := transportStatus, previousTransportStatus
	t.Cleanup(func() {
		config = savedConfig
		transportStatus, previousTransportStatus = savedStatus, savedPrevious
	})

	for _, tc := range []struct {
		name        string
		retryTimes  int
		failures    int
		cancel      bool
		wantCalls   int
		wantSuccess bool
	}{
		{"infinite retries recover", 0, 3, false, 4, true},
		{"one attempt exhausted", 1, 1, false, 1, false},
		{"multiple attempts exhausted", 3, 3, false, 3, false},
		{"finite retries recover", 3, 2, false, 3, true},
		{"infinite retries canceled", 0, 1, true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config = DefaultConfiguration()
			config.transport = "tcp"
			config.encryption = false
			config.payloadSize = 1500
			config.tcpRetryTimes = tc.retryTimes
			config.tcpRetryInterval = 1
			transportStatus, previousTransportStatus = "running", "running"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			transport := udpFuzzHandler{
				SendMessagesFunc: func([][]byte) error {
					calls++
					if calls > 1 && transportStatus != "connection unavailable" {
						t.Errorf("status during retries = %q, want connection unavailable", transportStatus)
					}
					if calls <= tc.failures {
						if tc.cancel {
							cancel()
						}
						return errors.New("connection unavailable")
					}
					return nil
				},
			}
			got := kafkaHandlerWithContext(ctx, time.Time{}, transport, "topic_0_1", nil, time.Now(), []byte("event"))
			if got != tc.wantSuccess || calls != tc.wantCalls {
				t.Errorf("handler = %v, calls = %d; want %v, %d", got, calls, tc.wantSuccess, tc.wantCalls)
			}
			wantStatus := "connection unavailable"
			if tc.wantSuccess {
				wantStatus = "running"
			}
			if transportStatus != wantStatus {
				t.Errorf("status after handler = %q, want %q", transportStatus, wantStatus)
			}
		})
	}
}

func FuzzKafkaHandler(f *testing.F) {
	f.Fuzz(func(t *testing.T, time_in int64, id string, time_in2 int64, received []byte, compressWhenLengthExceeds int) {
		config.payloadSize = 65507
		config.compressWhenLengthExceeds = compressWhenLengthExceeds
		config.encryption = false
		config.filter = nil
		time_unix := time.UnixMicro(time_in)
		time_unix2 := time.UnixMicro(time_in2)
		udpClient := udpFuzzHandler{
			SendMessageFunc:  func([]byte) error { return nil },
			SendMessagesFunc: func([][]byte) error { return nil },
			CloseFunc:        func() error { return nil },
		}

		kafkaHandler(
			time_unix,
			udpClient,
			id,
			[]byte(""),
			time_unix2,
			received)
	})
}

type udpFuzzHandler struct {
	SendMessageFunc  func([]byte) error
	SendMessagesFunc func([][]byte) error
	CloseFunc        func() error
}

func (u udpFuzzHandler) SendMessage(msg []byte) error {
	return u.SendMessageFunc(msg)
}

func (u udpFuzzHandler) SendMessages(msgs [][]byte) error {
	return u.SendMessagesFunc(msgs)
}

func (u udpFuzzHandler) Close() error {
	return u.CloseFunc()
}
