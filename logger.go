package skedulr

// Logger defines the interface for scheduler logging.
type Logger interface {
	Debug(msg string, keysAndValues ...interface{})
	Info(msg string, keysAndValues ...interface{})
	Error(msg string, err error, keysAndValues ...interface{})
}

type noopLogger struct{}

func (n noopLogger) Debug(string, ...interface{})        {}
func (n noopLogger) Info(string, ...interface{})         {}
func (n noopLogger) Error(string, error, ...interface{}) {}
