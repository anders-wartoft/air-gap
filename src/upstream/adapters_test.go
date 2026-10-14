package upstream

import (
	"math"
	"strconv"
	"testing"
)

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
