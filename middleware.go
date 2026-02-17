package skedulr

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// Middleware is a function that wraps a Job to add behavior before or after execution.
type Middleware func(Job) Job

// Recovery returns a middleware that captures panics within a job,
// logs them using the provided logger, and calls an optional onPanic function.
func Recovery(l Logger, onPanic func()) Middleware {
	return func(next Job) Job {
		return func(ctx context.Context) error {
			defer func() {
				if r := recover(); r != nil {
					if l != nil {
						l.Error("panic captured in job", fmt.Errorf("%v", r), "stack", string(debug.Stack()))
					}
					if onPanic != nil {
						onPanic()
					}
				}
			}()
			return next(ctx)
		}
	}
}

// Logging returns a middleware that logs the start and completion of a job.
func Logging(l Logger) Middleware {
	return func(next Job) Job {
		return func(ctx context.Context) error {
			if l == nil {
				return next(ctx)
			}
			id := TaskID(ctx)
			l.Info("task started", "task_id", id)
			start := time.Now()

			err := next(ctx)

			dur := time.Since(start)
			if err != nil {
				l.Error("task finished with error", err, "task_id", id, "duration", dur)
			} else {
				l.Info("task finished successfully", "task_id", id, "duration", dur)
			}
			return err
		}
	}
}
