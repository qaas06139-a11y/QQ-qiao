package proxy

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// =====================================================================
// Per-client rate limiting.
//
// We classify each incoming /v1 request by API key (when one is supplied)
// and fall back to client IP. Each key gets its own token bucket with a
// configurable per-minute quota and burst size. When the bucket is empty
// the proxy answers 429 with a Retry-After header.
//
// Buckets are stored in-process; this isn't multi-instance aware. Idle
// buckets are pruned by a background sweeper so memory stays bounded.
// =====================================================================

const (
	rateLimitIdleTTL    = 10 * time.Minute
	rateLimitSweepEvery = 2 * time.Minute
)

type tokenBucket struct {
	tokens     float64
	capacity   float64
	refillPerS float64
	last       time.Time
	updated    time.Time
}

// take attempts to consume 1 token. Returns true on success and the
// estimated number of seconds until the next token is available when it
// fails.
func (b *tokenBucket) take(now time.Time) (bool, float64) {
	if b.refillPerS <= 0 || b.capacity <= 0 {
		// Disabled or misconfigured: always allow.
		return true, 0
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillPerS
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	b.updated = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := 1 - b.tokens
	return false, missing / b.refillPerS
}

// rateLimiter manages per-client token buckets keyed by API key or IP.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket

	// Configuration; mutating these is safe under mu.
	enabled        bool
	requestsPerMin int
	burst          int

	sweepStarted bool
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
	}
}

// Configure updates the limiter parameters atomically. Buckets created
// before the change keep their state but adopt the new capacity / refill
// rate on their next request.
func (r *rateLimiter) Configure(enabled bool, requestsPerMin, burst int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
	r.requestsPerMin = requestsPerMin
	r.burst = burst
	if enabled && !r.sweepStarted {
		r.sweepStarted = true
		go r.sweepLoop()
	}
}

// Allow checks whether the given client may proceed with a request.
// Returns (allowed, retryAfterSeconds).
func (r *rateLimiter) Allow(clientKey string) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled || r.requestsPerMin <= 0 {
		return true, 0
	}
	now := time.Now()
	b, ok := r.buckets[clientKey]
	capacity := float64(r.burst)
	if capacity <= 0 {
		capacity = float64(r.requestsPerMin)
	}
	refillPerS := float64(r.requestsPerMin) / 60.0
	if !ok {
		b = &tokenBucket{
			tokens:     capacity,
			capacity:   capacity,
			refillPerS: refillPerS,
			last:       now,
			updated:    now,
		}
		r.buckets[clientKey] = b
	} else {
		// Live-adopt config changes.
		b.capacity = capacity
		b.refillPerS = refillPerS
	}

	ok, retry := b.take(now)
	if ok {
		return true, 0
	}
	wait := int(retry + 0.5)
	if wait < 1 {
		wait = 1
	}
	return false, wait
}

func (r *rateLimiter) sweepLoop() {
	ticker := time.NewTicker(rateLimitSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		r.sweepIdle()
	}
}

func (r *rateLimiter) sweepIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-rateLimitIdleTTL)
	for k, b := range r.buckets {
		if b.updated.Before(cutoff) {
			delete(r.buckets, k)
		}
	}
}

// clientRateKey builds the bucket key for a request: API key (if present)
// or a normalised client IP.
func clientRateKey(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return "key:" + k
	}
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		k := strings.TrimPrefix(v, "Bearer ")
		if k != "" {
			return "key:" + k
		}
	}
	return "ip:" + clientIP(r)
}

// clientIP extracts the best-effort remote address. Honours
// X-Forwarded-For when the proxy is fronted by another reverse proxy
// (nginx, cloudflare, etc.). Strips the port.
func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		// Take the first hop; subsequent entries can be forged.
		if comma := strings.IndexByte(xf, ','); comma > 0 {
			xf = xf[:comma]
		}
		if v := strings.TrimSpace(xf); v != "" {
			return v
		}
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// writeRateLimited writes a 429 response with Retry-After. Format depends
// on which API surface was hit (Claude / OpenAI / generic JSON).
func writeRateLimited(w http.ResponseWriter, retryAfter int, format string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	switch format {
	case "claude":
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded. Retry after ` + strconv.Itoa(retryAfter) + ` seconds."}}`))
	default: // openai-style
		w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"Rate limit exceeded. Retry after ` + strconv.Itoa(retryAfter) + ` seconds."}}`))
	}
}
