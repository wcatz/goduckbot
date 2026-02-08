package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/ledger/allegra"
	"github.com/blinklabs-io/gouroboros/ledger/alonzo"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
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

// LiveBlockInfo holds block data extracted from NtN block headers for live tail processing.
type LiveBlockInfo struct {
	Slot           uint64
	Epoch          int
	BlockHash      string
	BlockNumber    uint64
	IssuerVkeyHash string // 28-byte pool ID hex (Blake2b-224 of issuer vkey)
	BlockBodySize  uint64
	VrfOutput      []byte
}

// ChainSyncer performs chain sync from Shelley genesis (or resume point) to tip,
// then continues as a live tail using the gouroboros NtN ChainSync protocol.
type ChainSyncer struct {
	store        Store
	networkMagic int
	nodeAddress  string
	onBlock      func(slot uint64, epoch int, blockHash string, vrfOutput []byte)
	onLiveBlock  func(LiveBlockInfo)
	onCaughtUp   func()
	startFromTip bool // skip historical sync, start from tip (lite mode)
	conn         *ouroboros.Connection
	blockssynced uint64 // atomic counter for progress logging
	tipSlot      uint64 // atomic: current tip slot for progress tracking
	caughtUp     atomic.Bool
	lastBlockTime atomic.Int64 // unix timestamp of last block (for watchdog)
	// Progress tracking
	syncStart    time.Time
	lastLogTime  time.Time
	lastLogSlot  uint64
	currentEra   string
	currentEpoch int
}

// NewChainSyncer creates a chain syncer that handles both historical sync and live tail.
func NewChainSyncer(store Store, networkMagic int, nodeAddress string,
	onBlock func(slot uint64, epoch int, blockHash string, vrfOutput []byte),
	onLiveBlock func(LiveBlockInfo),
	onCaughtUp func(),
	startFromTip bool,
) *ChainSyncer {
	return &ChainSyncer{
		store:        store,
		networkMagic: networkMagic,
		nodeAddress:  nodeAddress,
		onBlock:      onBlock,
		onLiveBlock:  onLiveBlock,
		onCaughtUp:   onCaughtUp,
		startFromTip: startFromTip,
	}
}

// Start begins chain sync and blocks until error or ctx cancellation.
// After catching up to the tip, it continues as a live tail (does not stop).
// A watchdog goroutine force-closes the connection if no block arrives for 5 minutes.
func (s *ChainSyncer) Start(ctx context.Context) error {
	// Determine intersect point
	intersectPoints, err := s.getIntersectPoints(ctx)
	if err != nil {
		return fmt.Errorf("getting intersect points: %w", err)
	}

	if s.startFromTip {
		log.Printf("Starting chain sync from tip")
	} else {
		log.Printf("Starting chain sync from slot %d", intersectPoints[0].Slot)
	}

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

	log.Printf("Connected to node at %s", s.nodeAddress)

	// Seed lastBlockTime so watchdog doesn't fire immediately
	s.lastBlockTime.Store(time.Now().Unix())

	// Lite mode: find current tip and start from there
	if s.startFromTip {
		tip, tipErr := conn.ChainSync().Client.GetCurrentTip()
		if tipErr != nil {
			conn.Close()
			return fmt.Errorf("getting current tip: %w", tipErr)
		}
		intersectPoints = []pcommon.Point{tip.Point}
		s.caughtUp.Store(true)
		log.Printf("Starting from tip at slot %d", tip.Point.Slot)
	}

	// Start chain sync from intersect point
	if err := conn.ChainSync().Client.Sync(intersectPoints); err != nil {
		conn.Close()
		return fmt.Errorf("starting chain sync: %w", err)
	}

	// Watchdog: force-close connection if no block for 5 minutes
	done := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if !s.caughtUp.Load() {
					continue // don't watchdog during historical sync
				}
				stale := time.Since(time.Unix(s.lastBlockTime.Load(), 0))
				if stale > 5*time.Minute {
					log.Printf("[watchdog] no block for %s, forcing reconnect", stale.Round(time.Second))
					conn.Close()
					return
				}
			}
		}
	}()

	// Block until error or context cancellation
	select {
	case <-ctx.Done():
		close(done)
		conn.Close()
		<-watchdogDone
		return ctx.Err()
	case err := <-errChan:
		close(done)
		conn.Close()
		<-watchdogDone
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
	// Lite mode or no store: start from origin (ChainSync intersects at tip)
	if s.startFromTip || s.store == nil {
		return []pcommon.Point{pcommon.NewPointOrigin()}, nil
	}

	// Check if we have existing data to resume from
	lastSlot, err := s.store.GetLastSyncedSlot(ctx)
	if err == nil && lastSlot > 0 {
		log.Printf("Resuming sync from last synced slot %d", lastSlot)
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
	hash, err := s.store.GetBlockHash(ctx, slot)
	if err != nil {
		return pcommon.Point{}, fmt.Errorf("no block hash for slot %d: %w", slot, err)
	}
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return pcommon.Point{}, fmt.Errorf("decoding hash: %w", err)
	}
	return pcommon.NewPoint(slot, hashBytes), nil
}

// blockTypeToEra maps a gouroboros block type constant to an era name.
func blockTypeToEra(blockType uint) string {
	switch blockType {
	case ledger.BlockTypeShelley:
		return "Shelley"
	case ledger.BlockTypeAllegra:
		return "Allegra"
	case ledger.BlockTypeMary:
		return "Mary"
	case ledger.BlockTypeAlonzo:
		return "Alonzo"
	case ledger.BlockTypeBabbage:
		return "Babbage"
	case ledger.BlockTypeConway:
		return "Conway"
	default:
		return fmt.Sprintf("unknown(%d)", blockType)
	}
}

