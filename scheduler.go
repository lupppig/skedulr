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
	mu             sync.Mutex
	cond           *sync.Cond
	queue          taskQueue
	tasks          map[string]*task
	poolQueues     map[string]chan *task
	poolWorkers    map[string]int
	stop           chan struct{}
	stopped        int32 // Atomic flag to prevent new submissions
	maxWorkers     int
	currentWorkers int32
	queueSize      int64 // Atomic tracker for queue size
	successCount   int64 // Atomic tracker for successful tasks
	failureCount   int64 // Atomic tracker for failed tasks
	panicCount     int64 // Atomic tracker for panics caught
	defaultTimeout time.Duration
	retryStrategy  RetryStrategy
	middlewares    []Middleware
	logger         Logger
	storage        Storage
	registry       map[string]Job
	regMu          sync.RWMutex
	wg             sync.WaitGroup
	maxQueueSize   int
	activeKeys     map[string]struct{}
	instanceID     string
	leaseDuration  time.Duration
	loopWg         sync.WaitGroup
}

// TaskStatus represents the current state of a task.
type TaskStatus int

const (
	// StatusUnknown indicates the task state is unknown or finished.
	StatusUnknown TaskStatus = iota
	// StatusQueued indicates the task is in the priority queue waiting for a worker.
	StatusQueued
	// StatusRunning indicates the task is currently being executed.
	StatusRunning
	// StatusSucceeded indicates the task finished successfully.
	StatusSucceeded
	// StatusFailed indicates the task failed after all retry attempts.
	StatusFailed
	// StatusCancelled indicates the task was manually cancelled.
	StatusCancelled
)

func (s TaskStatus) String() string {
	return [...]string{"Unknown", "Queued", "Running", "Succeeded", "Failed", "Cancelled"}[s]
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
	status        TaskStatus
	progress      int
	pool          string
	typeName      string
	payload       []byte
	dependsOn     []string
}

