package skedulr

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrSchedulerStopped is returned when an operation is attempted on a stopped scheduler.
	ErrSchedulerStopped = errors.New("scheduler is stopped")
	// ErrQueueFull is returned when the task queue has reached its maximum capacity.
	ErrQueueFull = errors.New("scheduler queue is full")
	// ErrJobAlreadyRunning is returned when a job with the same key is already in the queue or running.
	ErrJobAlreadyRunning = errors.New("job with this key is already queued or running")
)

type taskQueue []*task

func (tsk *taskQueue) Push(ts interface{}) {
	t := ts.(*task)
	*tsk = append(*tsk, t)
}

func (tsk *taskQueue) Pop() interface{} {
	taskLen := tsk.Len()
	if taskLen == 0 {
		return nil
	}

	oldTsk := *tsk
	item := oldTsk[taskLen-1]
	oldTsk[taskLen-1] = nil
	*tsk = oldTsk[:taskLen-1]
	return item
}

func (tsk *taskQueue) Len() int {
	return len(*tsk)
}

func (tsk taskQueue) Less(i, j int) bool {
	return tsk[i].priority > tsk[j].priority
}

func (tsk taskQueue) Swap(i, j int) {
	tsk[i], tsk[j] = tsk[j], tsk[i]
}

// -------------------------------------- Job run ----------------------------------------
// Job defines the function signature for a task.
type Job func(ctx context.Context) error

// Scheduler manages the concurrent execution of prioritized tasks.
// It supports dynamic worker scaling, retries, and middleware.
type Scheduler struct {
	mu               sync.Mutex
	cond             *sync.Cond
	queue            taskQueue
	tasks            map[string]*task
	poolQueues       map[string]chan *task
	poolWorkers      map[string]int
	stop             chan struct{}
	stopped          int32 // Atomic flag to prevent new submissions
	maxWorkers       int
	currentWorkers   int32
	queueSize        int64 // Atomic tracker for queue size
	successCount     int64 // Atomic tracker for successful tasks
	failureCount     int64 // Atomic tracker for failed tasks
	deadCount        int64 // Atomic tracker for tasks in DLQ
	panicCount       int64 // Atomic tracker for panics caught
	defaultTimeout   time.Duration
	retryStrategy    RetryStrategy
	middlewares      []Middleware
	logger           Logger
	storage          Storage
	registry         map[string]Job
	regMu            sync.RWMutex
	wg               sync.WaitGroup
	maxQueueSize     int
	activeKeys       map[string]struct{}
	instanceID       string
	leaseDuration    time.Duration
	loopWg           sync.WaitGroup
	historyRetention time.Duration
	paused           int32
	recoveryInterval time.Duration
	poolControl      map[string]chan struct{}
	wheel            *TimingWheel
	wheelTick        time.Duration
	wheelSize        int
}

// TaskStatus represents the current state of a task.
type TaskStatus int

const (
	// StatusUnknown indicates the task is no longer tracked — either it
	// graduated to history and was evicted from the active map, or it never
	// existed.
	StatusUnknown TaskStatus = iota
	// StatusQueued indicates the task is in the priority queue waiting for a worker.
	StatusQueued
	// StatusRunning indicates the task is currently being executed by a worker.
	StatusRunning
	// StatusSucceeded indicates the task finished successfully (terminal).
	StatusSucceeded
	// StatusFailed indicates the task's last attempt returned an error and
	// the task has no retry strategy (or its strategy chose not to retry).
	// This is the terminal "failed without a safety net" state, distinct
	// from StatusDead (failed after exhausting the retry budget).
	StatusFailed
	// StatusCancelled indicates the task was explicitly cancelled (terminal).
	StatusCancelled
	// StatusDead indicates the task failed after exhausting every configured
	// retry attempt. Dead tasks remain visible in the dashboard and can be
	// re-armed via Resubmit. Terminal until manually resubmitted.
	StatusDead
	// StatusRetrying indicates the task's previous attempt failed but a
	// retry is currently parked in the timing wheel waiting for its backoff
	// to elapse. Transitions to StatusQueued when the retry fires.
	StatusRetrying
)

func (s TaskStatus) String() string {
	return [...]string{"Unknown", "Queued", "Running", "Succeeded", "Failed", "Cancelled", "Dead", "Retrying"}[s]
}

type task struct {
	id            string
	key           string
	job           Job
	timeout       time.Duration
	priority      int
	cancel        context.CancelFunc
	retryStrategy RetryStrategy
	attempts      int
	maxRetries    int
	status        TaskStatus
	progress      int
	pool          string
	typeName      string
	payload       []byte
	dependsOn     []string
	dependencies  []TaskDependency
}

