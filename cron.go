package skedulr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ScheduleCron schedules job to run on every cron-spec match. The spec is the
// classic five-field form "minute hour day month weekday" where each field is
// "*" (any) or a comma-separated list of integer values. Returns the task ID
// used to Cancel the recurrence.
func (s *Scheduler) ScheduleCron(spec string, job Job, priority int) (string, error) {
	t := NewTask(job, priority, 0)
	return s.ScheduleCronTask(t, spec)
}

// ScheduleCronTask is ScheduleCron with a caller-supplied task — use it when
// you need to set a custom ID, key, payload, or retry strategy on the cron job
// itself. The fire schedule lives in the timing wheel as a self-rescheduling
// entry: each fire submits a fresh execution task and re-arms itself for the
// next matching minute. Cancel(t.id) drops the pending arming via the wheel's
// per-ID index.
func (s *Scheduler) ScheduleCronTask(t *task, spec string) (string, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return "", fmt.Errorf("invalid cron spec: %s", spec)
	}

	// The cancel func is wired onto the task purely so Scheduler.Cancel can
	// surface "this task is cancelled" through the normal context path; the
	// returned context itself isn't used by the wheel-driven fire loop.
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
		// Compute next match from the current minute floor; nextExecution
		// always returns a strictly-greater time, so we won't double-fire
		// the minute we just matched.
		next := s.nextExecution(time.Now().Truncate(time.Minute), fields)
		_ = s.wheel.Schedule(t.id, next, fire)
	}

	first := s.nextExecution(time.Now().Truncate(time.Minute), fields)
	if err := s.wheel.Schedule(t.id, first, fire); err != nil {
		return "", err
	}
	return t.id, nil
}

// nextExecution walks forward one minute at a time from `from` and returns the
// first minute that matches the cron fields. Bounded at one year of search
// (525,600 minutes) so an unsatisfiable spec returns a far-future time instead
// of looping forever — cheap and dependency-free, which is the priority here
// over a more sophisticated cron expression parser.
func (s *Scheduler) nextExecution(from time.Time, fields []string) time.Time {
	curr := from.Add(time.Minute)
	for i := 0; i < 525600; i++ {
		if s.match(curr, fields) {
			return curr
		}
		curr = curr.Add(time.Minute)
	}
	return from.Add(time.Hour * 24 * 365)
}

// match reports whether t satisfies every field of the parsed cron spec.
// Field order is the standard minute, hour, day-of-month, month, day-of-week.
func (s *Scheduler) match(t time.Time, fields []string) bool {
	return s.matchField(strconv.Itoa(t.Minute()), fields[0]) &&
		s.matchField(strconv.Itoa(t.Hour()), fields[1]) &&
		s.matchField(strconv.Itoa(t.Day()), fields[2]) &&
		s.matchField(strconv.Itoa(int(t.Month())), fields[3]) &&
		s.matchField(strconv.Itoa(int(t.Weekday())), fields[4])
}

// matchField reports whether val satisfies a single cron-field pattern.
// Supported syntax: "*" (any value) or a comma-separated list of literals.
// Ranges and step expressions are deliberately not implemented — keeping the
// parser tiny is the design choice.
func (s *Scheduler) matchField(val string, pattern string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, ",")
	for _, p := range parts {
		if p == val {
			return true
		}
	}
	return false
}
