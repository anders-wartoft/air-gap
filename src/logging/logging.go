package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Log levels
type LogLevel int

const (
	TRACE LogLevel = iota
	DEBUG
	INFO
	WARN
	ERROR
	FATAL
)

var logLevel = INFO // default
var logger = log.New(os.Stderr, "", log.LstdFlags)

// StdLogger exposes the underlying *log.Logger for use with libraries like Sarama.
// Records written through it bypass the integrity chain, because they do not pass
// through emit.
var StdLogger = logger

// output mirrors the destination held by logger. The standard logger prefixes
// every line with a timestamp, which OTLP/JSON must not carry, so structured
// records are written to this writer directly.
var output io.Writer = os.Stderr

// logFile is the file descriptor owned by SetLogFile. Other outputs, including
// stderr and test writers, are not owned by the logging package.
var logFile *os.File

// emitMu serialises the whole emit path. Sequence numbers and chain values must
// follow the order records actually reach the output, so the integrity state is
// updated under the same lock that performs the write.
var emitMu sync.Mutex

var integrity integrityState

var logFormat = FormatText

// LoggerType for OOP-style logging
type LoggerType struct{}

var Logger = &LoggerType{}

// SetFormat selects the output encoding, either text or otel.
func (l *LoggerType) SetFormat(format string) error {
	normalized := strings.ToLower(strings.TrimSpace(format))
	switch normalized {
	case "", FormatText:
		normalized = FormatText
	case FormatOTel, "otlp", "otlp/json":
		normalized = FormatOTel
	default:
		return fmt.Errorf("unsupported log format %q, supported values are %s and %s",
			format, FormatText, FormatOTel)
	}

	emitMu.Lock()
	defer emitMu.Unlock()
	logFormat = normalized
	return nil
}

// GetFormat returns the active output encoding.
func (l *LoggerType) GetFormat() string {
	emitMu.Lock()
	defer emitMu.Unlock()
	return logFormat
}

// SetHashAlgorithm enables per-record payload hashing. An empty value or "none"
// disables hashing, and with it the chain.
func (l *LoggerType) SetHashAlgorithm(algorithm string) error {
	emitMu.Lock()
	defer emitMu.Unlock()
	return integrity.configureIntegrity(algorithm)
}

// SetHashChainLength sets the anchor interval of the hash chain. Zero leaves
// records with only their own payload hash.
func (l *LoggerType) SetHashChainLength(length int) error {
	emitMu.Lock()
	defer emitMu.Unlock()
	return integrity.configureChainLength(length)
}

// writeRecord renders and writes a single record, and advances the integrity
// state. The caller must hold emitMu.
//
// Semantic attributes are emitted in OTLP output only. Text output is the
// historical, human-readable format, and keeping it byte-identical is what
// allows otel to be opt-in without changing any existing installation.
func writeRecord(timestamp time.Time, level LogLevel, body string, semantic Attrs, extra []Attr) []Attr {
	if level >= ERROR {
		semantic = semantic.With(Failure())
	}
	stamped := integrity.stamp(body)
	writeRecordWithStamp(timestamp, level, body, semantic, extra, stamped)
	return stamped
}

func writeRecordWithStamp(timestamp time.Time, level LogLevel, body string, semantic Attrs, extra, stamped []Attr) {
	if logFormat != FormatOTel {
		var attrs []Attr
		if len(extra) > 0 || len(stamped) > 0 {
			attrs = make([]Attr, 0, len(extra)+len(stamped))
			attrs = append(attrs, extra...)
			attrs = append(attrs, stamped...)
		}

		logger.Print(formatTextRecord(level, body, attrs))
		return
	}

	semantic = sanitize(semantic)

	var attrs []Attr
	if len(semantic) > 0 || len(extra) > 0 || len(stamped) > 0 {
		attrs = make([]Attr, 0, len(semantic)+len(extra)+len(stamped))
		attrs = append(attrs, semantic...)
		attrs = append(attrs, extra...)
		attrs = append(attrs, stamped...)
	}

	line, err := encodeOTLP(timestamp, level, body, attrs)
	if err != nil {
		// Never lose a record because it could not be encoded.
		logger.Printf("[ERROR] failed to encode log record as OTLP/JSON: %v", err)
		return
	}

	line = append(line, '\n')
	if _, err := output.Write(line); err != nil {
		logger.Printf("[ERROR] failed to write log record: %v", err)
	}
}

