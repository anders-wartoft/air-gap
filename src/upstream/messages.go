package upstream

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sitia.nu/airgap/src/internal/pinnedtls"
	"sitia.nu/airgap/src/logging"
)

// Configurable Airgap events.
//
// An event is configured with a single property holding both the severity and
// the text to emit:
//
//	memoryHighWatermarkLog=WARN:Upstream {upstreamId} entering high memory watermark.
//	memoryLowWatermarkLog=NONE
//
// The value is either NONE, which turns the event off completely, or
// LEVEL:message. The level is the text before the first colon; everything after
// it is the message template. A value whose prefix is not a log level is taken
// as a template in its entirety and emitted at the event's default level, so a
// message may contain colons without being quoted.
//
// Every configurable variable is available as a placeholder in every template,
// written as {name}. Placeholder names are matched case-insensitively and
// ignoring underscores, so {lostEventDirectory}, {lost_event_directory} and
// {LostEventDirectory} are the same placeholder regardless of which notation
// the configuration file uses. Events add their own context placeholders on top
// of the configuration, for instance {topic_name} and {reason} for a lost
// event.

// messageDisabled is the value that turns an event off. Comparison is
// case-insensitive.
const messageDisabled = "NONE"

const (
	messageFormatText = "text"
	messageFormatJSON = "json"
)

// messageSpec is a parsed LEVEL:message configuration value.
type messageSpec struct {
	enabled  bool             // false when the configured value is NONE
	level    logging.LogLevel // severity to emit the message at
	template string           // message template, placeholders not yet expanded
}

// parseMessageSpec splits a configured value into a level and a template.
// defaultLevel is used when the value carries no recognisable level prefix.
func parseMessageSpec(value string, defaultLevel logging.LogLevel) messageSpec {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, messageDisabled) {
		return messageSpec{}
	}
	if separator := strings.Index(trimmed, ":"); separator >= 0 {
		if level, ok := logging.ParseLevel(trimmed[:separator]); ok {
			return messageSpec{
				enabled:  true,
				level:    level,
				template: strings.TrimSpace(trimmed[separator+1:]),
			}
		}
	}
	return messageSpec{enabled: true, level: defaultLevel, template: trimmed}
}

// looksLikeLevel reports whether a value's prefix was probably meant to be a
// log level. It exists so that a misspelled level is reported at startup
// instead of silently becoming part of every message.
func looksLikeLevel(value string) bool {
	separator := strings.Index(value, ":")
	if separator <= 0 {
		return false
	}
	prefix := strings.TrimSpace(value[:separator])
	if prefix == "" || len(prefix) > 10 {
		return false
	}
	for _, r := range prefix {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// validateMessageSpec warns once at startup about a value that looks like it
// was meant to carry a level but does not. The event still works; the prefix
// simply becomes part of the message.
func validateMessageSpec(property string, value string, defaultLevel logging.LogLevel) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, messageDisabled) {
		return
	}
	if separator := strings.Index(trimmed, ":"); separator >= 0 {
		if _, ok := logging.ParseLevel(trimmed[:separator]); ok {
			return
		}
	}
	if !looksLikeLevel(trimmed) {
		return
	}
	Logger.Warnf(configInvalid(property, value),
		"%s starts with %q, which is not a log level. The whole value is used as the message and it is emitted at %s. Legal levels are TRACE, DEBUG, INFO, WARN, ERROR and FATAL, and NONE turns the event off",
		property, trimmed[:strings.Index(trimmed, ":")], levelName(defaultLevel))
}

// levelName renders a LogLevel for configuration messages.
func levelName(level logging.LogLevel) string {
	switch level {
	case logging.TRACE:
		return "TRACE"
	case logging.DEBUG:
		return "DEBUG"
	case logging.WARN:
		return "WARN"
	case logging.ERROR:
		return "ERROR"
	case logging.FATAL:
		return "FATAL"
	default:
		return "INFO"
	}
}

