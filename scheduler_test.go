package skedulr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// Submit a blocker task to ensure subsequent tasks queue up
	blockerDone := make(chan bool)
	_, _ = sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		time.Sleep(500 * time.Millisecond)
		blockerDone <- true
		return nil
	}, 100, 0))

	// Give it a moment to be picked up by the worker
	time.Sleep(50 * time.Millisecond)

	// This task will be popped by dequeueLoop and block on the jobQueue
	// We use high priority to ensure it's picked up before others
	_, _ = sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		mu.Lock()
		executionOrder = append(executionOrder, 999)
		mu.Unlock()
		return nil
	}, 99, 0))

	// Now these will definitely stay in the heap and be sorted
	priorities := []int{1, 10, 5, 20, 2}
	for _, p := range priorities {
		priority := p
		_, err := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
			mu.Lock()
			executionOrder = append(executionOrder, priority)
			mu.Unlock()
			return nil
		}, priority, 0))
		if err != nil {
			t.Fatalf("failed to submit task: %v", err)
		}
	}

	<-blockerDone
	// Wait for all to finish
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// First is 999 (the one held by dequeueLoop)
	// Then 20, 10, 5, 2, 1
	expected := []int{999, 20, 10, 5, 2, 1}
	if len(executionOrder) != len(expected) {
		t.Fatalf("expected %d tasks, got %d", len(expected), len(executionOrder))
	}
	for i := range expected {
		if executionOrder[i] != expected[i] {
			t.Errorf("at index %d: expected priority %d, got %d", i, expected[i], executionOrder[i])
		}
	}
}

func TestBackpressure(t *testing.T) {
	// Set a very small capacity
	sch := skedulr.New(skedulr.WithMaxCapacity(2))
	defer sch.ShutDown(context.Background())

	job := func(ctx context.Context) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	// Fill the queue (2 tasks)
	_, err := sch.Submit(skedulr.NewTask(job, 10, 0))
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	_, err = sch.Submit(skedulr.NewTask(job, 10, 0))
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}

	// Third task should be rejected
	_, err = sch.Submit(skedulr.NewTask(job, 10, 0))
	if !errors.Is(err, skedulr.ErrQueueFull) {
		t.Errorf("expected ErrQueueFull, got %v", err)
	}
}

func TestOverlapPrevention(t *testing.T) {
	sch := skedulr.New(skedulr.WithMaxWorkers(1))
	defer sch.ShutDown(context.Background())

	jobDone := make(chan bool)
	job := func(ctx context.Context) error {
		time.Sleep(200 * time.Millisecond)
		jobDone <- true
		return nil
	}

	key := "singleton-job"

	// Submit first job with key
	_, err := sch.Submit(skedulr.NewTask(job, 10, 0).WithKey(key))
	if err != nil {
		t.Fatalf("failed to submit first job: %v", err)
	}

	// Submit second job with same key immediately
	_, err = sch.Submit(skedulr.NewTask(job, 10, 0).WithKey(key))
	if !errors.Is(err, skedulr.ErrJobAlreadyRunning) {
		t.Errorf("expected ErrJobAlreadyRunning, got %v", err)
	}

	// Wait for job to finish
	<-jobDone
	time.Sleep(100 * time.Millisecond) // Allow key cleanup

	// Now we should be able to submit it again
	_, err = sch.Submit(skedulr.NewTask(job, 10, 0).WithKey(key))
	if err != nil {
		t.Errorf("failed to submit job after cleanup: %v", err)
	}
}

type distributedMockStorage struct {
	mu                sync.Mutex
	tasks             map[string]*skedulr.PersistentTask
	leases            map[string]string // taskID -> instanceID
	cancelSubscribers []func(string)
	waiting           map[string][]*skedulr.PersistentTask // parentID -> children
	depCounts         map[string]int                       // childID -> remaining deps
}

func (m *distributedMockStorage) Save(ctx context.Context, t *skedulr.PersistentTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	return nil
}

func (m *distributedMockStorage) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, id)
	delete(m.leases, id)
	return nil
}

