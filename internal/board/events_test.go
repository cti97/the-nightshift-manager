package board

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) (*sql.DB, *FixedClock) {
	t.Helper()
	return OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
}

func TestAppendEventStoresPayloadAndTime(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	if err := AppendEvent(ctx, db, clock, "admin", "work_item", "w1", "created",
		map[string]any{"title": "nightly report"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, err := ListEvents(ctx, db, "work_item", "w1", 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].At != clock.Now() {
		t.Fatalf("At = %d, want %d", events[0].At, clock.Now())
	}
	if events[0].Payload != `{"title":"nightly report"}` {
		t.Fatalf("Payload = %q", events[0].Payload)
	}
}

func TestAppendEventInsideTransaction(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := AppendEvent(ctx, tx, clock, "agent", "work_item", "w2", "claimed", nil); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	events, err := ListEvents(ctx, db, "work_item", "w2", 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Payload != "" {
		t.Fatalf("Payload = %q, want empty for nil payload", events[0].Payload)
	}
}

func TestAppendEventRejectsBadPayload(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	if err := AppendEvent(ctx, db, clock, "admin", "work_item", "w3", "created",
		map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("AppendEvent with unmarshalable payload: got nil error, want error")
	}
}

func TestAdvanceMovesTheClock(t *testing.T) {
	clock := NewFixedClock(100)
	clock.Advance(30 * time.Second)
	if clock.Now() != 130 {
		t.Fatalf("Now() = %d, want 130", clock.Now())
	}
}
