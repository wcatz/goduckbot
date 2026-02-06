# goduckbot - Claude Code Context

Cardano stake pool notification bot with chain sync and built-in CPRAOS leaderlog calculation. Supports two modes: **lite** (adder tail + Koios fallback) and **full** (historical Shelley-to-tip sync via gouroboros NtN).

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Core: config, chain sync pipeline, block notifications, leaderlog orchestration, mode/social toggles |
| `leaderlog.go` | CPRAOS leader schedule calculation, VRF key parsing, epoch/slot math (`SlotToEpoch`, `GetEpochStartSlot`) |
| `nonce.go` | Nonce evolution tracker (VRF accumulation per block, genesis-seeded or zero-seeded) |
| `store.go` | `Store` interface + SQLite implementation (`SqliteStore` via `modernc.org/sqlite`, pure Go, no CGO) |
| `db.go` | PostgreSQL implementation (`PgStore` via `pgx/v5`) of the `Store` interface |
| `sync.go` | Historical chain syncer using gouroboros NtN ChainSync protocol |

## Modes

### Lite Mode (default)
- Uses blinklabs-io/adder to tail chain from tip
- Nonce tracker zero-seeded, relies on Koios for epoch nonces when local data unavailable
- No historical chain sync — starts tracking from first block seen after launch
- Config: `mode: "lite"`

### Full Mode
- Historical sync from Shelley genesis using gouroboros NtN ChainSync
- Nonce tracker seeded with Shelley genesis hash (`1a3be38bcbb7911969283716ad7aa550250226b76a61fc51cc9a9a35d9276d81`)
- Skips Byron era (no VRF data), starts from last Byron block intersect point
- Once caught up (within 120 slots of tip), transitions to adder live tail
- Builds complete local nonce history — enables retroactive leaderlog and missed block detection
- Config: `mode: "full"`

### Intersect Points (Shelley start)
| Network | Last Byron Slot | Block Hash |
|---------|----------------|------------|
| Mainnet | 4,492,799 | `f8084c61b6a238acec985b59310b6ecec49c0ab8352249afd7268da5cff2a457` |
| Preprod | 1,598,399 | `7e16781b40ebf8b6da18f7b5e8ade855d6738095ef2f1c58c77e88b6e45997a4` |
| Preview | Origin (no Byron) | N/A |

## Store Interface

Abstract database layer supporting SQLite (default) and PostgreSQL:

```go
type Store interface {
    InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue) error
    UpsertEvolvingNonce(ctx, epoch, nonce, blockCount) error
    SetCandidateNonce(ctx, epoch, nonce) error
    SetFinalNonce(ctx, epoch, nonce, source) error
    GetFinalNonce(ctx, epoch) ([]byte, error)
    GetEvolvingNonce(ctx, epoch) ([]byte, int, error)
    InsertLeaderSchedule(ctx, schedule) error
    IsSchedulePosted(ctx, epoch) bool
    MarkSchedulePosted(ctx, epoch) error
    GetLastSyncedSlot(ctx) (uint64, error)
    Close() error
}
```

- **SQLite** (`SqliteStore`): Default for Docker/standalone. Uses WAL mode, single-writer, `modernc.org/sqlite` (pure Go, CGO_ENABLED=0 compatible).
- **PostgreSQL** (`PgStore`): For K8s deployments with CNPG. Uses `pgx/v5` connection pool.

All INSERT operations use `ON CONFLICT` (upsert) for idempotency on restarts.

## Features
- Real-time block notifications via Telegram/Twitter (with duck images)
- Social network toggles: `telegram.enabled`, `twitter.enabled` in config
- Chain sync using blinklabs-io/adder with auto-reconnect and host failover
- Built-in CPRAOS leaderlog calculation (replaces cncli sidecar)
- VRF nonce evolution tracked per block in SQLite or PostgreSQL
- Koios API integration for stake data and nonce fallback
- WebSocket broadcast for block events
- Telegram message chunking for messages >4096 chars

## Leaderlog

### CPRAOS Algorithm
Validated against cncli for preview and mainnet. Key difference from gouroboros `consensus.IsSlotLeader()`: Cardano uses CPRAOS (256-bit) not TPraos (512-bit).

```
VRF input     = BLAKE2b-256(slot || epochNonce)
VRF output    = vrf.Prove(vrfSkey, vrfInput)
Leader value  = BLAKE2b-256(0x4C || vrfOutput)     -- "L" prefix
Threshold     = 2^256 * (1 - (1-0.05)^sigma)
Is leader     = leaderValue < threshold
```

