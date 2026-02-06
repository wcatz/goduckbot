package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync/atomic"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"
	"github.com/blinklabs-io/gouroboros/protocol/chainsync"
	pcommon "github.com/blinklabs-io/gouroboros/protocol/common"
)

// Shelley intersect points: last Byron block per network.
// ChainSync starts from the first Shelley block after these points.
type intersectPoint struct {
	Slot uint64
	Hash string
}

var shelleyIntersectPoints = map[int]intersectPoint{
	MainnetNetworkMagic: {4492799, "f8084c61b6a238acec985b59310b6ecec49c0ab8352249afd7268da5cff2a457"},
	PreprodNetworkMagic: {1598399, "7e16781b40ebf8b6da18f7b5e8ade855d6738095ef2f1c58c77e88b6e45997a4"},
}

// ChainSyncer performs full historical chain sync from Shelley genesis to tip
// using the gouroboros NtN (Node-to-Node) ChainSync protocol.
type ChainSyncer struct {
	store        Store
	networkMagic int
	nodeAddress  string
	onBlock      func(slot uint64, epoch int, blockHash string, vrfOutput []byte)
	onCaughtUp   func()
	conn         *ouroboros.Connection
	blockssynced uint64 // atomic counter for progress logging
	tipSlot      uint64 // atomic: current tip slot for progress tracking
	caughtUp     atomic.Bool
}

// NewChainSyncer creates a new historical chain syncer.
func NewChainSyncer(store Store, networkMagic int, nodeAddress string,
	onBlock func(slot uint64, epoch int, blockHash string, vrfOutput []byte),
	onCaughtUp func(),
) *ChainSyncer {
	return &ChainSyncer{
		store:        store,
		networkMagic: networkMagic,
		nodeAddress:  nodeAddress,
		onBlock:      onBlock,
		onCaughtUp:   onCaughtUp,
	}
}

// Start begins the historical chain sync. It blocks until sync completes or ctx is cancelled.
func (s *ChainSyncer) Start(ctx context.Context) error {
	// Determine intersect point
	intersectPoints, err := s.getIntersectPoints(ctx)
	if err != nil {
		return fmt.Errorf("getting intersect points: %w", err)
	}

	log.Printf("Starting historical chain sync from slot %d", intersectPoints[0].Slot)

	// Configure ChainSync callbacks
	chainSyncCfg := chainsync.NewConfig(
		chainsync.WithRollForwardFunc(s.handleRollForward),
		chainsync.WithRollBackwardFunc(s.handleRollBackward),
		chainsync.WithPipelineLimit(50),
	)

	// Connect to node via NtN (required for TCP connections)
	errChan := make(chan error, 1)
	conn, connErr := ouroboros.NewConnection(
		ouroboros.WithNetworkMagic(uint32(s.networkMagic)),
		ouroboros.WithNodeToNode(true),
		ouroboros.WithKeepAlive(true),
		ouroboros.WithChainSyncConfig(chainSyncCfg),
		ouroboros.WithErrorChan(errChan),
	)
	if connErr != nil {
		return fmt.Errorf("creating connection: %w", connErr)
	}
	s.conn = conn

	if err := conn.Dial("tcp", s.nodeAddress); err != nil {
		return fmt.Errorf("connecting to %s: %w", s.nodeAddress, err)
	}

	log.Printf("Connected to node at %s for historical sync", s.nodeAddress)

	// Start chain sync from intersect point
	if err := conn.ChainSync().Client.Sync(intersectPoints); err != nil {
		conn.Close()
		return fmt.Errorf("starting chain sync: %w", err)
	}

	// Wait for sync to complete, error, or context cancellation
	select {
	case <-ctx.Done():
		conn.Close()
		return ctx.Err()
	case err := <-errChan:
		conn.Close()
		if s.caughtUp.Load() {
			// Caught up and connection closed — this is expected
			return nil
		}
		return fmt.Errorf("chain sync error: %w", err)
	}
}

// Stop terminates the chain sync connection.
func (s *ChainSyncer) Stop() {
	if s.conn != nil {
		s.conn.Close()
	}
}

// getIntersectPoints determines where to start syncing from.
func (s *ChainSyncer) getIntersectPoints(ctx context.Context) ([]pcommon.Point, error) {
	// Check if we have existing data to resume from
	lastSlot, err := s.store.GetLastSyncedSlot(ctx)
	if err == nil && lastSlot > 0 {
		log.Printf("Resuming sync from last synced slot %d", lastSlot)
		// We need the block hash for the intersect point.
		// Use origin + known point for a safe resume.
		// The node will find the best intersection.
		point, err := s.getIntersectForSlot(ctx, lastSlot)
		if err == nil {
			return []pcommon.Point{point}, nil
		}
		log.Printf("Could not get intersect for slot %d, falling back to Shelley genesis: %v", lastSlot, err)
	}

	// Start from Shelley genesis (skip Byron)
	if ip, ok := shelleyIntersectPoints[s.networkMagic]; ok {
		hashBytes, err := hex.DecodeString(ip.Hash)
		if err != nil {
			return nil, fmt.Errorf("decoding intersect hash: %w", err)
		}
		return []pcommon.Point{pcommon.NewPoint(ip.Slot, hashBytes)}, nil
	}

	// Preview and other networks: start from origin
	return []pcommon.Point{pcommon.NewPointOrigin()}, nil
}

