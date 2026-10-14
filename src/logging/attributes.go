package logging

import (
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"

	"sitia.nu/airgap/src/version"
)

// Well-known semantic attribute keys. They follow the OpenTelemetry semantic
// conventions where one exists, so that a collector can route and query records
// without knowing anything about air-gap.
const (
	// AttrEventName is used internally to carry the event name from the call
	// site to the OTLP encoder. It is emitted as the LogRecord eventName field,
	// never as an attribute.
	AttrEventName = "eventName"
	// AttrErrorType classifies a failed operation using a predictable,
	// low-cardinality value.
	AttrErrorType = "error.type"
)

const internalFailureKey = "airgap.internal.failure"

// reservedAttrPrefix is owned by the log integrity machinery. A call site must
// not be able to forge a hash, a sequence number or a chain value, so
// attributes using this prefix are dropped before a record is encoded.
const reservedAttrPrefix = "airgap.log."

// Attrs is an ordered list of attributes for a single record. Order is
// preserved so that output stays byte-stable and therefore diffable in tests.
type Attrs []Attr

// serviceVersion is the value reported as service.version. It defaults to the
// build version and can be overridden, which the tests rely on.
var serviceVersion = version.GitVersion

// SetServiceVersion overrides service.version for this process.
func SetServiceVersion(v string) {
	resourceMu.Lock()
	serviceVersion = strings.TrimSpace(v)
	resourceMu.Unlock()
}

// resourceServiceVersion returns the configured service.version.
func resourceServiceVersion() string {
	resourceMu.RLock()
	defer resourceMu.RUnlock()

	if serviceVersion == "" {
		return "unknown"
	}

	return serviceVersion
}

// Str builds a string attribute.
func Str(key, value string) Attr {
	return Attr{Key: key, Value: value}
}

// Int builds an integer attribute. OTLP/JSON encodes 64-bit integers as decimal
// strings, which is what Attr.Value holds.
func Int(key string, value int64) Attr {
	return Attr{Key: key, Value: strconv.FormatInt(value, 10), IsInt: true}
}

// Decimal builds a string attribute from an integer. Some semantic convention
// identifiers, including messaging.destination.partition.id, are strings even
// when a particular backend uses numeric identifiers.
func Decimal(key string, value int64) Attr {
	return Str(key, strconv.FormatInt(value, 10))
}

// Bool builds a boolean attribute.
func Bool(key string, value bool) Attr {
	return Attr{Key: key, Value: strconv.FormatBool(value), IsBool: true}
}

// Failure marks a failed event. The encoder emits error.type=_OTHER unless a
// more specific Error or ErrorType attribute is present.
func Failure() Attr {
	return Bool(internalFailureKey, true)
}

// Error builds an error.type attribute. Values must be predictable and
// low-cardinality; human-readable details belong in the log body.
func Error(errorType string) Attr {
	if strings.TrimSpace(errorType) == "" {
		errorType = "_OTHER"
	}
	return Str(AttrErrorType, errorType)
}

// ErrorType returns a stable type for a Go error. Generic fmt/errors wrappers
// are unwrapped when they do not provide useful failure classification.
func ErrorType(err error) Attr {
	if err == nil {
		return Error("_OTHER")
	}

	for {
		t := reflect.TypeOf(err)
		if t == nil {
			return Error("_OTHER")
		}
		name := t.String()
		trimmed := strings.TrimPrefix(name, "*")
		if trimmed != "fmt.wrapError" && trimmed != "fmt.wrapErrors" &&
			trimmed != "errors.errorString" && trimmed != "errors.joinError" {
			return Error(trimmed)
		}
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			return Error("_OTHER")
		}
		err = unwrapped
	}
}

// Err builds the attributes that classify a failed Go operation.
func Err(err error) Attrs {
	return Attrs{ErrorType(err)}
}

// Exception describes a recovered Go panic using the OpenTelemetry exception
// conventions. Ordinary Go errors use ErrorType instead.
func Exception(recovered interface{}) Attrs {
	return Attrs{
		Error(fmt.Sprintf("%T", recovered)),
		Str("exception.type", fmt.Sprintf("%T", recovered)),
		Str("exception.message", fmt.Sprint(recovered)),
		Str("exception.stacktrace", string(debug.Stack())),
	}
}

// Event starts an attribute list for a record. The event name is mandatory,
// which is enforced by making this the only constructor the log methods accept.
func Event(name string, extra ...Attr) Attrs {
	attrs := make(Attrs, 0, 1+len(extra))
	attrs = append(attrs, Str(AttrEventName, name))
	attrs = append(attrs, extra...)
	return attrs
}

// With appends further attributes to an existing list without mutating it.
func (a Attrs) With(extra ...Attr) Attrs {
	if len(extra) == 0 {
		return a
	}

	result := make(Attrs, 0, len(a)+len(extra))
	result = append(result, a...)
	result = append(result, extra...)
	return result
}

// Merge appends another list, which keeps helpers such as Err composable.
func (a Attrs) Merge(other Attrs) Attrs {
	return a.With(other...)
}

// sanitize drops attributes that a call site is not allowed to set, and removes
// duplicate keys. The OpenTelemetry log data model requires attribute keys to be
// unique within a record, and the first occurrence is the one the call site
// meant.
func sanitize(attrs Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(attrs))
	result := make(Attrs, 0, len(attrs))
	for _, attr := range attrs {
		if attr.Key == "" || strings.HasPrefix(attr.Key, reservedAttrPrefix) {
			continue
		}

		if _, duplicate := seen[attr.Key]; duplicate {
			continue
		}

		seen[attr.Key] = struct{}{}
		result = append(result, attr)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}
