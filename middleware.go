package skedulr

import (
	"context"
	"fmt"
	"runtime/debug"
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
