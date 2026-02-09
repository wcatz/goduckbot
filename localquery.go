package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	"github.com/blinklabs-io/gouroboros/ledger"
	"github.com/blinklabs-io/gouroboros/protocol/localstatequery"
)

// NodeQueryClient queries cardano-node directly via the local state query mini-protocol (NtC).
// Each query creates a fresh connection to avoid exhausting NtC slots.
type NodeQueryClient struct {
	nodeAddress  string
	networkMagic uint32
	queryTimeout time.Duration
}

// NewNodeQueryClient creates a new node query client.
// nodeAddress can be a TCP address ("host:port"), a UNIX socket path ("/ipc/node.socket"),
// or explicitly prefixed ("unix:///ipc/node.socket", "tcp://host:port").
func NewNodeQueryClient(nodeAddress string, networkMagic int, queryTimeout time.Duration) *NodeQueryClient {
	if queryTimeout == 0 {
		queryTimeout = 10 * time.Minute
	}
	return &NodeQueryClient{
		nodeAddress:  nodeAddress,
		networkMagic: uint32(networkMagic),
		queryTimeout: queryTimeout,
	}
}

// parseNodeAddress detects protocol from the address string.
// Returns (network, address) for gouroboros Dial.
//
//	"/ipc/node.socket"          → ("unix", "/ipc/node.socket")
//	"unix:///ipc/node.socket"   → ("unix", "/ipc/node.socket")
//	"tcp://host:3001"           → ("tcp", "host:3001")
//	"host:3001"                 → ("tcp", "host:3001")
func parseNodeAddress(addr string) (string, string) {
	if strings.HasPrefix(addr, "unix://") {
		return "unix", strings.TrimPrefix(addr, "unix://")
	}
	if strings.HasPrefix(addr, "/") || strings.HasSuffix(addr, ".socket") || strings.HasSuffix(addr, ".sock") {
		return "unix", addr
	}
	if strings.HasPrefix(addr, "tcp://") {
		return "tcp", strings.TrimPrefix(addr, "tcp://")
	}
	return "tcp", addr
}

func (c *NodeQueryClient) Close() error {
	return nil
}

// withQuery creates a connection, acquires volatile tip, runs fn, then releases and closes.
// The ctx is used to enforce caller timeouts — gouroboros methods don't accept context
// natively, so the query runs in a goroutine and aborts if the context expires.
func (c *NodeQueryClient) withQuery(ctx context.Context, fn func(*localstatequery.Client) error) error {
	type result struct{ err error }
	ch := make(chan result, 1)

	go func() {
		conn, err := ouroboros.NewConnection(
			ouroboros.WithNetworkMagic(c.networkMagic),
			ouroboros.WithNodeToNode(false),
			ouroboros.WithKeepAlive(false),
			ouroboros.WithLocalStateQueryConfig(
				localstatequery.NewConfig(
					localstatequery.WithQueryTimeout(c.queryTimeout),
				),
			),
		)
		if err != nil {
			ch <- result{fmt.Errorf("creating connection: %w", err)}
			return
		}
		defer conn.Close()

		network, address := parseNodeAddress(c.nodeAddress)
		if err := conn.Dial(network, address); err != nil {
			ch <- result{fmt.Errorf("dialing %s://%s: %w", network, address, err)}
			return
		}

		client := conn.LocalStateQuery().Client
		client.Start()

		if err := client.Acquire(nil); err != nil {
			ch <- result{fmt.Errorf("acquire volatile tip: %w", err)}
			return
		}
		defer func() {
			if releaseErr := client.Release(); releaseErr != nil {
				log.Printf("localstatequery release: %v", releaseErr)
			}
		}()

		ch <- result{fn(client)}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		return r.err
	}
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

// SnapshotType selects which stake snapshot to read.
type SnapshotType int

const (
	SnapshotMark SnapshotType = iota // next epoch
	SnapshotSet                      // current epoch
	SnapshotGo                       // two epochs ago
)

// QueryStakeDistribution returns stake for all pools from the given snapshot.
// Returns map of bech32_pool_id -> stake_lovelace.
func (c *NodeQueryClient) QueryStakeDistribution(ctx context.Context, snap SnapshotType) (map[string]uint64, error) {
	var dist map[string]uint64
	err := c.withQuery(ctx, func(client *localstatequery.Client) error {
		result, qErr := client.GetStakeSnapshots(nil)
		if qErr != nil {
			return fmt.Errorf("GetStakeSnapshots: %w", qErr)
		}
		dist = make(map[string]uint64, len(result.PoolSnapshots))
		for poolHash, snapshot := range result.PoolSnapshots {
			poolId := ledger.PoolId(poolHash)
			switch snap {
			case SnapshotSet:
				dist[poolId.String()] = snapshot.StakeSet
			case SnapshotGo:
				dist[poolId.String()] = snapshot.StakeGo
			default:
				dist[poolId.String()] = snapshot.StakeMark
			}
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
// poolIdBech32 is the bech32 pool ID (e.g., "pool1...").
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
