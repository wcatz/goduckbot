package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/protocol/chainsync"
	pcommon "github.com/blinklabs-io/gouroboros/protocol/common"
)

// BlockRef identifies a canonical block.
type BlockRef struct {
	Slot uint64
	Hash string
}

// gapSource reports the canonical chain: block counts per epoch, and the
// blocks of one epoch in slot order.
type gapSource interface {
	EpochBlockCounts(ctx context.Context) (map[int]int, error)
	EpochBlocks(ctx context.Context, epoch int) ([]BlockRef, error)
}

// headerFetcher returns the blocks that follow `after`, in chain order, up to
// and including slot `through`.
type headerFetcher func(ctx context.Context, after BlockRef, through uint64) ([]BlockData, error)

// RepairBlockGaps inserts canonical blocks missing from the blocks table and
// rebuilds the nonce state from the earliest repaired epoch to the DB tip.
// The evolving nonce rolls across epochs, so one missing block corrupts every
// later candidate nonce, even in epochs whose own block count is complete.
func RepairBlockGaps(ctx context.Context, store Store, nt *NonceTracker, src gapSource, fetch headerFetcher, networkMagic int) error {
	lastSlot, err := store.GetLastSyncedSlot(ctx)
	if err != nil || lastSlot == 0 {
		return nil
	}
	tipEpoch := SlotToEpoch(lastSlot, networkMagic)

	counts, err := src.EpochBlockCounts(ctx)
	if err != nil {
		return fmt.Errorf("fetching epoch block counts: %w", err)
	}
	epochs := make([]int, 0, len(counts))
	for e := range counts {
		// The tip epoch is still being synced; its count cannot match yet.
		if e < tipEpoch {
			epochs = append(epochs, e)
		}
	}
	sort.Ints(epochs)

	firstRepaired := -1
	for _, epoch := range epochs {
		local, err := store.GetBlockCountForEpoch(ctx, epoch)
		if err != nil {
			return fmt.Errorf("counting blocks for epoch %d: %w", epoch, err)
		}
		if local == 0 || local >= counts[epoch] {
			continue
		}
		log.Printf("BLOCK GAP: epoch %d has %d blocks, canonical chain has %d — repairing",
			epoch, local, counts[epoch])
		n, err := repairEpochGaps(ctx, store, src, fetch, epoch, networkMagic)
		if err != nil {
			return fmt.Errorf("repairing epoch %d: %w", epoch, err)
		}
		log.Printf("BLOCK GAP: epoch %d repaired with %d blocks", epoch, n)
		if n > 0 && firstRepaired < 0 {
			firstRepaired = epoch
		}
	}
	if firstRepaired < 0 {
		return nil
	}
	if err := nt.RecomputeEpochRange(ctx, firstRepaired, tipEpoch); err != nil {
		return err
	}
	log.Printf("BLOCK GAP: nonce state rebuilt for epochs %d-%d", firstRepaired, tipEpoch)
	return nil
}

// repairEpochGaps inserts the blocks of epoch the store lacks, fetching each
// run of missing blocks from the block before it.
func repairEpochGaps(ctx context.Context, store Store, src gapSource, fetch headerFetcher, epoch, networkMagic int) (int, error) {
	canonical, err := src.EpochBlocks(ctx, epoch)
	if err != nil {
		return 0, err
	}
	stored, err := store.GetVrfOutputsForEpoch(ctx, epoch)
	if err != nil {
		return 0, err
	}
	have := make(map[uint64]bool, len(stored))
	for _, b := range stored {
		have[b.Slot] = true
	}

	inserted := 0
	for idx := 0; idx < len(canonical); idx++ {
		if have[canonical[idx].Slot] {
			continue
		}
		if idx == 0 {
			return inserted, fmt.Errorf("first block of epoch %d (slot %d) is missing", epoch, canonical[0].Slot)
		}
		end := idx
		for end+1 < len(canonical) && !have[canonical[end+1].Slot] {
			end++
		}
		want := make(map[uint64]string, end-idx+1)
		for _, b := range canonical[idx : end+1] {
			want[b.Slot] = b.Hash
		}
		blocks, err := fetch(ctx, canonical[idx-1], canonical[end].Slot)
		if err != nil {
			return inserted, fmt.Errorf("fetching slots %d-%d: %w", canonical[idx].Slot, canonical[end].Slot, err)
		}
		for _, b := range blocks {
			hash, ok := want[b.Slot]
			if !ok {
				continue
			}
			if b.BlockHash != hash {
				return inserted, fmt.Errorf("slot %d: node returned block %s, canonical is %s", b.Slot, b.BlockHash, hash)
			}
			nonceValue := vrfNonceValueForEpoch(b.VrfOutput, epoch, networkMagic)
			ok, err := store.InsertBlock(ctx, b.Slot, epoch, b.BlockHash, b.VrfOutput, nonceValue)
			if err != nil {
				return inserted, fmt.Errorf("inserting slot %d: %w", b.Slot, err)
			}
			if ok {
				inserted++
			}
			delete(want, b.Slot)
		}
		if len(want) > 0 {
			return inserted, fmt.Errorf("node did not return %d of the missing blocks after slot %d", len(want), canonical[idx-1].Slot)
		}
		idx = end
	}
	return inserted, nil
}