// New creates and starts a new Scheduler with the provided options.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:            make(map[string]*task),
		poolQueues:       make(map[string]chan *task),
		poolWorkers:      make(map[string]int),
		queue:            make(taskQueue, 0),
		stop:             make(chan struct{}),
		maxWorkers:       5,
		maxQueueSize:     1000,
		registry:         make(map[string]Job),
		storage:          &InMemoryStorage{history: make([]TaskInfo, 0)},
		activeKeys:       make(map[string]struct{}),
		poolControl:      make(map[string]chan struct{}),
		instanceID:       generateId(),
		leaseDuration:    30 * time.Second,   // Default lease
		historyRetention: 7 * 24 * time.Hour, // Default 7 days
		recoveryInterval: 1 * time.Minute,    // Default recovery interval
		wheelTick:        10 * time.Millisecond,
		wheelSize:        256,
	}
	s.cond = sync.NewCond(&s.mu)

	for _, opt := range opts {
		opt(s)
	}

	// The timing wheel must be running before loadTasks, because any tasks
	// recovered with a future fire time will route through it. A single
	// goroutine carries every delayed callback (retry backoff, ScheduleOnce,
	// ScheduleRecurring, ScheduleCron) instead of one goroutine per task.
	s.wheel = NewTimingWheel(s.wheelTick, s.wheelSize)
	s.wheel.Start()

	s.loadTasks()

	s.loopWg.Add(1)
	go s.dequeueLoop()

	s.loopWg.Add(1)
	go s.cleanupLoop()

	s.loopWg.Add(1)
	go s.recoveryLoop()

	s.storage.SubscribeCancel(context.Background(), func(id string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if t, ok := s.tasks[id]; ok {
			t.status = StatusCancelled
			if t.cancel != nil {
				t.cancel()
			}
		}
	})

	// Initialize default pool if not explicitly set
	if _, ok := s.poolQueues["default"]; !ok {
		s.poolQueues["default"] = make(chan *task)
		s.poolWorkers["default"] = s.maxWorkers
	}

	// Spawn workers for all pools
	for pool, count := range s.poolWorkers {
		s.spawnWorkersForPool(pool, count)
	}

	return s
}

// RegisterJob registers a job function with a name.
// This is required for task persistence and recovery.
func (s *Scheduler) RegisterJob(name string, job Job) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	s.registry[name] = job
}

func (s *Scheduler) loadTasks() {
	tasks, err := s.storage.LoadAll(context.Background())
	if err != nil {
		if s.logger != nil {
			s.logger.Error("failed to load tasks from storage", err)
		}
		return
	}

	for _, pt := range tasks {
		// Try to claim the task
		claimed, err := s.storage.Claim(context.Background(), pt.ID, s.instanceID, s.leaseDuration)
		if err != nil || !claimed {
			continue // Already claimed by another instance or error
		}

		s.regMu.RLock()
		job, ok := s.registry[pt.TypeName]
		s.regMu.RUnlock()

		if !ok {
			if s.logger != nil {
				s.logger.Error("unknown job type on task reload", nil, "type", pt.TypeName, "id", pt.ID)
			}
			continue
		}

		t := &task{
			id:       pt.ID,
			key:      pt.Key,
			pool:     pt.Pool,
			job:      job,
			typeName: pt.TypeName,
			payload:  pt.Payload,
			priority: pt.Priority,
			timeout:  pt.Timeout,
			attempts: pt.Attempts,
			status:   StatusQueued,
		}

		s.mu.Lock()
		s.tasks[t.id] = t
		heap.Push(&s.queue, t)
		atomic.AddInt64(&s.queueSize, 1)
		s.cond.Signal()
		s.mu.Unlock()
	}
}

func (s *Scheduler) dequeueLoop() {
	defer s.loopWg.Done()
	for {
		// Check if scheduler is paused
		if atomic.LoadInt32(&s.paused) == 1 {
			select {
			case <-s.stop:
				return
			case <-time.After(100 * time.Millisecond): // Wait a bit before re-checking
				continue
			}
		}

		var t *task

		s.mu.Lock()
		if s.queue.Len() > 0 {
			t = heap.Pop(&s.queue).(*task)
			atomic.AddInt64(&s.queueSize, -1)
		}
		s.mu.Unlock()

		if t == nil {
			pt, err := s.storage.Dequeue(context.Background(), s.instanceID, s.leaseDuration)
			if err != nil {
				if s.logger != nil {
					s.logger.Error("dequeue failed", err)
				}
				time.Sleep(1 * time.Second)
				continue
			}

			if pt == nil {
				// No task from storage, continue to wait or check in-memory queue again
				// This `continue` will lead to the select statement below
			} else {
				// Check if task was cancelled before dispatching
				if cancelled, _ := s.storage.IsCancelled(context.Background(), pt.ID); cancelled {
					s.storage.Delete(context.Background(), pt.ID)
					continue // Skip this task and try again
				}

				s.regMu.RLock()
				job, ok := s.registry[pt.TypeName]
				s.regMu.RUnlock()

				if ok {
					t = &task{
						id:       pt.ID,
						key:      pt.Key,
						pool:     pt.Pool,
						job:      job,
						typeName: pt.TypeName,
						payload:  pt.Payload,
						priority: pt.Priority,
						timeout:  pt.Timeout,
						attempts: pt.Attempts,
						status:   StatusQueued,
					}
					s.mu.Lock()
					s.tasks[t.id] = t
					s.mu.Unlock()
				}
			}
		}

		if t != nil {
			s.dispatchTask(t)
			continue
		}

		// Check if we should exit
		if atomic.LoadInt32(&s.stopped) == 1 {
			return
		}

		// Wait for signal or timeout
		select {
		case <-s.stop:
			return
		case <-time.After(100 * time.Millisecond):
			// Continue to next iteration to check queue and storage
		}
	}
}

