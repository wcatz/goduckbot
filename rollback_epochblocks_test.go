package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/blinklabs-io/adder/event"
	koios "github.com/cardano-community/koios-go-client/v3"
)

// stubStore is a minimal Store implementation for tests that only exercise
// a handful of methods; every other method is an unused no-op.
type stubStore struct {
	deletedCount int64

	schedule         *LeaderSchedule
	finalNonce       []byte
	insertedSchedule *LeaderSchedule
}

func (s *stubStore) InsertBlock(ctx context.Context, slot uint64, epoch int, blockHash string, vrfOutput, nonceValue []byte) (bool, error) {
	return false, nil
}
func (s *stubStore) InsertBlockBatch(ctx context.Context, blocks []BlockData) (int, error) {
	return 0, nil
}
func (s *stubStore) UpsertEvolvingNonce(ctx context.Context, epoch int, nonce []byte, blockCount int) error {
	return nil
}
func (s *stubStore) SetCandidateNonce(ctx context.Context, epoch int, nonce []byte) error { return nil }
func (s *stubStore) DeleteCandidateNonce(ctx context.Context, epoch int) error            { return nil }
func (s *stubStore) SetFinalNonce(ctx context.Context, epoch int, nonce []byte, source string) error {
	return nil
}
func (s *stubStore) GetFinalNonce(ctx context.Context, epoch int) ([]byte, error) {
	return s.finalNonce, nil
}
func (s *stubStore) GetEvolvingNonce(ctx context.Context, epoch int) ([]byte, int, error) {
	return nil, 0, nil
}
func (s *stubStore) GetBlockHash(ctx context.Context, slot uint64) (string, error) { return "", nil }
func (s *stubStore) GetBlockByHash(ctx context.Context, hashPrefix string) ([]BlockRecord, error) {
	return nil, nil
}
func (s *stubStore) InsertLeaderSchedule(ctx context.Context, schedule *LeaderSchedule) error {
	s.insertedSchedule = schedule
	return nil
}
func (s *stubStore) GetLeaderSchedule(ctx context.Context, epoch int) (*LeaderSchedule, error) {
	return s.schedule, nil
}
func (s *stubStore) IsSchedulePosted(ctx context.Context, epoch int) bool    { return false }
func (s *stubStore) MarkSchedulePosted(ctx context.Context, epoch int) error { return nil }
func (s *stubStore) GetForgedSlots(ctx context.Context, epoch int) ([]uint64, error) {
	return nil, nil
}
func (s *stubStore) GetLastSyncedSlot(ctx context.Context) (uint64, error) { return 0, nil }
func (s *stubStore) StreamBlockVrfOutputs(ctx context.Context) (BlockVrfRows, error) {
	return nil, nil
}
func (s *stubStore) GetLastNBlocks(ctx context.Context, n int) ([]BlockRecord, error) {
	return nil, nil
}
func (s *stubStore) GetBlockCountForEpoch(ctx context.Context, epoch int) (int, error) {
	return 0, nil
}
func (s *stubStore) GetVrfOutputsForEpoch(ctx context.Context, epoch int) ([]VrfBlock, error) {
	return nil, nil
}
func (s *stubStore) GetCandidateNonce(ctx context.Context, epoch int) ([]byte, error) {
	return nil, nil
}
func (s *stubStore) GetPrevHashOfLastBlock(ctx context.Context, epoch int) (string, error) {
	return "", nil
}
func (s *stubStore) UpsertSlotOutcomes(ctx context.Context, epoch int, outcomes []SlotOutcome) error {
	return nil
}
func (s *stubStore) GetSlotOutcomes(ctx context.Context, epoch int) ([]SlotOutcome, error) {
	return nil, nil
}
func (s *stubStore) IsEpochClassified(ctx context.Context, epoch int) bool    { return false }
func (s *stubStore) MarkEpochClassified(ctx context.Context, epoch int) error { return nil }
func (s *stubStore) DeleteSlotOutcomesBefore(ctx context.Context, epoch int) (int64, error) {
	return 0, nil
}
func (s *stubStore) HasBlockAtSlot(ctx context.Context, slot uint64) (bool, error) {
	return false, nil
}
func (s *stubStore) DeleteBlocksAfterSlot(ctx context.Context, slot uint64) (int64, error) {
	return s.deletedCount, nil
}
func (s *stubStore) TruncateAll(ctx context.Context) error { return nil }
func (s *stubStore) Close() error                          { return nil }

// TestHandleEventRollbackResyncsEpochBlocks is a regression test for #130:
// the rollback branch of handleEvent returned before the epoch-reset logic
// ran, and epochBlocks was never corrected for the pool's own blocks that
// may have been forked out. This asserts a rollback resyncs epochBlocks
// from Koios (the only source of truth, since the blocks table has no
// issuer column to recompute the pool's own count locally).
func TestHandleEventRollbackResyncsEpochBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/pool_blocks" {
			fmt.Fprint(w, `[{"epoch_no":100,"epoch_slot":1,"block_height":1,"block_time":"2026-01-01T00:00:00Z","block_hash":"abc"}]`)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	client, err := koios.New(koios.Host(srv.Listener.Addr().String()), koios.Scheme("http"), koios.APIVersion("v1"))
	if err != nil {
		t.Fatalf("failed to construct koios client: %v", err)
	}

	idx := &Indexer{
		koios:        client,
		store:        &stubStore{deletedCount: 3},
		networkMagic: MainnetNetworkMagic,
		bech32PoolId: "pool1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		epoch:        100,
		epochBlocks:  7, // stale count including forked-out blocks
	}

	rollbackSlot := GetEpochStartSlot(100, MainnetNetworkMagic) + 10
	evt := event.Event{
		Type:      "input.rollback",
		Timestamp: time.Now(),
		Payload:   event.RollbackEvent{SlotNumber: rollbackSlot},
	}

	if err := idx.handleEvent(evt); err != nil {
		t.Fatalf("handleEvent returned error: %v", err)
	}

	if idx.epochBlocks != 1 {
		t.Errorf("expected epochBlocks resynced to 1 (from Koios pool_blocks), got %d", idx.epochBlocks)
	}
}
