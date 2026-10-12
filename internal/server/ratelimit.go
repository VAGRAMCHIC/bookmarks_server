// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"sync"
	"time"
)

// rateLimiter allows at most limit events per window for each key, using a
// sliding log of event times.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu     sync.Mutex
	events map[string][]time.Time
	sweep  time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, events: map[string][]time.Time{}}
}

// allow records an event for key at now. If the limit is exceeded it
// returns false and how long to wait.
func (l *rateLimiter) allow(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	if now.Sub(l.sweep) > l.window {
		for k, ev := range l.events {
			if len(ev) == 0 || !ev[len(ev)-1].After(cutoff) {
				delete(l.events, k)
			}
		}
		l.sweep = now
	}
	ev := l.events[key]
	i := 0
	for i < len(ev) && !ev[i].After(cutoff) {
		i++
	}
	ev = ev[i:]
	if len(ev) >= l.limit {
		l.events[key] = ev
		return ev[0].Sub(cutoff), false
	}
	l.events[key] = append(ev, now)
	return 0, true
}
