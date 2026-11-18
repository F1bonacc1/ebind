package worker

import (
	"testing"
	"time"
)

func TestLeaseClock_ExpiresAckWaitAfterReceipt(t *testing.T) {
	t0 := time.Now()
	c := newLeaseClock(t0, 3*time.Second)
	if c.expired(t0.Add(2999 * time.Millisecond)) {
		t.Fatal("expired before AckWait passed")
	}
	if !c.expired(t0.Add(3 * time.Second)) {
		t.Fatal("not expired once AckWait passed")
	}
}

func TestLeaseClock_ConfirmExtendsFromSendTime(t *testing.T) {
	t0 := time.Now()
	c := newLeaseClock(t0, 3*time.Second)
	// Sent at +1s, acknowledged at +1.5s: the server stamped it somewhere in
	// between, so the lease runs from the send time.
	c.confirm(t0.Add(time.Second), t0.Add(1500*time.Millisecond))
	if got, want := c.expiry(), t0.Add(4*time.Second); !got.Equal(want) {
		t.Fatalf("expiry = %v after t0, want %v", got.Sub(t0), want.Sub(t0))
	}
	if c.expired(t0.Add(3500 * time.Millisecond)) {
		t.Fatal("expired although a heartbeat extended the lease")
	}
}

func TestLeaseClock_OlderConfirmDoesNotShorten(t *testing.T) {
	t0 := time.Now()
	c := newLeaseClock(t0, 3*time.Second)
	c.confirm(t0.Add(2*time.Second), t0.Add(2100*time.Millisecond))
	c.confirm(t0.Add(time.Second), t0.Add(2200*time.Millisecond))
	if got, want := c.expiry(), t0.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("expiry = %v after t0, want %v", got.Sub(t0), want.Sub(t0))
	}
}

func TestLeaseClock_LateAckDoesNotRevive(t *testing.T) {
	t0 := time.Now()
	c := newLeaseClock(t0, 3*time.Second)
	// Sent before expiry but acknowledged after it: JetStream may have
	// redelivered before it processed the heartbeat.
	c.confirm(t0.Add(2*time.Second), t0.Add(3100*time.Millisecond))
	if !c.lost {
		t.Fatal("a heartbeat acknowledged after expiry kept the lease")
	}
}

func TestOptions_HeartbeatIntervalDefaults(t *testing.T) {
	o := Options{AckWait: 9 * time.Second}
	o.setDefaults()
	if o.HeartbeatInterval != 3*time.Second {
		t.Errorf("HeartbeatInterval = %v, want AckWait/3 = 3s", o.HeartbeatInterval)
	}
	o = Options{}
	o.setDefaults()
	if o.HeartbeatInterval != 10*time.Second {
		t.Errorf("HeartbeatInterval with default AckWait = %v, want 10s", o.HeartbeatInterval)
	}
	o = Options{HeartbeatInterval: -1}
	o.setDefaults()
	if o.HeartbeatInterval != -1 {
		t.Errorf("negative HeartbeatInterval became %v; it must stay negative (disabled)", o.HeartbeatInterval)
	}
}

func TestLeaseClock_LostIsPermanent(t *testing.T) {
	t0 := time.Now()
	c := newLeaseClock(t0, time.Second)
	if !c.expired(t0.Add(2 * time.Second)) {
		t.Fatal("not expired")
	}
	c.confirm(t0.Add(2100*time.Millisecond), t0.Add(2200*time.Millisecond))
	if !c.expired(t0.Add(2300 * time.Millisecond)) {
		t.Fatal("a later heartbeat revived a lost lease")
	}
}
