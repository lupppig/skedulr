// example demonstrates every public feature of skedulr in one runnable program.
//
// Sections:
//   1. Scheduler construction & options
//   2. Middleware
//   3. Job registration
//   4. Dashboard
//   5. Basic submission with priority, pools, dedup keys
//   6. Retries — linear and exponential backoff with Dead tasks
//   7. Workflow DAGs — OnSuccess / OnFailure / DependsOn
//   8. Delayed firing — ScheduleOnce, ScheduleRecurring, ScheduleCron (all
//      backed by the shared timing wheel)
//   9. Cancelling a scheduled task before it fires
//  10. Status polling and Stats snapshots
//  11. Pause / Resume of the dequeue loop
//  12. Dynamic worker-pool scaling
//  13. Resubmitting a Dead task programmatically
//  14. Graceful shutdown
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// ─── 1. Create the scheduler ──────────────────────────────────────────
	// Redis is required. Override the address with REDIS_ADDR if your instance
	// is not on the default localhost:6379 (the run_example.sh script spins one
	// up via Docker automatically).
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	s := skedulr.New(
		skedulr.WithRedisStorage(redisAddr, "", 0),
		skedulr.WithMaxWorkers(20),
		skedulr.WithInitialWorkers(4),
		skedulr.WithWorkersForPool("critical", 5),
		skedulr.WithWorkersForPool("background", 3),
		skedulr.WithTaskTimeout(30*time.Second),
		skedulr.WithHistoryRetention(7*24*time.Hour),
		skedulr.WithRecoveryInterval(30*time.Second),
		skedulr.WithLeaseDuration(30*time.Second),
		skedulr.WithMaxCapacity(10000),
	)

	// ─── 2. Middleware ────────────────────────────────────────────────────
	// Middleware wraps every task execution in registration order.
	s.Use(
		skedulr.Logging(nil),         // log start/finish + error
		skedulr.Recovery(nil, nil),   // catch panics and turn them into errors
	)

	// ─── 3. Register job types ────────────────────────────────────────────
	// Registered jobs survive restarts: a PersistentTask is reloaded from
	// storage by typeName and re-attached to the job function registered here.

	s.RegisterJob("send_email", func(ctx context.Context) error {
		log.Printf("[email] sending for task=%s", skedulr.TaskID(ctx))
		time.Sleep(300 * time.Millisecond)
		return nil
	})

	s.RegisterJob("process_data", func(ctx context.Context) error {
		log.Printf("[process] start task=%s", skedulr.TaskID(ctx))
		for i := 0; i <= 100; i += 20 {
			skedulr.ReportProgress(ctx, i) // visible on the dashboard
			time.Sleep(200 * time.Millisecond)
		}
		return nil
	})

	s.RegisterJob("generate_report", func(ctx context.Context) error {
		log.Println("[report] generating PDF...")
		time.Sleep(1 * time.Second)
		return nil
	})

	s.RegisterJob("always_fails", func(ctx context.Context) error {
		return fmt.Errorf("simulated failure")
	})

	s.RegisterJob("flaky", func(ctx context.Context) error {
		// Fails the first two attempts, then succeeds — exercises exponential
		// backoff without permanently filling the Dead queue.
		return fmt.Errorf("flaky attempt")
	})

	s.RegisterJob("cleanup", func(ctx context.Context) error {
		log.Println("[cleanup] running failure cleanup...")
		return nil
	})

	// ─── 4. Dashboard ─────────────────────────────────────────────────────
	// Live view of pools, history, retries, Dead tasks, and active workers.
	http.Handle("/skedulr/", s.Dashboard("/skedulr"))
	go func() {
		log.Println("dashboard: http://localhost:8080/skedulr/")
		if err := http.ListenAndServe(":8080", nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// ─── 5. Basic submission ──────────────────────────────────────────────

	// Plain priority submission.
	s.Submit(skedulr.NewPersistentTask("send_email", nil, 10, 0))

	// Routed to a specific worker pool.
	s.Submit(
		skedulr.NewPersistentTask("process_data", []byte(`{"file":"data.csv"}`), 5, 0).
			WithPool("background"),
	)

	// Deduplicated by key — only one "daily_report" runs at a time.
	s.Submit(
		skedulr.NewPersistentTask("generate_report", nil, 1, 0).
			WithKey("daily_report"),
	)

	// ─── 6. Retries & Dead tasks ──────────────────────────────────────────

	// LinearRetry: 3 attempts, 2s between each. After the third failure the
	// task becomes Dead and is visible in the dashboard for resubmission.
	deadID, _ := s.Submit(
		skedulr.NewPersistentTask("always_fails", nil, 5, 0).
			WithID("dead-demo").
			WithMaxRetries(3).
			WithRetryStrategy(skedulr.NewLinearRetry(3, 2*time.Second)),
	)

	// ExponentialBackoff: 4 attempts, doubling from 500ms, capped at 5s,
	// with 20% jitter to avoid thundering herds when many tasks share a
	// backoff cycle.
	s.Submit(
		skedulr.NewPersistentTask("flaky", nil, 5, 0).
			WithMaxRetries(4).
			WithRetryStrategy(skedulr.NewExponentialBackoff(
				4,                     // maxAttempts
				500*time.Millisecond,  // baseDelay
				5*time.Second,         // maxDelay
				0.2,                   // jitter ratio
			)),
	)

	// ─── 7. Workflow DAGs ─────────────────────────────────────────────────

	// Parent task — children below trigger off its outcome.
	s.Submit(
		skedulr.NewPersistentTask("process_data", nil, 10, 0).
			WithID("import-job"),
	)

	// Runs only after "import-job" succeeds.
	s.Submit(
		skedulr.NewPersistentTask("send_email", nil, 5, 0).
			WithID("notify-success").
			OnSuccess("import-job"),
	)

	// Runs only if "import-job" fails — useful for compensating actions.
	s.Submit(
		skedulr.NewPersistentTask("cleanup", nil, 5, 0).
			WithID("failure-cleanup").
			OnFailure("import-job"),
	)

	// Multi-parent dependency: "final-step" waits for both parents to finish
	// successfully (legacy DependsOn API; equivalent to two OnSuccess calls).
	s.Submit(
		skedulr.NewPersistentTask("send_email", nil, 1, 0).
			WithID("final-step").
			DependsOn("import-job", "notify-success"),
	)

	// ─── 8. Delayed firing (timing-wheel backed) ──────────────────────────
	// Every delayed-fire API below — ScheduleOnce, ScheduleRecurring,
	// ScheduleCron, plus retry backoff — is parked in the scheduler's shared
	// hierarchical timing wheel. Cost is O(1) goroutines total regardless of
	// how many delayed tasks are pending.

	// One-shot fire 5 seconds from now.
	s.ScheduleOnce(func(ctx context.Context) error {
		log.Println("[scheduled] one-shot fire (5s after startup)")
		return nil
	}, time.Now().Add(5*time.Second), 1)

	// Recurring fire every 10 seconds. The returned ID drives Cancel.
	healthID, _ := s.ScheduleRecurring(func(ctx context.Context) error {
		log.Println("[recurring] health check")
		return nil
	}, 10*time.Second, 1)

	// Cron expression — minute hour day month weekday. "* * * * *" fires
	// every minute on the minute. Supported syntax is "*" or comma-separated
	// literal values per field; deliberately no range/step syntax.
	cronID, err := s.ScheduleCron("* * * * *", func(ctx context.Context) error {
		log.Println("[cron] every-minute task")
		return nil
	}, 1)
	if err != nil {
		log.Fatalf("ScheduleCron: %v", err)
	}

	// ─── 9. Cancelling a pending scheduled task ───────────────────────────
	// A scheduled fire we cancel before it ever runs. Because cancellation
	// unlinks the wheel entry in O(1), the callback never enters the queue.

	cancelID, _ := s.ScheduleOnce(func(ctx context.Context) error {
		// This body is unreachable in this demo — the Cancel below removes
		// the wheel entry before the 30s mark.
		log.Println("[scheduled] unreachable (was cancelled)")
		return nil
	}, time.Now().Add(30*time.Second), 1)

	go func() {
		time.Sleep(2 * time.Second)
		if err := s.Cancel(cancelID); err != nil {
			log.Printf("cancel error: %v", err)
			return
		}
		log.Printf("[cancel] dropped scheduled task=%s before it fired", cancelID)
	}()

	// ─── 10. Status polling and Stats snapshots ──────────────────────────
	// Status returns the current TaskStatus for a given ID. Stats is the
	// scheduler-wide counter snapshot used by the dashboard.

	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(8 * time.Second)
			stats := s.Stats()
			log.Printf("[stats] queue=%d ok=%d fail=%d dead=%d workers=%d paused=%v",
				stats.QueueSize, stats.SuccessCount, stats.FailureCount,
				stats.DeadCount, stats.CurrentWorkers, stats.IsPaused)
			log.Printf("[status] dead-demo=%s", s.Status(deadID))
		}
	}()

	// ─── 11. Pause / Resume ──────────────────────────────────────────────
	// Pausing stops the dequeue loop from pulling new tasks. Already-running
	// workers finish their current task. This is useful for live database
	// migrations or maintenance windows.

	go func() {
		time.Sleep(15 * time.Second)
		log.Println("[pause] pausing dequeue loop for 5s")
		s.Pause()
		time.Sleep(5 * time.Second)
		log.Printf("[pause] resuming (was paused=%v)", s.IsPaused())
		s.Resume()
	}()

	// ─── 12. Dynamic worker-pool scaling ─────────────────────────────────
	// ScalePool adjusts the number of worker goroutines in a pool at
	// runtime. Scaling up spawns new workers immediately; scaling down lets
	// idle workers exit on their next loop iteration.

	go func() {
		time.Sleep(20 * time.Second)
		log.Println("[scale] scaling 'background' pool to 8 workers")
		s.ScalePool("background", 8)
		time.Sleep(5 * time.Second)
		log.Println("[scale] scaling 'background' pool back to 2 workers")
		s.ScalePool("background", 2)
	}()

	// ─── 13. Resubmit a Dead task ────────────────────────────────────────
	// After "dead-demo" exhausts its retries, Resubmit re-enqueues it as
	// if freshly submitted. In a real system you'd call this from an
	// operator UI / API after fixing the underlying bug.

	go func() {
		// Wait long enough for the linear retry chain (initial + 3 retries
		// at 2s spacing = ~6s of attempts) plus slack.
		time.Sleep(25 * time.Second)
		if status := s.Status(deadID); status == skedulr.StatusDead {
			log.Printf("[resubmit] task=%s is Dead, resubmitting", deadID)
			if err := s.Resubmit(deadID); err != nil {
				log.Printf("[resubmit] error: %v", err)
			}
		} else {
			log.Printf("[resubmit] task=%s is %s, skipping", deadID, status)
		}
	}()

	// ─── 14. Recurring-cancel demonstration ──────────────────────────────
	// Cancel the health-check recurring task after a minute so it doesn't
	// log forever. The cron job is also cancelled to keep the demo tidy.

	go func() {
		time.Sleep(60 * time.Second)
		log.Println("[cancel] stopping recurring health check + cron")
		s.Cancel(healthID)
		s.Cancel(cronID)
	}()

	// A tiny counter just so we have something to print at shutdown.
	var ticks int64
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			atomic.AddInt64(&ticks, 1)
		}
	}()

	// ─── 15. Graceful shutdown ───────────────────────────────────────────
	// ShutDown stops accepting new submissions, drains in-flight workers
	// (subject to the context's deadline), stops the timing wheel, and
	// closes pool channels in the right order.

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	log.Println("running. Press Ctrl+C to stop.")
	<-stop

	log.Printf("draining; %d 30s ticks elapsed during run", atomic.LoadInt64(&ticks))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ShutDown(ctx); err != nil {
		log.Printf("shutdown returned: %v", err)
	}
	log.Println("shutdown complete.")
}
