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
	// 1. Initialize scheduler with Redis storage
	// We use localhost:6379 because run_example.sh maps the container port
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	s := skedulr.New(
		skedulr.WithStorage(skedulr.NewRedisStorage(redisAddr, "", 0, "skedulr:example:")),
		skedulr.WithMaxWorkers(10),
		skedulr.WithWorkersForPool("critical", 2),
		skedulr.WithTaskTimeout(10*time.Second),
	)

	// Add logging middleware to see what's happening
	s.Use(skedulr.Logging(nil))

	// 2. Register job types (Required for persistent tasks)
	s.RegisterJob("email_send", func(ctx context.Context) error {
		fmt.Printf("[Email] Sending notification to %s...\n", skedulr.TaskID(ctx))
		time.Sleep(1 * time.Second)
		return nil
	})

	s.RegisterJob("data_process", func(ctx context.Context) error {
		id := skedulr.TaskID(ctx)
		fmt.Printf("[Process] Starting data processing for %s...\n", id)
		for i := 1; i <= 5; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				skedulr.ReportProgress(ctx, i*20)
				fmt.Printf("[Process] %s progress: %d%%\n", id, i*20)
			}
		}
		return nil
	})

	// 3. Setup Dashboard
	srv := &http.Server{
		Addr:    ":8080",
		Handler: s.DashboardHandler(),
	}

	go func() {
		fmt.Println("🚀 Dashboard available at http://localhost:8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Dashboard error: %v\n", err)
		}
	}()

	// 4. Demonstrate Features

	// A. Dependency Chain
	fmt.Println("🔗 Submitting dependency chain: Parent -> Child")
	parentID, _ := s.Submit(skedulr.NewPersistentTask("email_send", nil, 10, 0).WithID("parent-task"))
	s.Submit(skedulr.NewPersistentTask("data_process", nil, 5, 0).
		WithID("child-task").
		DependsOn(parentID))

	// B. Singleton Task (Key-based overlap prevention)
	fmt.Println("🔑 Submitting singleton task (will prevent overlaps with same key)")
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Key] Running singleton task...")
		time.Sleep(3 * time.Second)
		return nil
	}, 10, 0).WithKey("unique-report-gen"))

	// C. Custom Worker Pool
	fmt.Println("🏊 Submitting high-priority task to 'critical' pool")
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Critical] Running in specialized pool...")
		return nil
	}, 100, 0).WithPool("critical"))

	// D. Recurring & Cron
	fmt.Println("⏰ Scheduling heartbeat and cron tasks")
	s.ScheduleRecurringTask(
		skedulr.NewTask(func(ctx context.Context) error {
			fmt.Println("💓 System heartbeat...")
			return nil
		}, 10, 0),
		30*time.Second,
	)

	// 5. Graceful Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	fmt.Println("\nExample is running. Press Ctrl+C to stop.")
	<-stop

	fmt.Println("\nStopping everything cleanly...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv.Shutdown(shutdownCtx)
	s.ShutDown(shutdownCtx)
	fmt.Println("👋 Finished.")
}
