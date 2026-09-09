package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := NewIPRateLimiter(0.0001, 3) // effectively no refill during the test
	for i := range 3 {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("request %d within the burst was blocked", i+1)
		}
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("request past the burst was allowed")
	}
	// A different client keeps its own bucket.
	if !l.Allow("10.0.0.2") {
		t.Fatal("a second IP was blocked by the first IP's usage")
	}
}

func TestIPRateLimiterRefills(t *testing.T) {
	l := NewIPRateLimiter(100, 1) // 100/s: one token back every 10ms
	if !l.Allow("10.0.0.1") {
		t.Fatal("first request blocked")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("second immediate request allowed")
	}
	time.Sleep(20 * time.Millisecond)
	if !l.Allow("10.0.0.1") {
		t.Fatal("bucket did not refill")
	}
}

func TestIPRateLimiterMiddlewareReturns429(t *testing.T) {
	l := NewIPRateLimiter(0.0001, 1)
	var served int
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without a Retry-After header")
	}
	if served != 1 {
		t.Fatalf("handler ran %d times, want 1", served)
	}
}

// X-Forwarded-For is client-controlled; honouring it would let a single caller
// mint a fresh bucket per request.
func TestIPRateLimiterIgnoresForwardedHeader(t *testing.T) {
	l := NewIPRateLimiter(0.0001, 1)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	call := func(xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:54321"
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("1.2.3.4"); code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	if code := call("5.6.7.8"); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 despite the spoofed header", code)
	}
}

func TestIPRateLimiterEvictsIdleBuckets(t *testing.T) {
	l := NewIPRateLimiter(1, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.nowFunc = func() time.Time { return now }

	l.Allow("10.0.0.1")
	now = now.Add(2 * time.Hour) // past both gcPeriod and ttl
	l.Allow("10.0.0.2")

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, stale := l.buckets["10.0.0.1"]; stale {
		t.Fatal("idle bucket was not evicted")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("bucket count = %d, want 1", len(l.buckets))
	}
}
