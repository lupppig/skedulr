package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// 1. Initialize scheduler with options and middleware
	s := skedulr.New(
		skedulr.WithMaxWorkers(5),
		skedulr.WithTaskTimeout(2*time.Second),
		skedulr.WithRetryStrategy(&skedulr.ExponentialBackoff{
			MaxAttempts: 3,
			BaseDelay:   100 * time.Millisecond,
		}),
	)

	// Add professional middleware
	s.Use(skedulr.Recovery(nil, nil))
	s.Use(skedulr.Logging(nil)) // nil logger goes to noop by default, but you get the idea

	// 2. Submit a one-off task with ID retrieval
	id, _ := s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		taskID := skedulr.TaskID(ctx)
		fmt.Printf("Processing task: %s\n", taskID)
		return nil
	}, 10, 0))
	fmt.Printf("Task %s submitted\n", id)

	// 3. Recurring task
	s.ScheduleRecurring(func(ctx context.Context) error {
		fmt.Println("Recurring heartbeat...")
		return nil
	}, 1*time.Second, 1)

	// Wait for a bit to see it in action
	time.Sleep(5 * time.Second)
}
