package skedulr

import "time"

// Option defines a functional option for configuring the Scheduler.
type Option func(*Scheduler)

// WithMaxWorkers sets the maximum number of concurrent workers.
func WithMaxWorkers(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.maxWorkers = n
		}
	}
}

// WithQueueSize sets the size of the internal job queue.
func WithQueueSize(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.jobQueue = make(chan task, n)
		}
	}
}

// WithTaskTimeout sets a default timeout for tasks if none is provided.
// Note: Individual tasks can still have their own timeouts.
func WithTaskTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		s.defaultTimeout = d
	}
}

// WithInitialWorkers spawns a fixed number of workers at startup.
func WithInitialWorkers(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.mu.Lock()
			s.spawnWorkers(n)
			s.mu.Unlock()
		}
	}
}

// WithRetryStrategy sets a default retry strategy for the scheduler.
func WithRetryStrategy(rs RetryStrategy) Option {
	return func(s *Scheduler) {
		s.retryStrategy = rs
	}
}
