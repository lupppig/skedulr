package skedulr

import (
	"math"
	"math/rand"
	"time"
)

// RetryStrategy defines how a failed task should be retried.
type RetryStrategy interface {
	// NextDelay returns the delay before the next retry, and whether to continue retrying.
	NextDelay(attempt int) (time.Duration, bool)
}

// ExponentialBackoff implements a retry strategy with exponential backoff and jitter.
type ExponentialBackoff struct {
	// MaxAttempts is the maximum number of retries.
	MaxAttempts int
	// BaseDelay is the delay for the first retry.
	BaseDelay time.Duration
	// MaxDelay is the upper bound for any retry delay.
	MaxDelay time.Duration
	// Jitter is a factor (0-1) to randomize the delay.
	Jitter float64 // Jitter factor (0-1), e.g., 0.1 for 10% jitter
}

// NextDelay calculates the next retry interval.
func (e *ExponentialBackoff) NextDelay(attempt int) (time.Duration, bool) {
	if attempt >= e.MaxAttempts {
		return 0, false
	}

	delay := time.Duration(float64(e.BaseDelay) * math.Pow(2, float64(attempt)))
	if delay > e.MaxDelay {
		delay = e.MaxDelay
	}

	if e.Jitter > 0 {
		// Apply random jitter
		jitterAmount := float64(delay) * e.Jitter
		randomJitter := (rand.Float64()*2 - 1) * jitterAmount // Between -jitterAmount and +jitterAmount
		delay += time.Duration(randomJitter)
	}

	return delay, true
}
