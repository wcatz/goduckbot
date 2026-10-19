package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/blinklabs-io/adder/event"
)

// rollbackTestEpoch is a Conway-era mainnet epoch, so the stability window
// is 4k/f slots before the epoch end.
const rollbackTestEpoch = 662

type testBlock struct {
	slot uint64
	hash string
	vrf  []byte
}

func rollbackTestStability() uint64 {
	return GetEpochStartSlot(rollbackTestEpoch, MainnetNetworkMagic) +
		StabilityWindowSlotsForEpoch(rollbackTestEpoch, MainnetNetworkMagic)
}

// testBlockAt returns a block at slot whose VRF output is derived from tag,
// so two blocks at one slot (a slot battle) carry different contributions.
func testBlockAt(slot uint64, tag string) testBlock {
	vrf := bytes.Repeat([]byte(tag+fmt.Sprint(slot)), 8)[:64]
	return testBlock{slot: slot, hash: hex.EncodeToString(vrf[:32]), vrf: vrf}
}

func newRollbackTestIndexer(t *testing.T) *Indexer {
	t.Helper()
	store := newTestStore(t)
	return &Indexer{
		store:        store,
		nonceTracker: NewNonceTracker(store, nil, rollbackTestEpoch, MainnetNetworkMagic, true),
		networkMagic: MainnetNetworkMagic,
		epoch:        rollbackTestEpoch,
	}
}

func applyBlocks(idx *Indexer, blocks ...testBlock) {
	for _, b := range blocks {
		idx.nonceTracker.ProcessBlock(b.slot, rollbackTestEpoch, b.hash, b.vrf)
	}
}

func rollbackTo(t *testing.T, idx *Indexer, slot uint64) {
	t.Helper()
	evt := event.Event{
		Type:      "input.rollback",
		Timestamp: time.Now(),
		Payload:   event.RollbackEvent{SlotNumber: slot},
	}
	if err := idx.handleEvent(evt); err != nil {
		t.Fatalf("handleEvent rollback: %v", err)
	}
}

type nonceState struct {
	evolving  string
	count     int
	candidate string
}

func readNonceState(t *testing.T, idx *Indexer) nonceState {
	t.Helper()
	ctx := context.Background()
	evolving, count, err := idx.store.GetEvolvingNonce(ctx, rollbackTestEpoch)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}
	candidate, err := idx.store.GetCandidateNonce(ctx, rollbackTestEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		candidate, err = nil, nil
	}
	if err != nil {
		t.Fatalf("GetCandidateNonce: %v", err)
	}
	if !bytes.Equal(idx.nonceTracker.evolvingNonce, evolving) {
		t.Fatalf("in-memory evolving nonce %x differs from stored %x",
			idx.nonceTracker.evolvingNonce, evolving)
	}
	return nonceState{
		evolving:  hex.EncodeToString(evolving),
		count:     count,
		candidate: hex.EncodeToString(candidate),
	}
}

// straightLine applies the canonical chain once, with no forks.
func straightLine(t *testing.T, blocks ...testBlock) nonceState {
	t.Helper()
	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, blocks...)
	return readNonceState(t, idx)
}

// A one-block slot battle: the losing block is applied, rolled back, and the
// winner applied. The nonce must equal the winner-only chain; the loser's VRF
// contribution must not survive the rollback.
func TestRollbackDropsForkedBlockFromNonce(t *testing.T) {
	start := GetEpochStartSlot(rollbackTestEpoch, MainnetNetworkMagic)
	b1 := testBlockAt(start+10, "a")
	b2 := testBlockAt(start+20, "a")
	loser := testBlockAt(start+30, "loser")
	winner := testBlockAt(start+30, "winner")

	want := straightLine(t, b1, b2, winner)

	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, b1, b2, loser)
	rollbackTo(t, idx, b2.slot)
	applyBlocks(idx, winner)

	if got := readNonceState(t, idx); got != want {
		t.Fatalf("after slot battle got %+v, want straight-line %+v", got, want)
	}
}

// A deep rollback followed by the same blocks re-applied (a re-intersection)
// must leave the nonce as if each block had been applied once.
func TestRollbackReapplySameBlocksIsIdempotent(t *testing.T) {
	start := GetEpochStartSlot(rollbackTestEpoch, MainnetNetworkMagic)
	var blocks []testBlock
	for i := range 10 {
		blocks = append(blocks, testBlockAt(start+uint64(10*(i+1)), "a"))
	}
	want := straightLine(t, blocks...)

	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, blocks...)
	rollbackTo(t, idx, blocks[3].slot)
	applyBlocks(idx, blocks[4:]...)

	if got := readNonceState(t, idx); got != want {
		t.Fatalf("after re-apply got %+v, want straight-line %+v", got, want)
	}
}

// A rollback to before the stability slot undoes the candidate freeze; the
// candidate must re-freeze on the post-rollback chain, before the first block
// at or after the stability slot.
func TestRollbackBeforeStabilityRefreezesCandidate(t *testing.T) {
	stab := rollbackTestStability()
	pre1 := testBlockAt(stab-20, "a")
	pre2 := testBlockAt(stab-10, "a")
	forkPre := testBlockAt(stab-5, "loser")
	post := testBlockAt(stab+10, "a")
	winnerPre := testBlockAt(stab-5, "winner")

	want := straightLine(t, pre1, pre2, winnerPre, post)

	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, pre1, pre2, forkPre, post)
	rollbackTo(t, idx, pre2.slot)
	applyBlocks(idx, winnerPre, post)

	got := readNonceState(t, idx)
	if got != want {
		t.Fatalf("after rollback across stability got %+v, want %+v", got, want)
	}
	if got.candidate == "" {
		t.Fatal("candidate must be frozen again once the stability slot is passed")
	}
}

