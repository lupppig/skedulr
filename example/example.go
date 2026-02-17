package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// 1. Initialize scheduler
	s := skedulr.New(
		skedulr.WithMaxWorkers(5),
		skedulr.WithTaskTimeout(30*time.Second),
		skedulr.WithJob("long_job", func(ctx context.Context) error {
			id := skedulr.TaskID(ctx)
			fmt.Printf("[Work] Starting long job %s...\n", id)
			for i := 0; i < 10; i++ {
				select {
				case <-ctx.Done():
					fmt.Printf("[Work] Job %s cancelled/stopping!\n", id)
					return ctx.Err()
				case <-time.After(1 * time.Second):
					progress := (i + 1) * 10
					fmt.Printf("[Work] Job %s progress: %d%%\n", id, progress)
					skedulr.ReportProgress(ctx, progress)
				}
			}
			fmt.Printf("[Work] Job %s completed.\n", id)
			return nil
		}),
	)

	// Add logging middleware
	s.Use(skedulr.Logging(nil))

	// 2. Setup HTTP Server for Dashboard
	srv := &http.Server{
		Addr:    ":8080",
		Handler: s.DashboardHandler(),
	}

	go func() {
		fmt.Println("🚀 Dashboard starting at http://localhost:8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Dashboard failed: %v\n", err)
		}
	}()

	// 3. Submit several tasks with descriptive IDs to populate the dashboard
	s.Submit(skedulr.NewPersistentTask("long_job", nil, 10, 0).WithID("initial-sync-01"))
	s.Submit(skedulr.NewPersistentTask("long_job", nil, 5, 0).WithID("cleanup-logs-weekly"))
	s.Submit(skedulr.NewPersistentTask("long_job", nil, 1, 0).WithID("index-refresh-task"))

	// Schedule a recurring task with a descriptive ID
	s.ScheduleRecurringTask(
		skedulr.NewTask(func(ctx context.Context) error {
			fmt.Println("⏰ Heartbeat task running...")
			return nil
		}, 10, 0).WithID("system-heartbeat"),
		1*time.Minute,
	)

	fmt.Println("Open http://localhost:8080 to see the dashboard.")
	fmt.Println("Press Ctrl+C to trigger graceful shutdown.")

	// 4. Graceful Shutdown Implementation
	// Create a channel to listen for OS signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Wait for signal
	<-stop
	fmt.Println("\n Shutdown signal received. Starting graceful shutdown...")

	// Create a deadline for the shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shut down HTTP server first (stops new requests)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("HTTP shutdown error: %v\n", err)
	} else {
		fmt.Println("✓ HTTP server stopped")
	}

	// Shut down Scheduler (waits for active workers)
	if err := s.ShutDown(shutdownCtx); err != nil {
		fmt.Printf("Scheduler shutdown error: %v\n", err)
	} else {
		fmt.Println("✓ Scheduler stopped (all workers finished)")
	}

	fmt.Println("👋 Shutdown complete.")
}