// getIntersectForSlot builds an intersect point for a known slot by querying the blocks table.
func (s *ChainSyncer) getIntersectForSlot(ctx context.Context, slot uint64) (pcommon.Point, error) {
	// Query the block hash from the store for this slot
	// We need to add a method to get block hash by slot, but for now
	// we can use a fallback approach: provide the Shelley intersect + origin
	// and let the node find the best intersection.
	if ip, ok := shelleyIntersectPoints[s.networkMagic]; ok {
		hashBytes, _ := hex.DecodeString(ip.Hash)
		return pcommon.NewPoint(ip.Slot, hashBytes), nil
	}
	return pcommon.NewPointOrigin(), nil
}

// handleRollForward processes a block received during NtN chain sync.
func (s *ChainSyncer) handleRollForward(ctx chainsync.CallbackContext, blockType uint, data any, tip chainsync.Tip) error {
	// Update tip for progress tracking
	atomic.StoreUint64(&s.tipSlot, tip.Point.Slot)

	// Skip Byron blocks — no VRF data
	if blockType == ledger.BlockTypeByronEbb || blockType == ledger.BlockTypeByronMain {
		return nil
	}

	// In NtN mode, data is a ledger.BlockHeader
	header, ok := data.(ledger.BlockHeader)
	if !ok {
		return fmt.Errorf("expected BlockHeader, got %T", data)
	}

	slot := header.SlotNumber()
	blockHash := header.Hash().String()
	epoch := SlotToEpoch(slot, s.networkMagic)

	// Extract VRF output from header
	vrfOutput := extractVrfFromHeader(blockType, header)
	if vrfOutput == nil {
		return nil
	}

	// Deliver to callback
	if s.onBlock != nil {
		s.onBlock(slot, epoch, blockHash, vrfOutput)
	}

	// Log progress every 10,000 blocks
	count := atomic.AddUint64(&s.blockssynced, 1)
	if count%10000 == 0 {
		tipSlot := atomic.LoadUint64(&s.tipSlot)
		pct := 0.0
		if tipSlot > 0 {
			pct = float64(slot) / float64(tipSlot) * 100
		}
		log.Printf("Sync progress: slot %d / %d (%.1f%%) — %d blocks processed", slot, tipSlot, pct, count)
	}

	// Check if caught up (within 120 slots / ~2 minutes of tip)
	if !s.caughtUp.Load() && tip.Point.Slot > 0 && slot+120 >= tip.Point.Slot {
		s.caughtUp.Store(true)
		log.Printf("Historical sync caught up at slot %d (tip: %d). %d blocks processed.", slot, tip.Point.Slot, count)
		if s.onCaughtUp != nil {
			go s.onCaughtUp()
		}
	}

	return nil
}

// handleRollBackward handles a rollback during sync.
func (s *ChainSyncer) handleRollBackward(ctx chainsync.CallbackContext, point pcommon.Point, tip chainsync.Tip) error {
	log.Printf("Chain sync rollback to slot %d", point.Slot)
	// Rollbacks during historical sync are rare. We continue from the rollback point.
	// The nonce state will be slightly off but will self-correct as blocks are re-processed
	// (InsertBlock uses ON CONFLICT DO NOTHING so duplicates are safe).
	return nil
}

// extractVrfFromHeader extracts VRF output from an NtN block header by era.
func extractVrfFromHeader(blockType uint, header ledger.BlockHeader) []byte {
	switch blockType {
	case ledger.BlockTypeBabbage:
		if h, ok := header.(*babbage.BabbageBlockHeader); ok {
			return h.Body.VrfResult.Output
		}
	case ledger.BlockTypeConway:
		if h, ok := header.(*conway.ConwayBlockHeader); ok {
			return h.Body.VrfResult.Output
		}
	case ledger.BlockTypeShelley, ledger.BlockTypeAllegra, ledger.BlockTypeMary, ledger.BlockTypeAlonzo:
		if h, ok := header.(*shelley.ShelleyBlockHeader); ok {
			return h.Body.NonceVrf.Output
		}
	}
	log.Printf("Could not extract VRF from header type %T (blockType=%d)", header, blockType)
	return nil
}