func (s *Scheduler) dispatchTask(t *task) bool {
	pool := t.pool
	if pool == "" {
		pool = "default"
	}

	s.mu.Lock()
	ch, ok := s.poolQueues[pool]
	if !ok {
		ch = make(chan *task)
		s.poolQueues[pool] = ch
		s.poolWorkers[pool] = 1
		s.spawnWorkersForPoolLocked(pool, 1)
	}
	s.mu.Unlock()

	select {
	case ch <- t:
		return true
	case <-s.stop:
		return false
	}
}

func (s *Scheduler) cleanupLoop() {
	defer s.loopWg.Done()
	ticker := time.NewTicker(s.leaseDuration / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.loadTasks()
		case <-s.stop:
			return
		}
	}
}

func (s *Scheduler) spawnWorkersForPool(pool string, n int) {
	s.mu.Lock()
	s.spawnWorkersForPoolLocked(pool, n)
	s.mu.Unlock()
}

func (s *Scheduler) spawnWorkersForPoolLocked(pool string, n int) {
	if _, ok := s.poolControl[pool]; !ok {
		s.poolControl[pool] = make(chan struct{}, 1000)
	}

	for i := 0; i < n; i++ {
		atomic.AddInt32(&s.currentWorkers, 1)
		s.wg.Add(1)
		go s.worker(pool)
	}
}

func (s *Scheduler) worker(pool string) {
	defer s.wg.Done()
	s.mu.Lock()
	ch := s.poolQueues[pool]
	control := s.poolControl[pool]
	s.mu.Unlock()

	for {
		select {
		case t, ok := <-ch:
			if !ok {
				return
			}
			s.mu.Lock()
			if trackTask, ok := s.tasks[t.id]; ok {
				trackTask.status = StatusRunning
			}
			s.mu.Unlock()
			s.runTask(t)
		case <-control:
			// Graceful scale down
			atomic.AddInt32(&s.currentWorkers, -1)
			return
		case <-s.stop:
			return
		}
	}
}

type contextKey string

type progressFunc func(int)

const (
	TaskIDKey   contextKey = "task_id"
	progressKey contextKey = "task_progress"
)

// TaskID returns the task ID associated with the context, if any.
func TaskID(ctx context.Context) string {
	if id, ok := ctx.Value(TaskIDKey).(string); ok {
		return id
	}
	return ""
}

// ReportProgress updates the progress of the current task.
// percent should be between 0 and 100.
func ReportProgress(ctx context.Context, percent int) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if f, ok := ctx.Value(progressKey).(progressFunc); ok {
		f(percent)
	}
}

