package main

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SqliteStore {
	t.Helper()
	store, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestInsertBlock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.InsertBlock(ctx, 100, 1, "abc123", []byte{1, 2, 3}, []byte{4, 5, 6})
	if err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}

	// Duplicate insert should not error (ON CONFLICT DO NOTHING)
	err = store.InsertBlock(ctx, 100, 1, "abc123", []byte{1, 2, 3}, []byte{4, 5, 6})
	if err != nil {
		t.Fatalf("InsertBlock duplicate: %v", err)
	}
}

func TestGetLastSyncedSlot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Empty table returns 0
	slot, err := store.GetLastSyncedSlot(ctx)
	if err != nil {
		t.Fatalf("GetLastSyncedSlot empty: %v", err)
	}
	if slot != 0 {
		t.Fatalf("expected 0, got %d", slot)
	}

	// Insert some blocks
	store.InsertBlock(ctx, 100, 1, "hash1", []byte{1}, []byte{2})
	store.InsertBlock(ctx, 200, 1, "hash2", []byte{3}, []byte{4})
	store.InsertBlock(ctx, 150, 1, "hash3", []byte{5}, []byte{6})

	slot, err = store.GetLastSyncedSlot(ctx)
	if err != nil {
		t.Fatalf("GetLastSyncedSlot: %v", err)
	}
	if slot != 200 {
		t.Fatalf("expected 200, got %d", slot)
	}
}

func TestEvolvingNonce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	nonce := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	err := store.UpsertEvolvingNonce(ctx, 100, nonce, 5)
	if err != nil {
		t.Fatalf("UpsertEvolvingNonce: %v", err)
	}

	got, blockCount, err := store.GetEvolvingNonce(ctx, 100)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}
	if blockCount != 5 {
		t.Fatalf("expected blockCount 5, got %d", blockCount)
	}
	if len(got) != len(nonce) {
		t.Fatalf("expected nonce len %d, got %d", len(nonce), len(got))
	}

	// Upsert again with different values
	nonce2 := []byte{0x11, 0x22, 0x33, 0x44}
	err = store.UpsertEvolvingNonce(ctx, 100, nonce2, 10)
	if err != nil {
		t.Fatalf("UpsertEvolvingNonce update: %v", err)
	}

	got, blockCount, err = store.GetEvolvingNonce(ctx, 100)
	if err != nil {
		t.Fatalf("GetEvolvingNonce after update: %v", err)
	}
	if blockCount != 10 {
		t.Fatalf("expected blockCount 10, got %d", blockCount)
	}
	if got[0] != 0x11 {
		t.Fatalf("expected nonce[0] 0x11, got 0x%x", got[0])
	}
}

func TestCandidateNonce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	nonce := make([]byte, 32)
	nonce[0] = 0xCA
	err := store.SetCandidateNonce(ctx, 200, nonce)
	if err != nil {
		t.Fatalf("SetCandidateNonce: %v", err)
	}
}

func TestFinalNonce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No nonce yet
	_, err := store.GetFinalNonce(ctx, 300)
	if err == nil {
		t.Fatal("expected error for missing nonce")
	}

	nonce := make([]byte, 32)
	nonce[0] = 0xFF
	err = store.SetFinalNonce(ctx, 300, nonce, "koios")
	if err != nil {
		t.Fatalf("SetFinalNonce: %v", err)
	}

	got, err := store.GetFinalNonce(ctx, 300)
	if err != nil {
		t.Fatalf("GetFinalNonce: %v", err)
	}
	if got[0] != 0xFF {
		t.Fatalf("expected nonce[0] 0xFF, got 0x%x", got[0])
	}
}

func TestLeaderSchedule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	schedule := &LeaderSchedule{
		Epoch:      400,
		EpochNonce: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		PoolStake:  1_000_000_000,
		TotalStake: 20_000_000_000,
		Sigma:      0.05,
		IdealSlots: 1080,
		AssignedSlots: []LeaderSlot{
			{No: 1, Slot: 100000, SlotInEpoch: 100, At: time.Now().UTC()},
			{No: 2, Slot: 100500, SlotInEpoch: 600, At: time.Now().UTC()},
		},
		CalculatedAt: time.Now().UTC(),
	}

	err := store.InsertLeaderSchedule(ctx, schedule)
	if err != nil {
		t.Fatalf("InsertLeaderSchedule: %v", err)
	}

	// Not posted yet
	if store.IsSchedulePosted(ctx, 400) {
		t.Fatal("expected schedule not posted")
	}

	// Mark as posted
	err = store.MarkSchedulePosted(ctx, 400)
	if err != nil {
		t.Fatalf("MarkSchedulePosted: %v", err)
	}

	if !store.IsSchedulePosted(ctx, 400) {
		t.Fatal("expected schedule posted")
	}

	// Upsert same epoch should update
	schedule.AssignedSlots = append(schedule.AssignedSlots, LeaderSlot{
		No: 3, Slot: 101000, SlotInEpoch: 1100, At: time.Now().UTC(),
	})
	err = store.InsertLeaderSchedule(ctx, schedule)
	if err != nil {
		t.Fatalf("InsertLeaderSchedule upsert: %v", err)
	}
}
