package upstream

import "fmt"

func (c TransferConfiguration) WarnProductionConfiguration(phase string) {
	warn := func(setting string, value interface{}, risk string) {
		Logger.ProductionWarning(phase, setting, value, risk)
	}
	if c.logLevel == "DEBUG" || c.logLevel == "TRACE" {
		warn("logLevel", c.logLevel, "Verbose logging increases volume and may expose event data.")
	}
	if c.logStatistics == 0 {
		warn("logStatistics", 0, "Periodic delivery counters are disabled.")
	}
	if c.source == "random" {
		warn("source", c.source, "Synthetic traffic is generated instead of consuming Kafka events.")
	}
	if c.deliverFilter != "" {
		warn("deliverFilter", c.deliverFilter, "Events are deliberately skipped; verify complementary sender coverage.")
	}
	if c.source == "kafka" && c.caFile == "" {
		warn("caFile", "", "Kafka input uses an unencrypted connection.")
	}
	if c.source == "kafka" {
		if c.memoryHighWatermarkPercent-c.memoryLowWatermarkPercent < 5 {
			warn("memoryHighWatermarkPercent", c.memoryHighWatermarkPercent,
				fmt.Sprintf("High watermark %d%% is fewer than 5 percentage points above memoryLowWatermarkPercent=%d; small memory fluctuations can cause frequent pause/resume cycles and repeated reclamation. Increase the gap between watermarks while retaining Go memory headroom.",
					c.memoryHighWatermarkPercent, c.memoryLowWatermarkPercent))
		}
		if c.goMemoryLimitPercent == 0 {
			warn("goMemoryLimitPercent", 0,
				"Go memory limit override is disabled; the inherited GOMEMLIMIT may leave insufficient headroom for admission to resume. Set a Go target at least 10 percentage points below memoryLowWatermarkPercent and size live Kafka buffers to fit.")
		} else if c.memoryLowWatermarkPercent-c.goMemoryLimitPercent < 10 {
			warn("goMemoryLimitPercent", c.goMemoryLimitPercent,
				fmt.Sprintf("Go target %d%% leaves fewer than 10 percentage points below memoryLowWatermarkPercent=%d; non-Go or live memory can keep Kafka admission paused. Lower the Go target or increase the memory budget; headroom does not guarantee every workload fits.",
					c.goMemoryLimitPercent, c.memoryLowWatermarkPercent))
		}
	}
	if c.transport == "tcp" {
		if c.tcpRetryTimes > 0 {
			warn("tcpRetryTimes", c.tcpRetryTimes, "Finite retries may lose events during downstream outages.")
		}
		if !c.tcpTLSEnabled {
			warn("tcpTLSEnabled", false, "TCP transport sends unencrypted event payloads.")
		}
	} else if !c.encryption {
		warn("encryption", false, "UDP transport sends unencrypted event payloads.")
	}
}
