package task

import (
	"math"
	"slices"
	"time"
)

// RetryPolicy configures exponential backoff + attempt limits + non-retryable
// error kinds. All methods are pure and safe for concurrent use.
type RetryPolicy struct {
	InitialInterval        time.Duration `json:"initial_interval"`
	BackoffCoefficient     float64       `json:"backoff_coefficient"`
	MaximumInterval        time.Duration `json:"maximum_interval"`
	MaximumAttempts        int           `json:"maximum_attempts"` // 0 = unlimited
	NonRetryableErrorKinds []string      `json:"non_retryable_error_kinds,omitempty"`
}

// DefaultRetryPolicy is a reasonable default matching common task-queue behavior.
// 5 attempts, 1s → 2s → 4s → 8s → 16s (capped at 60s).
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    time.Minute,
		MaximumAttempts:    5,
	}
}

// NoRetryPolicy disables retries — any failure terminates the task immediately.
func NoRetryPolicy() RetryPolicy {
	return RetryPolicy{MaximumAttempts: 1}
}

// NextDelay returns the wait before the Nth retry delivery (1-based attempt count).
// Returns 0 for attempt <= 0. Capped by MaximumInterval. Deterministic — no jitter;
// callers that want jitter can wrap this.
func (p RetryPolicy) NextDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	initial := p.InitialInterval
	if initial <= 0 {
		return 0
	}
	coeff := p.BackoffCoefficient
	if coeff <= 0 {
		coeff = 1
	}
	// delay = initial * coeff^(attempt-1)
	raw := float64(initial) * math.Pow(coeff, float64(attempt-1))
	if math.IsInf(raw, 1) || raw > float64(math.MaxInt64) {
		if p.MaximumInterval > 0 {
			return p.MaximumInterval
		}
		return time.Duration(math.MaxInt64)
	}
	d := time.Duration(raw)
	if p.MaximumInterval > 0 && d > p.MaximumInterval {
		return p.MaximumInterval
	}
	return d
}

// ShouldRetry reports whether a failed attempt warrants another delivery.
// Rules (short-circuit in order):
//  1. Unlimited attempts (MaximumAttempts == 0) with retryable err → true
//  2. Attempt already reached MaximumAttempts → false
//  3. err is marked non-retryable → false
//  4. err.Kind appears in NonRetryableErrorKinds → false
//  5. Otherwise → true
func (p RetryPolicy) ShouldRetry(err *TaskError, attempt int) bool {
	if err == nil {
		return false
	}
	if p.MaximumAttempts > 0 && attempt >= p.MaximumAttempts {
		return false
	}
	if !err.Retryable {
		return false
	}
	if slices.Contains(p.NonRetryableErrorKinds, err.Kind) {
		return false
	}
	return true
}
