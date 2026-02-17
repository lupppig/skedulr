package schedulr

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
	queue          taskQueue
	tasks          map[string]*task
	jobQueue       chan task
	stop           chan struct{}
	maxWorkers     int
	currentWorkers int32
	queueSize      int64 // Atomic tracker for queue size
	defaultTimeout time.Duration
	wg             sync.WaitGroup
}

type task struct {
	id       string
	job      Job
	timeout  time.Duration
	priority int
	cancel   context.CancelFunc
}

// New creates a new Scheduler with the provided options.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:      make(map[string]*task),
		jobQueue:   make(chan task, 100),
		queue:      make(taskQueue, 0),
		stop:       make(chan struct{}),
		maxWorkers: 10, // Default max workers
	}

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
		if s.queue.Len() > 0 {
			t := heap.Pop(&s.queue).(*task)
			atomic.AddInt64(&s.queueSize, -1)
			s.mu.Unlock()
			s.jobQueue <- *t
			continue
		}
		s.mu.Unlock()

		select {
		case <-s.stop:
			return
		case <-time.After(100 * time.Millisecond):
			// periodically check if no signal is used (Wait/Signal could be better but this is fine for now)
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

func (s *Scheduler) runTask(t task) {
	// Individual task timeout or default scheduler timeout
	delay := t.timeout
	if delay == 0 {
		delay = s.defaultTimeout
	}

	var ctx context.Context
	var cancel context.CancelFunc

	if delay > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), delay)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- t.job(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			// In production, we'd log this properly
		}
	case <-ctx.Done():
	}
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
	heap.Push(&s.queue, t)
	atomic.AddInt64(&s.queueSize, 1)
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
	close(s.stop)
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