func (s *Scheduler) runTask(t *task) {
	delay := t.timeout
	if delay == 0 {
		delay = s.defaultTimeout
	}

	var ctx context.Context
	var cancel context.CancelFunc

	s.mu.Lock()
	if t, ok := s.tasks[t.id]; !ok || t.status == StatusCancelled {
		if ok && t.status == StatusCancelled {
			s.recordHistory(t)
			delete(s.tasks, t.id)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	updateProgress := func(p int) {
		s.mu.Lock()
		if trackTask, ok := s.tasks[t.id]; ok {
			trackTask.progress = p
		}
		s.mu.Unlock()
	}

	baseCtx := context.WithValue(context.Background(), TaskIDKey, t.id)
	baseCtx = context.WithValue(baseCtx, progressKey, progressFunc(updateProgress))

	if delay > 0 {
		ctx, cancel = context.WithTimeout(baseCtx, delay)
	} else {
		ctx, cancel = context.WithCancel(baseCtx)
	}
	defer cancel()

	s.mu.Lock()
	if trackTask, ok := s.tasks[t.id]; ok {
		if trackTask.status == StatusCancelled {
			s.mu.Unlock()
			cancel()
			return
		}
		trackTask.cancel = cancel
		trackTask.status = StatusRunning
	}
	s.mu.Unlock()

	// Distributed heartbeat
	if t.typeName != "" {
		hbCtx, hbCancel := context.WithCancel(context.Background())
		defer hbCancel()
		go func() {
			ticker := time.NewTicker(s.leaseDuration / 3)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := s.storage.Heartbeat(hbCtx, t.id, s.instanceID, s.leaseDuration); err != nil {
						if s.logger != nil {
							s.logger.Error("failed lease heartbeat", err, "task_id", t.id, "instance_id", s.instanceID)
						}
						return // Stop heartbeat if we lost the lease
					}
				case <-hbCtx.Done():
					return
				case <-s.stop:
					return
				}
			}
		}()
	}

	// Apply middlewares
	finalJob := t.job
	for i := len(s.middlewares) - 1; i >= 0; i-- {
		finalJob = s.middlewares[i](finalJob)
	}

	done := make(chan error, 1)
	go func() {
		done <- finalJob(ctx)
	}()

	select {
	case err := <-done:
		// Release the dedup-key slot as soon as the worker is done — a
		// retry of the same task will re-acquire it from Submit.
		s.mu.Lock()
		if t.key != "" {
			delete(s.activeKeys, t.key)
		}
		s.mu.Unlock()

		if err != nil {
			if s.logger != nil {
				s.logger.Error("task failed", err, "task_id", t.id)
			}
			// handleFailure owns the post-failure decision: set status to
			// Retrying / Dead / Failed, fire workflow children only on a
			// terminal outcome, and record history exactly once.
			s.handleFailure(t, err)
		} else {
			s.mu.Lock()
			if trackTask, ok := s.tasks[t.id]; ok {
				trackTask.status = StatusSucceeded
				trackTask.progress = 100
			}
			t.status = StatusSucceeded
			s.recordHistory(t)
			s.mu.Unlock()
			if t.typeName != "" {
				s.storage.CompleteTask(context.Background(), t.id)
				s.resolveWorkflow(t, StatusSucceeded)
			}
			atomic.AddInt64(&s.successCount, 1)
		}
	case <-ctx.Done():
		// Context fired — either an explicit Cancel via Scheduler.Cancel
		// (which already set StatusCancelled in the tracking map) or a
		// timeout (which we treat as a failure that can still be retried).
		s.mu.Lock()
		finalStatus := StatusFailed
		if ctx.Err() == context.Canceled {
			finalStatus = StatusCancelled
		}
		if trackTask, ok := s.tasks[t.id]; ok {
			// If Cancel already set the status, honor it even if the
			// context says "deadline exceeded" — Cancel happens first
			// on a racy path where both fire close together.
			if trackTask.status == StatusCancelled {
				finalStatus = StatusCancelled
			}
		}
		if t.key != "" {
			delete(s.activeKeys, t.key)
		}
		t.status = finalStatus
		s.mu.Unlock()

		if s.logger != nil {
			s.logger.Error("task context cancelled or timed out", ctx.Err(), "task_id", t.id)
		}

		if finalStatus == StatusCancelled {
			// Terminal — finalize here. No retry path for an explicit cancel.
			s.mu.Lock()
			if trackTask, ok := s.tasks[t.id]; ok {
				trackTask.status = StatusCancelled
			}
			s.recordHistory(t)
			s.mu.Unlock()
			if t.typeName != "" {
				s.storage.CompleteTask(context.Background(), t.id)
				s.resolveWorkflow(t, StatusCancelled)
			}
		} else {
			// Timeout → let handleFailure decide retry vs Dead vs Failed.
			s.handleFailure(t, ctx.Err())
		}
	}

	// Eviction policy: keep terminal-but-actionable tasks (Dead/Failed) and
	// in-flight retries (Retrying) in the active map so the dashboard can
	// show them and Resubmit can find them. Drop everything else once it
	// reaches the bottom of this function — Succeeded / Cancelled tasks
	// are recorded in history and no longer interesting in the live view.
	s.mu.Lock()
	switch t.status {
	case StatusDead, StatusFailed, StatusRetrying:
		// keep
	default:
		delete(s.tasks, t.id)
	}
	s.mu.Unlock()
}

func (s *Scheduler) recordHistory(t *task) {
	if t == nil {
		return
	}

	info := TaskInfo{
		ID:       t.id,
		Key:      t.key,
		Pool:     t.pool,
		Type:     t.typeName,
		Status:   t.status.String(),
		Priority: t.priority,
		Progress: t.progress,
	}

	s.storage.AddToHistory(context.Background(), info, s.historyRetention)
}

func (s *Scheduler) resolveWorkflow(t *task, status TaskStatus) {
	readyTasks, _ := s.storage.ResolveDependencies(context.Background(), t.id, status)
	for _, rt := range readyTasks {
		job, ok := s.getJob(rt.TypeName)
		if ok {
			jt := &task{
				id:           rt.ID,
				key:          rt.Key,
				typeName:     rt.TypeName,
				payload:      rt.Payload,
				priority:     rt.Priority,
				timeout:      rt.Timeout,
				attempts:     rt.Attempts,
				maxRetries:   rt.MaxRetries,
				job:          job,
				status:       StatusQueued,
				dependencies: nil, // Ready for execution
			}
			s.Submit(jt)
		}
	}
}

