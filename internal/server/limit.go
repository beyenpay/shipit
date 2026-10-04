package server

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	requestsPerSecond = 10
	requestBurst      = 20
	maxAuthFailures   = 10 // per IP, per failure window
	failureWindow     = time.Minute
	maxTrackedIPs     = 10000
	idleIPAfter       = 10 * time.Minute
)

// limiter rate-limits requests per client IP. Each IP gets a token bucket for
// all requests, plus a separate, much stricter allowance for failed
// authentication: once an IP has failed maxAuthFailures times within the
// window it is refused (even with a valid signature) until the window ends.
//
// It is per IP on purpose. A single global bucket would let anyone who can
// reach the port exhaust it and lock the real CI out.
type limiter struct {
	mu  sync.Mutex
	ips map[string]*ipState

	rate  rate.Limit
	burst int
	now   func() time.Time
}

type ipState struct {
	bucket    *rate.Limiter
	failures  int
	windowEnd time.Time
	seen      time.Time
}

func newLimiter() *limiter {
	return &limiter{
		ips:   make(map[string]*ipState),
		rate:  requestsPerSecond,
		burst: requestBurst,
		now:   time.Now,
	}
}

// allow reports whether a request from ip may proceed.
func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.state(ip, now)
	if st == nil {
		return false // table full of recently active IPs: fail closed
	}
	if st.failures >= maxAuthFailures {
		if now.Before(st.windowEnd) {
			return false
		}
		st.failures = 0
	}
	return st.bucket.AllowN(now, 1)
}

// fail records a failed authentication attempt from ip.
func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.state(ip, now)
	if st == nil {
		return
	}
	if st.failures == 0 || !now.Before(st.windowEnd) {
		st.failures = 0
		st.windowEnd = now.Add(failureWindow)
	}
	st.failures++
}

func (l *limiter) state(ip string, now time.Time) *ipState {
	if st, ok := l.ips[ip]; ok {
		st.seen = now
		return st
	}
	if len(l.ips) >= maxTrackedIPs {
		for k, st := range l.ips {
			if now.Sub(st.seen) > idleIPAfter {
				delete(l.ips, k)
			}
		}
		if len(l.ips) >= maxTrackedIPs {
			return nil
		}
	}
	st := &ipState{bucket: rate.NewLimiter(l.rate, l.burst), seen: now}
	l.ips[ip] = st
	return st
}
