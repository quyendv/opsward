package app

import (
	"testing"
	"time"
)

func TestHourlyLimit(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	l := &hourly{max: 2, now: func() time.Time { return now }}
	for i, want := range []bool{true, true, false} {
		if got := l.take(); got != want {
			t.Fatalf("take %d = %v, want %v", i+1, got, want)
		}
	}
	now = now.Add(61 * time.Minute)
	if !l.take() {
		t.Fatal("the window did not roll")
	}
	if (&hourly{max: 0, now: time.Now}).take() {
		t.Fatal("maxPerHour 0 allowed one")
	}
}

// An investigation that failed before doing anything gives its slot back.
func TestHourlyRefund(t *testing.T) {
	l := &hourly{max: 1, now: time.Now}
	if !l.take() {
		t.Fatal("first take refused")
	}
	l.refund()
	if !l.take() {
		t.Fatal("the refunded slot is not free")
	}
}
