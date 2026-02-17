package skedulr

// Logger defines the interface for scheduler logging.
// It uses a semi-structured format with keys and values.
type Logger interface {
	// Debug logs a debug-level message.
	Debug(msg string, keysAndValues ...interface{})
	// Info logs an informational message.
	Info(msg string, keysAndValues ...interface{})
	// Error logs an error message with an associated error object.
	Error(msg string, err error, keysAndValues ...interface{})
}