// placeholderPattern matches a {name} placeholder in a template.
var placeholderPattern = regexp.MustCompile(`\{[A-Za-z0-9_.]+\}`)

// normalizePlaceholder makes placeholder lookups independent of notation, so
// that camelCase, snake_case and dotted names all resolve to the same variable.
func normalizePlaceholder(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '_' || r == '.':
			continue
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatSendingThreads renders the sending threads as name=offset pairs so the
// setting can be used in a template like any other configuration value.
func formatSendingThreads(threads []map[string]int) string {
	pairs := make([]string, 0, len(threads))
	for _, thread := range threads {
		names := make([]string, 0, len(thread))
		for name := range thread {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pairs = append(pairs, fmt.Sprintf("%s=%d", name, thread[name]))
		}
	}
	return strings.Join(pairs, ",")
}

// templateValues returns every configurable variable as a template value.
//
// Keys are the property names used in the configuration file. Every field of
// TransferConfiguration is represented, so no template placeholder is ever
// silently unavailable.
//
// Four fields hold something other than a configured setting and are rendered
// as a safe description of their state rather than as their contents:
//
//   - key and newkey hold live symmetric key material. A rendered template is
//     written to the log and delivered to downstream, and the symmetric key is
//     exactly what protects that delivery, so the material itself must never
//     travel that way. The placeholders resolve to the state of the key.
//   - publicKey holds the parsed RSA key read from publicKeyFile. A public key
//     is not secret, but its PEM body is far too long for a log line, so the
//     placeholder resolves to its SHA-256 fingerprint, which is what identifies
//     a key in practice.
//   - filter and inputFilter hold objects compiled from deliverFilter and
//     inputFilterRules, which are exposed verbatim in their own right. The
//     placeholders resolve to whether a filter is in effect.
//
// TestTemplateValuesCoversEveryConfigurationField in messages_test.go fails
// when a new configuration field is added without being listed here, so this
// map cannot silently fall behind the configuration.
func templateValues(c TransferConfiguration) map[string]string {
	return map[string]string{
		"id":                                  c.id,
		"upstreamId":                          c.id,
		"nic":                                 c.nic,
		"targetIP":                            c.targetIP,
		"targetPort":                          strconv.Itoa(c.targetPort),
		"transport":                           c.transport,
		"bootstrapServers":                    c.bootstrapServers,
		"topic":                               c.topic,
		"groupID":                             c.groupID,
		"payloadSize":                         strconv.Itoa(int(c.payloadSize)),
		"from":                                c.from,
		"encryption":                          strconv.FormatBool(c.encryption),
		"publicKeyFile":                       c.publicKeyFile,
		"source":                              c.source,
		"generateNewSymmetricKeyEvery":        strconv.Itoa(c.generateNewSymmetricKeyEvery),
		"logFileName":                         c.logFileName,
		"sendingThreads":                      formatSendingThreads(c.sendingThreads),
		"certFile":                            c.certFile,
		"keyFile":                             c.keyFile,
		"caFile":                              c.caFile,
		"keyPasswordFile":                     describeSecretSetting(c.keyPasswordFile),
		"deliverFilter":                       c.deliverFilter,
		"topicTranslations":                   c.topicTranslations,
		"inputFilterRules":                    c.inputFilterRules,
		"inputFilterDefaultAction":            c.inputFilterDefaultAction,
		"inputFilterTimeout":                  strconv.Itoa(c.inputFilterTimeout),
		"partitionStartValue":                 strconv.Itoa(c.partitionStartValue),
		"eps":                                 strconv.FormatFloat(c.eps, 'f', -1, 64),
		"logLevel":                            c.logLevel,
		"logStatistics":                       strconv.Itoa(int(c.logStatistics)),
		"compressWhenLengthExceeds":           strconv.Itoa(c.compressWhenLengthExceeds),
		"tcpRetryInterval":                    strconv.Itoa(c.tcpRetryInterval),
		"tcpRetryTimes":                       strconv.Itoa(c.tcpRetryTimes),
		"tcpRetryErrorLevel":                  c.tcpRetryErrorLevel,
		"tcpTLSEnabled":                       strconv.FormatBool(c.tcpTLSEnabled),
		"tcpTLSCertFile":                      c.tcpTLSCertFile,
		"tcpTLSKeyFile":                       c.tcpTLSKeyFile,
		"tcpTLSKeyPasswordFile":               c.tcpTLSKeyPasswordFile,
		"tcpTLSCAFile":                        c.tcpTLSCAFile,
		"tcpTLSServerCNRegex":                 c.tcpTLSServerCNRegex,
		"tcpTLSCipherSuites":                  c.tcpTLSCipherSuites,
		"tcpTLSAuthMode":                      c.tcpTLSAuthMode,
		"tcpTLSTrustedKeysDir":                c.tcpTLSTrustedKeysDir,
		"tcpTLSRotationEnabled":               strconv.FormatBool(c.tcpTLSRotationEnabled),
		"tcpTLSPinIdleSeconds":                strconv.Itoa(c.tcpTLSPinIdleSeconds),
		"kafkaFetchDefaultBytes":              strconv.Itoa(c.kafkaFetchDefaultBytes),
		"kafkaMaxMessageBytes":                strconv.Itoa(c.kafkaMaxMessageBytes),
		"kafkaChannelBufferSize":              strconv.Itoa(c.kafkaChannelBufferSize),
		"memoryLimitBytes":                    strconv.FormatInt(c.memoryLimitBytes, 10),
		"memoryHighWatermarkPercent":          strconv.Itoa(c.memoryHighWatermarkPercent),
		"memoryLowWatermarkPercent":           strconv.Itoa(c.memoryLowWatermarkPercent),
		"memoryCheckIntervalMs":               strconv.Itoa(c.memoryCheckIntervalMs),
		"goMemoryLimitPercent":                strconv.Itoa(c.goMemoryLimitPercent),
		"memoryHighWatermarkLog":              c.memoryHighWatermarkLog,
		"memoryLowWatermarkLog":               c.memoryLowWatermarkLog,
		"maxEventBytes":                       strconv.Itoa(c.maxEventBytes),
		"lostEventGuard":                      strconv.FormatBool(c.lostEventGuard),
		"lostEvent":                           c.lostEvent,
		"airgapLostEventMessage":              c.airgapLostEventMessage,
		"airgapLostEventMessageFormat":        c.airgapLostEventMessageFormat,
		"lostEventDirectory":                  lostEventDirectoryValue(c),
		"lostEventDirectoryCompressAfterDays": strconv.Itoa(c.lostEventDirectoryCompressAfterDays),
		"lostEventDirectoryDeleteAfterDays":   strconv.Itoa(c.lostEventDirectoryDeleteAfterDays),
		"lostMessageTruncatedMessage":         c.lostMessageTruncatedMessage,
		"lostMessageTruncatedMessageFormat":   c.lostMessageTruncatedMessageFormat,
		"key":                                 describeKeyMaterial(c.key),
		"newkey":                              describeKeyMaterial(c.newkey),
		"publicKey":                           describePublicKey(c.publicKey),
		"translations":                        formatTranslations(c.translations),
		"filter":                              describePresence(c.filter == nil),
		"inputFilter":                         describePresence(c.inputFilter == nil),
	}
}

// Renderings for the fields that do not hold a configured setting. They are
// deliberately short: a template is both logged and delivered, so an unbounded
// value would be a denial-of-service vector of its own.
const (
	valueNone     = "(none)"
	valueRedacted = "(redacted)"
	valueSet      = "(set)"
)

// describeKeyMaterial reports whether a symmetric key is in use without
// revealing any of it. The length is withheld as well: it tells an observer
// which cipher is in force.
func describeKeyMaterial(key []byte) string {
	if len(key) == 0 {
		return valueNone
	}
	return valueRedacted
}

func describeSecretSetting(value string) string {
	if value == "" {
		return valueNone
	}
	return valueRedacted
}

// describePublicKey renders a public key as the SHA-256 fingerprint of its
// DER encoding, the same form used for pinned TCP peers.
func describePublicKey(key *rsa.PublicKey) string {
	if key == nil {
		return valueNone
	}
	spki, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return valueSet
	}
	return pinnedtls.Fingerprint(spki)
}

