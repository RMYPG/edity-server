package server

import (
	"sync"
	"time"
)

// A RateLimit bounds how often each Site publishes (V5): Burst Publishes at
// once, and one more each Every after that. It keeps one Client's slip, or
// a runaway script, from spending the GitHub App's requests, which every
// Site of an installation shares, and from cutting a Release a minute.
type RateLimit struct {
	Burst int
	Every time.Duration
}

// DefaultRateLimit lets a Site publish ten times at once, and thirty times
// an hour after that.
var DefaultRateLimit = RateLimit{Burst: 10, Every: 2 * time.Minute}

// limiter holds a token bucket per Site, in memory: one server serves every
// Site, and a restart only forgives a Site its recent Publishes.
type limiter struct {
	limit RateLimit
	now   func() time.Time

	mu    sync.Mutex
	sites map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(limit RateLimit, now func() time.Time) *limiter {
	return &limiter{limit: limit, now: now, sites: map[string]*bucket{}}
}

// take spends one of site's Publishes, or returns how long until it has one.
func (l *limiter) take(site string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.sites[site]
	if !ok {
		b = &bucket{tokens: float64(l.limit.Burst), at: now}
		l.sites[site] = b
	}
	b.tokens = min(float64(l.limit.Burst), b.tokens+float64(now.Sub(b.at))/float64(l.limit.Every))
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) * float64(l.limit.Every)), false
}