func (m *distributedMockStorage) LoadAll(ctx context.Context) ([]*skedulr.PersistentTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*skedulr.PersistentTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		list = append(list, t)
	}
	return list, nil
}

func (m *distributedMockStorage) Claim(ctx context.Context, id, instanceID string, d time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner, ok := m.leases[id]; ok && owner != instanceID {
		return false, nil
	}
	m.leases[id] = instanceID
	return true, nil
}

func (m *distributedMockStorage) Heartbeat(ctx context.Context, id, instanceID string, d time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner, ok := m.leases[id]; !ok || owner != instanceID {
		return context.DeadlineExceeded
	}
	return nil
}

func (m *distributedMockStorage) PublishCancel(ctx context.Context, id string) error {
	m.mu.Lock()
	subs := make([]func(string), len(m.cancelSubscribers))
	copy(subs, m.cancelSubscribers)
	m.mu.Unlock()
	for _, sub := range subs {
		sub(id)
	}
	return nil
}
func (m *distributedMockStorage) SubscribeCancel(ctx context.Context, onCancel func(id string)) error {
	m.mu.Lock()
	m.cancelSubscribers = append(m.cancelSubscribers, onCancel)
	m.mu.Unlock()
	return nil
}
func (m *distributedMockStorage) SaveWaiting(ctx context.Context, t *skedulr.PersistentTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.depCounts[t.ID] = len(t.DependsOn)
	for _, pid := range t.DependsOn {
		m.waiting[pid] = append(m.waiting[pid], t)
	}
	return nil
}
func (m *distributedMockStorage) ResolveDependencies(ctx context.Context, parentID string) ([]*skedulr.PersistentTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	children := m.waiting[parentID]
	var ready []*skedulr.PersistentTask
	for _, child := range children {
		m.depCounts[child.ID]--
		if m.depCounts[child.ID] == 0 {
			ready = append(ready, child)
		}
	}
	delete(m.waiting, parentID)
	return ready, nil
}

func (m *distributedMockStorage) AddToHistory(ctx context.Context, t skedulr.TaskInfo, retention time.Duration) error {
	return nil
}

func (m *distributedMockStorage) GetHistory(ctx context.Context, limit int) ([]skedulr.TaskInfo, error) {
	return nil, nil
}

func (m *distributedMockStorage) Enqueue(ctx context.Context, t *skedulr.PersistentTask) error {
	return nil
}

func (m *distributedMockStorage) Dequeue(ctx context.Context, instanceID string, d time.Duration) (*skedulr.PersistentTask, error) {
	return nil, nil
}

func TestDistributedCoordination(t *testing.T) {
	storage := &distributedMockStorage{
		tasks:  make(map[string]*skedulr.PersistentTask),
		leases: make(map[string]string),
	}

	executions := sync.Map{}
	jobName := "dist_job"

	jobFunc := func(ctx context.Context) error {
		id := skedulr.TaskID(ctx)
		executions.Store(id, true)
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	// Instance 1
	sch1 := skedulr.New(
		skedulr.WithStorage(storage),
		skedulr.WithInstanceID("inst-1"),
		skedulr.WithJob(jobName, jobFunc),
	)
	defer sch1.ShutDown(context.Background())

	// Instance 2
	sch2 := skedulr.New(
		skedulr.WithStorage(storage),
		skedulr.WithInstanceID("inst-2"),
		skedulr.WithJob(jobName, jobFunc),
	)
	defer sch2.ShutDown(context.Background())

	// Submit a task to sch1
	taskID := "task-1"
	_, err := sch1.Submit(skedulr.NewPersistentTask(jobName, nil, 10, 0).WithID(taskID))
	if err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	// Wait for execution
	time.Sleep(500 * time.Millisecond)

	// Verify it was executed exactly once
	count := 0
	executions.Range(func(key, value interface{}) bool {
		count++
		return true
	})

	if count != 1 {
		t.Errorf("expected 1 execution, got %d", count)
	}
}

func TestDistributedCancellation(t *testing.T) {
	storage := &distributedMockStorage{
		tasks:  make(map[string]*skedulr.PersistentTask),
		leases: make(map[string]string),
	}

	jobName := "cancel_job"
	var cancelled atomic.Bool

	jobFunc := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	}

	// Instance 1
	sch1 := skedulr.New(
		skedulr.WithStorage(storage),
		skedulr.WithInstanceID("inst-1"),
		skedulr.WithJob(jobName, jobFunc),
	)
	defer sch1.ShutDown(context.Background())

	// Instance 2
	sch2 := skedulr.New(
		skedulr.WithStorage(storage),
		skedulr.WithInstanceID("inst-2"),
		skedulr.WithJob(jobName, jobFunc),
	)
	defer sch2.ShutDown(context.Background())

	// Submit task to sch1
	taskID := "task-to-cancel"
	_, err := sch1.Submit(skedulr.NewPersistentTask(jobName, nil, 10, 0).WithID(taskID))
	if err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	// Give it a moment to start
	time.Sleep(200 * time.Millisecond)

	// Cancel via sch2
	err = sch2.Cancel(taskID)
	if err != nil {
		t.Fatalf("failed to cancel: %v", err)
	}

	// Wait for cancellation to propagate and job to stop
	time.Sleep(500 * time.Millisecond)

	if !cancelled.Load() {
		t.Error("task was not cancelled across instances")
	}
}

