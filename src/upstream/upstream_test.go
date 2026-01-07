package upstream

import (
	"testing"
	"time"
)

func FuzzKafkaHandler(f *testing.F) {
	f.Fuzz(func(t *testing.T, time_in int64, id string, time_in2 int64, received []byte, compressWhenLengthExceeds int){
		config.payloadSize = 65507
		config.compressWhenLengthExceeds = compressWhenLengthExceeds
		config.encryption = false
		config.filter = nil
		time_unix := time.UnixMicro(time_in)
		time_unix2 := time.UnixMicro(time_in2)
		udpClient := udpFuzzHandler{
			SendMessageFunc: func([]byte) error { return nil },
			SendMessagesFunc: func([][]byte) error { return nil },
			CloseFunc: func() error { return nil },
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

