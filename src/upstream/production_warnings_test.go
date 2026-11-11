package upstream

import (
	"os"
	"syscall"
	"testing"

	"sitia.nu/airgap/src/internal/productiontest"
)

func TestProductionWarningsProcessHelper(t *testing.T) {
	if path := os.Getenv("AIRGAP_PRODUCTION_HELPER"); path != "" {
		os.Args = []string{"upstream", path}
		Main("test")
		os.Exit(0)
	}
}

func TestProductionWarningsLifecycle(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
				"id=test\nnic=lo0\nsource=random\ntargetIP=127.0.0.1\ntargetPort=12345\npayloadSize=1400\neps=10\nlogLevel=ERROR\n", signal)
		})
	}
	t.Run("file logging", func(t *testing.T) {
		productiontest.Lifecycle(t, "TestProductionWarningsProcessHelper",
			"id=test\nnic=lo0\nsource=random\ntargetIP=127.0.0.1\ntargetPort=12345\npayloadSize=1400\neps=10\nlogLevel=ERROR\n", syscall.SIGTERM, true)
	})
}

func productionWarningFixture() TransferConfiguration {
	c := DefaultConfiguration()
	c.id, c.nic, c.targetIP, c.targetPort = "upstream", "eth0", "downstream", 1234
	c.source, c.bootstrapServers, c.topic, c.groupID = "kafka", "broker:9093", "transfer", "upstream"
	c.transport, c.tcpTLSEnabled, c.tcpTLSCAFile = "tcp", true, "transport-ca.crt"
	c.certFile, c.keyFile, c.caFile = "client.crt", "client.key", "kafka-ca.crt"
	c.logStatistics = 30
	return c
}

func TestProductionWarnings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*TransferConfiguration)
		want   map[string]string
	}{
		{"nominal", func(c *TransferConfiguration) {}, nil},
		{"random input", func(c *TransferConfiguration) { c.source = "random" }, map[string]string{"source": "random"}},
		{"sampling", func(c *TransferConfiguration) { c.deliverFilter = "1,3,5" }, map[string]string{"deliverFilter": "1,3,5"}},
		{"debug", func(c *TransferConfiguration) { c.logLevel = "DEBUG" }, map[string]string{"logLevel": "DEBUG"}},
		{"trace", func(c *TransferConfiguration) { c.logLevel = "TRACE" }, map[string]string{"logLevel": "TRACE"}},
		{"no statistics", func(c *TransferConfiguration) { c.logStatistics = 0 }, map[string]string{"logStatistics": "0"}},
		{"finite retry", func(c *TransferConfiguration) { c.tcpRetryTimes = 1 }, map[string]string{"tcpRetryTimes": "1"}},
		{"plaintext Kafka", func(c *TransferConfiguration) {
			c.certFile, c.keyFile, c.caFile = "", "", ""
		}, map[string]string{"caFile": ""}},
		{"plaintext TCP", func(c *TransferConfiguration) { c.tcpTLSEnabled = false }, map[string]string{"tcpTLSEnabled": "false"}},
		{"plaintext UDP", func(c *TransferConfiguration) { c.transport = "udp" }, map[string]string{"encryption": "false"}},
		{"encrypted UDP ignores TCP knobs", func(c *TransferConfiguration) {
			c.transport, c.encryption, c.tcpRetryTimes, c.tcpTLSEnabled = "udp", true, 1, false
		}, nil},
		{"random ignores Kafka TLS", func(c *TransferConfiguration) {
			c.source, c.caFile = "random", ""
		}, map[string]string{"source": "random"}},
		{"positive statistics floor", func(c *TransferConfiguration) { c.logStatistics = 1 }, nil},
		{"one point of watermark hysteresis", func(c *TransferConfiguration) {
			c.memoryHighWatermarkPercent = 71
		}, map[string]string{"memoryHighWatermarkPercent": "71"}},
		{"four points of watermark hysteresis", func(c *TransferConfiguration) {
			c.memoryHighWatermarkPercent = 74
		}, map[string]string{"memoryHighWatermarkPercent": "74"}},
		{"five points of watermark hysteresis", func(c *TransferConfiguration) {
			c.memoryHighWatermarkPercent = 75
		}, nil},
		{"combined memory risks", func(c *TransferConfiguration) {
			c.memoryLowWatermarkPercent, c.memoryHighWatermarkPercent, c.goMemoryLimitPercent = 50, 54, 45
		}, map[string]string{"memoryHighWatermarkPercent": "54", "goMemoryLimitPercent": "45"}},
		{"random ignores narrow memory hysteresis", func(c *TransferConfiguration) {
			c.source, c.memoryHighWatermarkPercent = "random", 71
		}, map[string]string{"source": "random"}},
		{"Go target just below resume watermark", func(c *TransferConfiguration) {
			c.goMemoryLimitPercent = 69
		}, map[string]string{"goMemoryLimitPercent": "69"}},
		{"nine points of memory headroom", func(c *TransferConfiguration) {
			c.goMemoryLimitPercent = 61
		}, map[string]string{"goMemoryLimitPercent": "61"}},
		{"ten points of memory headroom", func(c *TransferConfiguration) {
			c.goMemoryLimitPercent = 60
		}, nil},
		{"Go target override disabled", func(c *TransferConfiguration) {
			c.goMemoryLimitPercent = 0
		}, map[string]string{"goMemoryLimitPercent": "0"}},
		{"custom low watermark with little headroom", func(c *TransferConfiguration) {
			c.memoryLowWatermarkPercent, c.goMemoryLimitPercent = 50, 45
		}, map[string]string{"goMemoryLimitPercent": "45"}},
		{"random source does not use memory admission", func(c *TransferConfiguration) {
			c.source, c.goMemoryLimitPercent = "random", 0
		}, map[string]string{"source": "random"}},
		{"combined rollout", func(c *TransferConfiguration) {
			c.source, c.logLevel, c.logStatistics = "random", "DEBUG", 0
			c.deliverFilter, c.tcpRetryTimes, c.tcpTLSEnabled = "1,3,5", 3, false
		}, map[string]string{
			"source": "random", "logLevel": "DEBUG", "logStatistics": "0",
			"deliverFilter": "1,3,5", "tcpRetryTimes": "3", "tcpTLSEnabled": "false",
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
	c.logStatistics = 0
	productiontest.CheckStartup(t, func() { checkConfiguration(c) }, map[string]string{"logStatistics": "0"})
}

func TestMemoryWarningsUseEffectiveOverrides(t *testing.T) {
	c := productionWarningFixture()
	c.goMemoryLimitPercent = 69
	t.Setenv("AIRGAP_UPSTREAM_GO_MEMORY_LIMIT_PERCENT", "60")
	c = overrideConfiguration(c)
	productiontest.Check(t, &c, nil)
	c = parseCommandLineOverrides([]string{"upstream", "--goMemoryLimitPercent=69"}, c)
	productiontest.Check(t, &c, map[string]string{"goMemoryLimitPercent": "69"})
	productiontest.CheckStartup(t, func() { checkConfiguration(c) },
		map[string]string{"goMemoryLimitPercent": "69"})
}
