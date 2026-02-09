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
// nonceValue = BLAKE2b-256(vrfOutput)
// Verified against pallas/cncli test vectors (no domain prefix).
func vrfNonceValue(vrfOutput []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(vrfOutput)
	return h.Sum(nil)
}

// evolveNonce updates the evolving nonce with a new nonce contribution.
// eta_v = BLAKE2b-256(eta_v || nonceValue)
// Verified against pallas/cncli test vectors.
func evolveNonce(currentNonce, nonceValue []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(currentNonce)
	h.Write(nonceValue)
	return h.Sum(nil)
}

// hashConcat computes BLAKE2b-256(a || b) for epoch nonce transitions.
func hashConcat(a, b []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(a)
	h.Write(b)
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

	// Insert block first — if it's a duplicate (already exists), skip nonce evolution
	// to prevent corrupting the evolving nonce on restart.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inserted, err := nt.store.InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue)
	if err != nil {
		log.Printf("Failed to insert block %d: %v", slot, err)
		return
	}

	// Only evolve nonce if this block was actually new (not a duplicate)
	if !inserted {
		return
	}

	// Update evolving nonce
	nt.evolvingNonce = evolveNonce(nt.evolvingNonce, nonceValue)
	nt.blockCount++

	if err := nt.store.UpsertEvolvingNonce(ctx, epoch, nt.evolvingNonce, nt.blockCount); err != nil {
		log.Printf("Failed to upsert evolving nonce for epoch %d: %v", epoch, err)
	}
}

