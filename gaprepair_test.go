package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"sync/atomic"
	"testing"

	koios "github.com/cardano-community/koios-go-client/v3"
)

// gapTestChain is two consecutive Conway epochs of blocks, each epoch with
// blocks both before and after its stability slot.
func gapTestChain() map[int][]testBlock {
	chain := map[int][]testBlock{}
	for _, epoch := range []int{rollbackTestEpoch, rollbackTestEpoch + 1} {
		start := GetEpochStartSlot(epoch, MainnetNetworkMagic)
		stability := start + StabilityWindowSlotsForEpoch(epoch, MainnetNetworkMagic)
		for _, slot := range []uint64{start + 10, start + 20, start + 30, stability + 5, stability + 15} {
			chain[epoch] = append(chain[epoch], testBlockAt(slot, "c"))
		}
	}
	return chain
}

type fakeGapSource map[int][]testBlock

func (f fakeGapSource) EpochBlockCounts(context.Context) (map[int]int, error) {
	counts := map[int]int{}
	for e, blocks := range f {
		counts[e] = len(blocks)
	}
	return counts, nil
}

func (f fakeGapSource) EpochBlocks(_ context.Context, epoch int) ([]BlockRef, error) {
	var refs []BlockRef
	for _, b := range f[epoch] {
		refs = append(refs, BlockRef{Slot: b.slot, Hash: b.hash})
	}
	return refs, nil
}

// fetcher serves the blocks after `after` from chain, in chain order, as a
// node would over ChainSync.
func fakeFetcher(chain map[int][]testBlock) headerFetcher {
	return func(_ context.Context, after BlockRef, through uint64) ([]BlockData, error) {
		var out []BlockData
		for _, epoch := range []int{rollbackTestEpoch, rollbackTestEpoch + 1} {
			for _, b := range chain[epoch] {
				if b.slot > after.Slot && b.slot <= through {
					out = append(out, BlockData{Slot: b.slot, Epoch: epoch, BlockHash: b.hash, VrfOutput: b.vrf})
				}
			}
		}
		return out, nil
	}
}

// syncChain feeds chain through a nonce tracker, skipping the slots in skip,
// as the live tail does.
func syncChain(t *testing.T, chain map[int][]testBlock, skip map[uint64]bool) (Store, *NonceTracker) {
	t.Helper()
	store := newTestStore(t)
	nt := NewNonceTracker(store, nil, rollbackTestEpoch, MainnetNetworkMagic, true)
	for _, epoch := range []int{rollbackTestEpoch, rollbackTestEpoch + 1} {
		for _, b := range chain[epoch] {
			if !skip[b.slot] {
				nt.ProcessBlock(b.slot, epoch, b.hash, b.vrf)
			}
		}
	}
	return store, nt
}

func epochNonces(t *testing.T, store Store, epoch int) (string, int, string) {
	t.Helper()
	ctx := context.Background()
	evolving, count, err := store.GetEvolvingNonce(ctx, epoch)
	if err != nil {
		t.Fatalf("GetEvolvingNonce(%d): %v", epoch, err)
	}
	candidate, err := store.GetCandidateNonce(ctx, epoch)
	if err != nil {
		t.Fatalf("GetCandidateNonce(%d): %v", epoch, err)
	}
	return hex.EncodeToString(evolving), count, hex.EncodeToString(candidate)
}

// A block missing from one epoch corrupts the candidate nonce of the next,
// even though the next epoch's own block count is complete. Repair must fill
// the block and rebuild both epochs to the state of a gap-free sync.
func TestRepairBlockGapsRestoresLaterEpochNonces(t *testing.T) {
	chain := gapTestChain()
	missing := chain[rollbackTestEpoch][1].slot
	wantStore, _ := syncChain(t, chain, nil)
	store, nt := syncChain(t, chain, map[uint64]bool{missing: true})

	next := rollbackTestEpoch + 1
	_, _, wantCandidate := epochNonces(t, wantStore, next)
	if _, _, got := epochNonces(t, store, next); got == wantCandidate {
		t.Fatal("test setup: the missing block should change the next epoch's candidate")
	}

	if err := RepairBlockGaps(context.Background(), store, nt, fakeGapSource(chain),
		fakeFetcher(chain), MainnetNetworkMagic); err != nil {
		t.Fatalf("RepairBlockGaps: %v", err)
	}

	if ok, _ := store.HasBlockAtSlot(context.Background(), missing); !ok {
		t.Fatalf("slot %d was not inserted", missing)
	}
	for _, epoch := range []int{rollbackTestEpoch, next} {
		wantEv, wantCount, wantCand := epochNonces(t, wantStore, epoch)
		gotEv, gotCount, gotCand := epochNonces(t, store, epoch)
		if gotEv != wantEv || gotCount != wantCount || gotCand != wantCand {
			t.Errorf("epoch %d: got (%s, %d, %s), want (%s, %d, %s)",
				epoch, gotEv, gotCount, gotCand, wantEv, wantCount, wantCand)
		}
	}
	if !bytes.Equal(nt.evolvingNonce, mustHex(t, epochEvolving(t, wantStore, next))) {
		t.Error("tracker's in-memory evolving nonce was not rebuilt")
	}
}

