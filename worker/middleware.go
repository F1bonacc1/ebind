package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// Handler is the terminal operation in the middleware chain: decode payload, run the
// registered function, return the JSON-encoded result (or nil for error-only handlers).
type Handler func(ctx context.Context, t *task.Task) ([]byte, error)

// Middleware wraps a Handler to add cross-cutting behavior.
type Middleware func(Handler) Handler

// Chain composes middlewares left-to-right (first in slice runs outermost).
func Chain(mws ...Middleware) Middleware {
	return func(final Handler) Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			final = mws[i](final)
		}
		return final
	}
}

// Recover converts panics in downstream handlers into TaskError{Kind:"panic"}.
// Always register this as the outermost middleware.
func Recover() Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, t *task.Task) (result []byte, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = &task.TaskError{
						Kind:      task.ErrKindPanic,
						Message:   fmt.Sprintf("%v", r),
						Retryable: false,
					}
				}
			}()
			return next(ctx, t)
		}
	}
}

// Log emits a structured log line at start, end, and on error.
func Log(log *slog.Logger) Middleware {
	if log == nil {
		log = slog.Default()
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, t *task.Task) ([]byte, error) {
			start := time.Now()
			log.InfoContext(ctx, "task.start",
				"task_id", t.ID, "name", t.Name, "attempt", t.Attempt,
			)
			result, err := next(ctx, t)
			dur := time.Since(start)
			if err != nil {
				log.ErrorContext(ctx, "task.error",
					"task_id", t.ID, "name", t.Name, "attempt", t.Attempt,
					"duration_ms", dur.Milliseconds(), "error", err.Error(),
				)
			} else {
				log.InfoContext(ctx, "task.done",
					"task_id", t.ID, "name", t.Name, "attempt", t.Attempt,
					"duration_ms", dur.Milliseconds(),
				)
			}
			return result, err
		}
	}
}

// Metrics is a minimal hook interface; wire it to Prometheus, OpenTelemetry, etc.
// Observe is called exactly once per task dispatch with success or error outcome.
type Metrics interface {
	Observe(name string, attempt int, duration time.Duration, err error)
}

// WithMetrics returns a middleware that calls m.Observe after each dispatch.
func WithMetrics(m Metrics) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, t *task.Task) ([]byte, error) {
			start := time.Now()
			result, err := next(ctx, t)
			m.Observe(t.Name, t.Attempt, time.Since(start), err)
			return result, err
		}
	}
}
