package skedulr

import (
	"math"
	"time"
)

// RetryStrategy defines how a failed task should be retried.
type RetryStrategy interface {
	// NextDelay returns the delay before the next retry, and whether to continue retrying.
	NextDelay(attempt int) (time.Duration, bool)
}

// ExponentialBackoff implements a retry strategy with exponential backoff.
type ExponentialBackoff struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (e *ExponentialBackoff) NextDelay(attempt int) (time.Duration, bool) {
	if attempt >= e.MaxAttempts {
		return 0, false
	}

	delay := time.Duration(float64(e.BaseDelay) * math.Pow(2, float64(attempt)))
	if delay > e.MaxDelay {
		delay = e.MaxDelay
	}

	return delay, true
}
