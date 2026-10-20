package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// loginLimiter is a per-key token bucket (key = client IP + email). In-memory,
// so it is per-process; fine for a single-instance cluster deployment.
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	limit   rate.Limit
	burst   int
	now     func() time.Time
}

type limiterEntry struct {
	l    *rate.Limiter
	seen time.Time
}

// newLoginLimiter allows `burst` attempts, then one every `every`.
func newLoginLimiter(burst int, every time.Duration) *loginLimiter {
	return &loginLimiter{
		entries: make(map[string]*limiterEntry),
		limit:   rate.Every(every),
		burst:   burst,
		now:     time.Now,
	}
}

// Allow consumes one attempt for key and reports whether it is permitted.
func (l *loginLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evict(now)
	e, ok := l.entries[key]
	if !ok {
		e = &limiterEntry{l: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = e
	}
	e.seen = now
	return e.l.AllowN(now, 1)
}

// evict drops idle entries so the map cannot grow without bound.
func (l *loginLimiter) evict(now time.Time) {
	if len(l.entries) < 1024 {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.seen) > time.Hour {
			delete(l.entries, k)
		}
	}
}
