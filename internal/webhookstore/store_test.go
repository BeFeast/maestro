package webhookstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sampleDelivery(id string) Delivery {
	return Delivery{
		DeliveryID: id,
		EventType:  "issues",
		Action:     "opened",
		Repo:       "BeFeast/maestro",
		Sender:     "octocat",
		HookID:     "42",
		ReceivedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Payload:    []byte(`{"action":"opened"}`),
	}
}

func TestInsertIdempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	stored, err := store.Insert(ctx, sampleDelivery("d-1"))
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !stored {
		t.Fatal("first insert should report stored=true")
	}

	// A redelivery of the same X-GitHub-Delivery must be a no-op that does not
	// duplicate — even if the payload/envelope differs, the original row wins.
	dup := sampleDelivery("d-1")
	dup.Action = "edited"
	dup.Payload = []byte(`{"action":"edited"}`)
	stored, err = store.Insert(ctx, dup)
	if err != nil {
		t.Fatalf("redelivery insert: %v", err)
	}
	if stored {
		t.Fatal("redelivery should report stored=false")
	}

	n, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 stored delivery after redelivery, got %d", n)
	}

	got, ok, err := store.Get(ctx, "d-1")
	if err != nil || !ok {
		t.Fatalf("get d-1: ok=%v err=%v", ok, err)
	}
	// The original (opened) payload must be preserved, not overwritten by the
	// redelivery's (edited) body.
	if got.Action != "opened" {
		t.Fatalf("original row overwritten by redelivery: action=%q", got.Action)
	}
	if string(got.Payload) != `{"action":"opened"}` {
		t.Fatalf("payload not preserved: %q", got.Payload)
	}
}

func TestInsertRequiresDeliveryID(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Insert(context.Background(), sampleDelivery("")); err == nil {
		t.Fatal("expected error for blank delivery_id")
	}
}

func TestCountsByEventTypeAndLastDelivery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	deliveries := []Delivery{
		{DeliveryID: "a", EventType: "issues", ReceivedAt: time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC), Payload: []byte("{}")},
		{DeliveryID: "b", EventType: "issues", ReceivedAt: time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC), Payload: []byte("{}")},
		{DeliveryID: "c", EventType: "pull_request", ReceivedAt: time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC), Payload: []byte("{}")},
	}
	for _, d := range deliveries {
		if _, err := store.Insert(ctx, d); err != nil {
			t.Fatalf("insert %s: %v", d.DeliveryID, err)
		}
	}

	counts, err := store.CountsByEventType(ctx)
	if err != nil {
		t.Fatalf("counts by type: %v", err)
	}
	if counts["issues"] != 2 || counts["pull_request"] != 1 {
		t.Fatalf("unexpected per-event counts: %+v", counts)
	}

	last, err := store.LastDeliveryAt(ctx)
	if err != nil {
		t.Fatalf("last delivery: %v", err)
	}
	want := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	if !last.Equal(want) {
		t.Fatalf("last delivery = %s, want %s", last, want)
	}
}

func TestLastDeliveryEmptyStore(t *testing.T) {
	store := openTestStore(t)
	last, err := store.LastDeliveryAt(context.Background())
	if err != nil {
		t.Fatalf("last delivery on empty store: %v", err)
	}
	if !last.IsZero() {
		t.Fatalf("want zero time on empty store, got %s", last)
	}
}

// TestDurabilityAcrossReopen proves an acknowledged delivery survives a store
// close+reopen (the SQLite WAL durability the #824 restart criterion needs).
func TestDurabilityAcrossReopen(t *testing.T) {
	db := filepath.Join(t.TempDir(), "maestro.db")
	store, err := Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Insert(context.Background(), sampleDelivery("survive-me")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	_, ok, err := reopened.Get(context.Background(), "survive-me")
	if err != nil || !ok {
		t.Fatalf("acknowledged delivery lost across reopen: ok=%v err=%v", ok, err)
	}
}
