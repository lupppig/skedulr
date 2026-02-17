package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lupppig/skedulr"
)

func main() {
	// 1. Initialize scheduler with Redis persistence and Job Registry
	s := skedulr.New(
		skedulr.WithMaxWorkers(5),
		skedulr.WithInitialWorkers(2),
		skedulr.WithTaskTimeout(5*time.Second),
		// Fallback to InMemoryStorage if no Redis is configured,
		// but let's show how to register for persistence
		skedulr.WithJob("greet", func(ctx context.Context) error {
			fmt.Println("Hello from persistent task!")
			return nil
		}),
	)

	// Add professional middleware
	s.Use(skedulr.Recovery(nil, nil))
	s.Use(skedulr.Logging(nil))

	// 2. Submit a persistent task
	// This task would survive a process restart if Redis were configured.
	id, err := s.Submit(skedulr.NewPersistentTask("greet", nil, 10, 0))
	if err != nil {
		fmt.Printf("Error submitting task: %v\n", err)
	} else {
		fmt.Printf("Persistent task %s submitted\n", id)
	}

	// 3. Submit a regular in-memory task
	s.Submit(skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("In-memory task execution")
		return nil
	}, 1, 0))

	// Allow some time for execution
	time.Sleep(2 * time.Second)

	// 4. Graceful Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.ShutDown(ctx)
}
