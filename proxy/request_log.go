package proxy

import (
	"context"
	"sync"
	"time"
)

// =====================================================================
// In-memory request log.
//
// Records every successful/failed API call so the admin panel can show
// recent activity without having to grep through stdout. Bounded by a
// fixed-size ring buffer to keep memory predictable; once full the oldest
// entries are overwritten.
//
// Stored entirely in RAM by design. Operators that want durable history
// should pipe stdout to a log collector — persisting structured JSON
// per request would add disk pressure on busy installs and is rarely
// what users actually need.
// =====================================================================

// RequestLogEntry captures the salient details of a single proxy request.
// Fields are flat strings/ints so the JSON the admin panel consumes stays
// stable and small.
type RequestLogEntry struct {
	Timestamp    int64   `json:"timestamp"`     // Unix seconds when the request finished
	DurationMs   int64   `json:"durationMs"`    // Wall-clock duration of the proxy turn
	Endpoint     string  `json:"endpoint"`      // "claude" | "openai" | "responses"
	Method       string  `json:"method"`        // HTTP method, normally POST
	Path         string  `json:"path"`          // Request URL path
	Status       int     `json:"status"`        // Final HTTP status (or 0 if unknown)
	Streaming    bool    `json:"streaming"`     // Whether the response was streamed
	Model        string  `json:"model"`         // Model name as the client requested it
	ActualModel  string  `json:"actualModel"`   // Model name routed to Kiro after mapping
	AccountID    string  `json:"accountId"`     // Kiro account that served the turn
	AccountEmail string  `json:"accountEmail"`  // Email for friendlier display
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	Credits      float64 `json:"credits"`
	ClientKey    string  `json:"clientKey"` // "key:..." or "ip:..." (already truncated)
	Error        string  `json:"error,omitempty"`
}

// requestLog is a fixed-capacity ring buffer of recent request entries.
type requestLog struct {
	mu       sync.RWMutex
	buf      []RequestLogEntry
	cap      int
	next     int // index where the next Add will write
	full     bool
	disabled bool
}

func newRequestLog(capacity int) *requestLog {
	if capacity <= 0 {
		capacity = 500
	}
	return &requestLog{
		buf: make([]RequestLogEntry, capacity),
		cap: capacity,
	}
}

// Add records an entry. Drops silently if the log was disabled.
func (l *requestLog) Add(e RequestLogEntry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled {
		return
	}
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().Unix()
	}
	// Hard cap on stored error/text fields so a runaway upstream message
	// can't blow up memory.
	if len(e.Error) > 512 {
		e.Error = e.Error[:512] + "..."
	}
	if len(e.ClientKey) > 80 {
		e.ClientKey = e.ClientKey[:80]
	}
	l.buf[l.next] = e
	l.next = (l.next + 1) % l.cap
	if l.next == 0 {
		l.full = true
	}
}

// Snapshot returns up to limit most-recent entries (newest first). Pass 0
// for limit to get the full window.
func (l *requestLog) Snapshot(limit int) []RequestLogEntry {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.cap
	if !l.full {
		count = l.next
	}
	if count == 0 {
		return []RequestLogEntry{}
	}
	if limit <= 0 || limit > count {
		limit = count
	}
	result := make([]RequestLogEntry, 0, limit)
	// Walk backwards from the most-recently-written slot.
	for i := 0; i < limit; i++ {
		idx := (l.next - 1 - i + l.cap) % l.cap
		result = append(result, l.buf[idx])
	}
	return result
}

// FilteredSnapshot returns recent entries filtered by predicates that are
// commonly useful from the admin UI. accountID/endpoint/onlyErrors empty
// means "no filter on this field".
func (l *requestLog) FilteredSnapshot(limit int, accountID, endpoint string, onlyErrors bool) []RequestLogEntry {
	all := l.Snapshot(0)
	if len(all) == 0 {
		return all
	}
	out := make([]RequestLogEntry, 0, limit)
	for _, e := range all {
		if accountID != "" && e.AccountID != accountID {
			continue
		}
		if endpoint != "" && e.Endpoint != endpoint {
			continue
		}
		if onlyErrors && e.Error == "" && e.Status < 400 {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Resize reallocates the underlying buffer to a new capacity, preserving
// existing entries (newest-first up to the new size). Allows the admin
// panel to enlarge / shrink the buffer without restarting the process.
func (l *requestLog) Resize(newCap int) {
	if newCap <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	existing := make([]RequestLogEntry, 0, l.cap)
	count := l.cap
	if !l.full {
		count = l.next
	}
	for i := 0; i < count; i++ {
		idx := (l.next - 1 - i + l.cap) % l.cap
		existing = append(existing, l.buf[idx])
	}

	l.buf = make([]RequestLogEntry, newCap)
	l.cap = newCap
	l.next = 0
	l.full = false
	// Restore newest-last so the most recent entries end up at the end of
	// the buffer and Snapshot returns them first.
	for i := len(existing) - 1; i >= 0 && (l.full || l.next < newCap); i-- {
		l.buf[l.next] = existing[i]
		l.next = (l.next + 1) % l.cap
		if l.next == 0 {
			l.full = true
		}
	}
}

// Clear empties the ring buffer.
func (l *requestLog) Clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.buf {
		l.buf[i] = RequestLogEntry{}
	}
	l.next = 0
	l.full = false
}

// SetEnabled toggles whether new entries get recorded. Existing buffer is
// preserved either way.
func (l *requestLog) SetEnabled(enabled bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.disabled = !enabled
	l.mu.Unlock()
}

// =====================================================================
// Per-request context.
//
// Sub-handlers (Claude / OpenAI / Responses, both streaming and non-
// streaming) update this struct as they progress. ServeHTTP installs a
// fresh one on every public-API call and writes the final RequestLogEntry
// from it on the way out, so individual handlers don't have to wire log
// calls into every error path.
// =====================================================================

type ctxKey int

const requestCtxKey ctxKey = 1

type requestCtx struct {
	startedAt    time.Time
	endpoint     string // "claude" | "openai" | "responses"
	model        string
	actualModel  string
	streaming    bool
	accountID    string
	accountEmail string
	inputTokens  int
	outputTokens int
	credits      float64
	clientKey    string
	err          string
}

func newRequestCtx(endpoint, clientKey string) *requestCtx {
	return &requestCtx{
		startedAt: time.Now(),
		endpoint:  endpoint,
		clientKey: clientKey,
	}
}

func withRequestCtx(parent context.Context, rc *requestCtx) context.Context {
	return context.WithValue(parent, requestCtxKey, rc)
}

func getRequestCtx(ctx context.Context) *requestCtx {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(requestCtxKey).(*requestCtx); ok {
		return v
	}
	return nil
}
