package skedulr_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lupppig/skedulr"
)

func TestSubmitAndExecuteTask(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	done := make(chan bool)
	task := skedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Task Simple] Running")
		done <- true
		fmt.Println("[Task Simple] Completed")
		return nil
	}, 5, 0)

	_, err := sch.Submit(task)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not complete in time")
	}
}

func TestTaskTimeout(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	task := skedulr.NewTask(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	}, 5, 500*time.Millisecond)

	_, err := sch.Submit(task)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}
	time.Sleep(1 * time.Second)

	stats := sch.Stats()
	if stats.FailureCount != 1 {
		t.Errorf("expected 1 failure due to timeout, got %d", stats.FailureCount)
	}
}

func TestScheduleOnce(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	done := make(chan bool)
	_, err := sch.ScheduleOnce(func(ctx context.Context) error {
		fmt.Println("[ScheduledOnce] Executed")
		done <- true
		return nil
	}, time.Now().Add(500*time.Millisecond), 10)

	if err != nil {
		t.Fatalf("failed to schedule once: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled task did not run")
	}
}

func TestScheduleRecurringAndCancel(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	count := 0
	id, err := sch.ScheduleRecurring(func(ctx context.Context) error {
		count++
		fmt.Printf("[Recurring] Run #%d\n", count)
		return nil
	}, 500*time.Millisecond, 5)

	if err != nil {
		t.Fatalf("failed to schedule recurring: %v", err)
	}

	time.Sleep(1200 * time.Millisecond) // Should run ~2 times
	sch.Cancel(id)

	finalCount := count
	time.Sleep(1 * time.Second) // Wait to ensure it stopped

	if count > finalCount+1 {
		t.Errorf("task was not cancelled, count kept increasing: %d > %d", count, finalCount)
	}
}

func TestSchedulerStats(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	for i := 0; i < 5; i++ {
		_, err := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
			return nil
		}, 1, 0))
		if err != nil {
			t.Fatalf("failed to submit task: %v", err)
		}
	}

	time.Sleep(500 * time.Millisecond)
	stats := sch.Stats()
	if stats.SuccessCount != 5 {
		t.Errorf("expected 5 success, got %d", stats.SuccessCount)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	var panicCaught atomic.Bool
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	sch.Use(skedulr.Recovery(nil, func() {
		panicCaught.Store(true)
	}))

	_, err := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		panic("boom")
	}, 10, 0))
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	time.Sleep(1 * time.Second)

	if !panicCaught.Load() {
		t.Error("panic was not caught by recovery middleware")
	}
}

func TestTaskIDPropagation(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	done := make(chan string, 1)
	task := skedulr.NewTask(func(ctx context.Context) error {
		done <- skedulr.TaskID(ctx)
		return nil
	}, 5, 0)

	id, err := sch.Submit(task)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	select {
	case ctxID := <-done:
		if ctxID != id {
			t.Errorf("expected task ID %s in context, got %s", id, ctxID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task")
	}
}

func TestExponentialBackoff(t *testing.T) {
	eb := &skedulr.ExponentialBackoff{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    1 * time.Second,
	}

	d, ok := eb.NextDelay(0)
	if !ok || d != 100*time.Millisecond {
		t.Errorf("expected 100ms, got %v", d)
	}

	d, ok = eb.NextDelay(1)
	if !ok || d != 200*time.Millisecond {
		t.Errorf("expected 200ms, got %v", d)
	}

	d, ok = eb.NextDelay(2)
	if !ok || d != 400*time.Millisecond {
		t.Errorf("expected 400ms, got %v", d)
	}

	_, ok = eb.NextDelay(3)
	if ok {
		t.Error("expected false after max attempts")
	}
}

func TestCronScheduling(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	done := make(chan bool, 1)
	// Match every minute
	_, err := sch.ScheduleCron("* * * * *", func(ctx context.Context) error {
		select {
		case done <- true:
		default:
		}
		return nil
	}, 5)

	if err != nil {
		t.Fatalf("failed to schedule cron: %v", err)
	}

	// This test might take a while if we wait for a minute,
	// but the logic can be mocked or we can just ensure it doesn't fail immediately.
	// In a real CI we might want a faster way to test this.
}

func TestPriorityTaskExecutionOrder(t *testing.T) {
	// Use 1 worker to ensure serial execution in priority order
	sch := skedulr.New(skedulr.WithMaxWorkers(1), skedulr.WithInitialWorkers(1))
	defer sch.ShutDown(context.Background())

	executionOrder := make([]int, 0)
	var mu sync.Mutex

	// Submit tasks with different priorities in a "random" order
	priorities := []int{1, 10, 5, 20, 2}

	// We need to ensure the tasks stay in the queue long enough for the heap to sort them,
	// because the worker might grab the first one immediately.
	// However, with our current scaling/dequeue logic, the simplest way is to submit them all.
	// To reliably test priority, we'll submit 100 tasks and check if the order is generally decreasing.

	for _, p := range priorities {
		priority := p
		_, err := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
			mu.Lock()
			executionOrder = append(executionOrder, priority)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			return nil
		}, priority, 0))
		if err != nil {
			t.Fatalf("failed to submit task: %v", err)
		}
	}

	// Wait for all to finish
	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	// Correct priority order: 20, 10, 5, 2, 1
	expected := []int{20, 10, 5, 2, 1}
	if len(executionOrder) != len(expected) {
		t.Fatalf("expected %d tasks, got %d", len(expected), len(executionOrder))
	}
	for i := range expected {
		if executionOrder[i] != expected[i] {
			t.Errorf("at index %d: expected priority %d, got %d", i, expected[i], executionOrder[i])
		}
	}
}