// New creates and starts a new Scheduler with the provided options.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:         make(map[string]*task),
		poolQueues:    make(map[string]chan *task),
		poolWorkers:   make(map[string]int),
		queue:         make(taskQueue, 0),
		stop:          make(chan struct{}),
		maxWorkers:    5,
		maxQueueSize:  1000,
		registry:      make(map[string]Job),
		storage:       &InMemoryStorage{history: make([]TaskInfo, 0)},
		activeKeys:    make(map[string]struct{}),
		instanceID:    generateId(),
		leaseDuration: 30 * time.Second, // Default lease
	}
	s.cond = sync.NewCond(&s.mu)

	for _, opt := range opts {
		opt(s)
	}

	s.loadTasks()

	s.loopWg.Add(1)
	go s.dequeueLoop()

	s.loopWg.Add(1)
	go s.cleanupLoop()

	s.storage.SubscribeCancel(context.Background(), func(id string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if t, ok := s.tasks[id]; ok && t.cancel != nil {
			t.cancel()
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
		var t *task

		s.mu.Lock()
		if s.queue.Len() > 0 {
			t = heap.Pop(&s.queue).(*task)
			atomic.AddInt64(&s.queueSize, -1)
		}
		s.mu.Unlock()

		if t == nil {
			pt, err := s.storage.Dequeue(context.Background(), s.instanceID, s.leaseDuration)
			if err == nil && pt != nil {
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
		s.spawnWorkersForPool(pool, 1)
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
		case <-s.stop:
			return
		}
	}
}

type contextKey string

type progressFunc func(int)

const (
	taskIDKey   contextKey = "task_id"
	progressKey contextKey = "task_progress"
)

// TaskID returns the task ID associated with the context, if any.
func TaskID(ctx context.Context) string {
	if id, ok := ctx.Value(taskIDKey).(string); ok {
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
	// Individual task timeout or default scheduler timeout
	delay := t.timeout
	if delay == 0 {
		delay = s.defaultTimeout
	}

	var ctx context.Context
	var cancel context.CancelFunc

	updateProgress := func(p int) {
		s.mu.Lock()
		if trackTask, ok := s.tasks[t.id]; ok {
			trackTask.progress = p
		}
		s.mu.Unlock()
	}

	baseCtx := context.WithValue(context.Background(), taskIDKey, t.id)
	baseCtx = context.WithValue(baseCtx, progressKey, progressFunc(updateProgress))

	if delay > 0 {
		ctx, cancel = context.WithTimeout(baseCtx, delay)
	} else {
		ctx, cancel = context.WithCancel(baseCtx)
	}
	defer cancel()

	s.mu.Lock()
	if trackTask, ok := s.tasks[t.id]; ok {
		trackTask.cancel = cancel
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
		s.mu.Lock()
		if trackTask, ok := s.tasks[t.id]; ok {
			if err != nil {
				trackTask.status = StatusFailed
			} else {
				trackTask.status = StatusSucceeded
				trackTask.progress = 100 // Ensure 100% on success
			}
		}

		if t.key != "" {
			delete(s.activeKeys, t.key)
		}
		s.mu.Unlock()

		if err != nil {
			if s.logger != nil {
				s.logger.Error("task failed", err, "task_id", t.id)
			}
			s.handleFailure(t, err)
		} else {
			if t.typeName != "" {
				s.storage.Delete(context.Background(), t.id)
				// Resolve dependencies
				readyTasks, _ := s.storage.ResolveDependencies(context.Background(), t.id)
				for _, rt := range readyTasks {
					job, ok := s.getJob(rt.TypeName)
					if ok {
						jt := &task{
							id:        rt.ID,
							key:       rt.Key,
							typeName:  rt.TypeName,
							payload:   rt.Payload,
							priority:  rt.Priority,
							timeout:   rt.Timeout,
							attempts:  rt.Attempts,
							job:       job,
							status:    StatusQueued,
							dependsOn: nil, // Clear dependencies
						}
						s.Submit(jt)
					}
				}
			}
			atomic.AddInt64(&s.successCount, 1)
		}
	case <-ctx.Done():
		s.mu.Lock()
		if trackTask, ok := s.tasks[t.id]; ok {
			trackTask.status = StatusFailed
		}
		if t.key != "" {
			delete(s.activeKeys, t.key)
		}
		s.mu.Unlock()

		if s.logger != nil {
			s.logger.Error("task context cancelled or timed out", ctx.Err(), "task_id", t.id)
		}
		if t.typeName != "" {
			s.storage.Delete(context.Background(), t.id)
		}
		s.handleFailure(t, ctx.Err())
	}

	// For one-off tasks (not identified as recurring), clean up from the tracking map
	// However, recurring tasks create new task objects for each run, so we need careful cleaning.
	// Simple approach: After terminal state, remove from map.
	// Record history before removing from active tasks
	s.mu.Lock()
	s.recordHistory(t)
	delete(s.tasks, t.id)
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

	s.storage.AddToHistory(context.Background(), info)
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

func (s *Scheduler) handleFailure(t *task, err error) {
	atomic.AddInt64(&s.failureCount, 1)

	if t.retryStrategy != nil {
		delay, retry := t.retryStrategy.NextDelay(t.attempts)
		if retry {
			// Create a fresh task for the retry to avoid sharing state with the failed instance
			retryTask := NewTask(t.job, t.priority, t.timeout)
			retryTask.id = t.id // Preserve ID for tracking
			retryTask.attempts = t.attempts + 1
			retryTask.retryStrategy = t.retryStrategy

			time.AfterFunc(delay, func() {
				s.Submit(retryTask)
			})
		}
	}
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
			ID:        t.id,
			Key:       t.key,
			Pool:      t.pool,
			TypeName:  t.typeName,
			Payload:   t.payload,
			Priority:  t.priority,
			Timeout:   t.timeout,
			Attempts:  t.attempts,
			DependsOn: t.dependsOn,
		}

		if len(t.dependsOn) > 0 {
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

// DependsOn specifies task IDs that this task must wait for.
func (t *task) DependsOn(ids ...string) *task {
	t.dependsOn = ids
	return t
}

// ScheduleOnce schedules a job to run at a specific time.
func (s *Scheduler) ScheduleOnce(job Job, at time.Time, priority int) (string, error) {
	t := NewTask(job, priority, 0)
	return s.ScheduleOnceTask(t, at)
}

// ScheduleOnceTask schedules a task to run at a specific time.
// This allows providing a custom Task ID or key.
func (s *Scheduler) ScheduleOnceTask(t *task, at time.Time) (string, error) {
	if atomic.LoadInt32(&s.stopped) == 1 {
		return "", ErrSchedulerStopped
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	s.mu.Lock()
	s.tasks[t.id] = t
	s.mu.Unlock()

	delay := time.Until(at)
	timer := time.NewTimer(delay)

	s.loopWg.Add(1)
	go func() {
		defer s.loopWg.Done()
		defer cancel()
		select {
		case <-timer.C:
			s.Submit(t)
		case <-ctx.Done():
			timer.Stop()
		case <-s.stop:
			timer.Stop()
		}
	}()

	return t.id, nil
}

// ScheduleRecurring schedules a job to run at fixed intervals.
func (s *Scheduler) ScheduleRecurring(job Job, interval time.Duration, priority int) (string, error) {
	t := NewTask(job, priority, interval)
	return s.ScheduleRecurringTask(t, interval)
}

// ScheduleRecurringTask schedules a recurring task at fixed intervals.
// This allows providing a custom Task ID or key.
func (s *Scheduler) ScheduleRecurringTask(t *task, interval time.Duration) (string, error) {
	if atomic.LoadInt32(&s.stopped) == 1 {
		return "", ErrSchedulerStopped
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	s.mu.Lock()
	s.tasks[t.id] = t
	s.mu.Unlock()

	s.loopWg.Add(1)
	go func() {
		defer s.loopWg.Done()
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Each execution is a new task instance in the queue,
				// but we preserve the type etc.
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
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()

	return t.id, nil
}

func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if ok {
		if t.cancel != nil {
			t.cancel()
		}
		delete(s.tasks, id)
	}
	s.mu.Unlock()

	// Global cancel via storage
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