// formatTextRecord keeps the historical layout and appends integrity attributes
// as trailing key=value pairs when they are enabled.
func formatTextRecord(level LogLevel, body string, attrs []Attr) string {
	line := "[" + severityText(level) + "] " + body
	if len(attrs) == 0 {
		return line
	}

	var builder strings.Builder
	builder.WriteString(line)
	for _, attr := range attrs {
		builder.WriteString(" ")
		builder.WriteString(attr.Key)
		builder.WriteString("=")
		builder.WriteString(attr.Value)
	}

	return builder.String()
}

// emit writes one record and, when the chain window is full, the anchor that
// closes it. The anchor is written under the same lock so that no other record
// can slip between the window and its anchor.
func emit(level LogLevel, attrs Attrs, body string) {
	emitMu.Lock()
	defer emitMu.Unlock()

	now := time.Now()
	writeRecord(now, level, body, attrs, nil)

	if integrity.anchorDue() {
		anchorAttrs := integrity.anchorAttributes()
		anchorStamped := integrity.stamp(anchorBody)
		signatureAttrs, err := integrity.signAnchor(anchorAttrs, anchorStamped)
		if err != nil {
			logger.Printf("[ERROR] failed to sign log checkpoint: %v", err)
			signatureAttrs = []Attr{{
				Key:   AttrSignatureError,
				Value: err.Error(),
			}}
		}
		anchorAttrs = append(anchorAttrs, signatureAttrs...)
		writeRecordWithStamp(now, INFO, anchorBody, Event(EventLogAnchor), anchorAttrs, anchorStamped)
		integrity.finishAnchor()
	}
}

// format renders the body. A call without arguments is taken literally, so that
// a message containing a percent sign is not mangled by the formatter.
func format(spec string, v []interface{}) string {
	if len(v) == 0 {
		return spec
	}

	return fmt.Sprintf(spec, v...)
}

// ProductionWarning is always visible without changing the configured threshold.
func (l *LoggerType) ProductionWarning(phase, setting string, value interface{}, risk string) {
	emit(WARN, Event(EventConfigProductionWarning,
		Str("airgap.config.phase", phase),
		Str("airgap.config.setting", setting),
		Str("airgap.config.value", fmt.Sprintf("%v", value)),
		Str("airgap.config.risk", risk),
	), fmt.Sprintf("[PRODUCTION-CONFIG] phase=%s setting=%s value=%v risk=%s",
		phase, setting, value, risk))
}

// Quick check to see if we can log in some level
func (l *LoggerType) CanLog(level LogLevel) bool {
	return logLevel <= level
}

// Non-formatted log methods. Every call carries an attribute list, which must
// be built with Event so that eventName is always present.
func (l *LoggerType) Trace(attrs Attrs, msg string) {
	if logLevel <= TRACE {
		emit(TRACE, attrs, msg)
	}
}

func (l *LoggerType) Debug(attrs Attrs, msg string) {
	if logLevel <= DEBUG {
		emit(DEBUG, attrs, msg)
	}
}
func (l *LoggerType) Info(attrs Attrs, msg string) {
	if logLevel <= INFO {
		emit(INFO, attrs, msg)
	}
}
func (l *LoggerType) Warn(attrs Attrs, msg string) {
	if logLevel <= WARN {
		emit(WARN, attrs, msg)
	}
}
func (l *LoggerType) Error(attrs Attrs, v interface{}) {
	if logLevel <= ERROR {
		switch val := v.(type) {
		case error:
			emit(ERROR, attrs, val.Error())
		case string:
			emit(ERROR, attrs, val)
		default:
			emit(ERROR, attrs, "unknown error")
		}
	}
}
func (l *LoggerType) Fatal(attrs Attrs, v interface{}) {
	if logLevel <= FATAL {
		switch val := v.(type) {
		case error:
			emit(FATAL, attrs, val.Error())
		case string:
			emit(FATAL, attrs, val)
		default:
			emit(FATAL, attrs, "unknown fatal error")
		}
		os.Exit(1)
	}
}

