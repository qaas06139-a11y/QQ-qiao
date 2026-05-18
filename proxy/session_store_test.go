package proxy

import (
	"testing"
	"time"
)

func TestSessionStoreSaveAndHistory(t *testing.T) {
	s := newSessionStore(10, time.Minute)
	s.Save("resp_1", "", SessionTurn{
		UserContent:   "hello",
		AssistantText: "hi there",
	})
	s.Save("resp_2", "resp_1", SessionTurn{
		UserContent:   "what's the time",
		AssistantText: "12:00",
	})

	hist := s.History("resp_2")
	if len(hist) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(hist))
	}
	if hist[0].AssistantText != "hi there" {
		t.Fatalf("first turn should be hi there, got %q", hist[0].AssistantText)
	}
	if hist[1].AssistantText != "12:00" {
		t.Fatalf("second turn should be 12:00, got %q", hist[1].AssistantText)
	}
}

func TestSessionStoreUnknownIdReturnsEmpty(t *testing.T) {
	s := newSessionStore(10, time.Minute)
	if got := s.History("nope"); len(got) != 0 {
		t.Fatalf("expected empty history for unknown id, got %d", len(got))
	}
}

func TestSessionStoreLRUEviction(t *testing.T) {
	s := newSessionStore(2, time.Minute)
	s.Save("r1", "", SessionTurn{AssistantText: "a"})
	s.Save("r2", "", SessionTurn{AssistantText: "b"})
	s.Save("r3", "", SessionTurn{AssistantText: "c"}) // pushes r1 out

	if h := s.History("r1"); len(h) != 0 {
		t.Fatalf("r1 should have been evicted, got %d turns", len(h))
	}
	if h := s.History("r3"); len(h) != 1 || h[0].AssistantText != "c" {
		t.Fatalf("r3 missing")
	}
}

func TestSessionStoreTTLExpiry(t *testing.T) {
	s := newSessionStore(10, 10*time.Millisecond)
	s.Save("r1", "", SessionTurn{AssistantText: "a"})
	time.Sleep(30 * time.Millisecond)
	if h := s.History("r1"); len(h) != 0 {
		t.Fatalf("expected expired entry to be skipped, got %d", len(h))
	}
}

func TestSessionStoreDisabledDropsWrites(t *testing.T) {
	s := newSessionStore(10, time.Minute)
	s.SetEnabled(false)
	s.Save("r1", "", SessionTurn{AssistantText: "a"})
	if h := s.History("r1"); len(h) != 0 {
		t.Fatalf("disabled store should drop writes")
	}
}

func TestSessionStoreDropAndClear(t *testing.T) {
	s := newSessionStore(10, time.Minute)
	s.Save("r1", "", SessionTurn{AssistantText: "a"})
	s.Save("r2", "", SessionTurn{AssistantText: "b"})
	s.Drop("r1")
	if h := s.History("r1"); len(h) != 0 {
		t.Fatalf("Drop should have removed r1")
	}
	if h := s.History("r2"); len(h) != 1 {
		t.Fatalf("r2 should still be there")
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("expected 1 entry after drop, got %d", got)
	}
	s.Clear()
	if got := s.Len(); got != 0 {
		t.Fatalf("expected 0 entries after Clear, got %d", got)
	}
}

func TestSessionStoreCycleSafe(t *testing.T) {
	// If a buggy chain ever points back at itself, History must not loop.
	s := newSessionStore(10, time.Minute)
	s.Save("r1", "r1", SessionTurn{AssistantText: "self-cycle"})
	hist := s.History("r1")
	if len(hist) != 1 {
		t.Fatalf("self-cycle should yield exactly one turn, got %d", len(hist))
	}
}

func TestPrependOpenAIHistoryShape(t *testing.T) {
	hist := []SessionTurn{
		{UserContent: "hi", AssistantText: "hello"},
		{
			UserContent:   "use a tool",
			AssistantText: "",
			ToolUses: []KiroToolUse{
				{ToolUseID: "call_1", Name: "ls", Input: map[string]interface{}{"dir": "/tmp"}},
			},
		},
	}
	out := prependOpenAIHistory(hist)
	if len(out) != 4 {
		t.Fatalf("expected 4 messages (user, assistant, user, assistant-tool), got %d", len(out))
	}
	if out[0].Role != "user" || out[1].Role != "assistant" {
		t.Fatalf("first pair must be user/assistant, got %s/%s", out[0].Role, out[1].Role)
	}
	if out[3].Role != "assistant" || len(out[3].ToolCalls) != 1 {
		t.Fatalf("expected assistant message with tool call, got %+v", out[3])
	}
	if out[3].ToolCalls[0].Function.Name != "ls" {
		t.Fatalf("tool name lost: %+v", out[3].ToolCalls[0])
	}
}
