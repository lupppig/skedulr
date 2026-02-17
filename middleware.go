package skedulr

import (
	"context"
	"fmt"
	"runtime/debug"
)

// Middleware is a function that wraps a Job to add behavior before or after execution.
type Middleware func(Job) Job

// Recovery returns a middleware that captures panics within a job and
// calls the provided onPanic function.
func Recovery(onPanic func()) Middleware {
	return func(next Job) Job {
		return func(ctx context.Context) error {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[Recovery] Panic captured: %v\nStack: %s\n", r, debug.Stack())
					if onPanic != nil {
						onPanic()
					}
				}
			}()
			return next(ctx)
		}
	}
}
