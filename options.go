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

// WithTaskTimeout sets a default timeout for all tasks.
func WithTaskTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		s.defaultTimeout = d
	}
}

// WithInitialWorkers spawns an initial pool of workers.
func WithInitialWorkers(n int) Option {
	return func(s *Scheduler) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.spawnWorkers(n)
	}
}

// WithRetryStrategy sets a default retry strategy for the scheduler.
func WithRetryStrategy(rs RetryStrategy) Option {
	return func(s *Scheduler) {
		s.retryStrategy = rs
	}
}

// WithLogger sets a custom logger for the scheduler.
func WithLogger(l Logger) Option {
	return func(s *Scheduler) {
		s.logger = l
	}
}
