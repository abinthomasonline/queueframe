package web

import (
	"sync"
	"time"
)

type attemptLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	items       map[string][]time.Time
	lastCleanup time.Time
	maxKeys     int
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{limit: limit, window: window, items: make(map[string][]time.Time), maxKeys: 1000}
}

func (l *attemptLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	if l.lastCleanup.IsZero() || now.Sub(l.lastCleanup) >= l.window {
		for itemKey, attempts := range l.items {
			kept := attempts[:0]
			for _, attempt := range attempts {
				if attempt.After(cutoff) {
					kept = append(kept, attempt)
				}
			}
			if len(kept) == 0 {
				delete(l.items, itemKey)
			} else {
				l.items[itemKey] = kept
			}
		}
		l.lastCleanup = now
	}
	if _, exists := l.items[key]; !exists && len(l.items) >= l.maxKeys {
		return false
	}
	attempts := l.items[key]
	kept := attempts[:0]
	for _, attempt := range attempts {
		if attempt.After(cutoff) {
			kept = append(kept, attempt)
		}
	}
	if len(kept) >= l.limit {
		l.items[key] = kept
		return false
	}
	l.items[key] = append(kept, now)
	return true
}

func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	delete(l.items, key)
	l.mu.Unlock()
}
