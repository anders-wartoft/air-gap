package upstream

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
