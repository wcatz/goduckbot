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

	inserted, err := store.InsertBlock(ctx, 100, 1, "abc123", []byte{1, 2, 3}, []byte{4, 5, 6})
	if err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if !inserted {
		t.Fatal("expected first insert to return inserted=true")
	}

	// Duplicate insert should not error (ON CONFLICT DO NOTHING), but returns inserted=false
	inserted, err = store.InsertBlock(ctx, 100, 1, "abc123", []byte{1, 2, 3}, []byte{4, 5, 6})
	if err != nil {
		t.Fatalf("InsertBlock duplicate: %v", err)
	}
	if inserted {
		t.Fatal("expected duplicate insert to return inserted=false")
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
	_, _ = store.InsertBlock(ctx, 100, 1, "hash1", []byte{1}, []byte{2})
	_, _ = store.InsertBlock(ctx, 200, 1, "hash2", []byte{3}, []byte{4})
	_, _ = store.InsertBlock(ctx, 150, 1, "hash3", []byte{5}, []byte{6})

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

func TestGetLastNBlocks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Empty DB returns empty slice
	blocks, err := store.GetLastNBlocks(ctx, 10)
	if err != nil {
		t.Fatalf("GetLastNBlocks empty: %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}

	// Insert 20 blocks across 2 epochs
	for i := 0; i < 20; i++ {
		epoch := 1
		if i >= 10 {
			epoch = 2
		}
		slot := uint64(100 + i*10)
		_, err := store.InsertBlock(ctx, slot, epoch, "hash"+string(rune('a'+i)), []byte{byte(i)}, []byte{byte(i)})
		if err != nil {
			t.Fatalf("InsertBlock %d: %v", i, err)
		}
	}

	// Request 10 — should get the 10 newest in descending order
	blocks, err = store.GetLastNBlocks(ctx, 10)
	if err != nil {
		t.Fatalf("GetLastNBlocks: %v", err)
	}
	if len(blocks) != 10 {
		t.Fatalf("expected 10 blocks, got %d", len(blocks))
	}
	// First block should be the highest slot
	if blocks[0].Slot != 290 {
		t.Fatalf("expected first block slot 290, got %d", blocks[0].Slot)
	}
	// Last block should be the 10th highest
	if blocks[9].Slot != 200 {
		t.Fatalf("expected last block slot 200, got %d", blocks[9].Slot)
	}
	// Should be in descending order
	for i := 1; i < len(blocks); i++ {
		if blocks[i].Slot >= blocks[i-1].Slot {
			t.Fatalf("blocks not in descending order at index %d: %d >= %d", i, blocks[i].Slot, blocks[i-1].Slot)
		}
	}

	// Request more than available
	blocks, err = store.GetLastNBlocks(ctx, 100)
	if err != nil {
		t.Fatalf("GetLastNBlocks overflow: %v", err)
	}
	if len(blocks) != 20 {
		t.Fatalf("expected 20 blocks, got %d", len(blocks))
	}
}

func TestGetBlockCountForEpoch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Empty epoch returns 0
	count, err := store.GetBlockCountForEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("GetBlockCountForEpoch empty: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	// Insert blocks in two epochs
	_, _ = store.InsertBlock(ctx, 100, 1, "h1", []byte{1}, []byte{1})
	_, _ = store.InsertBlock(ctx, 200, 1, "h2", []byte{2}, []byte{2})
	_, _ = store.InsertBlock(ctx, 300, 1, "h3", []byte{3}, []byte{3})
	_, _ = store.InsertBlock(ctx, 500, 2, "h4", []byte{4}, []byte{4})
	_, _ = store.InsertBlock(ctx, 600, 2, "h5", []byte{5}, []byte{5})

	count, err = store.GetBlockCountForEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("GetBlockCountForEpoch epoch 1: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3, got %d", count)
	}

	count, err = store.GetBlockCountForEpoch(ctx, 2)
	if err != nil {
		t.Fatalf("GetBlockCountForEpoch epoch 2: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2, got %d", count)
	}
}

func TestGetNonceValuesForEpoch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert blocks with known nonce values
	_, _ = store.InsertBlock(ctx, 100, 1, "h1", []byte{1}, []byte{0xAA})
	_, _ = store.InsertBlock(ctx, 200, 1, "h2", []byte{2}, []byte{0xBB})
	_, _ = store.InsertBlock(ctx, 300, 2, "h3", []byte{3}, []byte{0xCC})

	values, err := store.GetNonceValuesForEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("GetNonceValuesForEpoch: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(values))
	}
	// Should be ordered by slot (100 then 200)
	if values[0][0] != 0xAA {
		t.Fatalf("expected first nonce 0xAA, got 0x%x", values[0][0])
	}
	if values[1][0] != 0xBB {
		t.Fatalf("expected second nonce 0xBB, got 0x%x", values[1][0])
	}
}

