package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Log output formats.
const (
	// FormatText is the historical human-readable output.
	FormatText = "text"
	// FormatOTel is OTLP/JSON, one resourceLogs envelope per line. This is the
	// same encoding the OpenTelemetry Collector's file exporter produces, so a
	// log file can be consumed without a bespoke parser.
	FormatOTel = "otel"
)

// scopeName identifies the instrumentation scope of these records.
const scopeName = "sitia.nu/airgap"

// OpenTelemetry severity numbers. The data model reserves ranges per severity,
// and these are the base value of each range.
const (
	severityNumberTrace = 1
	severityNumberDebug = 5
	severityNumberInfo  = 9
	severityNumberWarn  = 13
	severityNumberError = 17
	severityNumberFatal = 21
)

var (
	resourceMu      sync.RWMutex
	serviceName     string
	serviceInstance string
	processStart    = time.Now()
)

// processStartUnixNano is used to bind a hash chain to one process run.
func processStartUnixNano() int64 {
	return processStart.UnixNano()
}

// resourceServiceName returns service.name, defaulting to the executable name
// so that every binary is identifiable without explicit configuration.
func resourceServiceName() string {
	resourceMu.RLock()
	defer resourceMu.RUnlock()

	if serviceName != "" {
		return serviceName
	}

	if len(os.Args) > 0 && os.Args[0] != "" {
		return filepath.Base(os.Args[0])
	}

	return "airgap"
}

// resourceInstanceID returns service.instance.id, defaulting to the hostname so
// that records from different hosts remain distinguishable.
func resourceInstanceID() string {
	resourceMu.RLock()
	defer resourceMu.RUnlock()

	if serviceInstance != "" {
		return serviceInstance
	}

	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}

	return "unknown"
}

// SetServiceName overrides service.name for this process.
func SetServiceName(name string) {
	emitMu.Lock()
	defer emitMu.Unlock()
	resetRepetitionLocked(time.Now())
	resourceMu.Lock()
	serviceName = strings.TrimSpace(name)
	resourceMu.Unlock()
}

// SetInstanceID sets service.instance.id, normally from the configured id of
// the running binary. Changing it restarts the hash chain, because the genesis
// value is bound to the service identity.
func (l *LoggerType) SetInstanceID(id string) {
	emitMu.Lock()
	defer emitMu.Unlock()
	resetRepetitionLocked(time.Now())
	resourceMu.Lock()
	serviceInstance = strings.TrimSpace(id)
	resourceMu.Unlock()

	integrity.resetChain()
}

// severityNumber maps an internal level to the OpenTelemetry severity number.
func severityNumber(level LogLevel) int {
	switch level {
	case TRACE:
		return severityNumberTrace
	case DEBUG:
		return severityNumberDebug
	case INFO:
		return severityNumberInfo
	case WARN:
		return severityNumberWarn
	case ERROR:
		return severityNumberError
	case FATAL:
		return severityNumberFatal
	default:
		return severityNumberInfo
	}
}

// severityText maps an internal level to the OpenTelemetry severity text.
func severityText(level LogLevel) string {
	switch level {
	case TRACE:
		return "TRACE"
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case FATAL:
		return "FATAL"
	default:
		return "INFO"
	}
}

// OTLP/JSON wire types. Integer fields are encoded as strings, as the protobuf
// JSON mapping requires for 64-bit values.

type otlpAnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpLogRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityNumber       int            `json:"severityNumber"`
	SeverityText         string         `json:"severityText"`
	EventName            string         `json:"eventName"`
	Body                 otlpAnyValue   `json:"body"`
	Attributes           []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpPayload struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

// stringAttr builds an OTLP string attribute.
func stringAttr(key, value string) otlpKeyValue {
	v := value
	return otlpKeyValue{Key: key, Value: otlpAnyValue{StringValue: &v}}
}

// toOTLPAttributes converts internal attributes to their OTLP representation,
// preserving order.
func toOTLPAttributes(attrs []Attr) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}

	result := make([]otlpKeyValue, 0, len(attrs))
	failed := false
	hasErrorType := false
	for _, attr := range attrs {
		if attr.Key == AttrEventName {
			continue
		}
		if attr.Key == internalFailureKey {
			failed = true
			continue
		}
		if attr.Key == AttrErrorType {
			hasErrorType = true
		}
		switch {
		case attr.IsInt:
			v := attr.Value
			result = append(result, otlpKeyValue{Key: attr.Key, Value: otlpAnyValue{IntValue: &v}})
		case attr.IsBool:
			v := attr.Value == "true"
			result = append(result, otlpKeyValue{Key: attr.Key, Value: otlpAnyValue{BoolValue: &v}})
		default:
			result = append(result, stringAttr(attr.Key, attr.Value))
		}
	}
	if failed && !hasErrorType {
		result = append(result, stringAttr(AttrErrorType, "_OTHER"))
	}

	return result
}

func otlpEventName(attrs []Attr) string {
	for _, attr := range attrs {
		if attr.Key == AttrEventName {
			return attr.Value
		}
	}
	return ""
}

// encodeOTLP renders one log record as a single OTLP/JSON line.
func encodeOTLP(timestamp time.Time, level LogLevel, body string, attrs []Attr) ([]byte, error) {
	nanos := strconv.FormatInt(timestamp.UnixNano(), 10)
	bodyValue := body
	eventName := otlpEventName(attrs)

	payload := otlpPayload{
		ResourceLogs: []otlpResourceLogs{
			{
				Resource: otlpResource{
					Attributes: []otlpKeyValue{
						stringAttr("service.name", resourceServiceName()),
						stringAttr("service.version", resourceServiceVersion()),
						stringAttr("service.instance.id", resourceInstanceID()),
					},
				},
				ScopeLogs: []otlpScopeLogs{
					{
						Scope: otlpScope{Name: scopeName},
						LogRecords: []otlpLogRecord{
							{
								TimeUnixNano:         nanos,
								ObservedTimeUnixNano: nanos,
								SeverityNumber:       severityNumber(level),
								SeverityText:         severityText(level),
								EventName:            eventName,
								Body:                 otlpAnyValue{StringValue: &bodyValue},
								Attributes:           toOTLPAttributes(attrs),
							},
						},
					},
				},
			},
		},
	}

	return json.Marshal(payload)
}