// Status returns the current status of a task.
// If the task graduated or never existed, it returns StatusUnknown.
func (s *Scheduler) Status(id string) TaskStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		return t.status
	}
	return StatusUnknown
}

// handleFailure decides what happens after a worker reports an error. There
// are three terminal outcomes the dashboard cares about — Dead, Failed, and
// Retrying — and the order of operations here is what keeps the status the
// user sees in sync with what the scheduler is actually doing.
//
//   - Exhausted retries → StatusDead, fire OnFailure children, finalize.
//   - Retry available → StatusRetrying, schedule retry on the wheel. Do NOT
//     fire OnFailure children yet — the task isn't terminally failed.
//   - No retry strategy at all → StatusFailed, fire OnFailure children,
//     finalize.
//
// Pre-fix, OnFailure children fired on every attempt of a retrying task,
// because resolveWorkflow ran in the worker right after the error, before
// handleFailure had a chance to decide whether a retry was coming.
func (s *Scheduler) handleFailure(t *task, err error) {
	atomic.AddInt64(&s.failureCount, 1)

	// Retry exhausted → Dead.
	if t.maxRetries > 0 && t.attempts >= t.maxRetries {
		atomic.AddInt64(&s.deadCount, 1)
		s.mu.Lock()
		if trackTask, ok := s.tasks[t.id]; ok {
			trackTask.status = StatusDead
		}
		s.mu.Unlock()
		if t.typeName != "" {
			s.storage.Save(context.Background(), &PersistentTask{
				ID:         t.id,
				Key:        t.key,
				Pool:       t.pool,
				TypeName:   t.typeName,
				Payload:    t.payload,
				Priority:   t.priority,
				Timeout:    t.timeout,
				Attempts:   t.attempts,
				MaxRetries: t.maxRetries,
			})
			s.storage.CompleteTask(context.Background(), t.id)
			// Children registered via OnFailure(parent) treat both Dead
			// (retries exhausted) and Failed (no retry strategy) as "the
			// parent ultimately failed", so the resolution key here uses
			// StatusFailed regardless of the precise terminal state. We
			// also resolve StatusDead so any consumer that explicitly
			// listens for the exhausted-retries signal gets it too.
			s.resolveWorkflow(t, StatusFailed)
			s.resolveWorkflow(t, StatusDead)
		}
		// Record the terminal failure in history now that the task is done.
		s.mu.Lock()
		s.recordHistory(t)
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Error("task exceeded max retries and is now DEAD", err, "task_id", t.id, "attempts", t.attempts)
		}
		return
	}

	// A retry is available — park it in the wheel and mark the task as
	// Retrying so the dashboard reflects "waiting for backoff" rather than
	// the misleading "Failed".
	if t.retryStrategy != nil {
		delay, retry := t.retryStrategy.NextDelay(t.attempts)
		if retry {
			s.mu.Lock()
			if trackTask, ok := s.tasks[t.id]; ok {
				trackTask.status = StatusRetrying
			}
			s.mu.Unlock()

			retryTask := NewTask(t.job, t.priority, t.timeout)
			retryTask.id = t.id // Preserve ID so Cancel + tracking align across attempts.
			retryTask.attempts = t.attempts + 1
			retryTask.maxRetries = t.maxRetries
			retryTask.retryStrategy = t.retryStrategy
			retryTask.pool = t.pool
			retryTask.typeName = t.typeName
			retryTask.payload = t.payload

			// The retry is parked in the shared timing wheel rather than its
			// own time.AfterFunc goroutine. The same ID flows through Cancel
			// to stop a pending retry mid-backoff.
			_ = s.wheel.Schedule(retryTask.id, time.Now().Add(delay), func() {
				s.Submit(retryTask)
			})
			return
		}
	}

	// No retry strategy (or strategy declined) → terminal Failed. The
	// distinction from Dead is intentional: Dead means "exhausted retries"
	// and is resubmittable; Failed means "no safety net was configured" and
	// is informational only.
	s.mu.Lock()
	if trackTask, ok := s.tasks[t.id]; ok {
		trackTask.status = StatusFailed
	}
	s.mu.Unlock()
	if t.typeName != "" {
		s.resolveWorkflow(t, StatusFailed)
	}
	s.mu.Lock()
	s.recordHistory(t)
	s.mu.Unlock()
}

// Use adds middlewares to the scheduler.
func (s *Scheduler) Use(mw ...Middleware) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.middlewares = append(s.middlewares, mw...)
}

// NewTask creates a new task instance.
// NewTask creates a new task with the given job, priority, and optional timeout.
func NewTask(job Job, priority int, timeout time.Duration) *task {
	return &task{
		id:       generateId(),
		job:      job,
		priority: priority,
		timeout:  timeout,
		status:   StatusQueued,
	}
}

