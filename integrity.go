package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/protocol/chainsync"
	pcommon "github.com/blinklabs-io/gouroboros/protocol/common"
)

// DBIntegrityResult describes the outcome of a startup integrity check.
type DBIntegrityResult struct {
	Valid     bool // DB is consistent with chain
	Truncated bool // DB was corrupt and has been wiped
	Repaired  bool // Nonce was stale and has been recomputed
}

// ValidateDBIntegrity checks database consistency against the cardano-node
// before resuming chain sync. This catches data loss from CNPG async
// replication failover where recent writes were lost.
//
// Layer 1: Block count in blocks table vs epoch_nonces.block_count
// Layer 2: FindIntersect last N blocks against cardano-node
//
// If blocks are orphaned: truncate all tables (forces full resync).
// If nonce is stale but blocks are valid: recompute current epoch nonce.
// If node is unreachable: return error (do NOT truncate blindly).
func ValidateDBIntegrity(ctx context.Context, store Store, nonceTracker *NonceTracker, nodeAddress string, networkMagic int) (DBIntegrityResult, error) {
	start := time.Now()
	log.Println("Starting database integrity check...")

	// Check if DB has any data
	lastSlot, err := store.GetLastSyncedSlot(ctx)
	if err != nil || lastSlot == 0 {
		log.Println("Database is empty, skipping integrity check")
		return DBIntegrityResult{Valid: true}, nil
	}

	epoch := SlotToEpoch(lastSlot, networkMagic)
	log.Printf("Last synced slot: %d (epoch %d)", lastSlot, epoch)

	// Layer 1: Block count consistency
	nonceStale := false
	dbCount, err := store.GetBlockCountForEpoch(ctx, epoch)
	if err != nil {
		log.Printf("WARNING: could not get block count for epoch %d: %v", epoch, err)
	} else {
		_, storedCount, nonceErr := store.GetEvolvingNonce(ctx, epoch)
		if nonceErr != nil {
			log.Printf("WARNING: no evolving nonce for epoch %d: %v", epoch, nonceErr)
			nonceStale = true
		} else if dbCount != storedCount {
			log.Printf("Nonce block count mismatch for epoch %d: blocks table has %d, nonce reflects %d",
				epoch, dbCount, storedCount)
			nonceStale = true
		} else {
			log.Printf("Layer 1 passed: epoch %d block count consistent (%d blocks)", epoch, dbCount)
		}
	}

	// Layer 2: FindIntersect validation against cardano-node
	blocks, err := store.GetLastNBlocks(ctx, 50)
	if err != nil {
		return DBIntegrityResult{}, fmt.Errorf("fetching recent blocks: %w", err)
	}
	if len(blocks) == 0 {
		log.Println("No blocks in database, skipping chain validation")
		return DBIntegrityResult{Valid: true}, nil
	}

	points := make([]pcommon.Point, len(blocks))
	for i, b := range blocks {
		hashBytes, decodeErr := hex.DecodeString(b.BlockHash)
		if decodeErr != nil {
			return DBIntegrityResult{}, fmt.Errorf("decoding block hash at slot %d: %w", b.Slot, decodeErr)
		}
		points[i] = pcommon.NewPoint(b.Slot, hashBytes)
	}

	log.Printf("Validating %d recent blocks against cardano-node at %s...", len(blocks), nodeAddress)
	intersectFound, findErr := checkIntersectWithNode(ctx, nodeAddress, networkMagic, points)

	if findErr != nil {
		// Node unreachable — do NOT truncate, let operator investigate
		return DBIntegrityResult{}, fmt.Errorf("chain validation failed (node unreachable): %w", findErr)
	}

	if !intersectFound {
		// No intersect — blocks are not on the canonical chain
		log.Println("INTEGRITY CHECK FAILED: no intersection found with cardano-node")
		log.Println("Database contains blocks not on the canonical chain")
		log.Println("Likely cause: CNPG async replication lag after failover")
		log.Println("Truncating all tables for clean resync...")

		if truncErr := store.TruncateAll(ctx); truncErr != nil {
			return DBIntegrityResult{}, fmt.Errorf("truncate failed: %w", truncErr)
		}

		log.Printf("All tables truncated in %v, will resync from Shelley genesis",
			time.Since(start).Round(time.Millisecond))
		return DBIntegrityResult{Truncated: true}, nil
	}

	log.Println("Layer 2 passed: blocks found on canonical chain")

	// If blocks are valid but nonce is stale, repair it
	if nonceStale {
		log.Printf("Repairing stale nonce for epoch %d from blocks table...", epoch)
		if repairErr := nonceTracker.RecomputeCurrentEpochNonce(ctx, epoch); repairErr != nil {
			return DBIntegrityResult{}, fmt.Errorf("nonce repair failed for epoch %d: %w", epoch, repairErr)
		}
		log.Printf("Nonce repaired for epoch %d", epoch)
		log.Printf("Integrity check completed in %v (repaired)", time.Since(start).Round(time.Millisecond))
		return DBIntegrityResult{Valid: true, Repaired: true}, nil
	}

	log.Printf("Integrity check passed in %v", time.Since(start).Round(time.Millisecond))
	return DBIntegrityResult{Valid: true}, nil
}

// checkIntersectWithNode opens a short-lived NtN connection to cardano-node
// and attempts to find an intersection with the provided points.
// Returns true if an intersection was found, false if not.
// Returns an error only if the node is unreachable or the connection fails.
func checkIntersectWithNode(ctx context.Context, nodeAddress string, networkMagic int, points []pcommon.Point) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	errChan := make(chan error, 1)
	chainSyncCfg := chainsync.NewConfig()

	conn, err := ouroboros.NewConnection(
		ouroboros.WithNetworkMagic(uint32(networkMagic)),
		ouroboros.WithNodeToNode(true),
		ouroboros.WithKeepAlive(false),
		ouroboros.WithChainSyncConfig(chainSyncCfg),
		ouroboros.WithErrorChan(errChan),
	)
	if err != nil {
		return false, fmt.Errorf("creating connection: %w", err)
	}
	defer conn.Close()

	if err := conn.Dial("tcp", nodeAddress); err != nil {
		return false, fmt.Errorf("connecting to %s: %w", nodeAddress, err)
	}

	// Sync() performs FindIntersect internally.
	// Returns nil on success (intersect found), ErrIntersectNotFound if no match.
	// After success it begins streaming blocks, but we close the connection immediately.
	type syncResult struct {
		err error
	}
	resultCh := make(chan syncResult, 1)

	go func() {
		syncErr := conn.ChainSync().Client.Sync(points)
		resultCh <- syncResult{syncErr}
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case result := <-resultCh:
		if result.err == nil {
			return true, nil // Intersect found
		}
		if errors.Is(result.err, chainsync.ErrIntersectNotFound) {
			return false, nil // No intersect — DB is stale/corrupt
		}
		return false, result.err // Connection error
	case connErr := <-errChan:
		return false, fmt.Errorf("connection error: %w", connErr)
	}
}
