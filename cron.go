package skedulr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ScheduleCron parses a simple cron string and schedules the job.
// Supported: "minute hour day month weekday"
// "*" means all.
func (s *Scheduler) ScheduleCron(spec string, job Job, priority int) (string, error) {
	t := NewTask(job, priority, 0)
	return s.ScheduleCronTask(t, spec)
}

// ScheduleCronTask schedules a cron job using a provided task object.
// This allows providing a custom Task ID or key.
func (s *Scheduler) ScheduleCronTask(t *task, spec string) (string, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return "", fmt.Errorf("invalid cron spec: %s", spec)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	s.mu.Lock()
	s.tasks[t.id] = t
	s.mu.Unlock()

	go func() {
		defer cancel()
		for {
			now := time.Now().Truncate(time.Minute)
			next := s.nextExecution(now, fields)

			delay := time.Until(next)
			if delay <= 0 {
				delay = time.Minute // safety
			}

			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
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
				// Wait for the next minute to avoid double scheduling if next is very close
				time.Sleep(1 * time.Second)
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.stop:
				timer.Stop()
				return
			}
		}
	}()

	return t.id, nil
}

func (s *Scheduler) nextExecution(from time.Time, fields []string) time.Time {
	// Simple implementation: check next minutes one by one
	// This is not the most efficient but stays small and dependency-light.
	// For production we might optimize this if we had many cron jobs.
	curr := from.Add(time.Minute)
	for i := 0; i < 525600; i++ { // check up to a year
		if s.match(curr, fields) {
			return curr
		}
		curr = curr.Add(time.Minute)
	}
	return from.Add(time.Hour * 24 * 365)
}

func (s *Scheduler) match(t time.Time, fields []string) bool {
	return s.matchField(strconv.Itoa(t.Minute()), fields[0]) &&
		s.matchField(strconv.Itoa(t.Hour()), fields[1]) &&
		s.matchField(strconv.Itoa(t.Day()), fields[2]) &&
		s.matchField(strconv.Itoa(int(t.Month())), fields[3]) &&
		s.matchField(strconv.Itoa(int(t.Weekday())), fields[4])
}

func (s *Scheduler) matchField(val string, pattern string) bool {
	if pattern == "*" {
		return true
	}
	// Support comma separated values
	parts := strings.Split(pattern, ",")
	for _, p := range parts {
		if p == val {
			return true
		}
	}
	return false
}