// A rollback that stays after the stability slot must keep the frozen
// candidate: the next block must not freeze a candidate that already includes
// post-stability blocks.
func TestRollbackAfterStabilityKeepsCandidate(t *testing.T) {
	stab := rollbackTestStability()
	pre := testBlockAt(stab-10, "a")
	post1 := testBlockAt(stab+10, "a")
	loser := testBlockAt(stab+20, "loser")
	winner := testBlockAt(stab+20, "winner")

	want := straightLine(t, pre, post1, winner)

	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, pre, post1, loser)
	rollbackTo(t, idx, post1.slot)
	applyBlocks(idx, winner)

	if got := readNonceState(t, idx); got != want {
		t.Fatalf("after post-stability rollback got %+v, want %+v", got, want)
	}
}

// Recomputing the current epoch's nonce after the stability slot must not
// unfreeze the candidate, or the next block freezes a wrong one.
func TestRecomputeAfterStabilityKeepsCandidate(t *testing.T) {
	stab := rollbackTestStability()
	pre := testBlockAt(stab-10, "a")
	post1 := testBlockAt(stab+10, "a")
	post2 := testBlockAt(stab+20, "a")

	want := straightLine(t, pre, post1, post2)

	idx := newRollbackTestIndexer(t)
	applyBlocks(idx, pre, post1)
	if err := idx.nonceTracker.RecomputeCurrentEpochNonce(
		context.Background(), rollbackTestEpoch,
	); err != nil {
		t.Fatalf("RecomputeCurrentEpochNonce: %v", err)
	}
	applyBlocks(idx, post2)

	if got := readNonceState(t, idx); got != want {
		t.Fatalf("after recompute got %+v, want %+v", got, want)
	}
}

// The next epoch's final nonce is stored before that epoch starts, which
// creates its epoch_nonces row with the final nonce in evolving_nonce and no
// blocks counted. Crossing into that epoch must keep rolling the evolving
// nonce, not restart it from the final nonce.
func TestEpochTransitionIgnoresPrecomputedFinalNonce(t *testing.T) {
	start := GetEpochStartSlot(rollbackTestEpoch, MainnetNetworkMagic)
	next := GetEpochStartSlot(rollbackTestEpoch+1, MainnetNetworkMagic)
	b1 := testBlockAt(start+10, "a")
	b2 := testBlockAt(next+10, "a")
	ctx := context.Background()

	ref := newRollbackTestIndexer(t)
	ref.nonceTracker.ProcessBlock(b1.slot, rollbackTestEpoch, b1.hash, b1.vrf)
	ref.nonceTracker.ProcessBlock(b2.slot, rollbackTestEpoch+1, b2.hash, b2.vrf)
	want, _, err := ref.store.GetEvolvingNonce(ctx, rollbackTestEpoch+1)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}

	idx := newRollbackTestIndexer(t)
	idx.nonceTracker.ProcessBlock(b1.slot, rollbackTestEpoch, b1.hash, b1.vrf)
	final := bytes.Repeat([]byte{0xFF}, 32)
	if err := idx.store.SetFinalNonce(ctx, rollbackTestEpoch+1, final, "computed"); err != nil {
		t.Fatalf("SetFinalNonce: %v", err)
	}
	idx.nonceTracker.ProcessBlock(b2.slot, rollbackTestEpoch+1, b2.hash, b2.vrf)
	got, count, err := idx.store.GetEvolvingNonce(ctx, rollbackTestEpoch+1)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}
	if !bytes.Equal(got, want) || count != 1 {
		t.Fatalf("evolving nonce after boundary = %x (count %d), want %x (count 1)",
			got, count, want)
	}
}

// A rollback across an epoch boundary must discard the later epoch's evolving
// state, or crossing the boundary again restores it with the rolled-back
// blocks still counted.
func TestRollbackAcrossEpochBoundary(t *testing.T) {
	start := GetEpochStartSlot(rollbackTestEpoch, MainnetNetworkMagic)
	next := GetEpochStartSlot(rollbackTestEpoch+1, MainnetNetworkMagic)
	last := testBlockAt(next-10, "a")
	loser := testBlockAt(next+5, "loser")
	winner := testBlockAt(next+5, "winner")
	first := testBlockAt(start+10, "a")
	ctx := context.Background()

	ref := newRollbackTestIndexer(t)
	ref.nonceTracker.ProcessBlock(first.slot, rollbackTestEpoch, first.hash, first.vrf)
	ref.nonceTracker.ProcessBlock(last.slot, rollbackTestEpoch, last.hash, last.vrf)
	ref.nonceTracker.ProcessBlock(winner.slot, rollbackTestEpoch+1, winner.hash, winner.vrf)
	want, wantCount, err := ref.store.GetEvolvingNonce(ctx, rollbackTestEpoch+1)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}

	idx := newRollbackTestIndexer(t)
	idx.nonceTracker.ProcessBlock(first.slot, rollbackTestEpoch, first.hash, first.vrf)
	idx.nonceTracker.ProcessBlock(last.slot, rollbackTestEpoch, last.hash, last.vrf)
	idx.nonceTracker.ProcessBlock(loser.slot, rollbackTestEpoch+1, loser.hash, loser.vrf)
	rollbackTo(t, idx, last.slot)
	idx.nonceTracker.ProcessBlock(winner.slot, rollbackTestEpoch+1, winner.hash, winner.vrf)
	got, count, err := idx.store.GetEvolvingNonce(ctx, rollbackTestEpoch+1)
	if err != nil {
		t.Fatalf("GetEvolvingNonce: %v", err)
	}
	if !bytes.Equal(got, want) || count != wantCount {
		t.Fatalf("after cross-boundary rollback got %x (count %d), want %x (count %d)",
			got, count, want, wantCount)
	}
}