// NewPersistentTask creates a task that can be saved to storage.
// It requires a typeName that has been registered with RegisterJob.
func NewPersistentTask(typeName string, payload []byte, priority int, timeout time.Duration) *task {
	return &task{
		id:       generateId(),
		typeName: typeName,
		payload:  payload,
		priority: priority,
		timeout:  timeout,
		status:   StatusQueued,
	}
}

// Submit adds a task to the priority queue.
// Returns an error if the scheduler is stopped.
func (s *Scheduler) Submit(t *task) (string, error) {
	if atomic.LoadInt32(&s.stopped) == 1 {
		return "", ErrSchedulerStopped
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Backpressure check
	if len(s.tasks) >= s.maxQueueSize {
		return "", ErrQueueFull
	}

	// Overlap prevention check
	if t.key != "" {
		if _, exists := s.activeKeys[t.key]; exists {
			return "", ErrJobAlreadyRunning
		}
		s.activeKeys[t.key] = struct{}{}
	}

	if t.retryStrategy == nil {
		t.retryStrategy = s.retryStrategy
	}

	t.status = StatusQueued

	// Link job from registry if not provided (for persistent tasks)
	if t.job == nil && t.typeName != "" {
		s.regMu.RLock()
		job, ok := s.registry[t.typeName]
		s.regMu.RUnlock()
		if !ok {
			return "", fmt.Errorf("job type %s not found in registry", t.typeName)
		}
		t.job = job
	}

	// Persist if it's a named job
	if t.typeName != "" {
		pt := &PersistentTask{
			ID:           t.id,
			Key:          t.key,
			Pool:         t.pool,
			TypeName:     t.typeName,
			Payload:      t.payload,
			Priority:     t.priority,
			Timeout:      t.timeout,
			Attempts:     t.attempts,
			MaxRetries:   t.maxRetries,
			DependsOn:    t.dependsOn,
			Dependencies: t.dependencies,
		}

		if len(t.dependsOn) > 0 || len(t.dependencies) > 0 {
			if err := s.storage.SaveWaiting(context.Background(), pt); err != nil {
				return "", fmt.Errorf("failed to save waiting task: %w", err)
			}
			s.tasks[t.id] = t
			return t.id, nil
		}

		if err := s.storage.Save(context.Background(), pt); err != nil {
			return "", fmt.Errorf("failed to persist task: %w", err)
		}

		if err := s.storage.Enqueue(context.Background(), pt); err != nil {
			return "", fmt.Errorf("failed to enqueue task: %w", err)
		}
	}

	s.tasks[t.id] = t
	heap.Push(&s.queue, t)
	atomic.AddInt64(&s.queueSize, 1)
	s.cond.Signal()
	return t.id, nil
}

// WithKey sets a unique key for the task to prevent overlapping executions.
func (t *task) WithKey(key string) *task {
	t.key = key
	return t
}

// WithPool sets the worker pool for the task.
func (t *task) WithPool(pool string) *task {
	t.pool = pool
	return t
}

// WithID sets a custom ID for the task.
func (t *task) WithID(id string) *task {
	t.id = id
	return t
}

// WithTypeName sets the job type name for persistence.
func (t *task) WithTypeName(name string) *task {
	t.typeName = name
	return t
}

// WithPayload sets the payload for the task.
func (t *task) WithPayload(payload []byte) *task {
	t.payload = payload
	return t
}

// DependsOn specifies task IDs that this task must wait for (success is required).
func (t *task) DependsOn(ids ...string) *task {
	t.dependsOn = append(t.dependsOn, ids...)
	return t
}

// OnSuccess specifies that this task depends on the successful completion of a parent task.
func (t *task) OnSuccess(parentID string) *task {
	t.dependencies = append(t.dependencies, TaskDependency{ParentID: parentID, Trigger: StatusSucceeded})
	return t
}

// OnFailure specifies that this task depends on the failure of a parent task.
func (t *task) OnFailure(parentID string) *task {
	t.dependencies = append(t.dependencies, TaskDependency{ParentID: parentID, Trigger: StatusFailed})
	return t
}

// ScheduleOnce parks job to fire once at the wall-clock instant `at`. The
// returned ID is the task ID — pass it to Cancel to stop the fire before it
// happens. Multiple ScheduleOnce calls cost O(1) goroutines in total: every
// pending fire lives in the scheduler's shared timing wheel, not its own
// timer + goroutine.
func (s *Scheduler) ScheduleOnce(job Job, at time.Time, priority int) (string, error) {
	t := NewTask(job, priority, 0)
	return s.ScheduleOnceTask(t, at)
}

// ScheduleOnceTask is ScheduleOnce with a caller-supplied task — use it when
// you need a custom ID, key, payload, type name, or retry strategy on the
// fired task. Memory and goroutine cost stay O(1) per pending fire regardless
// of how many tasks are scheduled, because every fire is a single entry in
// the shared timing wheel.
func (s *Scheduler) ScheduleOnceTask(t *task, at time.Time) (string, error) {
	if atomic.LoadInt32(&s.stopped) == 1 {
		return "", ErrSchedulerStopped
	}

	// The cancel func is held on the task so Scheduler.Cancel can surface
	// "cancelled" through the standard context path; the wheel's per-ID
	// index handles stopping the pending fire itself.
	_, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	s.mu.Lock()
	s.tasks[t.id] = t
	s.mu.Unlock()

	if err := s.wheel.Schedule(t.id, at, func() {
		// The wheel skips cancelled entries internally — reaching this body
		// means the task survived to its fire time. A non-nil Submit error
		// here is almost always ErrSchedulerStopped from a concurrent
		// ShutDown; log and drop, matching the pre-migration goroutine
		// loop's behavior on the same race.
		if _, err := s.Submit(t); err != nil && s.logger != nil {
			s.logger.Error("ScheduleOnce submit failed", err, "task_id", t.id)
		}
	}); err != nil {
		return "", err
	}

	return t.id, nil
}

// ScheduleRecurring runs job repeatedly every interval, starting one
// interval from now. Each fire submits a fresh execution task — the
// per-fire IDs are distinct, but the recurring schedule itself is
// addressable by the returned ID, which is what Cancel uses to stop the
// recurrence.
func (s *Scheduler) ScheduleRecurring(job Job, interval time.Duration, priority int) (string, error) {
	t := NewTask(job, priority, interval)
	return s.ScheduleRecurringTask(t, interval)
}

// ScheduleRecurringTask is ScheduleRecurring with a caller-supplied template
// task — useful for setting a custom ID, type name, payload, or retry
// strategy on each fired execution. The schedule lives as a single
// self-rescheduling wheel entry that re-arms itself after every fire, so
// cost is O(1) goroutines regardless of how many recurring tasks exist;
// Cancel(t.id) finds the next-armed entry via the wheel's per-ID index.
func (s *Scheduler) ScheduleRecurringTask(t *task, interval time.Duration) (string, error) {
	if atomic.LoadInt32(&s.stopped) == 1 {
		return "", ErrSchedulerStopped
	}

	_, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	s.mu.Lock()
	s.tasks[t.id] = t
	s.mu.Unlock()

	var fire func()
	fire = func() {
		if atomic.LoadInt32(&s.stopped) == 1 {
			return
		}
		child := &task{
			id:            generateId(),
			job:           t.job,
			typeName:      t.typeName,
			payload:       t.payload,
			priority:      t.priority,
			timeout:       t.timeout,
			retryStrategy: t.retryStrategy,
		}
		s.Submit(child)
		_ = s.wheel.Schedule(t.id, time.Now().Add(interval), fire)
	}
	if err := s.wheel.Schedule(t.id, time.Now().Add(interval), fire); err != nil {
		return "", err
	}

	return t.id, nil
}

// Cancel stops the task identified by id from progressing further. The
// behavior depends on what state the task is currently in:
//
//   - Running — the worker's context is cancelled; the worker observes
//     ctx.Done() and finalizes the task as Cancelled.
//   - Retrying / scheduled-in-wheel — the pending wheel entry is unlinked
//     so the callback never re-enters the queue, the task is finalized as
//     Cancelled here (no worker is going to see it otherwise), and history
//     is recorded.
//   - Queued — the task will be skipped by the next worker that pops it,
//     because dispatch checks the cancelled flag before running.
//   - Already terminal — no-op.
//
// Pre-fix this method only set the status and called wheel.Cancel; tasks
// that were sitting in the wheel (never running) lingered in the active
// map forever showing as "Cancelled" because no worker would ever fire to
// drive the finalization path.
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	t, ok := s.tasks[id]
	wasInactive := false
	if ok {
		// "Inactive" here means the task isn't being executed by a worker
		// right now — it's parked in the wheel waiting to be Submitted, or
		// already done. Those cases need explicit finalization because no
		// worker is going to drive the ctx.Done() path.
		switch t.status {
		case StatusRunning:
			// fall through — worker will finalize
		case StatusSucceeded, StatusFailed, StatusDead, StatusCancelled:
			// already terminal — nothing to do
			s.mu.Unlock()
			if s.wheel != nil {
				s.wheel.Cancel(id)
			}
			return nil
		default:
			wasInactive = true
		}
		t.status = StatusCancelled
		if t.cancel != nil {
			t.cancel()
		}
	}
	s.mu.Unlock()

	// Pull any in-flight wheel entry (retry backoff, ScheduleOnce, recurring
	// arming) so its callback never enters the queue after cancel.
	if s.wheel != nil {
		s.wheel.Cancel(id)
	}
	s.storage.UnscheduleDelayed(context.Background(), id)

	// Mark cancelled in storage so a recovering instance or in-flight dequeue
	// will skip it.
	s.storage.MarkCancelled(context.Background(), id)

	// For tasks that weren't running, finalize the cancellation locally:
	// record history, evict from the active map, release the dedup key.
	// Skipped for running tasks because the worker's ctx.Done() branch
	// already owns finalization for them.
	if wasInactive && ok {
		s.mu.Lock()
		if t.key != "" {
			delete(s.activeKeys, t.key)
		}
		s.recordHistory(t)
		delete(s.tasks, id)
		s.mu.Unlock()
		if t.typeName != "" {
			s.storage.CompleteTask(context.Background(), id)
			s.resolveWorkflow(t, StatusCancelled)
		}
	}

	// Cross-instance cancel for distributed deployments.
	return s.storage.PublishCancel(context.Background(), id)
}

