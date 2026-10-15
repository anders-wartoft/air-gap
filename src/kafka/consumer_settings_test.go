package kafka

import (
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestApplyConsumerSettingsBoundsFetchAndBufferedRecords(t *testing.T) {
	config := sarama.NewConfig()
	settings := ConsumerSettings{
		FetchDefaultBytes: 2 * 1024 * 1024,
		FetchMaxBytes:     8 * 1024 * 1024,
		ChannelBufferSize: 3,
		MaxProcessingTime: 5 * time.Second,
	}

	applyConsumerSettings(config, settings)

	if config.Consumer.Fetch.Default != settings.FetchDefaultBytes {
		t.Fatalf("Fetch.Default=%d, want %d", config.Consumer.Fetch.Default, settings.FetchDefaultBytes)
	}
	if config.Consumer.Fetch.Max != settings.FetchMaxBytes {
		t.Fatalf("Fetch.Max=%d, want %d", config.Consumer.Fetch.Max, settings.FetchMaxBytes)
	}
	if config.ChannelBufferSize != settings.ChannelBufferSize {
		t.Fatalf("ChannelBufferSize=%d, want %d", config.ChannelBufferSize, settings.ChannelBufferSize)
	}
	if config.Consumer.MaxProcessingTime != settings.MaxProcessingTime {
		t.Fatalf("Consumer.MaxProcessingTime=%s, want %s", config.Consumer.MaxProcessingTime, settings.MaxProcessingTime)
	}
}

func TestApplyConsumerSettingsLeavesDefaultsForZeroValues(t *testing.T) {
	config := sarama.NewConfig()
	defaultFetch := config.Consumer.Fetch.Default
	defaultBuffer := config.ChannelBufferSize
	defaultProcessingTime := config.Consumer.MaxProcessingTime

	applyConsumerSettings(config, ConsumerSettings{})

	if config.Consumer.Fetch.Default != defaultFetch {
		t.Fatalf("Fetch.Default=%d, want default %d", config.Consumer.Fetch.Default, defaultFetch)
	}
	if config.Consumer.Fetch.Max != 0 {
		t.Fatalf("Fetch.Max=%d, want default 0", config.Consumer.Fetch.Max)
	}
	if config.ChannelBufferSize != defaultBuffer {
		t.Fatalf("ChannelBufferSize=%d, want default %d", config.ChannelBufferSize, defaultBuffer)
	}
	if config.Consumer.MaxProcessingTime != defaultProcessingTime {
		t.Fatalf("Consumer.MaxProcessingTime=%s, want default %s", config.Consumer.MaxProcessingTime, defaultProcessingTime)
	}
}
