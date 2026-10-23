package logging

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// Global logger instance. Logging from goroutines can start before
	// Initialize, as in tests, so both are read and set atomically.
	logger atomic.Pointer[zap.Logger]
	sugar  atomic.Pointer[zap.SugaredLogger]
)

// Config represents logging configuration
type Config struct {
	Level  string
	Format string
	Output string
}

// Initialize initializes the global logger
func Initialize(config Config) error {
	var zapConfig zap.Config

	// Set log level
	level, err := parseLevel(config.Level)
	if err != nil {
		return err
	}

	// Configure based on format
	if strings.ToLower(config.Format) == "json" {
		zapConfig = zap.NewProductionConfig()
	} else {
		zapConfig = zap.NewDevelopmentConfig()
	}

	zapConfig.Level = zap.NewAtomicLevelAt(level)

	// Configure output
	if config.Output != "stdout" && config.Output != "stderr" {
		zapConfig.OutputPaths = []string{config.Output}
		zapConfig.ErrorOutputPaths = []string{config.Output}
	}

	// Build logger
	l, err := zapConfig.Build(
		zap.AddCallerSkip(1),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)
	if err != nil {
		return fmt.Errorf("failed to build logger: %w", err)
	}

	logger.Store(l)
	sugar.Store(l.Sugar())

	return nil
}

// parseLevel parses log level string
func parseLevel(level string) (zapcore.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("invalid log level: %s", level)
	}
}

// GetLogger returns the global logger
func GetLogger() *zap.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	// Fallback to default logger
	l, _ := zap.NewProduction()
	if logger.CompareAndSwap(nil, l) {
		return l
	}
	return logger.Load()
}

// GetSugaredLogger returns the global sugared logger
func GetSugaredLogger() *zap.SugaredLogger {
	if s := sugar.Load(); s != nil {
		return s
	}
	// Fallback to default logger
	l, _ := zap.NewProduction()
	if sugar.CompareAndSwap(nil, l.Sugar()) {
		return sugar.Load()
	}
	return sugar.Load()
}

// Sync flushes any buffered log entries
func Sync() error {
	if l := logger.Load(); l != nil {
		return l.Sync()
	}
	return nil
}

// Debug logs a debug message
func Debug(msg string, fields ...zap.Field) {
	GetLogger().Debug(msg, fields...)
}

// Info logs an info message
func Info(msg string, fields ...zap.Field) {
	GetLogger().Info(msg, fields...)
}

// Warn logs a warning message
func Warn(msg string, fields ...zap.Field) {
	GetLogger().Warn(msg, fields...)
}

// Error logs an error message
func Error(msg string, fields ...zap.Field) {
	GetLogger().Error(msg, fields...)
}

// Fatal logs a fatal message and exits
func Fatal(msg string, fields ...zap.Field) {
	GetLogger().Fatal(msg, fields...)
}

// Debugf logs a debug message with formatting
func Debugf(template string, args ...interface{}) {
	GetSugaredLogger().Debugf(template, args...)
}

// Infof logs an info message with formatting
func Infof(template string, args ...interface{}) {
	GetSugaredLogger().Infof(template, args...)
}

// Warnf logs a warning message with formatting
func Warnf(template string, args ...interface{}) {
	GetSugaredLogger().Warnf(template, args...)
}

// Errorf logs an error message with formatting
func Errorf(template string, args ...interface{}) {
	GetSugaredLogger().Errorf(template, args...)
}

// Fatalf logs a fatal message with formatting and exits
func Fatalf(template string, args ...interface{}) {
	GetSugaredLogger().Fatalf(template, args...)
}

// WithFields creates a logger with additional fields
func WithFields(fields ...zap.Field) *zap.Logger {
	return GetLogger().With(fields...)
}

// WithRequestID creates a logger with a request ID field
func WithRequestID(requestID string) *zap.Logger {
	return GetLogger().With(zap.String("request_id", requestID))
}

// WithOperation creates a logger with an operation field
func WithOperation(operation string) *zap.Logger {
	return GetLogger().With(zap.String("operation", operation))
}

// WithPath creates a logger with a path field
func WithPath(path string) *zap.Logger {
	return GetLogger().With(zap.String("path", path))
}

// WithError creates a logger with an error field
func WithError(err error) *zap.Logger {
	return GetLogger().With(zap.Error(err))
}

// LogOperation logs the start and end of an operation
func LogOperation(operation string, fn func() error) error {
	log := WithOperation(operation)
	log.Info("operation started")

	err := fn()

	if err != nil {
		log.Error("operation failed", zap.Error(err))
	} else {
		log.Info("operation completed")
	}

	return err
}

// InitDefault initializes a default logger for testing/development
func InitDefault() {
	cfg := Config{
		Level:  "info",
		Format: "text",
		Output: "stdout",
	}
	if err := Initialize(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
}

// Made with Bob