func TestTruncateAll(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Populate all 3 tables
	_, _ = store.InsertBlock(ctx, 100, 1, "h1", []byte{1}, []byte{1})
	_, _ = store.InsertBlock(ctx, 200, 1, "h2", []byte{2}, []byte{2})
	_ = store.UpsertEvolvingNonce(ctx, 1, []byte{0xAA}, 2)
	_ = store.InsertLeaderSchedule(ctx, &LeaderSchedule{
		Epoch:         1,
		EpochNonce:    "abc",
		PoolStake:     100,
		TotalStake:    1000,
		AssignedSlots: []LeaderSlot{},
		CalculatedAt:  time.Now().UTC(),
	})

	// Verify data exists
	slot, _ := store.GetLastSyncedSlot(ctx)
	if slot == 0 {
		t.Fatal("expected blocks before truncate")
	}

	// Truncate
	err := store.TruncateAll(ctx)
	if err != nil {
		t.Fatalf("TruncateAll: %v", err)
	}

	// Verify all tables are empty
	slot, _ = store.GetLastSyncedSlot(ctx)
	if slot != 0 {
		t.Fatalf("expected 0 after truncate, got %d", slot)
	}

	count, _ := store.GetBlockCountForEpoch(ctx, 1)
	if count != 0 {
		t.Fatalf("expected 0 blocks after truncate, got %d", count)
	}

	_, _, nonceErr := store.GetEvolvingNonce(ctx, 1)
	if nonceErr == nil {
		t.Fatal("expected error getting nonce after truncate")
	}

	sched, schedErr := store.GetLeaderSchedule(ctx, 1)
	if schedErr == nil && sched != nil {
		t.Fatal("expected no schedule after truncate")
	}
}

func TestTruncateAllEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Truncating empty tables should not error
	err := store.TruncateAll(ctx)
	if err != nil {
		t.Fatalf("TruncateAll empty: %v", err)
	}
}

func TestGetCandidateNonce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No candidate set — should return error
	_, err := store.GetCandidateNonce(ctx, 200)
	if err == nil {
		t.Fatal("expected error for missing candidate nonce")
	}

	// Set candidate and verify retrieval
	nonce := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	if err := store.SetCandidateNonce(ctx, 200, nonce); err != nil {
		t.Fatalf("SetCandidateNonce: %v", err)
	}

	got, err := store.GetCandidateNonce(ctx, 200)
	if err != nil {
		t.Fatalf("GetCandidateNonce: %v", err)
	}
	if len(got) != len(nonce) {
		t.Fatalf("expected %x, got %x", nonce, got)
	}
	for i := range nonce {
		if got[i] != nonce[i] {
			t.Fatalf("expected %x, got %x", nonce, got)
		}
	}
}

func TestGetLastBlockHashForEpoch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No blocks — should return error
	_, err := store.GetLastBlockHashForEpoch(ctx, 200)
	if err == nil {
		t.Fatal("expected error for empty epoch")
	}

	// Insert blocks across two epochs
	store.InsertBlock(ctx, 100, 200, "hash_a", []byte{1}, []byte{1})
	store.InsertBlock(ctx, 200, 200, "hash_b", []byte{2}, []byte{2})
	store.InsertBlock(ctx, 300, 200, "hash_c", []byte{3}, []byte{3})
	store.InsertBlock(ctx, 400, 201, "hash_d", []byte{4}, []byte{4})

	// Last block of epoch 200 should be hash_c (slot 300)
	got, err := store.GetLastBlockHashForEpoch(ctx, 200)
	if err != nil {
		t.Fatalf("GetLastBlockHashForEpoch: %v", err)
	}
	if got != "hash_c" {
		t.Fatalf("expected hash_c, got %s", got)
	}

	// Last block of epoch 201 should be hash_d
	got, err = store.GetLastBlockHashForEpoch(ctx, 201)
	if err != nil {
		t.Fatalf("GetLastBlockHashForEpoch epoch 201: %v", err)
	}
	if got != "hash_d" {
		t.Fatalf("expected hash_d, got %s", got)
	}
}