### Nonce Evolution
Per block: `nonceValue = BLAKE2b-256(0x4E || vrfOutput)`, then `eta_v = BLAKE2b-256(eta_v || nonceValue)`. Rolling eta_v accumulates across epoch boundaries (no reset). Candidate nonce freezes at 70% epoch progress (stability window). Koios used as fallback when local nonce unavailable.

### Trigger Flow
1. Every block: extract VRF output from header, update evolving nonce
2. At 70% epoch progress: freeze candidate nonce
3. After freeze: calculate next epoch schedule (mutex-guarded, one goroutine per epoch)
4. Post schedule to Telegram, store in database

### Race Condition Prevention
`checkLeaderlogTrigger` fires on every block after 70% — uses `leaderlogMu` mutex + `leaderlogCalcing` map to ensure only one goroutine runs per epoch. Map entry is cleaned up after goroutine completes.

### VRF Extraction by Era

Two extraction paths depending on sync mode:

**Live tail (adder)** — `extractVrfOutput()` in `main.go`:
- Must extract from `event.Event` payload BEFORE JSON marshal (`Block` field is `json:"-"`)
- Conway/Babbage: `header.Body.VrfResult.Output` (combined, 64 bytes)
- Shelley/Allegra/Mary/Alonzo: `header.Body.NonceVrf.Output` (separate)

**Historical sync (gouroboros NtN)** — `extractVrfFromHeader()` in `sync.go`:
- Receives `ledger.BlockHeader` directly, type-asserts to era-specific header
- Same VRF field access pattern as adder path
- Skips Byron blocks (no VRF data)

### Network Constants

| Network | Magic | Epoch Length | Shelley Start Epoch | Byron Epoch Slots |
|---------|-------|-------------|---------------------|-------------------|
| Mainnet | 764824073 | 432,000 | 208 | 21,600 |
| Preprod | 1 | 432,000 | 4 | 21,600 |
| Preview | 2 | 86,400 | N/A (no Byron) | N/A |

Constants defined in `leaderlog.go`: `MainnetNetworkMagic`, `PreprodNetworkMagic`, `PreviewNetworkMagic`, `MainnetEpochLength`, `PreviewEpochLength`, `ByronEpochLength`, `ShelleyStartEpoch`, `PreprodShelleyStartEpoch`.

### Slot-to-Time Conversion
`makeSlotToTime(networkMagic)` in `main.go` returns a closure handling:
- **Mainnet**: Shelley genesis 2020-07-29T21:44:51Z, Byron slots at 20s, Shelley slots at 1s
- **Preprod**: Genesis 2022-06-01T00:00:00Z, Byron slots at 20s (4 epochs), Shelley at 1s
- **Preview**: Genesis 2022-11-01T00:00:00Z, all slots at 1s (no Byron era)

### Slot/Epoch Math
- `GetEpochStartSlot(epoch, networkMagic)` — first slot of an epoch, accounts for Byron offset
- `SlotToEpoch(slot, networkMagic)` — inverse, determines epoch from slot number
- `GetEpochLength(networkMagic)` — returns epoch length for network

### Data Sources

| Data | Source | Notes |
|------|--------|-------|
| VRF signing key | Mounted K8s secret `/keys/vrf.skey` | CBOR envelope with `5840` prefix, 64-byte key |
| Epoch nonce | Local chain sync (primary), Koios (fallback) | `GetEpochParams` |
| Pool stake | Koios `GetPoolInfo` | `ActiveStake` is `decimal.Decimal`, use `.IntPart()` |
| Total stake | Koios `GetEpochInfo` | `ActiveStake` is `decimal.Decimal`, use `.IntPart()` |

### Database Schema (auto-created by Store constructors)
- `blocks` — per-block VRF data (slot, epoch, block_hash, vrf_output, nonce_value)
- `epoch_nonces` — evolving/candidate/final nonces per epoch with source tracking
- `leader_schedules` — calculated schedules with slots JSON, posted flag

## Config

```yaml
mode: "lite"  # "lite" or "full"

poolId: "POOL_ID_HEX"
ticker: "TICKER"
poolName: "Pool Name"
nodeAddress:
  host1: "node:3001"
  host2: "backup-node:3001"  # optional failover
networkMagic: 764824073

telegram:
  enabled: true              # toggle Telegram notifications
  channel: "CHANNEL_ID"
  # token via TELEGRAM_TOKEN env var

twitter:
  enabled: false             # toggle Twitter notifications
  # credentials via env vars

leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  timezone: "America/New_York"

database:
  driver: "sqlite"           # "sqlite" (default) or "postgres"
  path: "./goduckbot.db"     # SQLite file path
  # PostgreSQL settings (driver: postgres)
  host: "postgres-host"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
  password: ""               # Prefer GODUCKBOT_DB_PASSWORD env var
```

