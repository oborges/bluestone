package logging

import (
	"fmt"

	"go.uber.org/zap"
)

// KVLogger logs messages with alternating key/value pairs, the style shared
// by the filesystem layer and the protocol servers.
type KVLogger struct {
	zap *zap.Logger
}

// NewKVLogger wraps a zap logger.
func NewKVLogger(zapLogger *zap.Logger) *KVLogger {
	return &KVLogger{zap: zapLogger}
}

// Info logs an info message
func (l *KVLogger) Info(msg string, keysAndValues ...interface{}) {
	l.zap.Info(msg, kvFields(keysAndValues)...)
}

// Error logs an error message
func (l *KVLogger) Error(msg string, keysAndValues ...interface{}) {
	l.zap.Error(msg, kvFields(keysAndValues)...)
}

// Debug logs a debug message
func (l *KVLogger) Debug(msg string, keysAndValues ...interface{}) {
	l.zap.Debug(msg, kvFields(keysAndValues)...)
}

// kvFields converts alternating key/value pairs to zap fields; a trailing
// key without a value is dropped.
func kvFields(keysAndValues []interface{}) []zap.Field {
	fields := make([]zap.Field, 0, len(keysAndValues)/2)
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		fields = append(fields, zap.Any(fmt.Sprint(keysAndValues[i]), keysAndValues[i+1]))
	}
	return fields
}