func TestTaskDependencies(t *testing.T) {
	storage := &distributedMockStorage{
		tasks:     make(map[string]*skedulr.PersistentTask),
		leases:    make(map[string]string),
		waiting:   make(map[string][]*skedulr.PersistentTask),
		depCounts: make(map[string]int),
	}

	executions := make([]string, 0)
	var mu sync.Mutex

	jobA := "job_a"
	jobB := "job_b"

	sch := skedulr.New(
		skedulr.WithStorage(storage),
		skedulr.WithJob(jobA, func(ctx context.Context) error {
			mu.Lock()
			executions = append(executions, jobA)
			mu.Unlock()
			return nil
		}),
		skedulr.WithJob(jobB, func(ctx context.Context) error {
			mu.Lock()
			executions = append(executions, jobB)
			mu.Unlock()
			return nil
		}),
	)
	defer sch.ShutDown(context.Background())

	// Submit B depending on A
	idB := "task-b"
	idA := "task-a"
	_, err := sch.Submit(skedulr.NewPersistentTask(jobB, nil, 10, 0).WithID(idB).DependsOn(idA))
	if err != nil {
		t.Fatalf("failed to submit B: %v", err)
	}

	// Submit A
	_, err = sch.Submit(skedulr.NewPersistentTask(jobA, nil, 10, 0).WithID(idA))
	if err != nil {
		t.Fatalf("failed to submit A: %v", err)
	}

	// Wait for execution
	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	if len(executions) != 2 {
		t.Fatalf("expected 2 executions, got %d", len(executions))
	}
	if executions[0] != jobA || executions[1] != jobB {
		t.Errorf("expected execution order [job_a, job_b], got %v", executions)
	}
}

func TestDashboard(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	handler := sch.DashboardHandler()

	// Test UI
	reqUI, _ := http.NewRequest("GET", "/", nil)
	rrUI := httptest.NewRecorder()
	handler.ServeHTTP(rrUI, reqUI)
	if rrUI.Code != http.StatusOK {
		t.Errorf("expected UI status 200, got %d", rrUI.Code)
	}
	if !strings.Contains(rrUI.Body.String(), "Skedulr Dashboard") {
		t.Error("UI body missing 'Skedulr Dashboard'")
	}

	// Test Stats API
	reqStats, _ := http.NewRequest("GET", "/api/stats", nil)
	rrStats := httptest.NewRecorder()
	handler.ServeHTTP(rrStats, reqStats)
	if rrStats.Code != http.StatusOK {
		t.Errorf("expected stats status 200, got %d", rrStats.Code)
	}
	var s skedulr.Stats
	if err := json.Unmarshal(rrStats.Body.Bytes(), &s); err != nil {
		t.Fatalf("failed to decode stats: %v", err)
	}
}

