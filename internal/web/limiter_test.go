package web

import (
	"testing"
	"time"
)

func TestAttemptLimiter(t *testing.T) {
	limiter := newAttemptLimiter(2, time.Minute)
	now := time.Now()
	if !limiter.Allow("client", now) || !limiter.Allow("client", now) {
		t.Fatal("initial attempts should pass")
	}
	if limiter.Allow("client", now) {
		t.Fatal("third attempt should be limited")
	}
	if !limiter.Allow("client", now.Add(2*time.Minute)) {
		t.Fatal("expired attempts should be removed")
	}
}