**Note:** Database password uses `net/url.URL` for URL-safe encoding in connection strings. Passwords with special characters (`@`, `:`, `/`) are handled correctly.

## Helm Chart (v0.3.0)

Key values:
- `config.mode` — "lite" (default) or "full"
- `config.telegram.enabled` / `config.twitter.enabled` — social network toggles (env vars conditionally injected)
- `config.leaderlog.enabled` — enables VRF tracking and schedule calculation
- `config.database.driver` — "sqlite" (default) or "postgres"
- `config.database.path` — SQLite file path (default `/data/goduckbot.db`)
- `persistence.enabled` — creates PVC for SQLite data when `database.driver=sqlite`
- `vrfKey.secretName` — K8s secret containing vrf.skey (auto-mounted when `leaderlog.enabled` or `vrfKey.enabled`)
- DB password secret only required when `leaderlog.enabled` AND `database.driver=postgres`

## Dockerfile

Multi-stage build: `golang:1.24-bookworm` builder + `debian:bookworm-slim` runtime. Supports build args for VERSION, COMMIT_SHA, BUILD_DATE labels.

## Build & Deploy

```bash
# Build
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .

# Docker
docker buildx build --platform linux/amd64,linux/arm64 -t wcatz/goduckbot:latest --push .

# Helm package + push
helm package helm-chart/ --version 0.3.0
helm push goduckbot-0.3.0.tgz oci://ghcr.io/wcatz/helm-charts

# Deploy via helmfile (from infra repo)
helmfile -e apps -l app=duckbot apply
```

## Tests

```bash
go test ./... -v    # 16 tests: store, nonce, leaderlog
go vet ./...
helm lint helm-chart/
```

Test files:
- `store_test.go` — SQLite Store operations (in-memory `:memory:` DB)
- `nonce_test.go` — VRF nonce hashing, nonce evolution, genesis seed
- `leaderlog_test.go` — SlotToEpoch (all networks), round-trip, formatNumber

## Key Dependencies
- `blinklabs-io/adder` v0.37.0 — live chain tail (must match gouroboros version)
- `blinklabs-io/gouroboros` v0.153.0 — VRF, block headers, ledger types, NtN ChainSync
- `modernc.org/sqlite` — pure Go SQLite (no CGO required)
- `jackc/pgx/v5` — PostgreSQL driver
- `cardano-community/koios-go-client/v3` — Koios API
- `golang.org/x/crypto` — blake2b hashing

---

## Bugs To Fix (2026-02-06)

Three blocking bugs prevent full historical chain sync from completing. Fix in priority order — **all three must be resolved before re-syncing**.

After fixing, **truncate all three tables** and re-sync from scratch (epoch nonce data is corrupted from previous partial runs).

### Bug 1: Mary/Allegra/Alonzo VRF Extraction (BLOCKING — sync.go)

**Symptom:** `Could not extract VRF from header type *mary.MaryBlockHeader (blockType=4)` spams continuously. Sync runs but records zero blocks for Mary era and beyond. DB freezes at epoch ~235 (last Shelley/Allegra epoch).

**Root cause:** In `extractVrfFromHeader()` (sync.go:230-247), the type assertion `header.(*shelley.ShelleyBlockHeader)` fails for Mary blocks because gouroboros returns them as `*mary.MaryBlockHeader`, a distinct Go type that **embeds** `shelley.ShelleyBlockHeader` but doesn't satisfy the type assertion.

Same issue likely affects Allegra (`*allegra.AllegraBlockHeader`) and Alonzo (`*alonzo.AlonzoBlockHeader`).

**gouroboros source** (`ledger/mary/mary.go:181`):
```go
type MaryBlockHeader struct {
    shelley.ShelleyBlockHeader   // embeds, not aliases
}
```

**Fix in `sync.go`:**

1. Add imports:
```go
import (
    "github.com/blinklabs-io/gouroboros/ledger/allegra"
    "github.com/blinklabs-io/gouroboros/ledger/alonzo"
    "github.com/blinklabs-io/gouroboros/ledger/mary"
)
```

2. Replace `extractVrfFromHeader()` — use a type switch instead of blockType switch:
```go
func extractVrfFromHeader(blockType uint, header ledger.BlockHeader) []byte {
    // Skip Byron
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
```

