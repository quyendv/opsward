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
