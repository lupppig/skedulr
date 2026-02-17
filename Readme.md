# Skedulr

[![Go Reference](https://pkg.go.dev/badge/github.com/lupppig/skedulr.svg)](https://pkg.go.dev/github.com/lupppig/skedulr)
[![Go Report Card](https://goreportcard.com/status/github.com/lupppig/skedulr)](https://goreportcard.com/report/github.com/lupppig/skedulr)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Skedulr is a high-performance, concurrent task scheduler for Go, designed for production reliability and developer efficiency. It features dynamic worker scaling, prioritized execution, retry strategies with jitter, and a powerful middleware system.

## Features

- **Dynamic Scaling**: Automatically spins up workers based on task volume and idle-detects to scale down.
- **Priority Queuing**: Jobs are executed based on user-defined priority levels.
- **Recursive & Timing**: Support for Cron syntax, one-off delays, and fixed intervals.
- **Middleware System**: Comes with built-in Recovery and Logging support.
- **Smart Retries**: Exponential backoff with random jitter to prevent "thundering herds".
- **Zero-Idle CPU**: Uses sync.Cond for instant reaction times with zero polling overhead.
- **Context Propagation**: Task IDs are injected into context.Context for easy tracing.

## Installation

```bash
go get github.com/lupppig/skedulr
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "time"
    "github.com/lupppig/skedulr"
)

func main() {
    // 1. Initialize
    s := skedulr.New(
        skedulr.WithMaxWorkers(10),
        skedulr.WithTaskTimeout(5 * time.Second),
    )
    
    // 2. Define a Job
    job := func(ctx context.Context) error {
        fmt.Println("Doing work...")
        return nil
    }

    // 3. Submit
    id, err := s.Submit(skedulr.NewTask(job, 10, 0))
    if err != nil {
        panic(err)
    }

    // 4. Cleanup
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    s.ShutDown(ctx)
}
```

## Advanced Usage

### Middleware
Wrap your jobs with global or per-scheduler logic.
```go
s.Use(skedulr.Recovery(myLogger, nil))
s.Use(skedulr.Logging(myLogger))
```

### Scheduling (Cron & Intervals)
```go
// Every weekday at 9 AM
s.ScheduleCron("0 9 * * 1-5", myJob, 10)

// Every 30 seconds
s.ScheduleRecurring(myJob, 30*time.Second, 5)
```

### Persistent Storage (Redis)

Skedulr can persist tasks to Redis to survive process restarts. This requires a Job Registry so Skedulr can re-link saved tasks to your code.

```go
sch := skedulr.New(
    skedulr.WithRedisStorage("localhost:6379", "", 0),
    // Register the function name used for persistence
    skedulr.WithJob("email_worker", myEmailFunc),
)

// Submit a task that will survive a restart
sch.Submit(skedulr.NewPersistentTask("email_worker", []byte("payload"), 10, 0))
```

- **Registry**: Use WithJob or RegisterJob to map names to code.
- **Auto-Recovery**: Tasks are reloaded from Redis automatically on New.
- **Payloads**: Pass state as []byte (JSON/Protobuf compatible).
- **Graceful Cleanup**: Persistent tasks are deleted from Redis once they reach a terminal state (Success or Max Attempts).

### Smart Retries
```go
skedulr.WithRetryStrategy(&skedulr.ExponentialBackoff{
    MaxAttempts: 5,
    BaseDelay:   1s,
    Jitter:      0.1, // 10% random jitter
})
```

## Monitoring
Query the state of any task or the scheduler itself:
```go
// Get overall stats
stats := s.Stats() // SuccessCount, FailureCount, QueueSize

// Get specific task status
status := s.Status(taskID) // Succeeded, Failed, Running, Queued
```

## License
MIT License. See [LICENSE](LICENSE) for details.
