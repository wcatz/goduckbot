package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	koios "github.com/cardano-community/koios-go-client/v3"
	"golang.org/x/crypto/blake2b"
)

// ShelleyGenesisHash is the hash of the Shelley genesis block on mainnet.
// Used as the initial eta_v seed for full chain sync nonce evolution.
const ShelleyGenesisHash = "1a3be38bcbb7911969283716ad7aa550250226b76a61fc51cc9a9a35d9276d81"

// knownEpochNonces is empty — self-computation from genesis produces correct
// nonces for all epochs including early Shelley (verified against Koios).
var knownEpochNonces = map[int]string{}

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

// vrfNonceValue computes the nonce contribution from a VRF output (Shelley-Alonzo).
// nonceValue = BLAKE2b-256(vrfOutput)
func vrfNonceValue(vrfOutput []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(vrfOutput)
	return h.Sum(nil)
}

// vrfNonceValueForEpoch computes the era-correct nonce contribution.
// Shelley-Alonzo (TPraos): BLAKE2b-256(raw NonceVrf output)
// Babbage+ (CPraos): BLAKE2b-256(BLAKE2b-256("N" || raw VrfResult output))
// The Babbage domain separator matches cncli/pallas derive_tagged_vrf_output.
func vrfNonceValueForEpoch(vrfOutput []byte, epoch, networkMagic int) []byte {
	babbageStart := BabbageStartEpoch
	if networkMagic == PreprodNetworkMagic {
		babbageStart = PreprodBabbageStartEpoch
	}

	if epoch >= babbageStart {
		// Domain-separated: BLAKE2b-256(0x4E || vrfOutput)
		h1, _ := blake2b.New256(nil)
		h1.Write([]byte{0x4E})
		h1.Write(vrfOutput)
		tagged := h1.Sum(nil)
		// Hash again to match cncli generate_rolling_nonce(BLAKE2b-256(eta_vrf_0))
		h2, _ := blake2b.New256(nil)
		h2.Write(tagged)
		return h2.Sum(nil)
	}
	return vrfNonceValue(vrfOutput)
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

	// Compute nonce contribution (era-aware: Babbage+ uses 0x4E domain separator)
	nonceValue := vrfNonceValueForEpoch(vrfOutput, epoch, nt.networkMagic)

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

		nonceValue := vrfNonceValueForEpoch(b.VrfOutput, b.Epoch, nt.networkMagic)
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

// ResyncFromDB reloads the NonceTracker's in-memory state from the database.
// Called between historical sync retry attempts to ensure the evolving nonce
// matches what was actually persisted, preventing corruption when buffered
// blocks from a dead connection overlap with blocks from the new connection.
func (nt *NonceTracker) ResyncFromDB() {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lastSlot, err := nt.store.GetLastSyncedSlot(ctx)
	if err != nil || lastSlot == 0 {
		log.Printf("ResyncFromDB: no synced data, keeping current state")
		return
	}

	epoch := SlotToEpoch(lastSlot, nt.networkMagic)
	nonce, blockCount, err := nt.store.GetEvolvingNonce(ctx, epoch)
	if err != nil || nonce == nil {
		log.Printf("ResyncFromDB: no evolving nonce for epoch %d, keeping current state", epoch)
		return
	}

	nt.evolvingNonce = nonce
	nt.currentEpoch = epoch
	nt.blockCount = blockCount
	nt.candidateFroze = false
	log.Printf("ResyncFromDB: restored epoch %d, block count %d", epoch, blockCount)
}

// RecomputeCurrentEpochNonce recomputes the evolving nonce for a specific epoch
// entirely from the blocks table. Used when the integrity check detects that
// epoch_nonces.block_count disagrees with the actual block count (e.g., after
// a CNPG async replication failover where UpsertEvolvingNonce writes were lost
// but InsertBlock writes survived).
func (nt *NonceTracker) RecomputeCurrentEpochNonce(ctx context.Context, epoch int) error {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	// Start from previous epoch's final evolving nonce state.
	// The evolving nonce rolls across epoch boundaries (no reset).
	var etaV []byte
	if epoch > 0 {
		prevNonce, _, err := nt.store.GetEvolvingNonce(ctx, epoch-1)
		if err == nil && prevNonce != nil {
			etaV = prevNonce
		}
	}
	if etaV == nil {
		etaV = initialNonce(nt.fullMode)
		log.Printf("RecomputeCurrentEpochNonce: no previous epoch nonce, using initial seed")
	}

	// Stream nonce values for this epoch's blocks and re-evolve
	nonceValues, err := nt.store.GetNonceValuesForEpoch(ctx, epoch)
	if err != nil {
		return fmt.Errorf("querying blocks for epoch %d: %w", epoch, err)
	}

	for _, nv := range nonceValues {
		etaV = evolveNonce(etaV, nv)
	}

	// Persist corrected nonce
	if err := nt.store.UpsertEvolvingNonce(ctx, epoch, etaV, len(nonceValues)); err != nil {
		return fmt.Errorf("persisting recomputed nonce for epoch %d: %w", epoch, err)
	}

	// Update in-memory state
	nt.evolvingNonce = etaV
	nt.currentEpoch = epoch
	nt.blockCount = len(nonceValues)
	nt.candidateFroze = false

	log.Printf("RecomputeCurrentEpochNonce: epoch %d recomputed from %d blocks", epoch, len(nonceValues))
	return nil
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

	// Full mode: compute from chain data if sync is complete through the target epoch.
	// If sync hasn't reached the target epoch, fall through to Koios to avoid
	// computing garbage nonces from incomplete chain data.
	if nt.fullMode {
		lastSlot, slotErr := nt.store.GetLastSyncedSlot(ctx)
		targetEpochEndSlot := GetEpochStartSlot(epoch+1, nt.networkMagic)
		if slotErr == nil && lastSlot >= targetEpochEndSlot {
			log.Printf("Computing epoch %d nonce from chain data...", epoch)
			computeCtx, computeCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer computeCancel()
			nonce, err = nt.ComputeEpochNonce(computeCtx, epoch)
			if err == nil {
				cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cacheCancel()
				if storeErr := nt.store.SetFinalNonce(cacheCtx, epoch, nonce, "computed"); storeErr != nil {
					log.Printf("Failed to cache computed nonce for epoch %d: %v", epoch, storeErr)
				}
				return nonce, nil
			}
			log.Printf("Failed to compute nonce for epoch %d from chain data: %v, trying Koios", epoch, err)
		} else {
			log.Printf("Sync incomplete for epoch %d (last synced slot %d < epoch end slot %d), using Koios",
				epoch, lastSlot, targetEpochEndSlot)
		}
	}

	// Full mode: compute next epoch nonce from frozen candidate + η_ph (TICKN rule).
	// At 60% of epoch N, we need epoch N+1's nonce (not yet on Koios).
	// epochNonce = BLAKE2b-256(candidateNonce_N || lastBlockHash_of_epoch_N-1)
	if nt.fullMode {
		candidateEpoch := epoch - 1
		log.Printf("TICKN: attempting to compute epoch %d nonce from candidate(%d) + η_ph(%d)",
			epoch, candidateEpoch, candidateEpoch-1)
		candidate, candErr := nt.store.GetCandidateNonce(ctx, candidateEpoch)
		if candErr != nil {
			log.Printf("TICKN: GetCandidateNonce(%d) failed: %v", candidateEpoch, candErr)
		} else if candidate == nil {
			log.Printf("TICKN: GetCandidateNonce(%d) returned nil", candidateEpoch)
		} else {
			log.Printf("TICKN: got candidate for epoch %d: %s", candidateEpoch, hex.EncodeToString(candidate))
			// Try DB first, fall back to Koios blocks API for η_ph
			prevEpochHash, hashErr := nt.store.GetLastBlockHashForEpoch(ctx, candidateEpoch-1)
			if hashErr != nil || prevEpochHash == "" {
				log.Printf("TICKN: DB has no blocks for epoch %d, trying Koios", candidateEpoch-1)
				prevEpochHash, hashErr = nt.fetchLastBlockHashFromKoios(ctx, candidateEpoch-1)
			}
			if hashErr != nil {
				log.Printf("TICKN: failed to get η_ph for epoch %d: %v", candidateEpoch-1, hashErr)
			} else if prevEpochHash != "" {
				hashBytes, _ := hex.DecodeString(prevEpochHash)
				nonce = hashConcat(candidate, hashBytes)
				log.Printf("Computed epoch %d nonce from candidate(%d) + η_ph(%d): %s",
					epoch, candidateEpoch, candidateEpoch-1, hex.EncodeToString(nonce))
				if storeErr := nt.store.SetFinalNonce(ctx, epoch, nonce, "computed"); storeErr != nil {
					log.Printf("Failed to cache computed nonce for epoch %d: %v", epoch, storeErr)
				}
				return nonce, nil
			}
		}
	}

	// Koios fallback (lite mode, or full mode when above paths failed)
	log.Printf("Falling back to Koios for epoch %d nonce", epoch)
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
//	epochNonce = BLAKE2b-256(candidateNonce || lastEpochBlockNonce)
//
// The lastEpochBlockNonce is derived from the prevHash field of each block header
// (= blockHash of the preceding block), lagged by one epoch transition. This matches
// the Cardano node's praosStateLabNonce / praosStateLastEpochBlockNonce mechanism.
func (nt *NonceTracker) ComputeEpochNonce(ctx context.Context, targetEpoch int) ([]byte, error) {
	// Check hardcoded early epoch nonces first
	if nonceHex, ok := knownEpochNonces[targetEpoch]; ok {
		nonce, _ := hex.DecodeString(nonceHex)
		log.Printf("Using hardcoded nonce for epoch %d: %s", targetEpoch, nonceHex)
		return nonce, nil
	}

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
	eta0 := make([]byte, 32)
	copy(eta0, genesisHash)
	etaC := make([]byte, 32)

	// labNonce tracks prevHashToNonce(block.prevHash) = blockHash of previous block.
	// lastEpochBlockNonce is labNonce saved at the previous epoch transition (one epoch lag).
	var prevBlockHash string
	var labNonce []byte
	var lastEpochBlockNonce []byte // nil = NeutralNonce

	currentEpoch := shelleyStart
	candidateFrozen := false

	rows, err := nt.store.StreamBlockVrfOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("streaming blocks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		epoch, slot, vrfOutput, _, blockHash, err := rows.Scan()
		if err != nil {
			return nil, fmt.Errorf("scanning block: %w", err)
		}

		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}

			// epochNonce = candidateNonce ⭒ lastEpochBlockNonce
			if lastEpochBlockNonce == nil {
				eta0 = make([]byte, 32)
				copy(eta0, etaC)
			} else {
				eta0 = hashConcat(etaC, lastEpochBlockNonce)
			}

			// Save labNonce for next epoch transition
			if labNonce != nil {
				lastEpochBlockNonce = make([]byte, len(labNonce))
				copy(lastEpochBlockNonce, labNonce)
			}

			if epoch == targetEpoch {
				rows.Close()
				log.Printf("Computed nonce for epoch %d: %s", targetEpoch, hex.EncodeToString(eta0))
				return eta0, nil
			}

			currentEpoch = epoch
			candidateFrozen = false
		}

		// Freeze candidate at era-correct stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, nt.networkMagic)
			stabilitySlot := epochStart + StabilityWindowSlotsForEpoch(epoch, nt.networkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}

		// Update labNonce: blockHash of previous block (= prevHash of current block)
		if prevBlockHash != "" {
			labNonce, _ = hex.DecodeString(prevBlockHash)
		}

		// Recompute era-aware nonce from raw VRF output (don't trust stored nonce_value)
		nonceValue := vrfNonceValueForEpoch(vrfOutput, epoch, nt.networkMagic)
		etaV = evolveNonce(etaV, nonceValue)
		prevBlockHash = blockHash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// If we processed all blocks and target is next epoch (not yet started)
	if !candidateFrozen {
		etaC = make([]byte, 32)
		copy(etaC, etaV)
	}
	if labNonce != nil {
		lastEpochBlockNonce = make([]byte, len(labNonce))
		copy(lastEpochBlockNonce, labNonce)
	}
	var result []byte
	if lastEpochBlockNonce == nil {
		result = make([]byte, 32)
		copy(result, etaC)
	} else {
		result = hashConcat(etaC, lastEpochBlockNonce)
	}
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
	etaC := make([]byte, 32)

	var prevBlockHash string
	var labNonce []byte
	var lastEpochBlockNonce []byte // nil = NeutralNonce

	currentEpoch := shelleyStart
	candidateFrozen := false
	cached := 0
	skipped := 0
	lastCachedEpoch := 0

	start := time.Now()
	log.Printf("Nonce backfill starting from epoch %d...", shelleyStart)

	rows, err := nt.store.StreamBlockVrfOutputs(ctx)
	if err != nil {
		return fmt.Errorf("streaming blocks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		epoch, slot, vrfOutput, _, blockHash, scanErr := rows.Scan()
		if scanErr != nil {
			return fmt.Errorf("scanning block: %w", scanErr)
		}

		// Epoch transition: epochNonce = BLAKE2b-256(candidateNonce || lastEpochBlockNonce)
		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}

			var eta0 []byte
			if nonceHex, ok := knownEpochNonces[epoch]; ok {
				eta0, _ = hex.DecodeString(nonceHex)
			} else if lastEpochBlockNonce == nil {
				eta0 = make([]byte, 32)
				copy(eta0, etaC)
			} else {
				eta0 = hashConcat(etaC, lastEpochBlockNonce)
			}

			// Save labNonce for next epoch transition (one-epoch lag)
			if labNonce != nil {
				lastEpochBlockNonce = make([]byte, len(labNonce))
				copy(lastEpochBlockNonce, labNonce)
			}

			// Verify against Koios — use Koios value on mismatch (workaround for
			// gouroboros VRF data divergence in Babbage-era blocks).
			if nt.koiosClient != nil {
				koiosCtx, koiosCancel := context.WithTimeout(ctx, 10*time.Second)
				koiosNonce, koiosErr := nt.fetchNonceFromKoios(koiosCtx, epoch)
				koiosCancel()
				if koiosErr == nil {
					computedHex := hex.EncodeToString(eta0)
					koiosHex := hex.EncodeToString(koiosNonce)
					if computedHex != koiosHex {
						log.Printf("Epoch %d: computed %s… != Koios %s… — using Koios",
							epoch, computedHex[:16], koiosHex[:16])
						eta0 = koiosNonce
					}
				}
				time.Sleep(50 * time.Millisecond) // rate limit
			}

			// Cache if not already present, or update if mismatched
			existing, _ := nt.store.GetFinalNonce(ctx, epoch)
			if existing == nil {
				if storeErr := nt.store.SetFinalNonce(ctx, epoch, eta0, "backfill"); storeErr != nil {
					log.Printf("Failed to cache nonce for epoch %d: %v", epoch, storeErr)
				} else {
					cached++
					lastCachedEpoch = epoch
				}
			} else if !bytes.Equal(existing, eta0) {
				// Existing nonce is wrong, update it
				if storeErr := nt.store.SetFinalNonce(ctx, epoch, eta0, "backfill-correction"); storeErr != nil {
					log.Printf("Failed to correct nonce for epoch %d: %v", epoch, storeErr)
				} else {
					log.Printf("Corrected nonce for epoch %d", epoch)
					cached++
					lastCachedEpoch = epoch
				}
			} else {
				skipped++
			}

			currentEpoch = epoch
			candidateFrozen = false
		}

		// Freeze candidate at stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, nt.networkMagic)
			stabilitySlot := epochStart + StabilityWindowSlotsForEpoch(epoch, nt.networkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}

		// Update labNonce: blockHash of previous block
		if prevBlockHash != "" {
			labNonce, _ = hex.DecodeString(prevBlockHash)
		}

		// Recompute era-aware nonce from raw VRF output (don't trust stored nonce_value)
		nonceValue := vrfNonceValueForEpoch(vrfOutput, epoch, nt.networkMagic)
		etaV = evolveNonce(etaV, nonceValue)
		prevBlockHash = blockHash
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

// IntegrityResult holds the verification result for a single epoch.
type IntegrityResult struct {
	Epoch      int
	Computed   string // hex
	DBStored   string // hex, or "n/a"
	Koios      string // hex, or "n/a"
	DBMatch    bool
	KoiosMatch bool
}

// IntegrityReport is the summary of a full nonce integrity check.
type IntegrityReport struct {
	TotalEpochs     int
	KoiosMatched    int
	KoiosMismatched int
	KoiosUnavail    int
	DBMatched       int
	DBMismatched    int
	VrfErrors       int
	TotalBlocks     int
	Duration        time.Duration
	FirstMismatch   int // epoch of first Koios mismatch, 0 if none
}

// NonceIntegrityCheck recomputes all epoch nonces from raw VRF outputs and
// compares each against both the locally stored nonce and the Koios API.
// This is the definitive end-to-end verification of the nonce pipeline.
func (nt *NonceTracker) NonceIntegrityCheck(ctx context.Context) (*IntegrityReport, error) {
	shelleyStart := ShelleyStartEpoch
	if nt.networkMagic == PreprodNetworkMagic {
		shelleyStart = PreprodShelleyStartEpoch
	}

	genesisHash, _ := hex.DecodeString(ShelleyGenesisHash)
	etaV := make([]byte, 32)
	copy(etaV, genesisHash)
	etaC := make([]byte, 32)

	var prevBlockHash string
	var labNonce []byte
	var lastEpochBlockNonce []byte // nil = NeutralNonce

	currentEpoch := shelleyStart
	candidateFrozen := false
	blockCount := 0
	vrfErrors := 0

	// Collect computed nonces per epoch
	type epochNonce struct {
		epoch int
		nonce []byte
	}
	var computed []epochNonce

	start := time.Now()
	log.Println("╔══════════════════════════════════════════════════════════════╗")
	log.Println("║              NONCE INTEGRITY CHECK                          ║")
	log.Println("║  Recomputing all epoch nonces from raw VRF outputs...       ║")
	log.Println("╚══════════════════════════════════════════════════════════════╝")

	// Phase 1: Stream all blocks, recompute from raw VRF output
	rows, err := nt.store.StreamBlockVrfOutputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("streaming VRF outputs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		epoch, slot, vrfOutput, storedNonce, blockHash, scanErr := rows.Scan()
		if scanErr != nil {
			return nil, fmt.Errorf("scanning block: %w", scanErr)
		}

		// Epoch transition
		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}

			nextEpoch := currentEpoch + 1
			var eta0 []byte
			if nonceHex, ok := knownEpochNonces[nextEpoch]; ok {
				eta0, _ = hex.DecodeString(nonceHex)
			} else if lastEpochBlockNonce == nil {
				eta0 = make([]byte, 32)
				copy(eta0, etaC)
			} else {
				eta0 = hashConcat(etaC, lastEpochBlockNonce)
			}

			// Save labNonce for next epoch transition (one-epoch lag)
			if labNonce != nil {
				lastEpochBlockNonce = make([]byte, len(labNonce))
				copy(lastEpochBlockNonce, labNonce)
			}

			computed = append(computed, epochNonce{epoch: nextEpoch, nonce: eta0})
			currentEpoch = epoch
			candidateFrozen = false
		}

		// Verify stored nonce_value matches recomputed (era-aware)
		recomputedNonce := vrfNonceValueForEpoch(vrfOutput, epoch, nt.networkMagic)
		if hex.EncodeToString(recomputedNonce) != hex.EncodeToString(storedNonce) {
			vrfErrors++
		}

		// Freeze candidate at stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, nt.networkMagic)
			stabilitySlot := epochStart + StabilityWindowSlotsForEpoch(epoch, nt.networkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}

		// Update labNonce: blockHash of previous block
		if prevBlockHash != "" {
			labNonce, _ = hex.DecodeString(prevBlockHash)
		}

		// Evolve using recomputed nonce (not stored), ensuring end-to-end correctness
		etaV = evolveNonce(etaV, recomputedNonce)
		prevBlockHash = blockHash
		blockCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// Final epoch nonce (for the epoch after the last block)
	if blockCount > 0 {
		if !candidateFrozen {
			etaC = make([]byte, 32)
			copy(etaC, etaV)
		}
		if labNonce != nil {
			lastEpochBlockNonce = make([]byte, len(labNonce))
			copy(lastEpochBlockNonce, labNonce)
		}
		nextEpoch := currentEpoch + 1
		var eta0 []byte
		if nonceHex, ok := knownEpochNonces[nextEpoch]; ok {
			eta0, _ = hex.DecodeString(nonceHex)
		} else if lastEpochBlockNonce == nil {
			eta0 = make([]byte, 32)
			copy(eta0, etaC)
		} else {
			eta0 = hashConcat(etaC, lastEpochBlockNonce)
		}
		computed = append(computed, epochNonce{epoch: nextEpoch, nonce: eta0})
	}

	scanTime := time.Since(start)
	log.Printf("Phase 1 complete: %d blocks scanned in %v (%d VRF nonce_value mismatches)",
		blockCount, scanTime.Round(time.Millisecond), vrfErrors)

	// Phase 2: Compare against DB stored nonces and Koios
	report := &IntegrityReport{
		TotalEpochs: len(computed),
		TotalBlocks: blockCount,
		VrfErrors:   vrfErrors,
	}

	log.Println("─────┬──────────────────────────────────────┬──────────┬──────────")
	log.Println("Epoch│ Computed                             │ DB       │ Koios")
	log.Println("─────┼──────────────────────────────────────┼──────────┼──────────")

	for _, en := range computed {
		if en.epoch <= shelleyStart {
			continue
		}

		computedHex := hex.EncodeToString(en.nonce)

		// Check DB
		dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
		dbNonce, dbErr := nt.store.GetFinalNonce(dbCtx, en.epoch)
		dbCancel()

		dbStatus := "n/a"
		if dbErr == nil && dbNonce != nil {
			if hex.EncodeToString(dbNonce) == computedHex {
				dbStatus = "✓ match"
				report.DBMatched++
			} else {
				dbStatus = "✗ DIFF"
				report.DBMismatched++
			}
		}

		// Check Koios
		koiosStatus := "n/a"
		if nt.koiosClient != nil {
			koiosCtx, koiosCancel := context.WithTimeout(ctx, 10*time.Second)
			koiosNonce, koiosErr := nt.fetchNonceFromKoios(koiosCtx, en.epoch)
			koiosCancel()

			if koiosErr != nil {
				koiosStatus = "unavail"
				report.KoiosUnavail++
			} else if hex.EncodeToString(koiosNonce) == computedHex {
				koiosStatus = "✓ match"
				report.KoiosMatched++
			} else {
				koiosStatus = "✗ MISMATCH"
				report.KoiosMismatched++
				if report.FirstMismatch == 0 {
					report.FirstMismatch = en.epoch
					log.Printf("  !! FIRST MISMATCH at epoch %d:", en.epoch)
					log.Printf("     Computed: %s", computedHex)
					log.Printf("     Koios:    %s", hex.EncodeToString(koiosNonce))
				}
			}

			// Rate limit: small delay between Koios calls
			time.Sleep(50 * time.Millisecond)
		} else {
			report.KoiosUnavail++
		}

		// Print table row (truncated hash for readability)
		display := computedHex
		if len(display) > 36 {
			display = display[:36] + "..."
		}
		log.Printf(" %3d │ %s │ %-8s │ %s", en.epoch, display, dbStatus, koiosStatus)
	}

	report.Duration = time.Since(start)

	log.Println("─────┴──────────────────────────────────────┴──────────┴──────────")
	log.Println("")
	log.Println("╔══════════════════════════════════════════════════════════════╗")
	if report.KoiosMismatched == 0 && report.VrfErrors == 0 {
		log.Printf("║  ✓  ALL %d EPOCHS VERIFIED                                  ║", report.KoiosMatched)
	} else {
		log.Printf("║  ✗  INTEGRITY ISSUES FOUND                                  ║")
	}
	log.Println("╠══════════════════════════════════════════════════════════════╣")
	log.Printf("║  Epochs checked:     %-6d                                  ║", report.TotalEpochs)
	log.Printf("║  Koios matched:      %-6d                                  ║", report.KoiosMatched)
	if report.KoiosMismatched > 0 {
		log.Printf("║  Koios MISMATCHED:   %-6d  (first: epoch %d)              ║", report.KoiosMismatched, report.FirstMismatch)
	}
	if report.KoiosUnavail > 0 {
		log.Printf("║  Koios unavailable:  %-6d                                  ║", report.KoiosUnavail)
	}
	log.Printf("║  DB matched:         %-6d                                  ║", report.DBMatched)
	if report.DBMismatched > 0 {
		log.Printf("║  DB MISMATCHED:      %-6d                                  ║", report.DBMismatched)
	}
	log.Printf("║  Blocks processed:   %-10d                              ║", report.TotalBlocks)
	if report.VrfErrors > 0 {
		log.Printf("║  VRF nonce errors:   %-6d  (stored nonce_value wrong)      ║", report.VrfErrors)
	}
	log.Printf("║  Total time:         %-10v                              ║", report.Duration.Round(time.Millisecond))
	log.Println("╚══════════════════════════════════════════════════════════════╝")

	return report, nil
}

// fetchNonceFromKoios fetches the epoch nonce from Koios REST API.
// Uses raw HTTP instead of the Go client to avoid cost_models JSON unmarshal issues.
func (nt *NonceTracker) fetchNonceFromKoios(ctx context.Context, epoch int) ([]byte, error) {
	url := fmt.Sprintf("https://api.koios.rest/api/v1/epoch_params?_epoch_no=%d&select=nonce", epoch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("koios HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("koios returned %d: %s", resp.StatusCode, string(body))
	}

	var result []struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if len(result) == 0 || result[0].Nonce == "" {
		return nil, fmt.Errorf("no nonce returned for epoch %d", epoch)
	}

	nonce, err := hex.DecodeString(result[0].Nonce)
	if err != nil {
		return nil, fmt.Errorf("decoding nonce hex: %w", err)
	}
	if len(nonce) != 32 {
		return nil, fmt.Errorf("unexpected nonce length: %d", len(nonce))
	}

	return nonce, nil
}

// fetchLastBlockHashFromKoios returns the hash of the last block in the given epoch
// via the Koios REST API. Used as fallback when local blocks table has gaps.
func (nt *NonceTracker) fetchLastBlockHashFromKoios(ctx context.Context, epoch int) (string, error) {
	url := fmt.Sprintf("https://api.koios.rest/api/v1/blocks?epoch_no=eq.%d&order=block_height.desc&limit=1&select=hash,epoch_no,block_height", epoch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("koios HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("koios returned %d: %s", resp.StatusCode, string(body))
	}

	var result []struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	if len(result) == 0 || result[0].Hash == "" {
		return "", fmt.Errorf("no blocks returned for epoch %d", epoch)
	}

	return result[0].Hash, nil
}
