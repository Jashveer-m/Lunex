package auth

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IPRateLimiter is a per-IP token bucket held in process memory.
//
// TODO(phase-2): this is a single-instance limiter. It resets on restart and
// each replica keeps its own counters, so behind more than one API process the
// effective limit is N x the configured rate. Move to a shared store (Redis)
// when horizontal scaling lands — Redis is explicitly out of scope for Phase 1.
type IPRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*ipBucket
	rate     rate.Limit
	burst    int
	ttl      time.Duration
	lastGC   time.Time
	nowFunc  func() time.Time
	gcPeriod time.Duration
}

type ipBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewIPRateLimiter(r float64, burst int) *IPRateLimiter {
	return &IPRateLimiter{
		buckets:  make(map[string]*ipBucket),
		rate:     rate.Limit(r),
		burst:    burst,
		ttl:      time.Hour,
		gcPeriod: 10 * time.Minute,
		nowFunc:  time.Now,
	}
}

// Allow reports whether the key may make a request now.
func (l *IPRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	l.gcLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &ipBucket{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b.limiter.Allow()
}

// gcLocked drops idle buckets so the map cannot grow without bound.
func (l *IPRateLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.gcPeriod {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.ttl {
			delete(l.buckets, k)
		}
	}
}

// Middleware throttles requests by client IP and answers 429 with Retry-After.
func (l *IPRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			retryAfter := 1
			if l.rate > 0 {
				retryAfter = int(1/float64(l.rate)) + 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please slow down.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP uses the socket peer address only. X-Forwarded-For is deliberately
// ignored: it is attacker-controlled unless a trusted proxy is configured, and
// trusting it would let anyone reset their own rate-limit bucket at will.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
