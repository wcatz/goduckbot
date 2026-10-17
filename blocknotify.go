package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	koios "github.com/cardano-community/koios-go-client/v3"
)

// dingoProtoMinor is the header protocol minor version Dingo stamps on blocks
// it forges (e.g. 11.69). cardano-node (Haskell) uses a different minor.
const dingoProtoMinor = 69

// mintNotification holds everything shown in a minted-block notification.
type mintNotification struct {
	PoolName       string // omitted from the message when empty
	TxCount        uint64
	SizePercentage float64
	Interval       string
	EpochBlocks    int
	TotalBlocks    uint64
	// Script usage; HasBlock is false when the ledger block wasn't available.
	HasBlock  bool
	ScriptTxs int
	Steps     int64
	Mem       int64
	MaxSteps  int64 // 0 when the protocol limit is unknown
	MaxMem    int64
	// Forging node, from the header protocol version.
	HasProto   bool
	ProtoMinor uint64
	Links      []string // appended at the end, one per line
}

// formatMintMessage renders a minted-block notification for Telegram or X.
func formatMintMessage(n mintNotification) string {
	var b strings.Builder
	b.WriteString("Quack!(attention) \U0001F986\nduckBot notification!\n\n")
	if n.PoolName != "" {
		b.WriteString(n.PoolName + "\n")
	}
	b.WriteString("\U0001F4A5 New Block!\n")
	if n.HasProto {
		fmt.Fprintf(&b, "Node: %s\n", nodeImplementation(n.ProtoMinor))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Tx Count: %d\nBlock Size: %.2f%%\nInterval: %s\n\n",
		n.TxCount, n.SizePercentage, n.Interval)

	if n.HasBlock {
		fmt.Fprintf(&b, "Scripts: %d/%d txs\n", n.ScriptTxs, n.TxCount)
		if n.ScriptTxs > 0 {
			if n.MaxSteps > 0 && n.MaxMem > 0 {
				fmt.Fprintf(&b, "Steps %.1f%% · Mem %.1f%%\n",
					float64(n.Steps)/float64(n.MaxSteps)*100,
					float64(n.Mem)/float64(n.MaxMem)*100)
			} else {
				fmt.Fprintf(&b, "Steps %.2f bn · Mem %.2f M\n",
					float64(n.Steps)/1e9, float64(n.Mem)/1e6)
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Epoch Blocks: %d\nLifetime Blocks: %d",
		n.EpochBlocks, n.TotalBlocks)

	if len(n.Links) > 0 {
		b.WriteString("\n\n" + strings.Join(n.Links, "\n"))
	}
	return b.String()
}

// nodeImplementation names the node that forged a block from its header
// protocol minor version.
func nodeImplementation(protoMinor uint64) string {
	if protoMinor == dingoProtoMinor {
		return "Dingo"
	}
	return "Haskell"
}

// headerProtoMinor returns the protocol minor version from a Babbage/Conway header.
func headerProtoMinor(header ledger.BlockHeader) (minor uint64, ok bool) {
	switch h := header.(type) {
	case *conway.ConwayBlockHeader:
		return h.Body.ProtoVersion.Minor, true
	case *babbage.BabbageBlockHeader:
		return h.Body.ProtoVersion.Minor, true
	default:
		return 0, false
	}
}

// blockScriptUsage sums redeemer execution units across a block's
// transactions and counts the transactions that ran scripts.
func blockScriptUsage(block ledger.Block) (scriptTxs int, steps, mem int64) {
	for _, tx := range block.Transactions() {
		wits := tx.Witnesses()
		if wits == nil || wits.Redeemers() == nil {
			continue
		}
		hasRedeemer := false
		for _, rv := range wits.Redeemers().Iter() {
			hasRedeemer = true
			steps += rv.ExUnits.Steps
			mem += rv.ExUnits.Memory
		}
		if hasRedeemer {
			scriptTxs++
		}
	}
	return scriptTxs, steps, mem
}

// adastatBlockURL returns the AdaStat explorer link for a block hash.
func adastatBlockURL(networkMagic int, blockHash string) string {
	switch networkMagic {
	case PreprodNetworkMagic:
		return "https://preprod.adastat.net/blocks/" + blockHash
	case PreviewNetworkMagic:
		return "https://preview.adastat.net/blocks/" + blockHash
	default:
		return "https://adastat.net/blocks/" + blockHash
	}
}

// getMaxBlockExUnits returns the per-block execution unit limits for an epoch
// from Koios, cached per epoch. Returns zeros when unavailable.
func (i *Indexer) getMaxBlockExUnits(epoch int) (steps, mem int64) {
	if i.exUnitsEpoch == epoch && i.maxBlockSteps > 0 {
		return i.maxBlockSteps, i.maxBlockMem
	}
	if i.koios == nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e := koios.EpochNo(epoch)
	res, err := i.koios.GetEpochParams(ctx, &e, nil)
	if err != nil || res == nil || len(res.Data) == 0 {
		log.Printf("failed to fetch epoch %d params from Koios: %v", epoch, err)
		return 0, 0
	}
	i.exUnitsEpoch = epoch
	i.maxBlockSteps = int64(res.Data[0].MaxBlockExSteps)
	i.maxBlockMem = int64(res.Data[0].MaxBlockExMem)
	return i.maxBlockSteps, i.maxBlockMem
}
