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
	// 1. Initialize scheduler with Redis storage
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	s := skedulr.New(
		skedulr.WithRedisStorage(redisAddr, "", 0),
		skedulr.WithMaxWorkers(10),
		skedulr.WithWorkersForPool("critical", 4),
		skedulr.WithTaskTimeout(10*time.Second),
		// Global retry strategy for tasks that don't specify one
		skedulr.WithRetryStrategy(skedulr.NewExponentialBackoff(3, 1*time.Second, 10*time.Second, 0.1)),
	)

	// Add logging middleware
	s.Use(skedulr.Logging(nil))

	// 2. Register job types (Required for persistence and recovery)
	s.RegisterJob("email_send", func(ctx context.Context) error {
		log.Printf("[Email] Sending notification for task %s...\n", skedulr.TaskID(ctx))
		return nil
	})

	s.RegisterJob("data_process", func(ctx context.Context) error {
		id := skedulr.TaskID(ctx)
		log.Printf("[Process] Starting data processing for %s...\n", id)
		time.Sleep(2 * time.Second)

		// Demonstrate task failure
		if id == "process-fail-1" {
			return fmt.Errorf("simulated processing error")
		}
		return nil
	})

	// 3. Setup Dashboard
	// The dashboard is mounted at /skedulr/
	http.Handle("/skedulr/", s.Dashboard("/skedulr"))

	go func() {
		log.Println("🚀 Dashboard available at http://localhost:8080/skedulr/")
		if err := http.ListenAndServe(":8080", nil); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 4. Demonstrate Advanced Retry Policies & DLQ
	log.Println("🔄 Submitting task with MaxRetries (DLQ demo)")

	// This task will fail and retry 2 times before becoming DEAD
	s.Submit(
		skedulr.NewPersistentTask("data_process", nil, 10, 0).
			WithID("process-fail-1").
			WithMaxRetries(2).
			WithRetryStrategy(skedulr.NewLinearRetry(2, 5*time.Second)),
	)

	// 5. Demonstrate Workflows (DAGs)
	log.Println("🔗 Submitting workflow (Status-based triggers)")

	s.Submit(skedulr.NewPersistentTask("data_process", nil, 5, 0).WithID("base-task"))

	// Runs only on success of base-task
	s.Submit(
		skedulr.NewPersistentTask("email_send", nil, 5, 0).
			WithID("notify-success").
			OnSuccess("base-task"),
	)

	// 6. Graceful Shutdown handling
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	log.Println("\nSystem is running. Open the dashboard to see tasks in action.")
	log.Println("Dead Letter Queue tasks will appear in purple/violet.")
	<-stop

	log.Println("\nStopping scheduler cleanly...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.ShutDown(shutdownCtx)
	log.Println("👋 Finished.")
}
