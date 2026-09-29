package auth

import (
	"sync"
	"time"
)

// limiter implements docs/M6 第 7 节: a number of failures inside a window
// blocks the key for a while. It lives in memory: a Hub restart forgives
// everyone, which is acceptable for a throttle.
type limiter struct {
	mu        sync.Mutex
	now       func() time.Time
	entries   map[string]*limitEntry
	lastSweep time.Time
}

// limitMaxEntries bounds the table under a spray of keys; at the bound an
// unblocked entry makes room for the new key, so that a spray of addresses
// can neither grow the map nor lock everyone else out (复核第四轮 2).
const limitMaxEntries = 100000

type limitEntry struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

const (
	limitWindow   = 15 * time.Minute
	limitBlock    = 15 * time.Minute
	limitFailures = 10
)

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, entries: map[string]*limitEntry{}}
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	return e != nil && l.now().Before(e.blockedUntil)
}

// fail records a failure against the default threshold.
func (l *limiter) fail(key string) bool { return l.failN(key, limitFailures) }

// failN records a failure and reports whether the key just became blocked.
func (l *limiter) failN(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e := l.entries[key]
	if e == nil {
		l.evictLocked(now)
	}
	if e == nil || now.Sub(e.windowStart) > limitWindow {
		e = &limitEntry{windowStart: now}
		l.entries[key] = e
	}
	e.count++
	if e.count >= max {
		e.blockedUntil = now.Add(limitBlock)
		e.count, e.windowStart = 0, now
		return true
	}
	return false
}

// try reserves one attempt BEFORE the work is done: the attempt counts even
// while it is still running, so a burst of concurrent requests cannot all
// slip past the check (安全复核 H1). It reports whether the attempt may proceed;
// the one that exceeds max blocks the key.
func (l *limiter) try(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e := l.entries[key]
	if e != nil && now.Before(e.blockedUntil) {
		return false
	}
	if e == nil {
		l.evictLocked(now)
	}
	if e == nil || now.Sub(e.windowStart) > limitWindow {
		e = &limitEntry{windowStart: now}
		l.entries[key] = e
	}
	e.count++
	if e.count > max {
		e.blockedUntil = now.Add(limitBlock)
		e.count, e.windowStart = 0, now
		return false
	}
	return true
}

// evictLocked makes room for one more key when the table is full: expired
// windows go first (a full scan at most every 30 s), then any entry that is
// not blocked. A blocked key is never dropped: a spray of new addresses
// cannot lift a lock.
func (l *limiter) evictLocked(now time.Time) {
	if len(l.entries) < limitMaxEntries {
		return
	}
	if now.Sub(l.lastSweep) >= 30*time.Second {
		l.lastSweep = now
		for k, e := range l.entries {
			if now.After(e.blockedUntil) && now.Sub(e.windowStart) > limitWindow {
				delete(l.entries, k)
			}
		}
	}
	if len(l.entries) < limitMaxEntries {
		return
	}
	for k, e := range l.entries {
		if now.After(e.blockedUntil) {
			delete(l.entries, k)
			return
		}
	}
}

// sweepLocked bounds memory under a spray of keys (many usernames, many /64s).
func (l *limiter) sweepLocked(now time.Time) {
	if len(l.entries) < limitMaxEntries/2 || now.Sub(l.lastSweep) < 30*time.Second {
		return // a full scan at most every 30 s, and only when the table is big
	}
	l.lastSweep = now
	for k, e := range l.entries {
		if now.After(e.blockedUntil) && now.Sub(e.windowStart) > limitWindow {
			delete(l.entries, k)
		}
	}
}

// undo gives back one reserved attempt (a login that succeeded is not a failure).
func (l *limiter) undo(key string) {
	l.mu.Lock()
	if e := l.entries[key]; e != nil && e.count > 0 {
		e.count--
	}
	l.mu.Unlock()
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}
