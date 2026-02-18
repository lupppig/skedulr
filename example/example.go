package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// ─── 1. Create the scheduler ──────────────────────────────────────
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	s := skedulr.New(
		skedulr.WithRedisStorage(redisAddr, "", 0),
		skedulr.WithMaxWorkers(20),
		skedulr.WithWorkersForPool("critical", 5),
		skedulr.WithWorkersForPool("background", 3),
		skedulr.WithTaskTimeout(30*time.Second),
		skedulr.WithHistoryRetention(7*24*time.Hour),
		skedulr.WithRecoveryInterval(30*time.Second),
	)

	// ─── 2. Add middleware ────────────────────────────────────────────
	s.Use(
		skedulr.Logging(nil),
		skedulr.Recovery(nil, nil),
	)

	// ─── 3. Register job types ────────────────────────────────────────

	s.RegisterJob("send_email", func(ctx context.Context) error {
		log.Printf("[Email] Sending for task %s", skedulr.TaskID(ctx))
		time.Sleep(500 * time.Millisecond)
		return nil
	})

	s.RegisterJob("process_data", func(ctx context.Context) error {
		log.Printf("[Process] Starting %s", skedulr.TaskID(ctx))
		for i := 0; i <= 100; i += 20 {
			skedulr.ReportProgress(ctx, i)
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	})

	s.RegisterJob("generate_report", func(ctx context.Context) error {
		log.Println("[Report] Generating PDF...")
		time.Sleep(1 * time.Second)
		return nil
	})

	s.RegisterJob("always_fails", func(ctx context.Context) error {
		return fmt.Errorf("simulated failure")
	})

	s.RegisterJob("cleanup", func(ctx context.Context) error {
		log.Println("[Cleanup] Running failure cleanup...")
		return nil
	})

	// ─── 4. Mount the dashboard ───────────────────────────────────────
	http.Handle("/skedulr/", s.Dashboard("/skedulr"))
	go func() {
		log.Println("Dashboard: http://localhost:8080/skedulr/")
		if err := http.ListenAndServe(":8080", nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// ─── 5. Submit tasks ──────────────────────────────────────────────

	// Basic task with priority
	s.Submit(skedulr.NewPersistentTask("send_email", nil, 10, 0))

	// Task routed to a specific worker pool
	s.Submit(
		skedulr.NewPersistentTask("process_data", []byte(`{"file":"data.csv"}`), 5, 0).
			WithPool("background"),
	)

	// Deduplicated task (same key = only one runs at a time)
	s.Submit(
		skedulr.NewPersistentTask("generate_report", nil, 1, 0).
			WithKey("daily_report"),
	)

	// ─── 6. Retry policies & dead tasks ───────────────────────────────

	// This task always fails. After 3 retries (linear, 2s apart),
	// it becomes a Dead task visible in the dashboard for manual resubmission.
	s.Submit(
		skedulr.NewPersistentTask("always_fails", nil, 5, 0).
			WithMaxRetries(3).
			WithRetryStrategy(skedulr.NewLinearRetry(3, 2*time.Second)),
	)

	// ─── 7. Workflow DAGs ─────────────────────────────────────────────

	// Parent task
	s.Submit(
		skedulr.NewPersistentTask("process_data", nil, 10, 0).
			WithID("import-job"),
	)

	// Runs only if "import-job" succeeds
	s.Submit(
		skedulr.NewPersistentTask("send_email", nil, 5, 0).
			WithID("notify-success").
			OnSuccess("import-job"),
	)

	// Runs only if "import-job" fails
	s.Submit(
		skedulr.NewPersistentTask("cleanup", nil, 5, 0).
			WithID("failure-cleanup").
			OnFailure("import-job"),
	)

	// ─── 8. Scheduled tasks ───────────────────────────────────────────

	// Run once in 5 seconds
	s.ScheduleOnce(func(ctx context.Context) error {
		log.Println("[Scheduled] One-time task executed")
		return nil
	}, time.Now().Add(5*time.Second), 1)

	// Run every 10 seconds
	s.ScheduleRecurring(func(ctx context.Context) error {
		log.Println("[Cron] Recurring health check")
		return nil
	}, 10*time.Second, 1)

	// ─── 9. Graceful shutdown ─────────────────────────────────────────
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	log.Println("Running. Press Ctrl+C to stop.")
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.ShutDown(ctx)
	log.Println("Shutdown complete.")
}
