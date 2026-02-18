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
	// 1. Setup the Scheduler
	// We use Redis for persistence so tasks survive restarts.
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	s := skedulr.New(
		skedulr.WithRedisStorage(redisAddr, "", 0),
		skedulr.WithMaxWorkers(10),
		skedulr.WithWorkersForPool("critical", 2), // Reserved workers for high-priority stuff
	)

	// Add logging so we can see what's happening in the console
	s.Use(skedulr.Logging(nil))

	// 2. Register "Job Types"
	// This tells the scheduler how to run tasks saved in Redis.
	s.RegisterJob("email_notify", func(ctx context.Context) error {
		fmt.Printf("[Email] 📧 Sending update to user for task: %s\n", skedulr.TaskID(ctx))
		return nil
	})

	s.RegisterJob("data_crunch", func(ctx context.Context) error {
		id := skedulr.TaskID(ctx)
		fmt.Printf("[Crunch] 🔢 Processing numbers for %s...\n", id)
		time.Sleep(2 * time.Second)

		// We'll make this specific task fail to show error handling
		if id == "crunch-fail" {
			return fmt.Errorf("database timeout")
		}
		return nil
	})

	s.RegisterJob("cleanup_retry", func(ctx context.Context) error {
		fmt.Printf("[Recovery] 🛠️ Parent task failed! Running cleanup for: %s\n", skedulr.TaskID(ctx))
		return nil
	})

	// 3. Start the Dashboard
	// You can see everything at http://localhost:8080
	go func() {
		fmt.Println("🚀 Dashboard is live at http://localhost:8080")
		http.ListenAndServe(":8080", s.DashboardHandler())
	}()

	// 4. SHOWCASE: Advanced Workflows (DAGs)
	fmt.Println("\n--- Starting Workflows ---")

	// CASE A: Success Path
	// 'Success-Child' will ONLY run if 'Main-Task' finishes without errors.
	s.Submit(skedulr.NewTask(nil, 1, 0).WithID("Main-Task").WithTypeName("data_crunch"))
	s.Submit(skedulr.NewTask(nil, 1, 0).WithID("Success-Child").WithTypeName("email_notify").OnSuccess("Main-Task"))

	// CASE B: Failure Path (Error Handling)
	// 'Recovery-Task' will ONLY run if 'crunch-fail' errors out.
	s.Submit(skedulr.NewTask(nil, 1, 0).WithID("crunch-fail").WithTypeName("data_crunch"))
	s.Submit(skedulr.NewTask(nil, 1, 0).WithID("Recovery-Task").WithTypeName("cleanup_retry").OnFailure("crunch-fail"))

	// 5. SHOWCASE: Other Features

	// Singleton Tasks: Only one "Weekly-Report" can run at a time.
	fmt.Println("🔑 Submitting singleton task (keys prevent overlap)")
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Key] Running a unique task...")
		time.Sleep(3 * time.Second)
		return nil
	}, 10, 0).WithKey("unique-gen-report"))

	// Worker Pools: High priority task in a specific pool.
	fmt.Println("🏊 Submitting task to 'critical' worker pool")
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Critical] High priority work finished!")
		return nil
	}, 100, 0).WithPool("critical"))

	// Cron/Recurring: Run every 30 seconds.
	s.ScheduleRecurringTask(
		skedulr.NewTask(func(ctx context.Context) error {
			fmt.Println("💓 Pulse check...")
			return nil
		}, 1, 0),
		30*time.Second,
	)

	// 6. Wait for user to stop the app
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	fmt.Println("\nExample is running. Check the dashboard! Press Ctrl+C to exit.")
	<-stop

	fmt.Println("\nCleaning up...")
	s.ShutDown(context.Background())
	fmt.Println("Done. 👋")
}
