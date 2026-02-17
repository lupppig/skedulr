# Skedulr

A small, idiomatic, and highly performant in-memory task scheduler for Go. Designed for simplicity, bounded concurrency, and explicit control.

## 🚀 Getting Started

### Installation
```bash
go get github.com/kehl-gopher/skedulr
```

### Basic Usage
```go
package main

import (
    "context"
    "time"
    "github.com/kehl-gopher/skedulr"
)

func main() {
    // 1. Initialize with functional options
    s := skedulr.New(
        skedulr.WithMaxWorkers(10),
        skedulr.WithTaskTimeout(5 * time.Second),
    )
    defer s.ShutDown()

    // 2. Submit a one-off task
    s.Submit(skedulr.NewTask(func(ctx context.Context) error {
        id := skedulr.TaskID(ctx) // Jobs can access their own Task ID
        println("Running task:", id)
        return nil
    }, 10, 0)) // priority 10, no override timeout

    // 3. Schedule recurring tasks
    s.ScheduleRecurring(func(ctx context.Context) error {
        return nil
    }, 1 * time.Minute, 5) // Run every minute, priority 5
}
```

## 🛠 Features

- **Bounded Concurrency**: Workers scale dynamically based on load but Nunca exceed the configured limit.
- **Priority-Based**: Tasks are executed based on a heap-implemented priority queue.
- **Middleware Support**: Wrap jobs with cross-cutting concerns (Logging, Recovery, etc.).
- **Cron Support**: Lightweight parsing for `* * * * *` syntax.
- **Retry Strategies**: Built-in support for Exponential Backoff.
- **Zero Polling**: Uses `sync.Cond` for instant task execution with zero idle CPU overhead.

## 🧱 Middleware

```go
sch := skedulr.New()
sch.Use(skedulr.Recovery(myLogger, func() {
    // custom logic on panic
}))
```

## 🔢 Stats & Status

Monitor the health of your scheduler and individual tasks:
```go
// Scheduler-wide stats
stats := s.Stats()

// Individual task status
status := s.Status(taskID) // e.g., skedulr.StatusRunning
```

## ⚖️ License
MIT
lation

---

## 📐 Architecture

+-----------------------------+
|        Task Scheduler       |
+-----------------------------+
| Priority Queue (heap)       |
| Dynamic Worker Pool         |
| Task Timeout Context        |
| Scheduled & Recurring Tasks |
+-----------------------------+
```

---

## 📌 TODO / Improvements

- [ ] Retry failed tasks with backoff
- [ ] Persist task queue to disk
- [ ] Metrics (task count, failures, etc.)
- [ ] Web dashboard for visibility
- [ ] handle running cron task

---

## 🧠 Example Use Cases

- Running background jobs (e.g., emails, billing)
- Queueing delayed tasks (e.g., notifications)
- Real-time task dispatchers with load-based scaling
