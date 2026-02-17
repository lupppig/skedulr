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

// WithQueueSize sets the size of the internal job dispatch queue.
// Use 0 for strict priority (recommended).
func WithQueueSize(n int) Option {
	return func(s *Scheduler) {
		if n >= 0 {
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

// WithStorage sets a custom storage backend for the scheduler.
func WithStorage(st Storage) Option {
	return func(s *Scheduler) {
		s.storage = st
	}
}

// WithRedisStorage sets the Redis storage backend for the scheduler.
func WithRedisStorage(addr, password string, db int) Option {
	return func(s *Scheduler) {
		s.storage = NewRedisStorage(addr, password, db, "")
	}
}

// WithJob registers a job type during scheduler initialization.
// This is recommended for persistent tasks to ensure the registry is ready before recovery.
func WithJob(name string, job Job) Option {
	return func(s *Scheduler) {
		s.RegisterJob(name, job)
	}
}

// WithMaxCapacity sets the maximum number of tasks allowed in the queue.
func WithMaxCapacity(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.maxQueueSize = n
		}
	}
}

// WithInstanceID sets a custom unique ID for the scheduler instance.
func WithInstanceID(id string) Option {
	return func(s *Scheduler) {
		if id != "" {
			s.instanceID = id
		}
	}
}

// WithLeaseDuration sets the duration for task visibility leases in a distributed environment.
func WithLeaseDuration(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.leaseDuration = d
		}
	}
}
