package resend

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/productiontest"
)

type productionLifecycleKafka struct {
	mockKafkaClient
}

func (m *productionLifecycleKafka) ReadToEndPartition(partition int, ctx context.Context, brokers, topic, group string,
	fromOffset int64, callback func(string, []byte, time.Time, []byte) bool) error {
	if os.Getenv("AIRGAP_RESEND_WAIT") == "1" {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestProductionWarningsProcessHelper(t *testing.T) {
	if path := os.Getenv("AIRGAP_PRODUCTION_HELPER"); path != "" {
		os.Args = []string{"resend", path}
		mainWithKafka("test", &productionLifecycleKafka{})
		os.Exit(0)
	}
}

func TestProductionWarningsLifecycle(t *testing.T) {
	configuration := "id=test\nnic=lo0\ntargetIP=127.0.0.1\ntargetPort=12345\npayloadSize=1400\nbootstrapServers=broker:9092\ntopic=transfer\npartition=0\noffsetFrom=0\noffsetTo=0\nlogLevel=ERROR\n"
	productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper", configuration, nil)
	productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper", configuration, nil, true)
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			t.Setenv("AIRGAP_RESEND_WAIT", "1")
			productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper", configuration, signal)
		})
	}
}

func productionWarningFixture() TransferConfiguration {
	c := defaultConfiguration()
	c.id, c.nic, c.targetIP, c.targetPort = "resend", "eth0", "downstream", 1234
	c.bootstrapServers, c.topic, c.groupID = "broker:9093", "transfer", "resend"
	c.certFile, c.keyFile, c.caFile = "client.crt", "client.key", "kafka-ca.crt"
	c.encryption, c.generateNewSymmetricKeyEvery, c.logStatistics = true, 50, 30
	return c
}

func TestProductionWarnings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*TransferConfiguration)
		want   map[string]string
	}{
		{"nominal", func(c *TransferConfiguration) {}, nil},
		{"first gap is production valid", func(c *TransferConfiguration) { c.limit = "first" }, nil},
		{"debug", func(c *TransferConfiguration) { c.logLevel = "DEBUG" }, map[string]string{"logLevel": "DEBUG"}},
		{"trace", func(c *TransferConfiguration) { c.logLevel = "TRACE" }, map[string]string{"logLevel": "TRACE"}},
		{"no statistics", func(c *TransferConfiguration) { c.logStatistics = 0 }, map[string]string{"logStatistics": "0"}},
		{"positive statistics floor", func(c *TransferConfiguration) { c.logStatistics = 1 }, nil},
		{"plaintext Kafka", func(c *TransferConfiguration) { c.caFile = "" }, map[string]string{"caFile": ""}},
		{"plaintext UDP", func(c *TransferConfiguration) { c.encryption = false }, map[string]string{"encryption": "false"}},
		{"no key rotation", func(c *TransferConfiguration) { c.generateNewSymmetricKeyEvery = 0 }, map[string]string{"generateNewSymmetricKeyEvery": "0"}},
		{"positive rotation floor", func(c *TransferConfiguration) { c.generateNewSymmetricKeyEvery = 1 }, nil},
		{"unencrypted ignores rotation", func(c *TransferConfiguration) {
			c.encryption, c.generateNewSymmetricKeyEvery = false, 0
		}, map[string]string{"encryption": "false"}},
		{"combined rollout", func(c *TransferConfiguration) {
			c.limit, c.logLevel, c.logStatistics, c.caFile, c.encryption = "first", "TRACE", 0, "", false
		}, map[string]string{
			"logLevel": "TRACE", "logStatistics": "0", "caFile": "", "encryption": "false",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := productionWarningFixture()
			tt.change(&c)
			productiontest.Check(t, &c, tt.want)
		})
	}
}

func TestProductionWarningsAtEndOfCheckConfiguration(t *testing.T) {
	c := productionWarningFixture()
	c.encryption = false
	productiontest.CheckStartup(t, func() { checkConfiguration(c) }, map[string]string{"encryption": "false"})
}
