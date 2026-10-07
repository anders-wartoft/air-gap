package create

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"sitia.nu/airgap/src/internal/productiontest"
)

type productionLifecycleReader struct{}

func (productionLifecycleReader) ReadToEnd(ctx context.Context, brokers, topic, group string,
	callback func(string, []byte, time.Time, []byte) bool) error {
	if os.Getenv("AIRGAP_CREATE_WAIT") == "1" {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestProductionWarningsProcessHelper(t *testing.T) {
	if path := os.Getenv("AIRGAP_PRODUCTION_HELPER"); path != "" {
		os.Args = []string{"create", path}
		Main("test", productionLifecycleReader{})
	}
}

func TestProductionWarningsLifecycle(t *testing.T) {
	productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
		"bootstrapServers=broker:9092\ntopic=gaps\nlimit=first\nlogLevel=ERROR\n", nil)
	productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
		"bootstrapServers=broker:9092\ntopic=gaps\nlimit=first\nlogLevel=ERROR\n", nil, true)
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			t.Setenv("AIRGAP_CREATE_WAIT", "1")
			productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
				"bootstrapServers=broker:9092\ntopic=gaps\nlimit=first\nlogLevel=ERROR\n", signal)
		})
	}
}

func productionWarningFixture() TransferConfiguration {
	c := defaultConfiguration()
	c.bootstrapServers, c.topic, c.groupID = "broker:9093", "gaps", "create"
	c.certFile, c.keyFile, c.caFile = "client.crt", "client.key", "kafka-ca.crt"
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
		{"plaintext Kafka", func(c *TransferConfiguration) { c.caFile = "" }, map[string]string{"caFile": ""}},
		{"combined rollout", func(c *TransferConfiguration) {
			c.limit, c.logLevel, c.caFile = "first", "DEBUG", ""
		}, map[string]string{"logLevel": "DEBUG", "caFile": ""}},
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
	c.limit = "first"
	c.certFile, c.keyFile, c.caFile = "", "", ""
	productiontest.CheckStartup(t, func() { checkConfiguration(c) }, map[string]string{"caFile": ""})
}