**Key insight:** `MaryBlockHeader` embeds `ShelleyBlockHeader`, so `h.Body.NonceVrf.Output` works via promotion — but the type assertion must match the concrete type. Order doesn't matter in a type switch (unlike interface assertions), but it's clearest to go newest→oldest.

**Also fix `extractVrfOutput()` in `main.go`** (adder live tail path, line 715-728) — same pattern. The adder path may also receive era-specific types depending on the adder version. Add the same allegra/mary/alonzo cases to be safe.

### Bug 2: Resume Logic Corrupts Nonce Evolution (nonce.go + sync.go)

**Symptom:** `epoch_nonces.block_count` for early epochs inflated way beyond actual block count (epoch 208: 47,819 in epoch_nonces vs 21,556 actual blocks). Nonce values are wrong.

**Root cause:** On restart, `getIntersectPoints()` (sync.go:125-151) detects `GetLastSyncedSlot() = 5138240` but `getIntersectForSlot()` (sync.go:154-164) **always falls back to the Shelley intersect point** (slot 4492799) because it doesn't actually look up the block hash. So the chain sync restarts from Shelley genesis every time.

Meanwhile, `ProcessBlock()` (nonce.go:90-134) restores the evolving nonce from DB on epoch transition (line 102-108), then re-hashes the same VRF outputs into it. The block INSERTs are idempotent (`ON CONFLICT DO NOTHING`), but the nonce evolution accumulates duplicates:
- Run 1: hash(genesis, block1, block2, ..., blockN) → stored as evolving_nonce
- Run 2: hash(stored_nonce, block1, block2, ..., blockN) → wrong!

**Two fixes needed (choose one approach):**

**Option A (recommended): Actually resume from last synced slot**

Add `GetBlockHash(ctx, slot)` method to `Store` interface and both implementations:
```go
// Store interface addition
GetBlockHash(ctx context.Context, slot uint64) (string, error)

// PgStore implementation (db.go)
func (s *PgStore) GetBlockHash(ctx context.Context, slot uint64) (string, error) {
    var hash string
    err := s.pool.QueryRow(ctx,
        `SELECT block_hash FROM blocks WHERE slot = $1`, int64(slot),
    ).Scan(&hash)
    return hash, err
}
```

Then fix `getIntersectForSlot()` in sync.go:
```go
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
```

And in `ProcessBlock()` (nonce.go), skip blocks already in DB:
```go
// At start of ProcessBlock, before nonce evolution:
// Check if block already exists (idempotency for nonce too)
err := nt.store.InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue)
if err != nil {
    log.Printf("Failed to insert block %d: %v", slot, err)
}
// If block was a duplicate (already existed), skip nonce evolution
// Need a way to detect ON CONFLICT DO NOTHING affected 0 rows
```

For this to work cleanly, modify `InsertBlock` to return whether the row was actually inserted (not a conflict):
```go
// Change Store interface:
InsertBlock(ctx, ...) (bool, error)  // returns (inserted bool, err)

// PgStore:
result, err := s.pool.Exec(ctx, `INSERT ... ON CONFLICT DO NOTHING`, ...)
return result.RowsAffected() > 0, err
```

Then in `ProcessBlock`, only evolve nonce if `inserted == true`.

**Option B (simpler): Clear nonce state on fresh sync start**

In `getIntersectPoints()`, when falling back to Shelley genesis (line 137-147), also clear the epoch_nonces table:
```go
log.Printf("Could not get intersect for slot %d, falling back to Shelley genesis", lastSlot)
// Clear stale nonce data since we're re-syncing from scratch
s.store.ClearEpochNonces(ctx)  // new method: DELETE FROM epoch_nonces
```

This is simpler but means every restart re-syncs everything. Option A is better for production.

### Bug 3: Keep-Alive Timeout on Slow DB Writes (sync.go + nonce.go)

**Symptom:** `keep-alive: timeout waiting on transition from protocol state Client` after ~15 minutes of historical sync. The cardano-node drops the NtN connection because the client falls behind the keep-alive deadline.

**Root cause:** Each block triggers synchronous DB writes in `ProcessBlock()`: `InsertBlock()` + `UpsertEvolvingNonce()`. At ~5 blocks/sec with remote DB, accumulated latency exceeds the ouroboros mini-protocol keep-alive window.

**Observed performance:**

