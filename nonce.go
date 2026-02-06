package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	koios "github.com/cardano-community/koios-go-client/v3"
	"golang.org/x/crypto/blake2b"
)

// NonceTracker accumulates VRF nonce contributions from chain sync blocks
// and evolves the epoch nonce for leader schedule calculation.
type NonceTracker struct {
	mu             sync.Mutex
	db             *pgxpool.Pool
	koiosClient    *koios.Client
	evolvingNonce  []byte // current eta_v (32 bytes)
	currentEpoch   int
	blockCount     int
	candidateFroze bool // whether candidate nonce was frozen this epoch
	networkMagic   int
}

// NewNonceTracker creates a NonceTracker and attempts to restore state from DB.
func NewNonceTracker(db *pgxpool.Pool, koiosClient *koios.Client, epoch, networkMagic int) *NonceTracker {
	nt := &NonceTracker{
		db:           db,
		koiosClient:  koiosClient,
		currentEpoch: epoch,
		networkMagic: networkMagic,
	}

	// Try to restore evolving nonce from DB for current epoch
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nonce, blockCount, err := GetEvolvingNonce(ctx, db, epoch)
	if err == nil && nonce != nil {
		nt.evolvingNonce = nonce
		nt.blockCount = blockCount
		log.Printf("Restored evolving nonce for epoch %d (block count: %d)", epoch, blockCount)
	} else {
		// Initialize with zero nonce — will be seeded on first block or from Koios
		nt.evolvingNonce = make([]byte, 32)
		log.Printf("Starting fresh nonce tracking for epoch %d", epoch)
	}

	return nt
}

// vrfNonceValue computes the nonce contribution from a VRF output.
// nonceValue = BLAKE2b-256(0x4E || vrfOutput)  — the "N" prefix
func vrfNonceValue(vrfOutput []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte{0x4E}) // "N" prefix
	h.Write(vrfOutput)
	return h.Sum(nil)
}

// evolveNonce updates the evolving nonce with a new nonce contribution.
// eta_v = BLAKE2b-256(eta_v || nonceValue)
func evolveNonce(currentNonce, nonceValue []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(currentNonce)
	h.Write(nonceValue)
	return h.Sum(nil)
}

// ProcessBlock processes a block's VRF output for nonce evolution.
func (nt *NonceTracker) ProcessBlock(slot uint64, epoch int, blockHash string, vrfOutput []byte) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	// Handle epoch transition
	if epoch != nt.currentEpoch {
		log.Printf("Epoch transition: %d -> %d", nt.currentEpoch, epoch)
		nt.currentEpoch = epoch
		nt.evolvingNonce = make([]byte, 32)
		nt.blockCount = 0
		nt.candidateFroze = false
	}

	// Compute nonce contribution
	nonceValue := vrfNonceValue(vrfOutput)

	// Update evolving nonce
	nt.evolvingNonce = evolveNonce(nt.evolvingNonce, nonceValue)
	nt.blockCount++

	// Persist to DB (async-safe since we hold the lock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := InsertBlock(ctx, nt.db, slot, epoch, blockHash, vrfOutput, nonceValue); err != nil {
		log.Printf("Failed to insert block %d: %v", slot, err)
	}

	if err := UpsertEvolvingNonce(ctx, nt.db, epoch, nt.evolvingNonce, nt.blockCount); err != nil {
		log.Printf("Failed to upsert evolving nonce for epoch %d: %v", epoch, err)
	}
}

// FreezeCandidate freezes the candidate nonce at the stability window.
func (nt *NonceTracker) FreezeCandidate(epoch int) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if nt.candidateFroze {
		return
	}

	nt.candidateFroze = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := SetCandidateNonce(ctx, nt.db, epoch, nt.evolvingNonce); err != nil {
		log.Printf("Failed to freeze candidate nonce for epoch %d: %v", epoch, err)
	} else {
		log.Printf("Froze candidate nonce for epoch %d (block count: %d)", epoch, nt.blockCount)
	}
}

// GetNonceForEpoch returns the epoch nonce, trying local DB first then Koios fallback.
func (nt *NonceTracker) GetNonceForEpoch(epoch int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try local DB first
	nonce, err := GetFinalNonce(ctx, nt.db, epoch)
	if err == nil && nonce != nil {
		log.Printf("Using local nonce for epoch %d", epoch)
		return nonce, nil
	}

	// Fallback to Koios
	log.Printf("Local nonce not available for epoch %d, falling back to Koios", epoch)
	nonce, err = nt.fetchNonceFromKoios(ctx, epoch)
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce for epoch %d: %w", epoch, err)
	}

	// Cache the Koios nonce in DB
	if storeErr := SetFinalNonce(ctx, nt.db, epoch, nonce, "koios"); storeErr != nil {
		log.Printf("Failed to cache Koios nonce for epoch %d: %v", epoch, storeErr)
	}

	return nonce, nil
}

// fetchNonceFromKoios fetches the epoch nonce from Koios API.
func (nt *NonceTracker) fetchNonceFromKoios(ctx context.Context, epoch int) ([]byte, error) {
	epochNo := koios.EpochNo(epoch)
	res, err := nt.koiosClient.GetEpochParams(ctx, &epochNo, nil)
	if err != nil {
		return nil, fmt.Errorf("koios GetEpochParams: %w", err)
	}

	if len(res.Data) == 0 {
		return nil, fmt.Errorf("no epoch params returned for epoch %d", epoch)
	}

	nonceHex := res.Data[0].Nonce
	if nonceHex == "" {
		return nil, fmt.Errorf("empty nonce for epoch %d", epoch)
	}

	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return nil, fmt.Errorf("decoding nonce hex: %w", err)
	}

	if len(nonce) != 32 {
		return nil, fmt.Errorf("unexpected nonce length: %d (expected 32)", len(nonce))
	}

	log.Printf("Fetched nonce from Koios for epoch %d: %s", epoch, nonceHex)
	return nonce, nil
}
