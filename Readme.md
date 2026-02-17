# Skedulr

[![Go Reference](https://pkg.go.dev/badge/github.com/lupppig/skedulr.svg)](https://pkg.go.dev/github.com/lupppig/skedulr)
[![Go Report Card](https://goreportcard.com/status/github.com/lupppig/skedulr)](https://goreportcard.com/report/github.com/lupppig/skedulr)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Skedulr is a high-performance, concurrent task scheduler for Go, designed for production reliability and developer efficiency. It features dynamic worker scaling, prioritized execution, retry strategies with jitter, and a powerful middleware system.

## Features

- **Web Dashboard**: Integrated real-time monitoring and management interface.
- **Distributed Coordination**: Multi-instance safety using Redis-based "Claims" and "Leases".
- **Task Dependencies**: Powerful `DependsOn` mechanism for complex execution chains.
- **Global Cancellation**: Cluster-wide `Cancel(id)` via Redis Pub/Sub.
- **Backtracking & Backpressure**: Enforceable queue limits and unique task keys.
- **Smart Retries**: Exponential backoff with random jitter to prevent "thundering herds".
- **Zero-Idle CPU**: Internal condition variables for instant reaction times with zero polling.

## Installation

```bash
# Update to the latest version with Custom ID and Shutdown stability
go get github.com/lupppig/skedulr@v1.0.8
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

// Use custom IDs for descriptive identification
s.ScheduleOnceTask(
    skedulr.NewTask(myJob, 10, 0).WithID("cleanup-logs-daily"), 
    time.Now().Add(1*time.Hour),
)
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
- **Lease Heartbeat**: Active jobs maintain ownership in Redis to prevent double-execution.
- **Cleanup Loop**: Automatically recovers orphaned tasks from crashed instances.

### Distributed Cancellation
Cancellation is global. Calling `Cancel(id)` on any node propagates the command to the instance currently running the task.
```go
// Cancels task everywhere in the cluster
s.Cancel("my-active-task-id")
```

### Task Dependencies
Build execution chains where tasks wait for their parents to complete successfully.
```go
parentID, _ := s.Submit(skedulr.NewPersistentTask("job_a", nil, 10, 0))
s.Submit(skedulr.NewPersistentTask("job_b", nil, 5, 0).DependsOn(parentID))
```

### Web Dashboard
Skedulr includes a premium, real-time updated dashboard with zero external dependencies. The UI is embedded directly into your binary.

```go
import "net/http"

// Mount the dashboard anywhere in your router
http.Handle("/skedulr/", http.StripPrefix("/skedulr", s.DashboardHandler()))
http.ListenAndServe(":8080", nil)
```

- **Live Stats**: Real-time polling of queue size, success/failure counts, and active worker count.
- **Task Management**: View all active tasks across the cluster.
- **Integrated Cancellation**: Cancel any running task directly from the UI.

### Throughput & Concurrency Controls
Prevent memory exhaustion and overlapping tasks.
```go
s := skedulr.New(
    skedulr.WithMaxCapacity(5000), // Return ErrQueueFull when reached
    skedulr.WithMaxWorkers(5),     // Fixed deterministic pool
)

// Prevent overlaps for singleton tasks
s.Submit(skedulr.NewTask(job, 10, 0).WithKey("unique-sync-key"))
```

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
// Get overall stats (JSON serializable)
stats := s.Stats() 
fmt.Printf("Queue: %d, Success: %d\n", stats.QueueSize, stats.SuccessCount)

// Get active task info
for _, task := range stats.ActiveTasks {
    fmt.Printf("ID: %s, Status: %s\n", task.ID, task.Status)
}
```

## License
MIT License. See [LICENSE](LICENSE) for details.