// ProcessBatch evolves the nonce in-memory for a pre-inserted batch of blocks.
// Blocks must already be inserted into the DB via InsertBlockBatch (CopyFrom).
// Persists the evolving nonce once per epoch transition and once at the end.
func (nt *NonceTracker) ProcessBatch(blocks []BlockData) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	for _, b := range blocks {
		if b.Epoch != nt.currentEpoch {
			// Persist nonce for outgoing epoch before transition
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := nt.store.UpsertEvolvingNonce(ctx, nt.currentEpoch, nt.evolvingNonce, nt.blockCount); err != nil {
				log.Printf("Failed to persist nonce for epoch %d at transition: %v", nt.currentEpoch, err)
			}
			cancel()

			log.Printf("Epoch transition: %d -> %d (block count: %d)", nt.currentEpoch, b.Epoch, nt.blockCount)
			nt.currentEpoch = b.Epoch
			nt.blockCount = 0
			nt.candidateFroze = false
		}

		nonceValue := vrfNonceValue(b.VrfOutput)
		nt.evolvingNonce = evolveNonce(nt.evolvingNonce, nonceValue)
		nt.blockCount++
	}

	// Persist final state
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := nt.store.UpsertEvolvingNonce(ctx, nt.currentEpoch, nt.evolvingNonce, nt.blockCount); err != nil {
		log.Printf("Failed to upsert evolving nonce for epoch %d: %v", nt.currentEpoch, err)
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

// GetNonceForEpoch returns the epoch nonce. Priority:
// 1. Local DB final_nonce cache
// 2. Compute from chain data (full mode only)
// 3. Koios fallback (lite mode)
func (nt *NonceTracker) GetNonceForEpoch(epoch int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try local DB first
	nonce, err := nt.store.GetFinalNonce(ctx, epoch)
	if err == nil && nonce != nil {
		log.Printf("Using cached nonce for epoch %d", epoch)
		return nonce, nil
	}

	// Full mode: compute from chain data (no external dependencies)
	if nt.fullMode {
		log.Printf("Computing epoch %d nonce from chain data...", epoch)
		computeCtx, computeCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer computeCancel()
		nonce, err = nt.ComputeEpochNonce(computeCtx, epoch)
		if err != nil {
			return nil, fmt.Errorf("failed to compute nonce for epoch %d: %w", epoch, err)
		}
		// Cache computed nonce
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cacheCancel()
		if storeErr := nt.store.SetFinalNonce(cacheCtx, epoch, nonce, "computed"); storeErr != nil {
			log.Printf("Failed to cache computed nonce for epoch %d: %v", epoch, storeErr)
		}
		return nonce, nil
	}

	// Lite mode only: Koios fallback
	log.Printf("Falling back to Koios for epoch %d nonce (lite mode)", epoch)
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

// ComputeEpochNonce computes the epoch nonce for targetEpoch entirely from local chain data.
// Streams all blocks from Shelley genesis, evolving the nonce and freezing at the
// stability window of each epoch, then computing:
//
//	η(new) = BLAKE2b-256(η_c || η_ph)  (per pallas/cncli)
func (nt *NonceTracker) ComputeEpochNonce(ctx context.Context, targetEpoch int) ([]byte, error) {
	shelleyStart := ShelleyStartEpoch
	if nt.networkMagic == PreprodNetworkMagic {
		shelleyStart = PreprodShelleyStartEpoch
	}
	if targetEpoch <= shelleyStart {
		return nil, fmt.Errorf("cannot compute nonce for epoch %d (shelley starts at %d)", targetEpoch, shelleyStart)
	}

	genesisHash, _ := hex.DecodeString(ShelleyGenesisHash)
	etaV := make([]byte, 32)
	copy(etaV, genesisHash)
	eta0 := make([]byte, 32) // eta_0(shelleyStart) = shelley genesis hash
	copy(eta0, genesisHash)
	etaC := make([]byte, 32)
	prevHashNonce := make([]byte, 32) // η_ph — NeutralNonce at Shelley start
	var lastBlockHash string

	currentEpoch := shelleyStart
	candidateFrozen := false

	rows, err := nt.store.StreamBlockNonces(ctx)
	if err != nil {
		return nil, fmt.Errorf("streaming blocks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		epoch, slot, nonceValue, blockHash, err := rows.Scan()
		if err != nil {
			return nil, fmt.Errorf("scanning block: %w", err)
		}

		// Epoch transition: η(new) = BLAKE2b-256(η_c || η_ph)
		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}
			eta0 = hashConcat(etaC, prevHashNonce)
			if lastBlockHash != "" {
				prevHashNonce, _ = hex.DecodeString(lastBlockHash)
			}

			// If we just transitioned INTO the target epoch, we have eta_0(target)
			if epoch == targetEpoch {
				rows.Close()
				log.Printf("Computed nonce for epoch %d: %s", targetEpoch, hex.EncodeToString(eta0))
				return eta0, nil
			}

			currentEpoch = epoch
			candidateFrozen = false
		}

		// Evolve eta_v
		etaV = evolveNonce(etaV, nonceValue)
		lastBlockHash = blockHash

		// Freeze candidate at stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, nt.networkMagic)
			stabilitySlot := epochStart + StabilityWindowSlots(nt.networkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// If we processed all blocks and target is next epoch (not yet started)
	if !candidateFrozen {
		etaC = make([]byte, 32)
		copy(etaC, etaV)
	}
	if lastBlockHash != "" {
		prevHashNonce, _ = hex.DecodeString(lastBlockHash)
	}
	result := hashConcat(etaC, prevHashNonce)
	log.Printf("Computed nonce for epoch %d: %s", targetEpoch, hex.EncodeToString(result))
	return result, nil
}

// BackfillNonces streams all blocks from Shelley genesis in a single pass,
// computing and caching every epoch's nonce in the DB. Skips epochs that
// already have a final_nonce. After completion, verifies the latest nonce
// against Koios as an integrity check.
func (nt *NonceTracker) BackfillNonces(ctx context.Context) error {
	shelleyStart := ShelleyStartEpoch
	if nt.networkMagic == PreprodNetworkMagic {
		shelleyStart = PreprodShelleyStartEpoch
	}

	genesisHash, _ := hex.DecodeString(ShelleyGenesisHash)
	etaV := make([]byte, 32)
	copy(etaV, genesisHash)
	eta0 := make([]byte, 32)
	copy(eta0, genesisHash)
	etaC := make([]byte, 32)
	prevHashNonce := make([]byte, 32) // η_ph — NeutralNonce at Shelley start
	var lastBlockHash string

	currentEpoch := shelleyStart
	candidateFrozen := false
	cached := 0
	skipped := 0
	lastCachedEpoch := 0

	start := time.Now()
	log.Printf("Nonce backfill starting from epoch %d...", shelleyStart)

	rows, err := nt.store.StreamBlockNonces(ctx)
	if err != nil {
		return fmt.Errorf("streaming blocks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		epoch, slot, nonceValue, blockHash, scanErr := rows.Scan()
		if scanErr != nil {
			return fmt.Errorf("scanning block: %w", scanErr)
		}

		// Epoch transition: η(new) = BLAKE2b-256(η_c || η_ph)
		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}
			eta0 = hashConcat(etaC, prevHashNonce)
			if lastBlockHash != "" {
				prevHashNonce, _ = hex.DecodeString(lastBlockHash)
			}

			// Cache if not already present
			existing, _ := nt.store.GetFinalNonce(ctx, epoch)
			if existing == nil {
				if storeErr := nt.store.SetFinalNonce(ctx, epoch, eta0, "backfill"); storeErr != nil {
					log.Printf("Failed to cache nonce for epoch %d: %v", epoch, storeErr)
				} else {
					cached++
					lastCachedEpoch = epoch
				}
			} else {
				skipped++
			}

			currentEpoch = epoch
			candidateFrozen = false
		}

		// Evolve eta_v
		etaV = evolveNonce(etaV, nonceValue)
		lastBlockHash = blockHash

		// Freeze candidate at stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, nt.networkMagic)
			stabilitySlot := epochStart + StabilityWindowSlots(nt.networkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("row iteration: %w", err)
	}

	log.Printf("Nonce backfill complete in %v: %d cached, %d skipped (already present)",
		time.Since(start).Round(time.Second), cached, skipped)

	// Integrity check: verify most recent nonce against Koios
	if lastCachedEpoch > 0 && nt.koiosClient != nil {
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer verifyCancel()
		computed, _ := nt.store.GetFinalNonce(verifyCtx, lastCachedEpoch)
		koiosNonce, koiosErr := nt.fetchNonceFromKoios(verifyCtx, lastCachedEpoch)
		if koiosErr != nil {
			log.Printf("Nonce integrity check: could not fetch Koios nonce for epoch %d: %v", lastCachedEpoch, koiosErr)
		} else if hex.EncodeToString(computed) == hex.EncodeToString(koiosNonce) {
			log.Printf("Nonce integrity verified: epoch %d matches Koios", lastCachedEpoch)
		} else {
			log.Printf("WARNING: nonce mismatch for epoch %d! Computed: %x, Koios: %x",
				lastCachedEpoch, computed, koiosNonce)
		}
	}

	return nil
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
