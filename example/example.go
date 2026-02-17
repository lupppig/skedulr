package main

import (
	"context"
	"fmt"
	"time"

	"github.com/kehl-gopher/skedulr"
)

func main() {
	// Initialize scheduler with custom options
	// The package name is 'skedulr' inside the 'skedulr' module
	s := skedulr.New(
		skedulr.WithMaxWorkers(5),
		skedulr.WithTaskTimeout(10*time.Second),
	)
	defer s.ShutDown()

	// 1. Submit a simple task
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("Running a one-off task")
		return nil
	}, 10, 0))

	// 2. Schedule a task once
	s.ScheduleOnce(func(ctx context.Context) error {
		fmt.Println("Delayed task executed")
		return nil
	}, time.Now().Add(2*time.Second), 5)

	// 3. Schedule a recurring task
	s.ScheduleRecurring(func(ctx context.Context) error {
		fmt.Println("Recurring heartbeat...")
		return nil
	}, 1*time.Second, 1)

	// Wait for a bit to see it in action
	time.Sleep(5 * time.Second)
}
