package proxy

import (
	"container/list"
	"encoding/json"
	"sync"
	"time"
)

// =====================================================================
// Response session store.
//
// Powers OpenAI Responses-style multi-turn continuation: every successful
// turn is keyed by a server-issued `response_id` and stores the messages
// that were sent / received. A subsequent request with
// `previous_response_id = X` lets the proxy prepend the saved history
// without the client having to resend it.
//
// We expose the same continuation primitive on `/v1/chat/completions`
// (Chat clients can pass `previous_response_id` and read `response_id`
// from the response body / `X-Response-Id` header), so any OpenAI Chat
// client can ride the Responses session machinery.
//
// Sessions are in-memory + bounded by entry count and per-entry TTL.
// Operators that want durable session history can enable the request log
// instead.
// =====================================================================

const (
	defaultSessionTTL = 30 * time.Minute
	defaultSessionCap = 1000
	sessionSweepEvery = 5 * time.Minute
)

// SessionTurn captures one user/assistant exchange. The Content fields are
// stored as the polymorphic shape Claude/OpenAI accept (string or block
// list) so we can replay them unchanged.
type SessionTurn struct {
	UserContent      interface{}   // user-side content of the turn (may be []block or string)
	AssistantText    string        // assistant text reply (may be empty when only tool calls)
	ToolUses         []KiroToolUse // assistant tool_use calls in order
	UserToolResults  []interface{} // optional follow-up tool_result blocks for the next turn
}

type sessionEntry struct {
	id        string
	createdAt time.Time
	updatedAt time.Time
	parentID  string         // chain root for full history reconstruction
	turns     []SessionTurn  // turns appended at this node only
}

// sessionStore keeps a bounded LRU of response sessions keyed by response_id.
type sessionStore struct {
	mu       sync.Mutex
	entries  map[string]*list.Element // response_id -> list element
	order    *list.List               // LRU; front = most recently used
	cap      int
	ttl      time.Duration
	disabled bool

	sweeperOnce sync.Once
}

func newSessionStore(capacity int, ttl time.Duration) *sessionStore {
	if capacity <= 0 {
		capacity = defaultSessionCap
	}
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return &sessionStore{
		entries: make(map[string]*list.Element, capacity),
		order:   list.New(),
		cap:     capacity,
		ttl:     ttl,
	}
}

// ensureSweeper starts the background expiry sweeper exactly once.
func (s *sessionStore) ensureSweeper() {
	s.sweeperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(sessionSweepEvery)
			defer ticker.Stop()
			for range ticker.C {
				s.sweepExpired()
			}
		}()
	})
}

// SetEnabled toggles whether new turns get persisted. Existing entries are
// preserved either way.
func (s *sessionStore) SetEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.disabled = !enabled
	s.mu.Unlock()
}

// Save persists a turn under newID. parentID may be empty for a fresh
// session, otherwise it's the previous_response_id from the request and
// becomes the new entry's parent so we can rebuild the chain.
func (s *sessionStore) Save(newID, parentID string, turn SessionTurn) {
	if s == nil || newID == "" {
		return
	}
	s.ensureSweeper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return
	}
	now := time.Now()
	if elem, ok := s.entries[newID]; ok {
		entry := elem.Value.(*sessionEntry)
		entry.turns = append(entry.turns, turn)
		entry.updatedAt = now
		s.order.MoveToFront(elem)
		return
	}
	entry := &sessionEntry{
		id:        newID,
		parentID:  parentID,
		turns:     []SessionTurn{turn},
		createdAt: now,
		updatedAt: now,
	}
	elem := s.order.PushFront(entry)
	s.entries[newID] = elem
	for s.order.Len() > s.cap {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.evictLocked(oldest)
	}
}

func (s *sessionStore) evictLocked(elem *list.Element) {
	entry := elem.Value.(*sessionEntry)
	s.order.Remove(elem)
	delete(s.entries, entry.id)
}

// History walks the parent chain starting at id and returns the turns in
// chronological order (oldest first). Returns empty when id is unknown or
// expired. Touches each visited entry to extend its life.
func (s *sessionStore) History(id string) []SessionTurn {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-s.ttl)

	chain := make([]*sessionEntry, 0, 4)
	cursor := id
	visited := make(map[string]bool, 4)
	for cursor != "" && !visited[cursor] {
		visited[cursor] = true
		elem, ok := s.entries[cursor]
		if !ok {
			break
		}
		entry := elem.Value.(*sessionEntry)
		if entry.updatedAt.Before(cutoff) {
			break
		}
		chain = append(chain, entry)
		s.order.MoveToFront(elem)
		entry.updatedAt = now
		cursor = entry.parentID
	}

	if len(chain) == 0 {
		return nil
	}
	// chain is newest-first; flip and flatten turns.
	total := 0
	for _, e := range chain {
		total += len(e.turns)
	}
	out := make([]SessionTurn, 0, total)
	for i := len(chain) - 1; i >= 0; i-- {
		out = append(out, chain[i].turns...)
	}
	return out
}

// Drop removes a single session entry. Useful when the upstream response
// failed and the session shouldn't be reused.
func (s *sessionStore) Drop(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if elem, ok := s.entries[id]; ok {
		s.evictLocked(elem)
	}
}

// Len returns the current number of stored entries (for debugging/admin UI).
func (s *sessionStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// Clear empties the entire store.
func (s *sessionStore) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*list.Element, s.cap)
	s.order.Init()
}

func (s *sessionStore) sweepExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	for elem := s.order.Back(); elem != nil; {
		entry := elem.Value.(*sessionEntry)
		prev := elem.Prev()
		if entry.updatedAt.Before(cutoff) {
			s.evictLocked(elem)
		}
		elem = prev
	}
}

// prependOpenAIHistory turns a slice of stored SessionTurn (Claude shape)
// into the OpenAI Chat message slice form the Chat handler accepts. Each
// turn is rendered as up to three messages: user, assistant (text +
// optional tool_calls), and a `tool` follow-up if the previous turn left
// a tool_result waiting.
func prependOpenAIHistory(history []SessionTurn) []OpenAIMessage {
	if len(history) == 0 {
		return nil
	}
	out := make([]OpenAIMessage, 0, len(history)*2)
	for _, turn := range history {
		if turn.UserContent != nil {
			out = append(out, OpenAIMessage{Role: "user", Content: turn.UserContent})
		}
		assistant := OpenAIMessage{Role: "assistant"}
		if turn.AssistantText != "" {
			assistant.Content = turn.AssistantText
		}
		for _, tu := range turn.ToolUses {
			args, _ := json.Marshal(tu.Input)
			tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
			tc.Function.Name = tu.Name
			tc.Function.Arguments = string(args)
			assistant.ToolCalls = append(assistant.ToolCalls, tc)
		}
		if assistant.Content != nil || len(assistant.ToolCalls) > 0 {
			out = append(out, assistant)
		}
	}
	return out
}

// lastUserContentFromOpenAI walks an OpenAIRequest message list and
// returns the user-side content of the most recent user turn. Used to
// stash the turn into the session store so a later request with
// previous_response_id can replay it.
func lastUserContentFromOpenAI(messages []OpenAIMessage) interface{} {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return nil
}
