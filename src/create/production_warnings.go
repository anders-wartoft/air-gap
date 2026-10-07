package create

func (c TransferConfiguration) WarnProductionConfiguration(phase string) {
	if c.logLevel == "DEBUG" || c.logLevel == "TRACE" {
		Logger.ProductionWarning(phase, "logLevel", c.logLevel, "Verbose logging increases volume and may expose event data.")
	}
	if c.caFile == "" {
		Logger.ProductionWarning(phase, "caFile", "", "Kafka input uses an unencrypted connection.")
	}
}
