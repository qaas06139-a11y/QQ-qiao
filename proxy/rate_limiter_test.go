package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterDisabledAllowsAll(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < 100; i++ {
		ok, _ := rl.Allow("ip:1.2.3.4")
		if !ok {
			t.Fatalf("disabled limiter denied request %d", i)
		}
	}
}

func TestRateLimiterEnforcesBurstThenRefuses(t *testing.T) {
	rl := newRateLimiter()
	rl.Configure(true, 60, 3) // 60 rpm, burst 3

	for i := 0; i < 3; i++ {
		ok, _ := rl.Allow("key:foo")
		if !ok {
			t.Fatalf("burst rejected request %d unexpectedly", i)
		}
	}
	ok, retry := rl.Allow("key:foo")
	if ok {
		t.Fatalf("expected 4th request to be rate-limited")
	}
	if retry < 1 {
		t.Fatalf("expected positive retry-after, got %d", retry)
	}
}

func TestRateLimiterIsolatesClients(t *testing.T) {
	rl := newRateLimiter()
	rl.Configure(true, 60, 1)

	if ok, _ := rl.Allow("a"); !ok {
		t.Fatalf("a should be allowed once")
	}
	if ok, _ := rl.Allow("a"); ok {
		t.Fatalf("a should be rate-limited on second call")
	}
	if ok, _ := rl.Allow("b"); !ok {
		t.Fatalf("b should be unaffected by a's bucket")
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	rl := newRateLimiter()
	rl.Configure(true, 60, 1) // 1 token / sec

	if ok, _ := rl.Allow("key:slow"); !ok {
		t.Fatalf("first allow should succeed")
	}
	if ok, _ := rl.Allow("key:slow"); ok {
		t.Fatalf("second allow should be rate-limited")
	}
	// Manually advance the bucket by mutating last/updated to simulate >1s elapsed.
	rl.mu.Lock()
	if b, ok := rl.buckets["key:slow"]; ok {
		b.last = b.last.Add(-2 * time.Second)
	}
	rl.mu.Unlock()
	if ok, _ := rl.Allow("key:slow"); !ok {
		t.Fatalf("after refill the request should be allowed")
	}
}

func TestClientRateKeyPrefersApiKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Api-Key", "sk-abc")
	r.RemoteAddr = "9.9.9.9:1234"
	if got := clientRateKey(r); got != "key:sk-abc" {
		t.Fatalf("expected key:sk-abc, got %q", got)
	}
}

func TestClientRateKeyFallsBackToBearer(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-xyz")
	if got := clientRateKey(r); got != "key:sk-xyz" {
		t.Fatalf("expected key:sk-xyz, got %q", got)
	}
}

func TestClientRateKeyFallsBackToIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.RemoteAddr = "10.0.0.5:9000"
	if got := clientRateKey(r); got != "ip:10.0.0.5" {
		t.Fatalf("expected ip:10.0.0.5, got %q", got)
	}
}

func TestClientIPHonoursForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	r.RemoteAddr = "127.0.0.1:1"
	if got := clientIP(r); got != "1.1.1.1" {
		t.Fatalf("expected 1.1.1.1, got %q", got)
	}
}

func TestWriteRateLimitedSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRateLimited(rec, 7, "openai")
	res := rec.Result()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("expected Retry-After=7, got %q", got)
	}
}
