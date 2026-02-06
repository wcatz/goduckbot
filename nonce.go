package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	koios "github.com/cardano-community/koios-go-client/v3"
	"golang.org/x/crypto/blake2b"
)

// ShelleyGenesisHash is the hash of the Shelley genesis block on mainnet.
// Used as the initial eta_v seed for full chain sync nonce evolution.
const ShelleyGenesisHash = "1a3be38bcbb7911969283716ad7aa550250226b76a61fc51cc9a9a35d9276d81"

// NonceTracker accumulates VRF nonce contributions from chain sync blocks
// and evolves the epoch nonce for leader schedule calculation.
type NonceTracker struct {
	mu             sync.Mutex
	store          Store
	koiosClient    *koios.Client
	evolvingNonce  []byte // current eta_v (32 bytes)
	currentEpoch   int
	blockCount     int
	candidateFroze bool // whether candidate nonce was frozen this epoch
	networkMagic   int
	fullMode       bool // true = genesis-seeded rolling nonce, false = lite (zero-seeded)
}

// NewNonceTracker creates a NonceTracker and attempts to restore state from DB.
// In full mode, the initial nonce is seeded with the Shelley genesis hash.
// In lite mode, the initial nonce is zero (current behavior).
func NewNonceTracker(store Store, koiosClient *koios.Client, epoch, networkMagic int, fullMode bool) *NonceTracker {
	nt := &NonceTracker{
		store:        store,
		koiosClient:  koiosClient,
		currentEpoch: epoch,
		networkMagic: networkMagic,
		fullMode:     fullMode,
	}

	// Try to restore evolving nonce from DB for current epoch
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nonce, blockCount, err := store.GetEvolvingNonce(ctx, epoch)
	if err == nil && nonce != nil {
		nt.evolvingNonce = nonce
		nt.blockCount = blockCount
		log.Printf("Restored evolving nonce for epoch %d (block count: %d)", epoch, blockCount)
	} else {
		nt.evolvingNonce = initialNonce(fullMode)
		log.Printf("Starting fresh nonce tracking for epoch %d (full=%v)", epoch, fullMode)
	}

	return nt
}

// initialNonce returns the initial eta_v seed based on mode.
func initialNonce(fullMode bool) []byte {
	if fullMode {
		seed, _ := hex.DecodeString(ShelleyGenesisHash)
		return seed
	}
	return make([]byte, 32)
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
		nt.blockCount = 0
		nt.candidateFroze = false

		// Try to restore evolving nonce from DB (e.g., after restart mid-epoch)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		nonce, bc, err := nt.store.GetEvolvingNonce(ctx, epoch)
		cancel()
		if err == nil && nonce != nil {
			nt.evolvingNonce = nonce
			nt.blockCount = bc
			log.Printf("Restored evolving nonce for epoch %d from DB (block count: %d)", epoch, bc)
		}
		// In full mode: eta_v rolls across epoch boundaries (no reset).
		// In lite mode: eta_v also continues (it was zero-seeded initially).
		// We only reset if we couldn't restore AND it's lite mode.
		// In both cases, the nonce just continues from wherever it was.
	}

	// Compute nonce contribution
	nonceValue := vrfNonceValue(vrfOutput)

	// Update evolving nonce
	nt.evolvingNonce = evolveNonce(nt.evolvingNonce, nonceValue)
	nt.blockCount++

	// Persist to DB (async-safe since we hold the lock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := nt.store.InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue); err != nil {
		log.Printf("Failed to insert block %d: %v", slot, err)
	}

	if err := nt.store.UpsertEvolvingNonce(ctx, epoch, nt.evolvingNonce, nt.blockCount); err != nil {
		log.Printf("Failed to upsert evolving nonce for epoch %d: %v", epoch, err)
	}
}

// FreezeCandidate freezes the candidate nonce at the stability window.
func (nt *NonceTracker) FreezeCandidate(epoch int) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if nt.candidateFroze || epoch != nt.currentEpoch {
		return
	}

	nt.candidateFroze = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := nt.store.SetCandidateNonce(ctx, epoch, nt.evolvingNonce); err != nil {
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
	nonce, err := nt.store.GetFinalNonce(ctx, epoch)
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
	if storeErr := nt.store.SetFinalNonce(ctx, epoch, nonce, "koios"); storeErr != nil {
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
