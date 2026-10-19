package auth

import (
	"sync"
	"time"
)

// LoginLimiter blocks a key (IP + username) after too many failed logins within a window.
type LoginLimiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	failures map[string][]time.Time
	now      func() time.Time
}

func NewLoginLimiter(max int, window time.Duration) *LoginLimiter {
	return &LoginLimiter{max: max, window: window, failures: map[string][]time.Time{}, now: time.Now}
}

// Allowed reports whether another attempt is permitted for key.
func (l *LoginLimiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) < l.max
}

func (l *LoginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.prune(key), l.now())
	if len(l.failures) > 10000 {
		for k := range l.failures {
			l.prune(k)
		}
	}
}

func (l *LoginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

func (l *LoginLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.failures[key][:0]
	for _, t := range l.failures[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = kept
	return kept
}
