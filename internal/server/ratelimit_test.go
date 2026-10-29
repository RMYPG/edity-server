package server

import (
	"testing"
	"time"
)

func TestASiteEarnsItsPublishesBack(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	l := newLimiter(RateLimit{Burst: 3, Every: time.Minute}, func() time.Time { return now })
	for range 3 {
		if _, ok := l.take("a"); !ok {
			t.Fatal("a burst of 3 was refused")
		}
	}
	if wait, ok := l.take("a"); ok || wait != time.Minute {
		t.Errorf("fourth: %v, %v, want a wait of a minute", wait, ok)
	}
	now = now.Add(30 * time.Second)
	if wait, ok := l.take("a"); ok || wait != 30*time.Second {
		t.Errorf("half a minute later: %v, %v, want a wait of 30s", wait, ok)
	}
	now = now.Add(30 * time.Second)
	if _, ok := l.take("a"); !ok {
		t.Error("a minute later, still refused")
	}
	// Idle for long, a Site earns back its burst and no more.
	now = now.Add(24 * time.Hour)
	for range 3 {
		if _, ok := l.take("a"); !ok {
			t.Fatal("after a day, a burst of 3 was refused")
		}
	}
	if _, ok := l.take("a"); ok {
		t.Error("after a day, more than the burst")
	}
	if _, ok := l.take("b"); !ok {
		t.Error("another Site shares the limit")
	}
}
