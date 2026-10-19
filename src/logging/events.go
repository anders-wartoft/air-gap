package logging

// Event names used across the air-gap binaries. The value of eventName is what
// a machine uses to classify a record, so the vocabulary is closed and shared:
// a new kind of event gets a constant here rather than a literal at the call
// site.
//
// The names are hierarchical, from the most general domain to the specific
// action, so that a consumer can match on a prefix such as "kafka." without
// enumerating every event.
const (
	// Application lifecycle.
	EventAppStart    = "app.lifecycle.start"
	EventAppReady    = "app.lifecycle.ready"
	EventAppReload   = "app.lifecycle.reload"
	EventAppStop     = "app.lifecycle.stop"
	EventAppShutdown = "app.lifecycle.shutdown"
	EventAppFailure  = "app.lifecycle.failure"
	EventAppPanic    = "app.lifecycle.panic.exception"

	// Configuration handling.
	EventConfigLoad              = "config.load"
	EventConfigParse             = "config.parse"
	EventConfigInvalid           = "config.invalid"
	EventConfigApplied           = "config.applied"
	EventConfigOverride          = "config.override"
	EventConfigProductionWarning = "config.production.warning"

	// Kafka client activity.
	EventKafkaConnect            = "kafka.connect"
	EventKafkaConsume            = "kafka.consume"
	EventKafkaProduce            = "kafka.produce"
	EventKafkaCommit             = "kafka.commit"
	EventKafkaRebalance          = "kafka.rebalance"
	EventKafkaOffset             = "kafka.offset"
	EventKafkaClient             = "kafka.client"
	EventKafkaConnectionRestored = "kafka.connection.restored"

	// UDP transport across the diode.
	EventUDPBind    = "udp.bind"
	EventUDPSend    = "udp.send"
	EventUDPReceive = "udp.receive"

	// TLS, including certificate pinning.
	EventTLSHandshake  = "tls.handshake"
	EventTLSPinVerify  = "tls.pin.verify"
	EventTLSPinReload  = "tls.pin.reload"
	EventTLSPinRotate  = "tls.pin.rotate"
	EventTLSPinAging   = "tls.pin.aging"
	EventTLSCertReload = "tls.certificate.reload"

	// Transport across the diode where the record is not specific to UDP or
	// TCP, which is the case wherever the transport is selectable.
	EventTransportConnect            = "transport.connect"
	EventTransportSend               = "transport.send"
	EventTransportConnectionRestored = "transport.connection.restored"
	EventTransportSendRestored       = "transport.send.restored"

	// TCP transport across the diode.
	EventTCPListen     = "tcp.listen"
	EventTCPAccept     = "tcp.accept"
	EventTCPConnection = "tcp.connection"
	EventTCPReceive    = "tcp.receive"
	EventTCPClose      = "tcp.close"

	// Key handling and payload cryptography.
	EventCryptoKeyLoad  = "crypto.key.load"
	EventCryptoKeyPurge = "crypto.key.purge"
	EventCryptoEncrypt  = "crypto.encrypt"
	EventCryptoDecrypt  = "crypto.decrypt"
	// EventCryptoKeyExchange covers sending a new symmetric key downstream.
	EventCryptoKeyExchange = "crypto.key.exchange"

	// The message pipeline itself.
	EventProtocolEncode   = "protocol.encode"
	EventProtocolDecode   = "protocol.decode"
	EventProtocolCache    = "protocol.cache"
	EventMessageFilter    = "message.filter"
	EventMessageDeliver   = "message.deliver"
	EventMessageLost      = "message.lost"
	EventMessageTruncated = "message.lost.truncated"

	// Gap detection and recovery.
	EventGapDetected = "gap.detected"
	EventGapClosed   = "gap.closed"
	EventGapReport   = "gap.report"

	// Resend and resend-file creation.
	EventResendRequest   = "resend.request"
	EventResendReplay    = "resend.replay"
	EventResendFileRead  = "resend.file.read"
	EventCreateScan      = "create.scan"
	EventCreateFileWrite = "create.file.write"

	// Log integrity. The anchor record closes a hash chain window.
	EventLogAnchor                   = "log.integrity.anchor"
	EventLogRepeat                   = "log.repeat"
	EventLogSigningCertificateRotate = "log.integrity.signing_certificate.rotate"

	// Operational reporting that does not belong to a single domain.
	EventStatsReport    = "stats.report"
	EventDiagnostic     = "app.diagnostic"
	EventMemoryLimit    = "memory.limit.applied"
	EventMemoryThrottle = "memory.throttle"
)