// describePresence renders whether a derived object was built.
func describePresence(isNil bool) string {
	if isNil {
		return valueNone
	}
	return valueSet
}

// formatTranslations renders the derived topic translation map as sorted
// from=to pairs so the setting in effect can be used in a template.
func formatTranslations(translations map[string]string) string {
	if len(translations) == 0 {
		return valueNone
	}
	names := make([]string, 0, len(translations))
	for name := range translations {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, fmt.Sprintf("%s=%s", name, translations[name]))
	}
	return strings.Join(pairs, ",")
}

// renderTemplate expands the {placeholders} of a template. Context values
// describe the event being reported and take precedence over the configuration,
// so an event can report, for example, the actual memory limit in use rather
// than the configured one.
//
// An unknown placeholder is left untouched. A typo is then visible in the
// delivered data instead of silently rendering an empty field.
func renderTemplate(template string, context map[string]string) string {
	if template == "" || !strings.Contains(template, "{") {
		return template
	}
	values := make(map[string]string, len(context)+64)
	for name, value := range templateValues(config) {
		values[normalizePlaceholder(name)] = value
	}
	for name, value := range context {
		values[normalizePlaceholder(name)] = value
	}
	return placeholderPattern.ReplaceAllStringFunc(template, func(placeholder string) string {
		if value, ok := values[normalizePlaceholder(placeholder[1:len(placeholder)-1])]; ok {
			return value
		}
		return placeholder
	})
}

