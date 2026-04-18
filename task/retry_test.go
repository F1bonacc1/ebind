package task

import (
	"math"
	"testing"
	"time"
)

func TestRetryPolicy_NextDelay_Zero(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, a := range []int{-1, 0} {
		if got := p.NextDelay(a); got != 0 {
			t.Errorf("NextDelay(%d) = %v, want 0", a, got)
		}
	}
}

func TestRetryPolicy_NextDelay_ExponentialGrowth(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    time.Hour,
	}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
	}
	for _, c := range cases {
		if got := p.NextDelay(c.attempt); got != c.want {
			t.Errorf("NextDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestRetryPolicy_NextDelay_CapAtMaximumInterval(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 10.0,
		MaximumInterval:    3 * time.Second,
	}
	if got := p.NextDelay(1); got != time.Second {
		t.Errorf("attempt 1: got %v, want 1s", got)
	}
	if got := p.NextDelay(2); got != 3*time.Second {
		t.Errorf("attempt 2: got %v, want 3s (capped)", got)
	}
	if got := p.NextDelay(10); got != 3*time.Second {
		t.Errorf("attempt 10: got %v, want 3s (capped)", got)
	}
}

func TestRetryPolicy_NextDelay_NoCap(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
	}
	if got := p.NextDelay(5); got != 16*time.Second {
		t.Errorf("got %v", got)
	}
}

func TestRetryPolicy_NextDelay_ZeroInitial(t *testing.T) {
	p := RetryPolicy{BackoffCoefficient: 2.0, MaximumInterval: time.Minute}
	if got := p.NextDelay(3); got != 0 {
		t.Errorf("zero initial: got %v, want 0", got)
	}
}

func TestRetryPolicy_NextDelay_ZeroCoefficient(t *testing.T) {
	// Coefficient defaults to 1.0 when <= 0: delay stays at InitialInterval.
	p := RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 0,
		MaximumInterval:    time.Minute,
	}
	if got := p.NextDelay(5); got != time.Second {
		t.Errorf("zero coefficient: got %v, want 1s", got)
	}
}

func TestRetryPolicy_NextDelay_OverflowCapped(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Hour,
		BackoffCoefficient: 1e10,
		MaximumInterval:    24 * time.Hour,
	}
	if got := p.NextDelay(1000); got != 24*time.Hour {
		t.Errorf("overflow got %v, want 24h", got)
	}
}

func TestRetryPolicy_NextDelay_InfinityHandled(t *testing.T) {
	p := RetryPolicy{
		InitialInterval:    time.Hour,
		BackoffCoefficient: math.Inf(1),
	}
	// Should not panic; should return MaxInt64 since no cap.
	got := p.NextDelay(5)
	if got <= 0 {
		t.Errorf("got %v, want positive huge value", got)
	}
}

func TestRetryPolicy_ShouldRetry_NilError(t *testing.T) {
	if DefaultRetryPolicy().ShouldRetry(nil, 0) {
		t.Error("nil err should not retry")
	}
}

func TestRetryPolicy_ShouldRetry_UnderLimit(t *testing.T) {
	p := RetryPolicy{MaximumAttempts: 5}
	err := &TaskError{Kind: "handler", Retryable: true}
	for attempt := 0; attempt < 5; attempt++ {
		if !p.ShouldRetry(err, attempt) {
			t.Errorf("attempt %d: should retry", attempt)
		}
	}
	if p.ShouldRetry(err, 5) {
		t.Error("attempt 5 at limit: should not retry")
	}
	if p.ShouldRetry(err, 6) {
		t.Error("attempt 6 over limit: should not retry")
	}
}

func TestRetryPolicy_ShouldRetry_Unlimited(t *testing.T) {
	p := RetryPolicy{MaximumAttempts: 0}
	err := &TaskError{Kind: "handler", Retryable: true}
	for _, attempt := range []int{0, 1, 100, 10000} {
		if !p.ShouldRetry(err, attempt) {
			t.Errorf("unlimited: attempt %d should retry", attempt)
		}
	}
}

func TestRetryPolicy_ShouldRetry_NonRetryableFlag(t *testing.T) {
	p := DefaultRetryPolicy()
	err := &TaskError{Kind: "handler", Retryable: false}
	if p.ShouldRetry(err, 0) {
		t.Error("err.Retryable=false: should not retry")
	}
}

func TestRetryPolicy_ShouldRetry_NonRetryableKinds(t *testing.T) {
	p := RetryPolicy{
		MaximumAttempts:        5,
		NonRetryableErrorKinds: []string{"validation", "auth"},
	}
	if p.ShouldRetry(&TaskError{Kind: "validation", Retryable: true}, 0) {
		t.Error("validation kind should not retry")
	}
	if p.ShouldRetry(&TaskError{Kind: "auth", Retryable: true}, 0) {
		t.Error("auth kind should not retry")
	}
	if !p.ShouldRetry(&TaskError{Kind: "handler", Retryable: true}, 0) {
		t.Error("handler kind should retry")
	}
}

func TestRetryPolicy_ShouldRetry_Precedence(t *testing.T) {
	// MaximumAttempts wins over NonRetryableErrorKinds (which wins over Retryable=true).
	p := RetryPolicy{
		MaximumAttempts:        2,
		NonRetryableErrorKinds: []string{"auth"},
	}
	err := &TaskError{Kind: "auth", Retryable: true}
	// Under limit but non-retryable kind.
	if p.ShouldRetry(err, 1) {
		t.Error("should not retry non-retryable kind even under limit")
	}
	// Over limit: also no retry regardless of kind.
	if p.ShouldRetry(&TaskError{Kind: "handler", Retryable: true}, 2) {
		t.Error("should not retry when at limit")
	}
}

func TestDefaultRetryPolicy_Shape(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaximumAttempts != 5 || p.InitialInterval != time.Second || p.BackoffCoefficient != 2.0 || p.MaximumInterval != time.Minute {
		t.Errorf("unexpected defaults: %+v", p)
	}
}

func TestNoRetryPolicy_Shape(t *testing.T) {
	p := NoRetryPolicy()
	if p.MaximumAttempts != 1 {
		t.Errorf("NoRetryPolicy MaximumAttempts = %d, want 1", p.MaximumAttempts)
	}
	err := &TaskError{Kind: "handler", Retryable: true}
	if p.ShouldRetry(err, 1) {
		t.Error("NoRetry at attempt 1 should not retry")
	}
}
