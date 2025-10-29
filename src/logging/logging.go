package logging

import (
	"log"
	"os"
	"path/filepath"
	"strings"
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

// StdLogger exposes the underlying *log.Logger for use with libraries like Sarama
var StdLogger = logger

// LoggerType for OOP-style logging
type LoggerType struct{}

var Logger = &LoggerType{}

// Quick check to see if we can log in some level
func (l *LoggerType) CanLog(level LogLevel) bool {
	return logLevel <= level
}

// Non-formatted log methods (like log.Print)
func (l *LoggerType) Trace(msg string) {
	if logLevel <= TRACE {
		logger.Print("[TRACE] " + msg)
	}
}

// Non-formatted log methods (like log.Print)
func (l *LoggerType) Debug(msg string) {
	if logLevel <= DEBUG {
		logger.Print("[DEBUG] " + msg)
	}
}
func (l *LoggerType) Info(msg string) {
	if logLevel <= INFO {
		logger.Print("[INFO] " + msg)
	}
}
func (l *LoggerType) Warn(msg string) {
	if logLevel <= WARN {
		logger.Print("[WARN] " + msg)
	}
}
func (l *LoggerType) Error(v interface{}) {
	if logLevel <= ERROR {
		switch val := v.(type) {
		case error:
			logger.Print("[ERROR] " + val.Error())
		case string:
			logger.Print("[ERROR] " + val)
		default:
			logger.Print("[ERROR] unknown error")
		}
	}
}
func (l *LoggerType) Fatal(v interface{}) {
	if logLevel <= FATAL {
		switch val := v.(type) {
		case error:
			logger.Print("[FATAL] " + val.Error())
		case string:
			logger.Print("[FATAL] " + val)
		default:
			logger.Print("[FATAL] unknown fatal error")
		}
		os.Exit(1)
	}
}

// Formatted log wrappers (like log.Printf)
func (l *LoggerType) Tracef(format string, v ...interface{}) {
	if logLevel <= TRACE {
		logger.Printf("[TRACE] "+format, v...)
	}
}
func (l *LoggerType) Debugf(format string, v ...interface{}) {
	if logLevel <= DEBUG {
		logger.Printf("[DEBUG] "+format, v...)
	}
}
func (l *LoggerType) Infof(format string, v ...interface{}) {
	if logLevel <= INFO {
		logger.Printf("[INFO] "+format, v...)
	}
}
func (l *LoggerType) Warnf(format string, v ...interface{}) {
	if logLevel <= WARN {
		logger.Printf("[WARN] "+format, v...)
	}
}
func (l *LoggerType) Errorf(format string, v ...interface{}) {
	if logLevel <= ERROR {
		logger.Printf("[ERROR] "+format, v...)
	}
}
func (l *LoggerType) Fatalf(format string, v ...interface{}) {
	if logLevel <= FATAL {
		logger.Printf("[FATAL] "+format, v...)
		os.Exit(1)
	}
}

// Utility methods
func (l *LoggerType) Print(str string) {
	if logLevel <= INFO {
		logger.Print("[INFO] " + str)
	}
}
func (l *LoggerType) Println(format string, v ...interface{}) {
	if logLevel <= INFO {
		logger.Printf("[INFO] "+format, v...)
	}
}

func (l *LoggerType) Printf(format string, v ...interface{}) {
	if logLevel <= INFO {
		logger.Printf("[INFO] "+format, v...)
	}
}

func (l *LoggerType) Panicf(format string, v ...interface{}) {
	logger.Printf("[PANIC] "+format, v...)
	os.Exit(1)
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
	logger.SetOutput(f)
	return nil
}
