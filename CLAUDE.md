# goduckbot - Claude Code Context

Cardano stake pool notification bot with chain sync and built-in CPRAOS leaderlog calculation. Supports two modes: **lite** (adder tail + Koios fallback) and **full** (historical Shelley-to-tip sync via gouroboros NtN).

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Core: config, adder pipeline, block notifications, leaderlog orchestration, mode/social toggles, batch processing goroutine |
| `commands.go` | Telegram bot command handlers, inline keyboard buttons (`btnLeaderlogNext`, `btnNonceNext`, `btnDuckGif`, etc.), callback routing |
| `leaderlog.go` | CPRAOS leader schedule calculation, VRF key parsing, epoch/slot math (`SlotToEpoch`, `GetEpochStartSlot`) |
| `nonce.go` | Nonce evolution tracker (VRF accumulation per block, genesis-seeded or zero-seeded), batch processing (`ProcessBatch`) |
| `store.go` | `Store` interface + SQLite implementation (`SqliteStore` via `modernc.org/sqlite`, pure Go, no CGO) |
| `db.go` | PostgreSQL implementation (`PgStore` via `pgx/v5`) of the `Store` interface, bulk insert via pgx CopyFrom |
| `sync.go` | Historical chain syncer using gouroboros NtN ChainSync protocol, era-specific VRF extraction |

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
    InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue) (bool, error)  // returns (inserted, err)
    GetBlockHash(ctx, slot) (string, error)
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
- Real-time block notifications via Telegram/Twitter (with duck GIFs/images)
- Configurable duck media: `duck.media` = "gif", "img", or "both"
- Inline keyboard buttons for `/leaderlog`, `/nonce`, `/duck` subcommands
- Social network toggles: `telegram.enabled`, `twitter.enabled` in config
- Chain sync using blinklabs-io/adder with auto-reconnect and host failover
- Built-in CPRAOS leaderlog calculation (replaces cncli sidecar)
- Multi-network epoch calculation (mainnet, preprod, preview)
- VRF nonce evolution tracked per block in SQLite or PostgreSQL
- NtC local state query for direct stake snapshots (mark/set/go)
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
Per block: `vrfNonceValue = BLAKE2b-256(vrfOutput)`, then `eta_v = BLAKE2b-256(eta_v || vrfNonceValue)` (Cardano Nonce semigroup: BLAKE2b-256 concatenation, NOT XOR). Rolling eta_v accumulates across epoch boundaries (no reset). Candidate nonce (`η_c`) freezes at 60% epoch progress (stability window = 259,200 slots on mainnet). At each epoch boundary, the TICKN rule computes: `epochNonce = BLAKE2b-256(η_c || η_ph)` where `η_ph` is the block hash from the last block of the prior epoch. Koios used as fallback in lite mode when local nonce unavailable.

**Batch processing:** `ProcessBatch()` method in `nonce.go` performs in-memory nonce evolution for batches of blocks (used during historical sync), then persists the final nonce state in a single DB transaction. This dramatically improves sync performance vs per-block DB writes.

### Trigger Flow
1. Every block: extract VRF output from header, update evolving nonce via XOR
2. At 60% epoch progress (stability window): freeze candidate nonce
3. After freeze: calculate next epoch schedule (mutex-guarded, one goroutine per epoch)
4. Post schedule to Telegram, store in database

### Race Condition Prevention
`checkLeaderlogTrigger` fires on every block after 60% — uses `leaderlogMu` mutex + `leaderlogCalcing` map to ensure only one goroutine runs per epoch. Map entry is cleaned up after goroutine completes.

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

## Helm Chart (v0.7.15)

Key values:
- `config.mode` — "lite" (default) or "full"
- `config.telegram.enabled` / `config.twitter.enabled` — social network toggles (env vars conditionally injected)
- `config.leaderlog.enabled` — enables VRF tracking and schedule calculation
- `config.duck.media` — "gif", "img", or "both" (default)
- `config.duck.customUrl` — optional custom duck image URL
- `config.database.driver` — "sqlite" (default) or "postgres"
- `config.database.path` — SQLite file path (default `/data/goduckbot.db`)
- `persistence.enabled` — creates PVC for SQLite data when `database.driver=sqlite`
- `vrfKey.secretName` — K8s secret containing vrf.skey (auto-mounted when `leaderlog.enabled` or `vrfKey.enabled`)
- DB password secret only required when `leaderlog.enabled` AND `database.driver=postgres`

## Dockerfile