// Formatted log wrappers (like log.Printf), with the attribute list first.
func (l *LoggerType) Tracef(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= TRACE {
		emit(TRACE, attrs, format(spec, v))
	}
}
func (l *LoggerType) Debugf(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= DEBUG {
		emit(DEBUG, attrs, format(spec, v))
	}
}
func (l *LoggerType) Infof(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= INFO {
		emit(INFO, attrs, format(spec, v))
	}
}
func (l *LoggerType) Warnf(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= WARN {
		emit(WARN, attrs, format(spec, v))
	}
}
func (l *LoggerType) Errorf(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= ERROR {
		emit(ERROR, attrs, format(spec, v))
	}
}
func (l *LoggerType) Fatalf(attrs Attrs, spec string, v ...interface{}) {
	if logLevel <= FATAL {
		emit(FATAL, attrs, format(spec, v))
		os.Exit(1)
	}
}

func (l *LoggerType) Panicf(attrs Attrs, spec string, v ...interface{}) {
	emit(FATAL, attrs, "[PANIC] "+format(spec, v))
	os.Exit(1)
}

// ParseLevel converts a level name to a LogLevel. The boolean reports whether
// the name was recognised, so a caller reading a level from configuration can
// tell a typo from a deliberate choice instead of silently falling back.
func ParseLevel(level string) (LogLevel, bool) {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "TRACE":
		return TRACE, true
	case "DEBUG":
		return DEBUG, true
	case "INFO":
		return INFO, true
	case "WARN":
		return WARN, true
	case "ERROR":
		return ERROR, true
	case "FATAL":
		return FATAL, true
	}
	return INFO, false
}

// Log writes one record at a level that is only known at runtime, for messages
// whose severity comes from configuration. FATAL terminates the process, just
// as Fatal does.
func (l *LoggerType) Log(level LogLevel, attrs Attrs, msg string) {
	if logLevel > level {
		return
	}
	emit(level, attrs, msg)
	if level == FATAL {
		os.Exit(1)
	}
}

// Parse log level from string
func (l *LoggerType) SetLogLevel(level string) {
	switch strings.ToUpper(level) {
	case "FATAL":
		logLevel = FATAL
	case "ERROR":
		logLevel = ERROR
	case "WARN":
		logLevel = WARN
	case "INFO":
		logLevel = INFO
	case "DEBUG":
		logLevel = DEBUG
	case "TRACE":
		logLevel = TRACE
	default:
		logLevel = INFO
	}
}

// Get the log level as a string
func (l *LoggerType) GetLogLevel() string {
	switch logLevel {
	case FATAL:
		return "FATAL"
	case ERROR:
		return "ERROR"
	case WARN:
		return "WARN"
	case INFO:
		return "INFO"
	case DEBUG:
		return "DEBUG"
	case TRACE:
		return "TRACE"
	default:
		return "INFO"
	}
}

// This needs to be in all main packages
func (l *LoggerType) SetLogFile(filename string) error {
	err := os.MkdirAll(filepath.Dir(filename), 0755)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	emitMu.Lock()
	defer emitMu.Unlock()

	previousLogFile := logFile
	logger.SetOutput(f)
	output = f
	logFile = f
	if previousLogFile != nil {
		if err := closeReplacedLogFile(previousLogFile); err != nil {
			return fmt.Errorf("close previous log file: %w", err)
		}
	}
	return nil
}

// setOutputForTest redirects both writers. Tests need the raw writer and the
// standard logger to agree.
func setOutputForTest(w io.Writer) {
	emitMu.Lock()
	defer emitMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logger.SetOutput(w)
	output = w
}
