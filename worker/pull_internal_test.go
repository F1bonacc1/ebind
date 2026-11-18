package worker

import (
	"context"
	"testing"
	"time"
)

func TestSlots_AcquireRelease(t *testing.T) {
	s := newSlots(2)
	ctx := context.Background()
	for i := range 2 {
		if !s.acquire(ctx) {
			t.Fatalf("acquire %d failed with free slots", i)
		}
	}
	if got := s.inUse(); got != 2 {
		t.Fatalf("inUse = %d, want 2", got)
	}
	if s.contended() {
		t.Fatal("contended with no waiter")
	}
	s.release()
	if got := s.inUse(); got != 1 {
		t.Fatalf("inUse after release = %d, want 1", got)
	}
}

func TestSlots_ContendedWhileWaiting(t *testing.T) {
	s := newSlots(1)
	ctx := context.Background()
	if !s.acquire(ctx) {
		t.Fatal("acquire failed")
	}
	got := make(chan bool)
	go func() { got <- s.acquire(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for !s.contended() {
		if time.Now().After(deadline) {
			t.Fatal("waiter never reported as contended")
		}
		time.Sleep(time.Millisecond)
	}
	s.release()
	if !<-got {
		t.Fatal("waiter did not get the released slot")
	}
	if s.contended() {
		t.Fatal("still contended after the waiter got its slot")
	}
}

func TestSlots_AcquireFailsAfterCancel(t *testing.T) {
	s := newSlots(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.acquire(ctx) {
		t.Fatal("acquire succeeded on a canceled context")
	}
	if got := s.inUse(); got != 0 {
		t.Fatalf("canceled acquire holds %d slots", got)
	}
}

func TestSlots_BlockedAcquireUnblocksOnCancel(t *testing.T) {
	s := newSlots(1)
	if !s.acquire(context.Background()) {
		t.Fatal("acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool)
	go func() { got <- s.acquire(ctx) }()
	cancel()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("blocked acquire succeeded after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked acquire did not return after cancel")
	}
	if got := s.inUse(); got != 1 {
		t.Fatalf("inUse = %d, want 1 (only the original holder)", got)
	}
}

func TestSlots_TryAcquireTakesOnlyFreeSlots(t *testing.T) {
	s := newSlots(4)
	if !s.acquire(context.Background()) {
		t.Fatal("acquire failed")
	}
	if got := s.tryAcquire(10); got != 3 {
		t.Fatalf("tryAcquire(10) with 3 free = %d, want 3", got)
	}
	if got := s.tryAcquire(1); got != 0 {
		t.Fatalf("tryAcquire on full pool = %d, want 0", got)
	}
	s.releaseN(3)
	if got := s.inUse(); got != 1 {
		t.Fatalf("inUse after releaseN(3) = %d, want 1", got)
	}
}

func TestGrowBackoff(t *testing.T) {
	lo, hi := 50*time.Millisecond, time.Second
	cases := []struct{ cur, want time.Duration }{
		{0, lo},
		{lo / 2, lo},
		{lo, 2 * lo},
		{400 * time.Millisecond, 800 * time.Millisecond},
		{800 * time.Millisecond, hi},
		{hi, hi},
	}
	for _, c := range cases {
		if got := growBackoff(c.cur, lo, hi); got != c.want {
			t.Errorf("growBackoff(%v) = %v, want %v", c.cur, got, c.want)
		}
	}
}