// renderJSONTemplate expands placeholders inside JSON string values and then
// serializes the object again. Re-serialization is what makes substitution
// safe: quotes, backslashes and control characters in a value are escaped by
// encoding/json rather than copied into the JSON source.
//
// Placeholders must be inside JSON strings. This keeps the template valid
// before substitution and matches OTLP JSON, where int64 values are represented
// as strings as well.
func renderJSONTemplate(template string, context map[string]string) (string, error) {
	value, err := parseJSONTemplate(template)
	if err != nil {
		return "", err
	}
	renderJSONValue(value, context)
	rendered, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("serialize rendered JSON template: %w", err)
	}
	return string(rendered), nil
}

func validatePayloadTemplate(format string, template string) error {
	switch format {
	case messageFormatText:
		return nil
	case messageFormatJSON:
		_, err := parseJSONTemplate(template)
		return err
	default:
		return fmt.Errorf("illegal message format %q; legal values are text or json", format)
	}
}

func parseJSONTemplate(template string) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(template))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON template: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("invalid JSON template: more than one JSON value")
		}
		return nil, fmt.Errorf("invalid JSON template: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid JSON template: top-level value must be an object")
	}
	return object, nil
}

func renderJSONValue(value any, context map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if text, ok := child.(string); ok {
				typed[name] = renderTemplate(text, context)
				continue
			}
			renderJSONValue(child, context)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = renderTemplate(text, context)
				continue
			}
			renderJSONValue(child, context)
		}
	}
}

// emitConfiguredMessage renders a configurable event and writes it at the level
// the configuration asked for. It returns the rendered message, or an empty
// string when the event is turned off.
func emitConfiguredMessage(spec messageSpec, attrs logging.Attrs, context map[string]string) string {
	if !spec.enabled {
		return ""
	}
	message := renderTemplate(spec.template, context)
	Logger.Log(spec.level, attrs, message)
	return message
}