// ShutDown gracefully stops the scheduler, waiting for active workers to finish
// or the context to expire.
func (s *Scheduler) ShutDown(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&s.stopped, 0, 1) {
		return nil // Already stopped
	}

	s.mu.Lock()
	close(s.stop)
	s.cond.Broadcast()
	s.mu.Unlock()

	// Stop the timing wheel before the background loops. Anything still
	// parked (pending retry, scheduled fire, recurring re-arm) is dropped
	// here, which is what keeps Submit from being called on a closing
	// scheduler — the workers and dequeue loop won't be there to handle it.
	if s.wheel != nil {
		s.wheel.Stop()
	}

	// Wait for background loops to exit before closing pool queues
	s.loopWg.Wait()

	// Safe to close channels now
	s.mu.Lock()
	for _, ch := range s.poolQueues {
		close(ch)
	}
	s.mu.Unlock()

	// Wait for workers or timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Clean up
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Error("shutdown timeout exceeded", ctx.Err())
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		if t.cancel != nil {
			t.cancel()
		}
	}
	return nil
}

func (s *Scheduler) getJob(typeName string) (Job, bool) {
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	j, ok := s.registry[typeName]
	return j, ok
}

// Pause stops the scheduler from dequeuing new tasks.
func (s *Scheduler) Pause() {
	atomic.StoreInt32(&s.paused, 1)
}