// A block the node returns with a different hash than the canonical chain is
// not inserted.
func TestRepairBlockGapsRejectsMismatchedHash(t *testing.T) {
	chain := gapTestChain()
	missing := chain[rollbackTestEpoch][1]
	store, nt := syncChain(t, chain, map[uint64]bool{missing.slot: true})

	forked := map[int][]testBlock{}
	for e, blocks := range chain {
		forked[e] = append([]testBlock(nil), blocks...)
	}
	forked[rollbackTestEpoch][1] = testBlockAt(missing.slot, "f")

	err := RepairBlockGaps(context.Background(), store, nt, fakeGapSource(chain),
		fakeFetcher(forked), MainnetNetworkMagic)
	if err == nil {
		t.Fatal("expected a hash mismatch error")
	}
	if ok, _ := store.HasBlockAtSlot(context.Background(), missing.slot); ok {
		t.Fatalf("forked block at slot %d was inserted", missing.slot)
	}
}

// A complete store is left untouched.
func TestRepairBlockGapsNoGaps(t *testing.T) {
	chain := gapTestChain()
	store, nt := syncChain(t, chain, nil)
	calls := 0
	fetch := func(context.Context, BlockRef, uint64) ([]BlockData, error) {
		calls++
		return nil, nil
	}
	if err := RepairBlockGaps(context.Background(), store, nt, fakeGapSource(chain), fetch, MainnetNetworkMagic); err != nil {
		t.Fatalf("RepairBlockGaps: %v", err)
	}
	if calls != 0 {
		t.Fatalf("fetcher called %d times on a complete store", calls)
	}
}

// Blocks the live tail announced before historical sync finished must stay
// fenced when the tail restarts from the DB to replay them.
func TestRestartLiveTailFromDBRaisesFence(t *testing.T) {
	idx := &Indexer{liveTailRestart: make(chan struct{}, 1)}
	atomic.StoreUint64(&idx.replayFenceSlot, 100)
	atomic.StoreUint64(&idx.liveTailSlot, 500)

	idx.restartLiveTailFromDB()

	if got := atomic.LoadUint64(&idx.replayFenceSlot); got != 500 {
		t.Fatalf("replay fence = %d, want 500", got)
	}
	select {
	case <-idx.liveTailRestart:
	default:
		t.Fatal("live tail restart was not requested")
	}

	// The fence never moves down.
	atomic.StoreUint64(&idx.liveTailSlot, 50)
	idx.restartLiveTailFromDB()
	if got := atomic.LoadUint64(&idx.replayFenceSlot); got != 500 {
		t.Fatalf("replay fence lowered to %d", got)
	}
}

// TICKN must not be used when the stored state does not reproduce the nonce
// of the current epoch, which Koios has already published.
func TestTICKNRejectedWhenStoredStateMisses(t *testing.T) {
	chain := gapTestChain()
	store, nt := syncChain(t, chain, nil)
	nt.koiosClient, _ = koios.New()
	ctx := context.Background()

	// The epoch after the test chain: candidate(rollbackTestEpoch+1) ⭒ η_ph(rollbackTestEpoch).
	published := rollbackTestEpoch + 2
	candidate, _ := store.GetCandidateNonce(ctx, published-1)
	etaPh, _ := store.GetPrevHashOfLastBlock(ctx, published-2)
	if candidate == nil || etaPh == "" {
		t.Fatal("test setup: no candidate nonce or η_ph")
	}
	right := hashConcat(candidate, mustHex(t, etaPh))

	for _, tc := range []struct {
		name  string
		koios []byte
		ok    bool
	}{
		{"matches", right, true},
		{"differs", bytes.Repeat([]byte{0xAB}, 32), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			koiosNonceServer(t, hex.EncodeToString(tc.koios))
			err := nt.verifyTICKN(ctx, published)
			if tc.ok && err != nil {
				t.Fatalf("verifyTICKN: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("verifyTICKN accepted state that does not reproduce Koios")
			}
		})
	}
}

func epochEvolving(t *testing.T, store Store, epoch int) string {
	t.Helper()
	ev, _, _ := epochNonces(t, store, epoch)
	return ev
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return b
}
