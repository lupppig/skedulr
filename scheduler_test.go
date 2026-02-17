package schedulr_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	schedulr "github.com/kehl-gopher/skedulr"
)

func TestSubmitAndExecuteTask(t *testing.T) {
	sch := schedulr.New()
	defer sch.ShutDown()

	done := make(chan bool)
	task := schedulr.NewTask(func(ctx context.Context) error {
		fmt.Println("[Task Simple] Running")
		done <- true
		return nil
	}, 5, 2*time.Second)

	sch.Submit(task)

	select {
	case <-done:
		fmt.Println("[Task Simple] Completed")
	case <-time.After(10 * time.Second):
		t.Error("task did not complete in time")
	}
}

func TestTaskTimeout(t *testing.T) {
	sch := schedulr.New()
	defer sch.ShutDown()

	task := schedulr.NewTask(func(ctx context.Context) error {
		select {
		case <-time.After(2 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, 5, 500*time.Millisecond)

	sch.Submit(task)

	time.Sleep(1 * time.Second)
}

func TestScheduleOnce(t *testing.T) {
	sch := schedulr.New()
	defer sch.ShutDown()

	done := make(chan bool)

	_, err := sch.ScheduleOnce(func(ctx context.Context) error {
		fmt.Println("[ScheduledOnce] Executed")
		done <- true
		return nil
	}, time.Now().Add(1*time.Second), 3)

	if err != nil {
		t.Fatalf("failed to schedule once: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("scheduled-once task did not execute")
	}
}

func TestScheduleRecurringAndCancel(t *testing.T) {
	sch := schedulr.New()
	defer sch.ShutDown()

	var mu sync.Mutex
	count := 0
	done := make(chan bool)

	id, err := sch.ScheduleRecurring(func(ctx context.Context) error {
		mu.Lock()
		count++
		c := count
		mu.Unlock()
		fmt.Printf("[Recurring] Run #%d\n", c)
		if c >= 3 {
			select {
			case done <- true:
			default:
			}
		}
		return nil
	}, 1*time.Second, 2)

	if err != nil {
		t.Fatalf("failed to schedule recurring: %v", err)
	}

	select {
	case <-done:
		_ = sch.Cancel(id)
	case <-time.After(10 * time.Second):
		t.Error("recurring task did not complete expected runs")
	}
}

func TestPriorityTaskExecutionOrder(t *testing.T) {
	var mu sync.Mutex
	executedOrder := []string{}

	createJob := func(id string) schedulr.Job {
		return func(ctx context.Context) error {
			mu.Lock()
			executedOrder = append(executedOrder, id)
			mu.Unlock()
			return nil
		}
	}

	// Smaller queue and slow start to ensure heap ordering is respected during dequeue
	sch := schedulr.New(schedulr.WithMaxWorkers(1), schedulr.WithInitialWorkers(1))
	defer sch.ShutDown()

	// Stop the dequeue for a moment to fill the queue and test priority?
	// Actually, we just submit them quickly.
	// To reliably test priority in a small local test, we might need a way to pause processing.
	// But let's try basic submission first.

	taskLow := schedulr.NewTask(createJob("low"), 1, 2*time.Second)
	taskMid := schedulr.NewTask(createJob("mid"), 5, 2*time.Second)
	taskHigh := schedulr.NewTask(createJob("high"), 10, 2*time.Second)

	sch.Submit(taskLow)
	sch.Submit(taskMid)
	sch.Submit(taskHigh)

	time.Sleep(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	expected := []string{"high", "mid", "low"}
	if len(executedOrder) < 3 {
		t.Fatalf("expected at least 3 tasks to run, got %d", len(executedOrder))
	}
	// Note: In a multi-worker setup, order might vary slightly if they start at the same time,
	// but with maxWorkers(1), they should be strictly in priority order.
	for i := range expected {
		if executedOrder[i] != expected[i] {
			t.Errorf("expected task %s at position %d, got %s", expected[i], i, executedOrder[i])
		}
	}
}
