package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
)

// backfillTruth replays chain from the network's initial nonce, as
// BackfillNonces does: the evolving nonce after all blocks, and the candidate
// nonce of the second epoch.
func backfillTruth(t *testing.T, chain map[int][]testBlock) (evolving, candidate []byte) {
	t.Helper()
	etaV := initialNonce(true, MainnetNetworkMagic)
	second := rollbackTestEpoch + 1
	stability := GetEpochStartSlot(second, MainnetNetworkMagic) +
		StabilityWindowSlotsForEpoch(second, MainnetNetworkMagic)
	for _, epoch := range []int{rollbackTestEpoch, second} {
		for _, b := range chain[epoch] {
			if epoch == second && candidate == nil && b.slot >= stability {
				candidate = etaV
			}
			etaV = evolveNonce(etaV, vrfNonceValueForEpoch(b.vrf, epoch, MainnetNetworkMagic))
		}
	}
	return etaV, candidate
}

// bufferedStore reads the whole block stream before BackfillNonces iterates
// it. SqliteStore has one connection, which an open stream holds while
// BackfillNonces queries the store; Postgres streams on its own connection.
type bufferedStore struct{ *SqliteStore }

func (s bufferedStore) StreamBlockVrfOutputs(ctx context.Context) (BlockVrfRows, error) {
	rows, err := s.SqliteStore.StreamBlockVrfOutputs(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buf := &bufferedRows{i: -1}
	for rows.Next() {
		var r bufferedRow
		r.epoch, r.slot, r.vrf, r.nonce, r.hash, err = rows.Scan()
		if err != nil {
			return nil, err
		}
		buf.rows = append(buf.rows, r)
	}
	return buf, rows.Err()
}

type bufferedRow struct {
	epoch      int
	slot       uint64
	vrf, nonce []byte
	hash       string
}

type bufferedRows struct {
	rows []bufferedRow
	i    int
}

func (b *bufferedRows) Next() bool { b.i++; return b.i < len(b.rows) }
func (b *bufferedRows) Scan() (int, uint64, []byte, []byte, string, error) {
	r := b.rows[b.i]
	return r.epoch, r.slot, r.vrf, r.nonce, r.hash, nil
}
func (b *bufferedRows) Close()     {}
func (b *bufferedRows) Err() error { return nil }

// liveFromWrongSeed starts the live tracker from an evolving nonce the chain
// never produced (as after a restart whose DB held no evolving nonce for the
// tip epoch while sync resumed epochs earlier) and feeds it the chain's blocks
// for which keep returns true.
func liveFromWrongSeed(t *testing.T, chain map[int][]testBlock, keep func(epoch int, b testBlock) bool) (Store, *NonceTracker) {
	t.Helper()
	store := bufferedStore{newTestStore(t)}
	nt := NewNonceTracker(store, nil, rollbackTestEpoch, MainnetNetworkMagic, true)
	nt.evolvingNonce = bytes.Repeat([]byte{0x11}, 32)
	feed(nt, chain, keep)
	return store, nt
}

func feed(nt *NonceTracker, chain map[int][]testBlock, keep func(epoch int, b testBlock) bool) {
	for _, epoch := range []int{rollbackTestEpoch, rollbackTestEpoch + 1} {
		for _, b := range chain[epoch] {
			if keep(epoch, b) {
				nt.ProcessBlock(b.slot, epoch, b.hash, b.vrf)
			}
		}
	}
}

// After a backfill, the live tail must freeze the candidate nonce from the
// backfilled chain state, not from the evolving nonce it carried before the
// backfill (2026-10-29: epoch 664's candidate, and so epoch 665's nonce and
// leader schedule, came from a tracker seeded wrong at startup).
func TestBackfillReseedsLiveTrackerBeforeCandidateFreeze(t *testing.T) {
	chain := gapTestChain()
	second := rollbackTestEpoch + 1
	stability := GetEpochStartSlot(second, MainnetNetworkMagic) +
		StabilityWindowSlotsForEpoch(second, MainnetNetworkMagic)
	beforeFreeze := func(epoch int, b testBlock) bool { return epoch < second || b.slot < stability }

	store, nt := liveFromWrongSeed(t, chain, beforeFreeze)
	ctx := context.Background()
	if err := nt.BackfillNonces(ctx); err != nil {
		t.Fatalf("BackfillNonces: %v", err)
	}
	feed(nt, chain, func(epoch int, b testBlock) bool { return !beforeFreeze(epoch, b) })

	wantEvolving, wantCandidate := backfillTruth(t, chain)
	ev, _, cand := epochNonces(t, store, second)
	if cand != hex.EncodeToString(wantCandidate) {
		t.Fatalf("candidate nonce for epoch %d = %s, want %x (frozen from the pre-backfill state)", second, cand, wantCandidate)
	}
	if ev != hex.EncodeToString(wantEvolving) {
		t.Fatalf("stored evolving nonce for epoch %d = %s, want %x", second, ev, wantEvolving)
	}
	if !bytes.Equal(nt.evolvingNonce, wantEvolving) {
		t.Fatalf("in-memory evolving nonce = %x, want %x", nt.evolvingNonce, wantEvolving)
	}
}

// A next-epoch nonce already computed from a wrong candidate must be replaced
// once the backfill rebuilds that candidate, so the leaderlog trigger sees the
// nonce change and posts a corrected schedule.
func TestBackfillCorrectsNextEpochNonceComputedFromWrongCandidate(t *testing.T) {
	chain := gapTestChain()
	second := rollbackTestEpoch + 1
	store, nt := liveFromWrongSeed(t, chain, func(int, testBlock) bool { return true })
	ctx := context.Background()

	wrongCandidate, err := store.GetCandidateNonce(ctx, second)
	if err != nil || wrongCandidate == nil {
		t.Fatalf("test setup: no live candidate for epoch %d: %v", second, err)
	}
	etaPh, err := store.GetPrevHashOfLastBlock(ctx, rollbackTestEpoch)
	if err != nil || etaPh == "" {
		t.Fatalf("test setup: no η_ph for epoch %d: %v", rollbackTestEpoch, err)
	}
	if err := store.SetFinalNonce(ctx, second+1, hashConcat(wrongCandidate, mustHex(t, etaPh)), "computed"); err != nil {
		t.Fatalf("SetFinalNonce: %v", err)
	}

	if err := nt.BackfillNonces(ctx); err != nil {
		t.Fatalf("BackfillNonces: %v", err)
	}

	_, wantCandidate := backfillTruth(t, chain)
	want := hashConcat(wantCandidate, mustHex(t, etaPh))
	got, err := store.GetFinalNonce(ctx, second+1)
	if err != nil {
		t.Fatalf("GetFinalNonce(%d): %v", second+1, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("epoch %d nonce = %x, want %x (still computed from the wrong candidate)", second+1, got, want)
	}
}
