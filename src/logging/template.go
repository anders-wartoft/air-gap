package logging

import (
	"strings"
	"time"
)

// AttrMessageTemplate carries the unexpanded template of a templated record in
// OTLP output.
const AttrMessageTemplate = "airgap.message.template"

// AttrErrorMessage carries the human-readable error text (OpenTelemetry
// error.message). The low-cardinality classification is error.type.
const AttrErrorMessage = "error.message"

// ErrMessage builds the error.message attribute for err.
func ErrMessage(err error) Attr {
	if err == nil {
		return Str(AttrErrorMessage, "<nil>")
	}
	return Str(AttrErrorMessage, err.Error())
}

// renderTemplate replaces {attribute.key} placeholders with the value of the
// first attribute with that key, searching attrs before params. Unknown
// placeholders and unmatched braces are kept literally.
func renderTemplate(template string, attrs Attrs, params []Attr) string {
	if !strings.Contains(template, "{") {
		return template
	}
	lookup := func(key string) (string, bool) {
		for _, attr := range attrs {
			if attr.Key == key {
				return attr.Value, true
			}
		}
		for _, attr := range params {
			if attr.Key == key {
				return attr.Value, true
			}
		}
		return "", false
	}
	var builder strings.Builder
	builder.Grow(len(template))
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			builder.WriteString(rest)
			break
		}
		closing := strings.IndexByte(rest[open+1:], '}')
		if closing < 0 {
			builder.WriteString(rest)
			break
		}
		closing += open + 1
		builder.WriteString(rest[:open])
		if value, found := lookup(rest[open+1 : closing]); found {
			builder.WriteString(value)
		} else {
			builder.WriteString(rest[open : closing+1])
		}
		rest = rest[closing+1:]
	}
	return builder.String()
}

// templateAttrs returns the attributes of an emitted templated record: the
// identity attributes, the parameters and the template.
func templateAttrs(attrs Attrs, template string, params []Attr) Attrs {
	result := make(Attrs, 0, len(attrs)+len(params)+1)
	result = append(result, attrs...)
	result = append(result, params...)
	return append(result, Str(AttrMessageTemplate, template))
}

func emitTemplate(level LogLevel, attrs Attrs, template string, params []Attr) {
	emitMu.Lock()
	defer emitMu.Unlock()

	now := time.Now()
	if suppressRepeatedTemplateLocked(now, level, attrs, template, params) {
		return
	}
	writeEventLocked(now, level, renderTemplate(template, attrs, params), templateAttrs(attrs, template, params))
}

// Templated log methods. The message is a template whose {placeholders} are
// OpenTelemetry attribute keys, resolved from attrs and then params.
//
// attrs identify the record: repeated records with the same severity,
// template and attrs are summarized every AIRGAP_LOG_REPEAT_INTERVAL like any
// other repeat. params are values that do not identify the record (message
// IDs, offsets, retry counts): they do not split repeat groups, and a summary
// shows the latest values. Put the error text (ErrMessage) in attrs so each
// distinct cause keeps its own count.
func (l *LoggerType) DebugT(attrs Attrs, template string, params ...Attr) {
	if logLevel <= DEBUG {
		emitTemplate(DEBUG, attrs, template, params)
	}
}

func (l *LoggerType) InfoT(attrs Attrs, template string, params ...Attr) {
	if logLevel <= INFO {
		emitTemplate(INFO, attrs, template, params)
	}
}

func (l *LoggerType) WarnT(attrs Attrs, template string, params ...Attr) {
	if logLevel <= WARN {
		emitTemplate(WARN, attrs, template, params)
	}
}

func (l *LoggerType) ErrorT(attrs Attrs, template string, params ...Attr) {
	if logLevel <= ERROR {
		emitTemplate(ERROR, attrs, template, params)
	}
}
