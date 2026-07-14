package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipLimiter is a small per-IP token bucket for the unauthenticated redemption
// endpoint. It is best-effort abuse control, not a security boundary (the token
// is the real credential, and household IPs collapse behind CGNAT), so it is
// deliberately lenient: a burst of a few attempts, refilling slowly.
type ipLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newIPLimiter allows up to capacity attempts in a burst, refilling one token
// every refillEvery.
func newIPLimiter(capacity int, refillEvery time.Duration) *ipLimiter {
	return &ipLimiter{
		buckets:  map[string]*bucket{},
		capacity: float64(capacity),
		refill:   1 / refillEvery.Seconds(),
	}
}

// allow consumes a token for ip, returning false when the bucket is empty.
func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(l.capacity, b.tokens+now.Sub(b.last).Seconds()*l.refill)
	b.last = now

	// Opportunistic cleanup so the map does not grow without bound: drop
	// buckets that have fully refilled (i.e. have been idle long enough).
	if len(l.buckets) > 1024 {
		for k, v := range l.buckets {
			if now.Sub(v.last) > time.Hour {
				delete(l.buckets, k)
			}
		}
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP extracts the best-effort client address. Behind the Cloudflare
// tunnel and Traefik the real client is the leftmost X-Forwarded-For entry
// (Traefik is configured to trust these from the pod network); it falls back to
// the transport remote address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