| Placement | Blocks/sec | Notes |
|-----------|-----------|-------|
| goduckbot on k3s-mini-1, node on k3s-mobile-2 (same LAN) | ~1,700 | Best — LAN node + local postgres |
| goduckbot on k3s-mini-1, node on k3s-control-1 (Tailscale mesh) | ~94 | Good — local postgres, remote node |
| goduckbot on k3s-control-1, node on k3s-control-1 (co-located) | ~5 | Worst — CPU contention + remote postgres |

At 1,700 blocks/sec (LAN node), the sync may complete without timeout (~2.1 hours for ~13M blocks). But for robustness, these fixes are needed:

**Fix 3a: Decouple sync from DB with a buffered channel**

In `main.go` where the ChainSyncer is created (line 363-378), use a channel instead of direct `ProcessBlock`:

```go
type BlockData struct {
    Slot      uint64
    Epoch     int
    BlockHash string
    VrfOutput []byte
}

blockCh := make(chan BlockData, 10000)

// DB writer goroutine — drains channel in batches
go func() {
    batch := make([]BlockData, 0, 1000)
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case b, ok := <-blockCh:
            if !ok { return }
            batch = append(batch, b)
            if len(batch) >= 1000 {
                flushBatch(batch, i.nonceTracker)
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                flushBatch(batch, i.nonceTracker)
                batch = batch[:0]
            }
        }
    }
}()

// ChainSyncer callback just sends to channel (non-blocking)
i.syncer = NewChainSyncer(
    i.store, i.networkMagic, i.nodeAddresses[0],
    func(slot uint64, epoch int, blockHash string, vrfOutput []byte) {
        blockCh <- BlockData{slot, epoch, blockHash, vrfOutput}
    },
    onCaughtUp,
)
```

**Fix 3b: Batch DB writes in Store**

Add batch methods to `Store` interface:
```go
InsertBlockBatch(ctx context.Context, blocks []BlockData) error
```

PostgreSQL implementation using multi-row INSERT or `COPY`:
```go
func (s *PgStore) InsertBlockBatch(ctx context.Context, blocks []BlockData) error {
    // Use pgx CopyFrom for best performance
    rows := make([][]interface{}, len(blocks))
    for i, b := range blocks {
        nonce := vrfNonceValue(b.VrfOutput)
        rows[i] = []interface{}{int64(b.Slot), b.Epoch, b.BlockHash, b.VrfOutput, nonce}
    }
    _, err := s.pool.CopyFrom(ctx,
        pgx.Identifier{"blocks"},
        []string{"slot", "epoch", "block_hash", "vrf_output", "nonce_value"},
        pgx.CopyFromRows(rows),
    )
    return err
}
```

**Fix 3c: Retry/reconnect on timeout**

In `main.go` (line 380-384), add retry loop around historical sync:
```go
maxRetries := 10
for attempt := 1; attempt <= maxRetries; attempt++ {
    if err := i.syncer.Start(syncCtx); err != nil {
        log.Printf("Historical sync error (attempt %d/%d): %s", attempt, maxRetries, err)
        if attempt < maxRetries {
            time.Sleep(time.Duration(attempt) * 5 * time.Second)
            // Recreate syncer — it will resume from GetLastSyncedSlot
            i.syncer = NewChainSyncer(...)
            continue
        }
        log.Printf("Historical sync failed after %d attempts, falling through to adder", maxRetries)
    }
    break
}
```

### Deployment After Fixes

1. Fix all three bugs
2. Build and push new image
3. Truncate DB tables (data is corrupted from partial runs):
   ```sql
   -- Connect to k3s-postgres-2 (current primary)
   TRUNCATE blocks, epoch_nonces, leader_schedules;
   ```
4. Restart goduckbot pod — it will start a clean sync from Shelley genesis
5. At ~1,700 blocks/sec, full sync takes ~2.1 hours

### Current K8s Deployment State

- **Pod**: `goduckbot` on `k3s-mini-1` (co-located with postgres primary k3s-postgres-2)
- **Node address**: `cardano-node-mainnet-mobile2.cardano.svc.cluster.local:3001` (k3s-mobile-2, same LAN as mini-1)
- **Fallback**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:3001`
- **DB**: PostgreSQL on CNPG cluster (`k3s-postgres-rw.postgres.svc.cluster.local`)
- **Mode**: full, leaderlog enabled, telegram enabled, twitter disabled
- **Image**: `wcatz/goduckbot:master` (pullPolicy: Always)

### Estimated Full Sync (clean run)

- **Total blocks:** ~13M (Shelley epoch 208 → current epoch 611)
- **At ~1,700 blocks/sec** (LAN node): ~2.1 hours
- **Database size:** ~3–3.5 GB (blocks table: ~235 bytes/row × 13M rows)
