# Skedulr

[![Go Reference](https://pkg.go.dev/badge/github.com/lupppig/skedulr.svg)](https://pkg.go.dev/github.com/lupppig/skedulr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

**Skedulr** is a high-performance, production-grade background task scheduler for Go. 
It’s built for reliability, horizontal scale, and operational visibility — ensuring your tasks execute correctly even across restarts, crashes, or multiple application instances.

---

## Why Skedulr?

### 🛡️ Zero Task Loss
With Redis-backed persistence and **Reliable Queuing**, tasks are never lost if a worker crashes. Lease-based heartbeats ensure abandoned tasks are automatically recovered.

### 📈 Dynamic Scaling
Adjust worker pool counts in real-time. Use the built-in dashboard to scale your processing power up or down without restarts.

### 🕸️ Complex Workflows (DAGs)
Create task chains with conditional triggers:
- Run `Cleanup` only if `VideoEncode` **fails**.
- Run `NotifyUser` only if `Payment` **succeeds**.

### 🔄 Advanced Retries & DLQ
Configurable per-task retry limits. Tasks that exceed their limit are moved to a **Dead Letter Queue (DLQ)** for manual inspection and resubmission.

---

## Installation

```bash
go get github.com/lupppig/skedulr
```

---

## Production Setup Example

This example demonstrates a full production setup with Redis persistence, multiple worker pools, and a mounted dashboard.

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// 1. Initialize Scheduler with Redis persistence
	s := skedulr.New(
		skedulr.WithRedisStorage("localhost:6379", "", 0),
		skedulr.WithMaxWorkers(10), // Base capacity
		skedulr.WithWorkersForPool("critical", 5), // Dedicated pool
		skedulr.WithRetryStrategy(skedulr.NewExponentialBackoff(3, 1*time.Second, 10*time.Second, 0.1)),
	)

	// 2. Register Jobs (Required for Persistence/Recovery)
	s.RegisterJob("email_sender", func(ctx context.Context) error {
		// Access task metadata or payload
		log.Printf("Sending email for task %s...", skedulr.GetTaskID(ctx))
		return nil
	})

	// 3. Mount the Operations Dashboard
	http.Handle("/skedulr/", s.Dashboard("/skedulr"))
	go http.ListenAndServe(":8080", nil)

	// 4. Submit Persistent Tasks with Advanced Policies
	s.Submit(
		skedulr.NewPersistentTask("email_sender", []byte(`{"to": "user@example.com"}`), 10, 30*time.Second).
			WithPool("critical").      // Route to dedicated workers
			WithMaxRetries(5).         // Move to DLQ after 5 failures
			WithKey("welcome_email_1"). // Prevent duplicate execution
	)

	// 5. Build a Workflow (DAG)
	s.Submit(
		skedulr.NewPersistentTask("cleanup", nil, 1, 0).
			OnFailure("important-task-id"), // Only runs if parent fails
	)

	// Block until signal
	select {}
}
```

---

## Core Concepts

### Reliable Queueing
Skedulr uses a "Processing Set" and Lease system. When a worker claims a task, it's moved from the queue to the processing set. If the worker disappears without finishing, another instance will recover the task after the lease expires.

### Dead Letter Queue (DLQ)
When a task fails more than its `MaxRetries`, it is marked as `Dead`.
- **Visibility**: The dashboard highlights dead tasks in a specialized view.
- **Resubmission**: Manually resubmit dead tasks from the dashboard after fixing the underlying issue (e.g., faulty API or DB).

### Directed Acyclic Graphs (DAG)
Submit tasks that depend on others. Trigger conditions include:
- `OnSuccess(parentID)`
- `OnFailure(parentID)`

---

## Monitoring Dashboard

Skedulr comes with a premium real-time dashboard. 
- **Search & Filter**: Find tasks by ID, Type, or Status (Queued, Running, Succeeded, Failed, Dead).
- **Scale Control**: Live-scale worker pools using sliders.
- **DLQ Management**: Inspect failures and resubmit dead tasks with one click.

---

## Benchmark & Performance
Skedulr is designed for high-throughput environments, utilizing Go's concurrency primitives and optimized Lua scripts for Redis operations to minimize network round-trips.

---

## License
MIT License.
