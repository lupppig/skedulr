# Skedulr

**A horizontally-scalable background task scheduler for Go, backed by Redis.**

[![Go Reference](https://pkg.go.dev/badge/github.com/lupppig/skedulr.svg)](https://pkg.go.dev/github.com/lupppig/skedulr)
[![Go Report Card](https://goreportcard.com/badge/github.com/lupppig/skedulr)](https://goreportcard.com/report/github.com/lupppig/skedulr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

Skedulr runs background work — immediate, delayed, recurring, cron — across as many Go processes as you care to start, with no leader, no broker, and no coordinator. Every instance is identical, every cross-process decision happens inside an atomic Lua script on a shared Redis, and every deferred firing — your one-second retry, your nightly cron, your three-hour reminder — passes through a single hierarchical timing wheel running on one goroutine per process.

```bash
go get github.com/lupppig/skedulr
```

```go
s := skedulr.New(
    skedulr.WithRedisStorage("localhost:6379", "", 0),
    skedulr.WithMaxWorkers(10),
)

s.RegisterJob("send_email", func(ctx context.Context) error {
    log.Println("sending email...")
    return nil
})

s.Submit(
    skedulr.NewPersistentTask("send_email", []byte(`{"to":"user@example.com"}`), 10, 0).
        WithPool("critical").
        WithMaxRetries(3).
        WithRetryStrategy(skedulr.NewExponentialBackoff(3, 500*time.Millisecond, 10*time.Second, 0.2)),
)
```

A complete runnable demo is in [`example/example.go`](example/example.go).

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Scheduler Instance                             │
│                                                                             │
│   Submit() ──▶ ┌──────────────────┐    dequeueLoop    ┌─────────────────┐   │
│                │  Priority Queue  │ ────────────────▶ │   Worker Pools  │   │
│                │   (binary heap)  │                   │  default        │   │
│                └────────┬─────────┘                   │  critical       │   │
│                         ▲                             │  background     │   │
│                         │ fire                        └────────┬────────┘   │
│                         │                                      │            │
│                ┌────────┴───────────┐                  runTask( )           │
│                │  Hierarchical      │                  │ middleware chain   │
│                │  Timing Wheel      │                  ▼                    │
│                │  O(1) insert       │            ┌──────────────┐           │
│                │  O(1) cancel       │            │  Success     │           │
│                │  one goroutine     │            │  Retry       │           │
│                │  unbounded depth   │            │  Dead (DLQ)  │           │
│                └────────────────────┘            └──────┬───────┘           │
│                         ▲                               │                   │
│                         │ retry / delayed               ▼                   │
│                         │ hydration               resolveWorkflow()         │
│                         │                         OnSuccess / OnFailure     │
├─────────────────────────┼───────────────────────────────────────────────────┤
│                         │           Redis (Storage Interface)               │
│                         │                                                   │
│  ZSET  queue            │   ZSET  delayed   │   ZSET  history   │   DLQ     │
│  HASH  task bodies      │   KEY   leases    │   PUBSUB cancel   │   waiting │
│                         │                                                   │
│  Atomic Lua scripts: dequeue · claim · resolve-deps · pop-due-delayed       │
└─────────────────────────────────────────────────────────────────────────────┘
```

Two layers, one rule between them: Redis owns every fact that two instances might disagree about; Go owns the hot path. A submission lands in Redis, becomes durable, and a dequeue loop on some instance — whichever wins the atomic Lua pop — pulls it into a local binary heap. From there, dispatch is a heap pop and a channel send to a worker pool, with one short-held mutex between them. The worker runs the user-supplied `Job` through the middleware chain (logging, panic recovery, anything you've added), records the outcome to Redis, and either resolves dependent workflow tasks, schedules a retry on the timing wheel, or routes the task to the dead-letter queue.

Anything that fires in the future — a `ScheduleOnce`, a recurring tick, a cron firing, an exponential back-off retry — is parked in the timing wheel and persisted to a Redis sorted set as a backstop, so a process restart re-hydrates the wheel from durable state instead of forgetting the pending work.

---

## Why a timing wheel

The natural way to schedule "fire `cb` in 47 minutes" in Go is `time.AfterFunc`. It works. It also allocates a `time.Timer`, parks a goroutine, and pays an `O(log n)` heap update inside the runtime for every active timer. At a few hundred pending timers that is invisible. At fifty thousand — a not-unusual number once you mix retry back-offs with delayed user-facing reminders with cron firings — the runtime's timer heap and the per-timer goroutines become the dominant cost of running your scheduler. Cancelling a timer means searching it out of that heap; cancelling many in quick succession is where the latency tail lives.

A timing wheel inverts the trade. Instead of one ordered data structure of timers, you keep a ring of buckets where each bucket covers a fixed quantum of wall-clock time — say, ten milliseconds. To schedule an entry for `t`, you index into bucket `(t / tickMs) mod wheelSize` and append. To cancel an entry, you flip a flag. Both operations are O(1) and allocation-free in the steady state. The cost you pay is bounded precision: every entry fires at the bucket boundary, not at its exact requested instant, so the wheel is the wrong choice when you need microsecond accuracy. Skedulr's bottom level uses a 10 ms tick, which is below the noise floor of any reasonable retry policy or user-facing reminder.

The naive wheel only handles delays up to `tickMs × wheelSize`. Skedulr handles arbitrary delays the way Kafka does: a **hierarchical timing wheel**. The bottom level covers a few seconds of span at 10 ms resolution. When an insert's deadline exceeds that span, it escalates into an overflow level whose tick is one full rotation of the level below it. That level itself has an overflow. The hierarchy is allocated lazily — most workloads never produce a delay long enough to allocate beyond level two — and unbounded in depth, so "schedule this for next Tuesday" works without special-casing. When the bottom level's clock advances past an upper-level bucket's deadline, the bucket is drained and its entries are demoted into the now-shorter level, eventually landing in the bottom and firing. An entry written for next Tuesday and an entry written for next millisecond pass through exactly the same code path; only the number of demotions differs.

One goroutine drives the entire hierarchy. The buckets themselves are members of a min-heap (`delayQueue`) keyed by their next-due time, and the driver goroutine — `tickLoop` — sleeps on a single `time.Timer` set to the head of that heap. When a bucket comes due, it is flushed; bottom-level entries dispatch, upper-level entries re-insert into the wheel one rung down. The cost of N pending entries to the Go runtime is one goroutine and one timer, regardless of N. That is the property that makes the wheel worth the implementation complexity over `time.AfterFunc`: throughput, cancel cost, and memory footprint all decouple from how much deferred work is outstanding.

Two implementation details matter enough to surface here:

**Entries are intrusive linked-list nodes carrying a back-pointer to their bucket.** That makes `Cancel(taskID)` an O(1) unlink. The cancelled flag is atomic, so cancellation races safely against the tick loop without a lock — every fire path re-checks the flag before invoking the callback, so a callback whose entry was cancelled mid-flight never runs.

**Callbacks run on a fresh goroutine, never on the tick loop.** A handler that blocks for a second would otherwise stall every other scheduled firing in the process. Dispatching on a goroutine costs an allocation per fire; on the workloads Skedulr targets, that is several orders of magnitude cheaper than the latency a stalled clock would produce.

The wheel is in-process and forgets pending entries when the process exits. Durability comes from above: every delayed task is also written to a Redis sorted set scored by its fire time, and a fresh Scheduler instance hydrates its wheel from that set on startup. The wheel is the hot cache; Redis is the source of truth.

This is the same trade — atomic in-memory operations backed by an external log — that makes the rest of the system work, just specialized to time.

---

## How the cluster coordinates

Every Skedulr process is symmetric. There is no leader, no membership protocol, no broker. You add capacity by starting another process; you remove it by stopping one. The cluster has exactly as much capacity as the union of its workers.

The operations that must be globally serialized — dequeueing a task, claiming a lease, popping every due delayed task, resolving a workflow dependency — are pushed into Lua scripts that execute atomically inside Redis. The same task is never delivered to two instances, and a delayed task that becomes due is handed to exactly one caller across the cluster, because Redis enforces it; Skedulr never holds a cross-process lock from Go. Cancellation propagates through a Pub/Sub channel — `Cancel(id)` publishes, every instance subscribes, and the instance currently running the task observes the message and cancels its `context.Context`.

Crash recovery falls out of the same design. Every running task holds a Redis lease refreshed by a heartbeat. When a process dies, its leases expire after `WithLeaseDuration` (30 s by default), and the next recovery scan on any surviving instance returns the abandoned tasks to the queue. Delayed and recurring tasks are unaffected by a crash because their state lives in the `delayed` sorted set; any instance that restarts re-arms its timing wheel directly from Redis without consulting the original submitter.

The honest version of the semantics is: at-most-once **dispatch** per dequeue, exactly-once **delivery** of due-delayed tasks across instances, at-least-once **execution** under crashes. A worker that runs a side effect and then dies before releasing its lease will see the task redelivered. Handlers should be idempotent. This is the trade — atomic Lua and lease keys instead of a consensus protocol — that lets the system scale by adding stateless workers behind Redis, and it is stated up front because the trade is the whole design.

---

## What you get from the API

`Submit`, `ScheduleOnce`, `ScheduleRecurring`, and `ScheduleCron` cover the four shapes of work. Tasks carry a priority used by the in-process heap, target a named worker pool (`default`, `critical`, `background`, or anything you scale into existence with `WithWorkersForPool` and `ScalePool`), and can be wired into workflow DAGs via `DependsOn`, `OnSuccess`, and `OnFailure`. Retries are configured per task with `WithMaxRetries` and `WithRetryStrategy` — `LinearRetry` and `NewExponentialBackoff(maxAttempts, base, cap, jitter)` ship in the box. A task that exhausts its retries lands in the dead-letter queue and is resubmittable via `Resubmit(id)` or the dashboard. The whole scheduler pauses and resumes (`Pause`/`Resume`) and pools scale independently (`ScalePool`).

Middleware is a `func(Job) Job` chain — the same shape as `net/http` — so `Logging`, `Recovery`, and anything you write compose without ceremony.

A dashboard ships with the binary. One handler, embedded HTML/CSS/JS via `go:embed`, live counters and history filters and pool controls and dead-task resubmission. Mount it where you like:

```go
http.Handle("/skedulr/", s.Dashboard("/skedulr"))
```

---

## When Skedulr is not the right fit

Skedulr is local to one Redis deployment, so it is not the tool for cross-datacenter consensus or strong global ordering. It is a library rather than a hosted service, so it is not the tool when you want a separate broker process to manage independently. And because execution is at-least-once under crashes, it is not the tool when your handlers cannot be made idempotent.

For everything else — a Go application that needs durable, prioritized, retryable background work and is willing to operate Redis — Skedulr aims to be the smallest thing that does the job correctly.

---

## License

MIT. See [LICENSE](LICENSE).