// RecomputeEpochRange rebuilds the evolving and candidate nonces of epochs
// from..through from the blocks table, each epoch starting from the one before.
func (nt *NonceTracker) RecomputeEpochRange(ctx context.Context, from, through int) error {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	for epoch := from; epoch <= through; epoch++ {
		if err := nt.restoreEpochFromDB(ctx, epoch); err != nil {
			return err
		}
	}
	return nil
}

// koiosGapSource reads the canonical chain from Koios.
type koiosGapSource struct {
	networkMagic int
}

func (k koiosGapSource) EpochBlockCounts(ctx context.Context) (map[int]int, error) {
	var rows []struct {
		Epoch    int `json:"epoch_no"`
		BlkCount int `json:"blk_count"`
	}
	counts := make(map[int]int)
	for offset := 0; ; offset += 1000 {
		if err := koiosGet(ctx, fmt.Sprintf("%s/epoch_info?select=epoch_no,blk_count&order=epoch_no.asc&offset=%d&limit=1000",
			koiosRESTBase(k.networkMagic), offset), &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			counts[r.Epoch] = r.BlkCount
		}
		if len(rows) < 1000 {
			return counts, nil
		}
	}
}

func (k koiosGapSource) EpochBlocks(ctx context.Context, epoch int) ([]BlockRef, error) {
	var rows []struct {
		Slot uint64 `json:"abs_slot"`
		Hash string `json:"hash"`
	}
	var blocks []BlockRef
	for offset := 0; ; offset += 1000 {
		if err := koiosGet(ctx, fmt.Sprintf("%s/blocks?epoch_no=eq.%d&select=abs_slot,hash&order=abs_slot.asc&offset=%d&limit=1000",
			koiosRESTBase(k.networkMagic), epoch, offset), &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			blocks = append(blocks, BlockRef{Slot: r.Slot, Hash: r.Hash})
		}
		if len(rows) < 1000 {
			return blocks, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func koiosGet(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := koiosHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("koios returned %d for %s", resp.StatusCode, url)
	}
	return json.Unmarshal(body, out)
}

// ntnHeaderFetcher fetches block headers over a short-lived NtN ChainSync
// connection intersecting at `after`.
func ntnHeaderFetcher(nodeAddress string, networkMagic int) headerFetcher {
	return func(ctx context.Context, after BlockRef, through uint64) ([]BlockData, error) {
		hash, err := hex.DecodeString(after.Hash)
		if err != nil {
			return nil, err
		}
		var blocks []BlockData
		done := make(chan struct{})
		var closed bool
		rollForward := func(_ chainsync.CallbackContext, blockType uint, data any, _ chainsync.Tip) error {
			header, ok := data.(ledger.BlockHeader)
			if !ok || closed {
				return nil
			}
			slot := header.SlotNumber()
			if vrf := extractVrfFromHeader(blockType, header); vrf != nil {
				blocks = append(blocks, BlockData{
					Slot:         slot,
					Epoch:        SlotToEpoch(slot, networkMagic),
					BlockHash:    header.Hash().String(),
					VrfOutput:    vrf,
					NetworkMagic: networkMagic,
				})
			}
			if slot >= through {
				closed = true
				close(done)
			}
			return nil
		}
		rollBackward := func(chainsync.CallbackContext, pcommon.Point, chainsync.Tip) error { return nil }

		errChan := make(chan error, 1)
		conn, err := ouroboros.NewConnection(
			ouroboros.WithNetworkMagic(uint32(networkMagic)),
			ouroboros.WithNodeToNode(true),
			ouroboros.WithKeepAlive(false),
			ouroboros.WithChainSyncConfig(chainsync.NewConfig(
				chainsync.WithRollForwardFunc(rollForward),
				chainsync.WithRollBackwardFunc(rollBackward),
			)),
			ouroboros.WithErrorChan(errChan),
		)
		if err != nil {
			return nil, err
		}
		if err := conn.Dial("tcp", nodeAddress); err != nil {
			return nil, fmt.Errorf("connecting to %s: %w", nodeAddress, err)
		}
		defer conn.Close()
		if err := conn.ChainSync().Client.Sync([]pcommon.Point{pcommon.NewPoint(after.Slot, hash)}); err != nil {
			return nil, fmt.Errorf("starting chain sync at slot %d: %w", after.Slot, err)
		}
		select {
		case <-done:
			return blocks, nil
		case err := <-errChan:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Minute):
			return nil, fmt.Errorf("timed out fetching blocks after slot %d", after.Slot)
		}
	}
}
