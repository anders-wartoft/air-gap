package resend

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
	if c.caFile == "" {
		warn("caFile", "", "Kafka input uses an unencrypted connection.")
	}
	if !c.encryption {
		warn("encryption", false, "UDP resend sends unencrypted event payloads.")
	} else if c.generateNewSymmetricKeyEvery == 0 {
		warn("generateNewSymmetricKeyEvery", 0, "One symmetric key is used throughout the resend job.")
	}
}