// handleRollForward processes a block received during NtN chain sync.
func (s *ChainSyncer) handleRollForward(ctx chainsync.CallbackContext, blockType uint, data any, tip chainsync.Tip) error {
	// Update tip for progress tracking
	atomic.StoreUint64(&s.tipSlot, tip.Point.Slot)

	// Skip Byron blocks — no VRF data
	if blockType == ledger.BlockTypeByronEbb || blockType == ledger.BlockTypeByronMain {
		return nil
	}

	// Initialize timing on first post-Byron block
	now := time.Now()
	if s.syncStart.IsZero() {
		s.syncStart = now
		s.lastLogTime = now
	}

	// In NtN mode, data is a ledger.BlockHeader
	header, ok := data.(ledger.BlockHeader)
	if !ok {
		return fmt.Errorf("expected BlockHeader, got %T", data)
	}

	slot := header.SlotNumber()
	blockHash := header.Hash().String()
	epoch := SlotToEpoch(slot, s.networkMagic)

	// Detect era transitions
	era := blockTypeToEra(blockType)
	if era != s.currentEra {
		log.Printf("[sync] ━━━ entering %s era at slot %d (epoch %d) ━━━", era, slot, epoch)
		s.currentEra = era
	}

	// Detect epoch transitions
	if epoch != s.currentEpoch && s.currentEpoch > 0 {
		log.Printf("[sync] epoch %d started at slot %d", epoch, slot)
	}
	s.currentEpoch = epoch

	// Extract VRF output from header
	vrfOutput := extractVrfFromHeader(blockType, header)
	if vrfOutput == nil {
		return nil
	}

	// Update watchdog timestamp
	s.lastBlockTime.Store(now.Unix())

	// Check if caught up (within 120 slots / ~2 minutes of tip).
	// Must happen BEFORE callback dispatch so the transition block
	// goes to exactly one path (onLiveBlock), not both.
	if !s.caughtUp.Load() && tip.Point.Slot > 0 && slot+120 >= tip.Point.Slot {
		s.caughtUp.Store(true)
		if !s.syncStart.IsZero() {
			elapsed := time.Since(s.syncStart)
			count := atomic.LoadUint64(&s.blockssynced)
			avgBlkSec := float64(count) / elapsed.Seconds()
			log.Printf("[sync] caught up in %s | %d blocks | avg %.0f blk/s",
				elapsed.Round(time.Second), count, avgBlkSec)
		}
		if s.onCaughtUp != nil {
			s.onCaughtUp()
		}
	}

	// Dispatch to exactly one callback: onLiveBlock after caught up, onBlock during historical sync.
	if s.caughtUp.Load() {
		if s.onLiveBlock != nil {
			s.onLiveBlock(LiveBlockInfo{
				Slot:           slot,
				Epoch:          epoch,
				BlockHash:      blockHash,
				BlockNumber:    header.BlockNumber(),
				IssuerVkeyHash: header.IssuerVkey().Hash().String(),
				BlockBodySize:  header.BlockBodySize(),
				VrfOutput:      vrfOutput,
			})
		}
		return nil
	}
	if s.onBlock != nil {
		s.onBlock(slot, epoch, blockHash, vrfOutput)
	}

	// Historical sync: log progress every 5,000 blocks
	count := atomic.AddUint64(&s.blockssynced, 1)
	if count%5000 == 0 {
		tipSlot := atomic.LoadUint64(&s.tipSlot)
		pct := 0.0
		if tipSlot > 0 {
			pct = float64(slot) / float64(tipSlot) * 100
		}
		elapsed := now.Sub(s.syncStart)
		sinceLastLog := now.Sub(s.lastLogTime)
		var slotsPerSec float64
		var eta time.Duration
		if sinceLastLog.Seconds() > 0 && slot > s.lastLogSlot {
			slotsPerSec = float64(slot-s.lastLogSlot) / sinceLastLog.Seconds()
			if slotsPerSec > 0 && tipSlot > slot {
				eta = time.Duration(float64(tipSlot-slot)/slotsPerSec) * time.Second
			}
		}
		blkPerSec := float64(count) / elapsed.Seconds()

		log.Printf("[sync] slot %d/%d (%.1f%%) | epoch %d | %s | %.0f blk/s | elapsed %s | ETA %s",
			slot, tipSlot, pct, epoch, era, blkPerSec,
			elapsed.Round(time.Second), eta.Round(time.Second))

		s.lastLogTime = now
		s.lastLogSlot = slot
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
	// Skip Byron blocks
	if blockType == ledger.BlockTypeByronEbb || blockType == ledger.BlockTypeByronMain {
		return nil
	}
	// Each era has its own Go type that embeds the previous era's header.
	// Must check each concrete type — Go type assertions don't match embedded types.
	switch h := header.(type) {
	case *conway.ConwayBlockHeader:
		return h.Body.VrfResult.Output
	case *babbage.BabbageBlockHeader:
		return h.Body.VrfResult.Output
	case *alonzo.AlonzoBlockHeader:
		return h.Body.NonceVrf.Output
	case *mary.MaryBlockHeader:
		return h.Body.NonceVrf.Output
	case *allegra.AllegraBlockHeader:
		return h.Body.NonceVrf.Output
	case *shelley.ShelleyBlockHeader:
		return h.Body.NonceVrf.Output
	default:
		log.Printf("Could not extract VRF from header type %T (blockType=%d)", header, blockType)
		return nil
	}
}