func TestDashboardHistory(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	// Run a quick task
	done := make(chan struct{})
	id, _ := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		close(done)
		return nil
	}, 10, 0))

	<-done
	time.Sleep(500 * time.Millisecond) // Wait for runTask to finish cleaning up and recording history

	stats := sch.Stats()

	// Should be in history, not active
	foundInHistory := false
	for _, task := range stats.History {
		if task.ID == id {
			foundInHistory = true
			if task.Status != "Succeeded" {
				t.Errorf("expected history status 'Succeeded', got %s", task.Status)
			}
		}
	}

	if !foundInHistory {
		t.Error("task not found in dashboard history")
	}

	for _, task := range stats.ActiveTasks {
		if task.ID == id {
			t.Error("task still shown as active after completion")
		}
	}
}

func TestCustomTaskIDs(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	customID := "my-awesome-task"
	id, err := sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		return nil
	}, 10, 0).WithID(customID))

	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	if id != customID {
		t.Errorf("expected ID %s, got %s", customID, id)
	}
}

func TestScheduledTasksWithIDs(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	// Test ScheduleOnceTask
	onceID := "once-task-id"
	id1, _ := sch.ScheduleOnceTask(
		skedulr.NewTask(func(ctx context.Context) error { return nil }, 10, 0).WithID(onceID),
		time.Now().Add(100*time.Millisecond),
	)
	if id1 != onceID {
		t.Errorf("ScheduleOnceTask: expected ID %s, got %s", onceID, id1)
	}

	// Test ScheduleRecurringTask
	recID := "recurring-task-id"
	id2, _ := sch.ScheduleRecurringTask(
		skedulr.NewTask(func(ctx context.Context) error { return nil }, 10, 0).WithID(recID),
		100*time.Millisecond,
	)
	if id2 != recID {
		t.Errorf("ScheduleRecurringTask: expected ID %s, got %s", recID, id2)
	}
}

func TestProgressReporting(t *testing.T) {
	sch := skedulr.New()
	defer sch.ShutDown(context.Background())

	processed := make(chan bool, 1)
	testID := "progress-task"

	sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		skedulr.ReportProgress(ctx, 50)
		// Give some time for stats to reflect it
		time.Sleep(100 * time.Millisecond)
		skedulr.ReportProgress(ctx, 100)
		processed <- true
		return nil
	}, 10, 0).WithID(testID))

	// Wait for task to start and report initial progress
	time.Sleep(50 * time.Millisecond)

	stats := sch.Stats()
	found := false
	for _, tInfo := range stats.ActiveTasks {
		if tInfo.ID == testID {
			found = true
			if tInfo.Progress < 50 {
				t.Errorf("expected progress >= 50, got %d", tInfo.Progress)
			}
		}
	}
	if !found {
		t.Error("task not found in active tasks")
	}

	<-processed
}

func TestWorkerPools(t *testing.T) {
	sch := skedulr.New(
		skedulr.WithMaxWorkers(2), // Default pool
		skedulr.WithWorkersForPool("cpu-intensive", 1),
		skedulr.WithWorkersForPool("io-intensive", 3),
	)
	defer sch.ShutDown(context.Background())

	results := make(chan string, 3)

	// CPU-intensive task
	sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		results <- "cpu"
		return nil
	}, 1, 0).WithPool("cpu-intensive"))

	// IO-intensive task
	sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		results <- "io"
		return nil
	}, 1, 0).WithPool("io-intensive"))

	// Default task
	sch.Submit(skedulr.NewTask(func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		results <- "default"
		return nil
	}, 1, 0))

	// Collect 3 results
	received := make(map[string]bool)
	for i := 0; i < 3; i++ {
		select {
		case res := <-results:
			received[res] = true
		case <-time.After(1 * time.Second):
			t.Errorf("Timed out waiting for task %d", i)
		}
	}

	if !received["cpu"] || !received["io"] || !received["default"] {
		t.Errorf("Missing results: %+v", received)
	}
}