Multi-stage build: `golang:1.24-bookworm` builder + `debian:bookworm-slim` runtime. Supports build args for VERSION, COMMIT_SHA, BUILD_DATE labels.

## Build & Deploy

CI/CD handles Docker images AND helm charts on merge to master. NEVER build locally.

```bash
# Build locally (dev only)
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .

# CI builds on merge to master:
#   - Docker: wcatz/goduckbot:latest, wcatz/goduckbot:master
#   - Helm: oci://ghcr.io/wcatz/helm-charts/goduckbot

# Versioned tags require git tag:
git tag v2.5.0 && git push origin v2.5.0
#   - Docker: wcatz/goduckbot:2.5.0, wcatz/goduckbot:2.5, wcatz/goduckbot:2
#   - Helm chart published with matching version

# Deploy via helmfile (from infra repo)
helmfile -e apps -l app=goduckbot apply
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
- `blinklabs-io/adder` v0.37.1-pre (commit 460d03e, fixes auto-reconnect channel orphaning) — live chain tail
- `blinklabs-io/gouroboros` v0.153.1 — VRF (ECVRF-ED25519-SHA512-Elligator2), NtN ChainSync, NtC LocalStateQuery, ledger types
- `modernc.org/sqlite` — pure Go SQLite (no CGO required)
- `jackc/pgx/v5` — PostgreSQL driver with COPY protocol support for bulk inserts
- `cardano-community/koios-go-client/v3` — Koios API
- `golang.org/x/crypto` — blake2b hashing

## Performance Architecture

**Historical sync pipeline:**
1. `sync.go` ChainSync reads blocks from cardano-node via gouroboros NtN
2. Blocks sent to buffered channel (10,000 capacity) — decouples network I/O from DB writes
3. Batch processor goroutine in `main.go` drains channel (1000 blocks or 2-second timeout)
4. `nonce.go` ProcessBatch() evolves nonce in-memory for entire batch
5. `db.go` PgStore persists batch via pgx CopyFrom (PostgreSQL COPY protocol)
6. Result: ~3,300 blocks/sec sustained throughput, ~43 minutes for full Shelley-to-tip sync

---

## Fixed Bugs Reference (2026-02-06)

Three blocking bugs prevented full historical chain sync from completing. **All three have been FIXED and deployed** as of v1.2.0. This section is preserved for architectural context.

### Bug 1: Mary/Allegra/Alonzo VRF Extraction — FIXED

**Status:** RESOLVED in `sync.go` with type switch for all era-specific header types

**Original symptom:** `Could not extract VRF from header type *mary.MaryBlockHeader (blockType=4)` spammed continuously. Sync ran but recorded zero blocks for Mary era and beyond.

**Root cause:** In `extractVrfFromHeader()`, the type assertion `header.(*shelley.ShelleyBlockHeader)` failed for Mary/Allegra/Alonzo blocks because gouroboros returns them as distinct Go types (`*mary.MaryBlockHeader`, etc.) that **embed** `shelley.ShelleyBlockHeader` but don't satisfy direct type assertions.

**Solution implemented:** Replaced blockType switch with type switch covering all era-specific header types:

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

**Key insight:** `MaryBlockHeader` embeds `ShelleyBlockHeader`, so `h.Body.NonceVrf.Output` works via promotion — but the type assertion must match the concrete type.

### Bug 2: Resume Logic Corrupted Nonce Evolution — FIXED

**Status:** RESOLVED with proper intersect point lookup and duplicate block detection

**Original symptom:** `epoch_nonces.block_count` for early epochs inflated way beyond actual block count. Nonce values were corrupted.

**Root cause:** On restart, `getIntersectForSlot()` always fell back to Shelley genesis because it didn't actually look up the block hash. The chain sync restarted from scratch every time, but `ProcessBlock()` restored the evolving nonce from DB and re-hashed the same VRF outputs into it, accumulating duplicates.

**Solution implemented:**

1. **Added `GetBlockHash()` method** to Store interface and both implementations:
```go
GetBlockHash(ctx context.Context, slot uint64) (string, error)
```

2. **Fixed `getIntersectForSlot()`** to actually query the DB:
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

3. **Modified `InsertBlock()` to return bool** indicating whether row was actually inserted:
```go
InsertBlock(ctx, ...) (bool, error)  // returns (inserted bool, err)
```

4. **Updated `ProcessBlock()` in nonce.go** to only evolve nonce if block was newly inserted (not a duplicate).

### Bug 3: Keep-Alive Timeout on Slow DB Writes — FIXED

**Status:** RESOLVED with buffered channel, batch writes via pgx CopyFrom, and retry/reconnect loop

**Original symptom:** `keep-alive: timeout waiting on transition from protocol state Client` after ~15 minutes of historical sync. The cardano-node dropped the NtN connection because the client fell behind the keep-alive deadline.

**Root cause:** Each block triggered synchronous DB writes (`InsertBlock()` + `UpsertEvolvingNonce()`). At ~5 blocks/sec with remote DB, accumulated latency exceeded the ouroboros mini-protocol keep-alive window.

**Solution implemented:**

1. **Decoupled sync from DB with buffered channel** (10,000 capacity) in `main.go`
2. **Added batch processing** with `ProcessBatch()` method in `nonce.go`:
   - Accumulates blocks in-memory
   - Evolves nonce locally for entire batch
   - Single DB persist per batch
3. **Implemented pgx CopyFrom** for bulk inserts in `db.go`:
   - Uses PostgreSQL COPY protocol for maximum performance
   - Batch size: 1000 blocks or 2-second timeout
4. **Added retry/reconnect loop** around historical sync in `main.go`:
   - Max 10 retries with exponential backoff
   - Recreates syncer on failure — resumes from GetLastSyncedSlot

**Performance achieved:** ~3,300 blocks/sec (with CopyFrom batch writes)

### Current K8s Deployment State

- **Pod**: `goduckbot` on `k3s-control-1`
- **Node address**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:3001`
- **NtC**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:30000` (same-node socat, no WireGuard)
- **DB**: PostgreSQL on CNPG cluster (`k3s-postgres-rw.postgres.svc.cluster.local`)
- **Mode**: full, leaderlog enabled, telegram enabled, twitter disabled
- **Image**: `wcatz/goduckbot:2.5.0` (pullPolicy: Always)
- **Chart**: 0.7.15
- **Duck media**: gif
- **Test instance**: `goduckbot-test` on `k3s-mr-slave` (latest image)

### Full Sync Performance (clean run)

- **Total blocks:** ~8.5M (Shelley epoch 208 → current epoch 611)
- **At ~3,300 blocks/sec** (with CopyFrom batch writes): ~43 minutes
- **Database size:** ~2 GB (blocks table: ~235 bytes/row × 8.5M rows)

---

## v2.7.8 Critical Fixes (2026-02-11)

Comprehensive bug fix release addressing goroutine leaks, query performance, and race conditions.

### NtC Connection Leak Fix (localquery.go)

**Problem:** `withQuery()` returned on context timeout but goroutine remained blocked in `GetStakeSnapshots()`, leaking both goroutines and TCP connections.

**Solution:** Added connection cleanup on context expiration:
```go
select {
case <-ctx.Done():
    if conn != nil {
        conn.Close()  // Unblock goroutine
    }
    <-connClosed  // Wait for cleanup
    return ctx.Err()
case r := <-ch:
    return r.err
}
```

### GetStakeSnapshots CBOR Encoding Fix (localquery.go)

**Problem:** `PoolId [28]byte` encoded as CBOR array, causing cardano-node to ignore filter and return all ~3000 pools (2+ minute query).

**Solution:** Wrap with `Blake2b224` type which has proper `MarshalCBOR()`:
```go
poolHash := ledger.Blake2b224(poolId)  // Encodes as CBOR bytestring
result, qErr := client.GetStakeSnapshots([]any{poolHash})
```

### WebSocket Data Race Fix (main.go)

**Problem:** Concurrent map access from HTTP handlers and `handleMessages` goroutine without synchronization.

**Solution:** Added `clientsMutex sync.RWMutex` protecting all `clients` map operations.

### Log.Fatal Crash Fix (main.go)

**Problem:** Single bad WebSocket connection killed entire bot process.

**Solution:** Changed `log.Fatal(err)` to `log.Printf` + return in WebSocket upgrade handler.

### Keepalive Tuning (sync.go)

Increased NtN keepalive from 60s/10s to 120s/30s to reduce reconnection frequency during heavy sync.

### ResyncFromDB Between Retries (main.go)

Added `ResyncFromDB()` call between retry attempts to prevent nonce corruption when historical sync reconnects mid-epoch.

### Deployment Status

- **Image:** `wcatz/goduckbot:2.7.8`
- **Helm Chart:** `0.7.16`
- **Nonce Computation:** Verified correct (matches Koios API for all tested epochs)
- **Historical Sync:** Completed successfully from Shelley genesis
- **Live Tailing:** Working at tip (epoch 612+)
- **Known Issue:** NtC stake queries still timeout (Koios fallback working)
