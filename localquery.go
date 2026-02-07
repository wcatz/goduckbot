package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/protocol/localstatequery"
)

// NodeQueryClient queries cardano-node directly via the local state query mini-protocol (NtC).
// Each query creates a fresh connection to avoid exhausting NtC slots.
type NodeQueryClient struct {
	nodeAddress  string
	networkMagic uint32
}

// NewNodeQueryClient creates a new node query client.
func NewNodeQueryClient(nodeAddress string, networkMagic int) *NodeQueryClient {
	return &NodeQueryClient{
		nodeAddress:  nodeAddress,
		networkMagic: uint32(networkMagic),
	}
}

func (c *NodeQueryClient) Close() error {
	return nil
}

// withQuery creates a connection, acquires volatile tip, runs fn, then releases and closes.
func (c *NodeQueryClient) withQuery(ctx context.Context, fn func(*localstatequery.Client) error) error {
	conn, err := ouroboros.NewConnection(
		ouroboros.WithNetworkMagic(c.networkMagic),
		ouroboros.WithNodeToNode(false),
		ouroboros.WithKeepAlive(true),
	)
	if err != nil {
		return fmt.Errorf("creating connection: %w", err)
	}
	defer conn.Close()

	if err := conn.Dial("tcp", c.nodeAddress); err != nil {
		return fmt.Errorf("dialing %s: %w", c.nodeAddress, err)
	}

	client := conn.LocalStateQuery().Client
	client.Start()

	if err := client.Acquire(nil); err != nil {
		return fmt.Errorf("acquire volatile tip: %w", err)
	}
	defer func() {
		if releaseErr := client.Release(); releaseErr != nil {
			log.Printf("localstatequery release: %v", releaseErr)
		}
	}()

	return fn(client)
}

// QueryTip returns the current chain tip (slot, blockHash, epoch).
func (c *NodeQueryClient) QueryTip(ctx context.Context) (slot uint64, blockHash string, epoch int, err error) {
	err = c.withQuery(ctx, func(client *localstatequery.Client) error {
		point, pointErr := client.GetChainPoint()
		if pointErr != nil {
			return fmt.Errorf("GetChainPoint: %w", pointErr)
		}
		epochNo, epochErr := client.GetEpochNo()
		if epochErr != nil {
			return fmt.Errorf("GetEpochNo: %w", epochErr)
		}
		slot = point.Slot
		blockHash = hex.EncodeToString(point.Hash)
		epoch = epochNo
		return nil
	})
	return
}

// QueryStakeDistribution returns the mark snapshot stake for all pools.
// Returns map of bech32_pool_id -> mark_stake_lovelace.
func (c *NodeQueryClient) QueryStakeDistribution(ctx context.Context) (map[string]uint64, error) {
	var dist map[string]uint64
	err := c.withQuery(ctx, func(client *localstatequery.Client) error {
		result, qErr := client.GetStakeSnapshots(nil)
		if qErr != nil {
			return fmt.Errorf("GetStakeSnapshots: %w", qErr)
		}
		dist = make(map[string]uint64, len(result.PoolSnapshots))
		for poolHash, snapshot := range result.PoolSnapshots {
			poolId := ledger.PoolId(poolHash)
			dist[poolId.String()] = snapshot.StakeMark
		}
		return nil
	})
	return dist, err
}

// StakeSnapshots holds mark/set/go stake for a pool plus network totals.
type StakeSnapshots struct {
	PoolStakeMark  uint64
	PoolStakeSet   uint64
	PoolStakeGo    uint64
	TotalStakeMark uint64
	TotalStakeSet  uint64
	TotalStakeGo   uint64
}

// QueryPoolStakeSnapshots returns mark/set/go snapshots for a specific pool.
// poolIdHex is the 56-char hex pool ID (e.g., from config).
func (c *NodeQueryClient) QueryPoolStakeSnapshots(ctx context.Context, poolIdBech32 string) (*StakeSnapshots, error) {
	poolId, err := ledger.NewPoolIdFromBech32(poolIdBech32)
	if err != nil {
		return nil, fmt.Errorf("invalid pool ID %s: %w", poolIdBech32, err)
	}

	var snapshots *StakeSnapshots
	err = c.withQuery(ctx, func(client *localstatequery.Client) error {
		result, qErr := client.GetStakeSnapshots([]any{poolId})
		if qErr != nil {
			return fmt.Errorf("GetStakeSnapshots: %w", qErr)
		}
		poolHash := ledger.Blake2b224(poolId)
		poolSnap, ok := result.PoolSnapshots[poolHash]
		if !ok {
			return fmt.Errorf("pool %s not found in snapshot result", poolIdBech32)
		}
		snapshots = &StakeSnapshots{
			PoolStakeMark:  poolSnap.StakeMark,
			PoolStakeSet:   poolSnap.StakeSet,
			PoolStakeGo:    poolSnap.StakeGo,
			TotalStakeMark: result.TotalStakeMark,
			TotalStakeSet:  result.TotalStakeSet,
			TotalStakeGo:   result.TotalStakeGo,
		}
		return nil
	})
	return snapshots, err
}
