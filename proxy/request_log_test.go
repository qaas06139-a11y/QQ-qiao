package proxy

import (
	"testing"
)

func TestRequestLogAddSnapshotOrder(t *testing.T) {
	rl := newRequestLog(3)
	for i := 1; i <= 4; i++ {
		rl.Add(RequestLogEntry{Path: "/v1/messages", Status: 200 + i})
	}
	got := rl.Snapshot(0)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries (capacity), got %d", len(got))
	}
	// Newest-first ordering: latest Add was 204 then 203 then 202.
	if got[0].Status != 204 || got[1].Status != 203 || got[2].Status != 202 {
		t.Fatalf("unexpected order: %+v", got)
	}
}

func TestRequestLogSnapshotLimit(t *testing.T) {
	rl := newRequestLog(10)
	for i := 0; i < 5; i++ {
		rl.Add(RequestLogEntry{Status: 200 + i})
	}
	if got := rl.Snapshot(2); len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got := rl.Snapshot(0); len(got) != 5 {
		t.Fatalf("limit=0 should return all 5, got %d", len(got))
	}
	if got := rl.Snapshot(100); len(got) != 5 {
		t.Fatalf("limit > size should return all 5, got %d", len(got))
	}
}

func TestRequestLogFilters(t *testing.T) {
	rl := newRequestLog(10)
	rl.Add(RequestLogEntry{Endpoint: "claude", AccountID: "a", Status: 200})
	rl.Add(RequestLogEntry{Endpoint: "openai", AccountID: "b", Status: 200})
	rl.Add(RequestLogEntry{Endpoint: "openai", AccountID: "a", Status: 500, Error: "boom"})
	rl.Add(RequestLogEntry{Endpoint: "responses", AccountID: "c", Status: 200})

	if got := rl.FilteredSnapshot(0, "", "openai", false); len(got) != 2 {
		t.Fatalf("expected 2 openai entries, got %d", len(got))
	}
	if got := rl.FilteredSnapshot(0, "a", "", false); len(got) != 2 {
		t.Fatalf("expected 2 entries for account a, got %d", len(got))
	}
	if got := rl.FilteredSnapshot(0, "", "", true); len(got) != 1 {
		t.Fatalf("expected 1 error entry, got %d", len(got))
	}
}

func TestRequestLogResizePreservesNewest(t *testing.T) {
	rl := newRequestLog(5)
	for i := 1; i <= 5; i++ {
		rl.Add(RequestLogEntry{Status: i})
	}
	rl.Resize(3)
	got := rl.Snapshot(0)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries after shrink, got %d", len(got))
	}
	// Newest 3 should be statuses 5, 4, 3 (newest-first).
	if got[0].Status != 5 || got[1].Status != 4 || got[2].Status != 3 {
		t.Fatalf("unexpected statuses after resize: %+v", got)
	}
}

func TestRequestLogClearAndDisable(t *testing.T) {
	rl := newRequestLog(3)
	rl.Add(RequestLogEntry{Status: 200})
	rl.Clear()
	if got := rl.Snapshot(0); len(got) != 0 {
		t.Fatalf("expected empty after Clear, got %d entries", len(got))
	}

	rl.SetEnabled(false)
	rl.Add(RequestLogEntry{Status: 999})
	if got := rl.Snapshot(0); len(got) != 0 {
		t.Fatalf("expected disabled log to drop entry, got %d", len(got))
	}
	rl.SetEnabled(true)
	rl.Add(RequestLogEntry{Status: 201})
	if got := rl.Snapshot(0); len(got) != 1 || got[0].Status != 201 {
		t.Fatalf("expected single re-enabled entry, got %+v", got)
	}
}

func TestRequestLogTruncatesError(t *testing.T) {
	rl := newRequestLog(1)
	long := make([]byte, 1024)
	for i := range long {
		long[i] = 'x'
	}
	rl.Add(RequestLogEntry{Error: string(long)})
	got := rl.Snapshot(0)
	if len(got[0].Error) > 600 {
		t.Fatalf("error not truncated: len=%d", len(got[0].Error))
	}
}
