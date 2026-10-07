package downstream

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
	if c.target == "cmd" || c.target == "null" {
		warn("target", c.target, "Events are printed or discarded instead of delivered to Kafka.")
	}
	if c.target == "kafka" && c.caFile == "" {
		warn("caFile", "", "Kafka output uses an unencrypted connection.")
	}
	if c.channelBufferSize < 16384 {
		warn("channelBufferSize", c.channelBufferSize, "Small queues increase backpressure and packet-drop risk.")
	}
	if c.maximumDecompressSize < 1048576 {
		warn("maximumDecompressSize", c.maximumDecompressSize, "Legitimate large decompressed events may be rejected.")
	}
	if c.transport == "tcp" {
		if c.tcpTLSCertFile == "" {
			warn("tcpTLSCertFile", "", "TCP listener accepts unencrypted event payloads.")
		} else if c.tcpTLSClientAuth != "require" {
			warn("tcpTLSClientAuth", c.tcpTLSClientAuth, "TLS clients are not required to authenticate.")
		}
	} else {
		if c.rcvBufSize < 4194304 {
			warn("rcvBufSize", c.rcvBufSize, "Small requested socket buffers increase UDP overflow risk.")
		}
		if c.readBufferMultiplier < 16 {
			warn("readBufferMultiplier", c.readBufferMultiplier, "Reduced UDP receive-buffer headroom may limit bursts.")
		}
	}
}
