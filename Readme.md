# Skedulr

[![Go Reference](https://pkg.go.dev/badge/github.com/lupppig/skedulr.svg)](https://pkg.go.dev/github.com/lupppig/skedulr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

Skedulr is a concurrent task scheduler for Go, built for production environments that need more than just a basic worker pool. It handles the "hard parts" of scheduling—retries, persistence, and distributed coordination—without adding massive complexity to your codebase.

## Why Skedulr?

- **Real reliability**: Handles retries with exponential backoff and jitter out of the box.
- **Persistence**: Optionally back your tasks with Redis so they survive app restarts.
- **Distributed native**: Safely run multiple instances of your app with atomic task claiming, leases, and heartbeat protection.
- **Persistent History**: Task history is now durable in Redis with configurable retention (sorted set indexing).
- **Production Tuned**: Silences external dependency logs (like Redis) by default to keep your production console clean.
- **Visuals**: Comes with a real-time dashboard so you can actually see what your workers are doing.
- **Zero-poll engine**: Uses condition variables internally, meaning zero idle CPU usage while still reacting instantly to new tasks.

## Installation

```bash
# Pull the latest stable version
go get github.com/lupppig/skedulr
```

## Quick Start

Getting a scheduler up and running takes about ten lines of code.

```go
package main

import (
    "context"
    "time"
    "github.com/lupppig/skedulr"
)

func main() {
    // 1. Fire up the scheduler
    s := skedulr.New(
        skedulr.WithMaxWorkers(10),
        skedulr.WithTaskTimeout(5 * time.Second),
    )
    
    // 2. Define your work
    job := func(ctx context.Context) error {
        // Your logic here...
        return nil
    }

    // 3. Queue it up
    s.Submit(skedulr.NewTask(job, 10, 0))

    // 4. Graceful shutdown
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    s.ShutDown(ctx)
}
```

## Practical Features

### Distributed Coordination
If you scale your app to multiple containers, Skedulr ensures only one instance runs a specific persistent task at a time. It uses a lease system with heartbeats to recover tasks if a node crashes mid-job.

### Dashboards that actually help
Instead of guessing why your queue is slow, mount the built-in dashboard to any HTTP path. It shows live stats, success/failure counts, and lets you cancel tasks manually.

```go
// Mount to your existing router
http.Handle("/debug/skedulr/", http.StripPrefix("/debug/skedulr", s.DashboardHandler()))
```

### Persistence (Redis)
Persistent tasks solve the "lost work" problem during deployments. Register your job functions, and Skedulr will reload them from Redis on startup.

```go
sch := skedulr.New(
    skedulr.WithRedisStorage("localhost:6379", "", 0),
    skedulr.WithJob("send_email", myEmailFunc),
)

// This task is now safe from restarts
sch.Submit(skedulr.NewPersistentTask("send_email", payload, 10, 0))
```

### Retries & Backpressure
- **Exponential Backoff**: Prevent "thundering herd" issues with randomized jitter.
- **Queue Limits**: Use `WithMaxCapacity` to prevent memory exhaustion when your system is overloaded.
- **Singleton Tasks**: Prevent overlapping executions of the same job using `WithKey`.

## Monitoring

You can pull stats programmatically for your own Prometheus/Grafana export:
```go
stats := s.Stats() 
fmt.Printf("Queued: %d, Workers: %d\n", stats.QueueSize, stats.CurrentWorkers)
```

## License
MIT. Use it freely for whatever you're building.