// Resume allows the scheduler to resume dequeuing tasks.
func (s *Scheduler) Resume() {
	atomic.StoreInt32(&s.paused, 0)
}

// IsPaused returns true if the scheduler is currently paused.
func (s *Scheduler) IsPaused() bool {
	return atomic.LoadInt32(&s.paused) == 1
}

func (s *Scheduler) recoveryLoop() {
	defer s.loopWg.Done()
	ticker := time.NewTicker(s.recoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			count, err := s.storage.RecoverOrphaned(context.Background())
			if err != nil {
				if s.logger != nil {
					s.logger.Error("failed to recover orphaned tasks", err)
				}
			} else if count > 0 && s.logger != nil {
				s.logger.Info("recovered abandoned tasks", "count", count)
			}
		case <-s.stop:
			return
		}
	}
}

// ScalePool adjusts the number of workers in a specific pool.
// It will gracefully stop workers if n < current, or spawn new ones if n > current.
func (s *Scheduler) ScalePool(pool string, n int) {
	if n < 0 {
		n = 0
	}
	// Safety limit: 100 workers per pool to prevent resource exhaustion
	if n > 100 {
		n = 100
	}

	s.mu.Lock()
	current := s.poolWorkers[pool]
	s.poolWorkers[pool] = n

	// Ensure pool queue exists to avoid zombie workers
	if _, ok := s.poolQueues[pool]; !ok {
		s.poolQueues[pool] = make(chan *task)
	}
	s.mu.Unlock()

	if n > current {
		s.spawnWorkersForPool(pool, n-current)
	} else if n < current {
		diff := current - n
		s.mu.Lock()
		control, ok := s.poolControl[pool]
		s.mu.Unlock()
		if ok {
			for i := 0; i < diff; i++ {
				select {
				case control <- struct{}{}:
				default:
				}
			}
		}
	}
}

// WithMaxRetries sets the maximum number of times a task should be retried before becoming Dead.
func (t *task) WithMaxRetries(n int) *task {
	t.maxRetries = n
	return t
}

// Resubmit takes a task out of the Dead Letter Queue and puts it back into the scheduler.
// It resets the attempt count to 0.
func (s *Scheduler) Resubmit(id string) error {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}

	if t.status != StatusDead && t.status != StatusFailed {
		s.mu.Unlock()
		return fmt.Errorf("task %s is not in a failed or dead state", id)
	}

	// Reset attempts for a fresh start
	if t.status == StatusDead {
		atomic.AddInt64(&s.deadCount, -1)
	}
	t.attempts = 0
	t.status = StatusQueued
	heap.Push(&s.queue, t)
	atomic.AddInt64(&s.queueSize, 1)
	s.cond.Signal()
	s.mu.Unlock()

	return nil
}

// WithRetryStrategy sets a custom retry strategy for the task.
func (t *task) WithRetryStrategy(rs RetryStrategy) *task {
	t.retryStrategy = rs
	return t
}
