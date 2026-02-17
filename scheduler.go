package skedulr

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
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

type Scheduler struct {
	mu             sync.Mutex
	cond           *sync.Cond
	queue          taskQueue
	tasks          map[string]*task
	jobQueue       chan task
	stop           chan struct{}
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
	wg             sync.WaitGroup
}

// SchedulerStats provides a snapshot of the scheduler's current state.
type SchedulerStats struct {
	QueueSize      int64
	SuccessCount   int64
	FailureCount   int64
	PanicCount     int64
	CurrentWorkers int32
}

type task struct {
	id            string
	job           Job
	timeout       time.Duration
	priority      int
	cancel        context.CancelFunc
	retryStrategy RetryStrategy
	attempts      int
}

// New creates a new Scheduler with the provided options.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:      make(map[string]*task),
		jobQueue:   make(chan task, 100),
		queue:      make(taskQueue, 0),
		stop:       make(chan struct{}),
		maxWorkers: 10, // Default max workers
		logger:     noopLogger{},
	}
	s.cond = sync.NewCond(&s.mu)

	for _, opt := range opts {
		opt(s)
	}

	go s.dequeueLoop()
	go s.scalingLoop()
	return s
}

func (s *Scheduler) dequeueLoop() {
	for {
		s.mu.Lock()
		for s.queue.Len() == 0 {
			select {
			case <-s.stop:
				s.mu.Unlock()
				return
			default:
				s.cond.Wait()
			}
		}

		select {
		case <-s.stop:
			s.mu.Unlock()
			return
		default:
			if s.queue.Len() > 0 {
				t := heap.Pop(&s.queue).(*task)
				atomic.AddInt64(&s.queueSize, -1)
				s.mu.Unlock()
				s.jobQueue <- *t
			} else {
				s.mu.Unlock()
			}
		}
	}
}

func (s *Scheduler) scalingLoop() {
	tick := time.NewTicker(1 * time.Second) // More frequent check
	defer tick.Stop()

	// Initial scale check
	s.checkScale()

	for {
		select {
		case <-tick.C:
			s.checkScale()
		case <-s.stop:
			return
		}
	}
}

func (s *Scheduler) checkScale() {
	currentQueueSize := int(atomic.LoadInt64(&s.queueSize))
	target := calculateWorkerPertTask(currentQueueSize)
	current := int(atomic.LoadInt32(&s.currentWorkers))

	if target > current && current < s.maxWorkers {
		s.mu.Lock()
		toSpawn := target - current
		if toSpawn+current > s.maxWorkers {
			toSpawn = s.maxWorkers - current
		}
		if toSpawn > 0 {
			s.spawnWorkers(toSpawn)
		}
		s.mu.Unlock()
	}
}

func (s *Scheduler) spawnWorkers(n int) {
	for i := 0; i < n; i++ {
		atomic.AddInt32(&s.currentWorkers, 1)
		s.wg.Add(1)
		go s.worker()
	}
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		select {
		case t, ok := <-s.jobQueue:
			if !ok {
				return
			}
			s.runTask(t)
		case <-s.stop:
			return
		}
	}
}

type contextKey string

const taskIDKey contextKey = "task_id"

// TaskID returns the task ID associated with the context, if any.
func TaskID(ctx context.Context) string {
	if id, ok := ctx.Value(taskIDKey).(string); ok {
		return id
	}
	return ""
}

func (s *Scheduler) runTask(t task) {
	// Individual task timeout or default scheduler timeout
	delay := t.timeout
	if delay == 0 {
		delay = s.defaultTimeout
	}

	var ctx context.Context
	var cancel context.CancelFunc

	baseCtx := context.WithValue(context.Background(), taskIDKey, t.id)
	if delay > 0 {
		ctx, cancel = context.WithTimeout(baseCtx, delay)
	} else {
		ctx, cancel = context.WithCancel(baseCtx)
	}
	defer cancel()

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
		if err != nil {
			s.logger.Error("task failed", err, "task_id", t.id)
			s.handleFailure(t, err)
		} else {
			atomic.AddInt64(&s.successCount, 1)
		}
	case <-ctx.Done():
		s.logger.Error("task context cancelled or timed out", ctx.Err(), "task_id", t.id)
		s.handleFailure(t, ctx.Err())
	}
}

func (s *Scheduler) handleFailure(t task, err error) {
	atomic.AddInt64(&s.failureCount, 1)

	if t.retryStrategy != nil {
		delay, retry := t.retryStrategy.NextDelay(t.attempts)
		if retry {
			t.attempts++
			// Schedule retry
			time.AfterFunc(delay, func() {
				s.Submit(&t)
			})
		}
	}
}

// Stats returns a snapshot of the scheduler's statistics.
func (s *Scheduler) Stats() SchedulerStats {
	return SchedulerStats{
		QueueSize:      atomic.LoadInt64(&s.queueSize),
		SuccessCount:   atomic.LoadInt64(&s.successCount),
		FailureCount:   atomic.LoadInt64(&s.failureCount),
		PanicCount:     atomic.LoadInt64(&s.panicCount),
		CurrentWorkers: atomic.LoadInt32(&s.currentWorkers),
	}
}

// Use adds middlewares to the scheduler.
func (s *Scheduler) Use(mw ...Middleware) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.middlewares = append(s.middlewares, mw...)
}

// NewTask creates a new task instance.
func NewTask(job Job, priority int, timeout time.Duration) *task {
	return &task{
		id:       generateId(),
		job:      job,
		priority: priority,
		timeout:  timeout,
	}
}

// Submit adds a task to the priority queue.
func (s *Scheduler) Submit(t *task) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t.retryStrategy == nil {
		t.retryStrategy = s.retryStrategy
	}

	heap.Push(&s.queue, t)
	atomic.AddInt64(&s.queueSize, 1)
	s.cond.Signal()
	return t.id
}

// ScheduleOnce schedules a job to run at a specific time.
func (s *Scheduler) ScheduleOnce(job Job, at time.Time, priority int) (string, error) {
	id := generateId()
	ctx, cancel := context.WithCancel(context.Background())

	t := &task{
		id:       id,
		job:      job,
		priority: priority,
		cancel:   cancel,
	}

	s.mu.Lock()
	s.tasks[id] = t
	s.mu.Unlock()

	delay := time.Until(at)
	timer := time.NewTimer(delay)

	go func() {
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

	return id, nil
}

// ScheduleRecurring schedules a job to run at fixed intervals.
func (s *Scheduler) ScheduleRecurring(job Job, interval time.Duration, priority int) (string, error) {
	id := generateId()
	ctx, cancel := context.WithCancel(context.Background())

	t := &task{
		id:       id,
		job:      job,
		priority: priority,
		cancel:   cancel,
	}

	s.mu.Lock()
	s.tasks[id] = t
	s.mu.Unlock()

	go func() {
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Each execution is a new task instance in the queue
				s.Submit(NewTask(job, priority, interval))
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
	}()

	return id, nil
}

// Cancel cancels a scheduled or recurring task.
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}

	if t.cancel != nil {
		t.cancel()
	}
	delete(s.tasks, id)
	return nil
}

// ShutDown gracefully stops the scheduler, waiting for active workers to finish.
func (s *Scheduler) ShutDown() error {
	s.mu.Lock()
	close(s.stop)
	s.cond.Broadcast()
	s.mu.Unlock()

	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		if t.cancel != nil {
			t.cancel()
		}
	}
	close(s.jobQueue)
	return nil
}
