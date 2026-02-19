# goduckbot - Claude Code Context

Cardano stake pool notification bot with chain sync and built-in CPraos leaderlog calculation. Supports two modes: **lite** (adder tail + Koios fallback) and **full** (historical Shelley-to-tip sync via gouroboros NtN).

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Core: config, adder pipeline, block notifications, leaderlog orchestration, mode/social toggles, batch processing goroutine |
| `cli.go` | CLI subcommands: `leaderlog`, `nonce`, `version`, `help` — lightweight init without daemon startup |
| `commands.go` | Telegram bot command handlers, inline keyboard buttons (`btnLeaderlogNext`, `btnNonceNext`, `btnDuckGif`, etc.), callback routing |
| `leaderlog.go` | CPraos leader schedule calculation, VRF key parsing (secure memory), epoch/slot math (`SlotToEpoch`, `GetEpochStartSlot`) |
| `nonce.go` | Nonce evolution tracker (VRF accumulation per block, genesis-seeded or zero-seeded), TICKN computation, batch processing (`ProcessBatch`) |
| `securekey.go` | Secure memory primitives: `secureAlloc` (mmap+mlock), `secureReadOnly` (mprotect), `secureFree` (zero+munmap) |
| `store.go` | `Store` interface + SQLite implementation (`SqliteStore` via `modernc.org/sqlite`, pure Go, no CGO) |
| `db.go` | PostgreSQL implementation (`PgStore` via `pgx/v5`) of the `Store` interface, bulk insert via staging table + CopyFrom |
| `integrity.go` | Startup DB integrity check — FindIntersect validation + nonce repair for HA failover |
| `localquery.go` | NtC local state query client for direct stake snapshots from cardano-node |
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
- Builds complete local nonce history — enables TICKN nonce computation, retroactive leaderlog, and missed block detection
- Unlimited retry with capped backoff (5s-30s) on keep-alive timeouts during historical sync
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
    InsertBlock(ctx, slot, epoch, blockHash, vrfOutput, nonceValue) (bool, error)
    InsertBlockBatch(ctx, blocks []BlockData) error
    GetBlockHash(ctx, slot) (string, error)
    GetBlockByHash(ctx, hashPrefix) ([]BlockRecord, error)
    GetLastNBlocks(ctx, n) ([]BlockRecord, error)
    GetBlockCountForEpoch(ctx, epoch) (int, error)
    GetForgedSlots(ctx, epoch) ([]uint64, error)
    GetLastSyncedSlot(ctx) (uint64, error)
    UpsertEvolvingNonce(ctx, epoch, nonce, blockCount) error
    SetCandidateNonce(ctx, epoch, nonce) error
    SetFinalNonce(ctx, epoch, nonce, source) error
    GetFinalNonce(ctx, epoch) ([]byte, error)
    GetEvolvingNonce(ctx, epoch) ([]byte, int, error)
    GetCandidateNonce(ctx, epoch) ([]byte, error)
    GetLastBlockHashForEpoch(ctx, epoch) (string, error)
    GetNonceValuesForEpoch(ctx, epoch) ([][]byte, error)
    StreamBlockNonces(ctx) (BlockNonceRows, error)
    StreamBlockVrfOutputs(ctx) (BlockVrfRows, error)
    InsertLeaderSchedule(ctx, schedule) error
    GetLeaderSchedule(ctx, epoch) (*LeaderSchedule, error)
    IsSchedulePosted(ctx, epoch) bool
    MarkSchedulePosted(ctx, epoch) error
    TruncateAll(ctx) error
    Close() error
}
```

- **SQLite** (`SqliteStore`): Default for Docker/standalone. Uses WAL mode, single-writer, `modernc.org/sqlite` (pure Go, CGO_ENABLED=0 compatible).
- **PostgreSQL** (`PgStore`): For K8s deployments with CNPG. Uses `pgx/v5` connection pool. `InsertBlockBatch` uses temp staging table + `INSERT ... ON CONFLICT DO NOTHING` for duplicate-safe bulk inserts.

All INSERT operations use `ON CONFLICT` (upsert) for idempotency on restarts.

## Features
- Real-time block notifications via Telegram/Twitter (with duck GIFs/images)
- Configurable duck media: `duck.media` = "gif", "img", or "both"
- Inline keyboard buttons for `/leaderlog`, `/nonce`, `/duck` subcommands
- Social network toggles: `telegram.enabled`, `twitter.enabled` in config
- Chain sync using blinklabs-io/adder with auto-reconnect and host failover
- Built-in CPraos leaderlog calculation (replaces cncli sidecar)
- Multi-network epoch calculation (mainnet, preprod, preview)
- VRF nonce evolution tracked per block in SQLite or PostgreSQL
- TICKN nonce computation from local chain data (full mode) with Koios fallback
- NtC local state query for direct stake snapshots (mark/set/go)
- Koios API integration for stake data and nonce/block-hash fallback
- WebSocket broadcast for block events
- Telegram message chunking for messages >4096 chars
- DB integrity check on startup with nonce repair

## Leaderlog

### CPraos Algorithm
Validated against cncli for preview and mainnet (epoch 444: 35/35 actual blocks matched predictions). Key difference from gouroboros `consensus.IsSlotLeader()`: Cardano uses CPraos (256-bit) not TPraos (512-bit).

```
VRF input     = BLAKE2b-256(slot || epochNonce)
VRF output    = vrf.Prove(vrfSkey, vrfInput)
Leader value  = BLAKE2b-256(0x4C || vrfOutput)     -- "L" prefix
Threshold     = 2^256 * (1 - (1-0.05)^sigma)
Is leader     = leaderValue < threshold
```

### Nonce Evolution & TICKN Rule

Per block: `vrfNonceValue = BLAKE2b-256(vrfOutput)`, then `eta_v = BLAKE2b-256(eta_v || vrfNonceValue)` (Cardano Nonce semigroup: BLAKE2b-256 concatenation, NOT XOR). Rolling eta_v accumulates across epoch boundaries (no reset).

Candidate nonce (`η_c`) freezes at 60% epoch progress (stability window = 259,200 slots on mainnet).

**TICKN rule** (epoch boundary nonce computation):
```
epochNonce(N+1) = BLAKE2b-256(η_c(N) || η_ph(N-1))
```
Where `η_c(N)` is the frozen candidate nonce from epoch N, and `η_ph(N-1)` is the block hash of the last block in epoch N-1.

**Data sources for TICKN:**
- `GetCandidateNonce(epoch)` — from local DB (primary)
- `GetLastBlockHashForEpoch(epoch)` — from local DB, falls back to `fetchLastBlockHashFromKoios()` via Koios REST API `/api/v1/blocks?select=hash&epoch_no=eq.{N}&order=block_no.desc&limit=1`
- Koios epoch nonce fallback in lite mode: `GetEpochParams` API

**Table semantics:** `epoch_nonces[N].final_nonce` = the nonce used **for** epoch N's leader election. For epoch N leaderlog, use `GetNonceForEpoch(N)` directly.

**Batch processing:** `ProcessBatch()` method in `nonce.go` performs in-memory nonce evolution for batches of blocks (used during historical sync), then persists the final nonce state in a single DB transaction.

### Trigger Flow
1. Every block: extract VRF output from header, update evolving nonce
2. At 60% epoch progress (stability window): freeze candidate nonce
3. After freeze: compute next epoch nonce via TICKN, calculate leader schedule (mutex-guarded, one goroutine per epoch)
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
- Must match concrete Go types (e.g. `*mary.MaryBlockHeader`), not embedded base types

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
| VRF signing key | K8s secret or inline `vrfKeyValue` | CBOR envelope with `5840` prefix, 64-byte key, stored in secure memory (mmap+mlock+mprotect) |
| Epoch nonce | TICKN from local data (primary), Koios (fallback) | `GetEpochParams` |
| Pool stake | NtC LocalStateQuery (primary), Koios (fallback) | `GetPoolInfo` → `ActiveStake.IntPart()` |
| Total stake | NtC LocalStateQuery (primary), Koios (fallback) | `GetEpochInfo` → `ActiveStake.IntPart()` |
| η_ph (prev epoch block hash) | Local DB (primary), Koios blocks API (fallback) | For TICKN computation |

### Database Schema (auto-created by Store constructors)
- `blocks` — per-block VRF data (slot PK, epoch, block_hash, vrf_output, nonce_value)
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
  ntcHost: "node:30000"      # NtC for stake queries (optional)
networkMagic: 764824073

telegram:
  enabled: true
  token: "BOT_TOKEN"         # or TELEGRAM_TOKEN env var
  channel: "CHANNEL_ID"
  allowedUsers: [USER_ID]    # admin user IDs
  allowedGroups: [GROUP_ID]  # groups where safe commands allowed

twitter:
  enabled: false
  apiKey: ""                 # or TWITTER_API_KEY env var
  apiKeySecret: ""           # or TWITTER_API_KEY_SECRET env var
  accessToken: ""            # or TWITTER_ACCESS_TOKEN env var
  accessTokenSecret: ""      # or TWITTER_ACCESS_TOKEN_SECRET env var

duck:
  media: "gif"               # "gif", "img", or "both"

leaderlog:
  enabled: true
  vrfKeyValue: "5840..."     # inline CBOR hex (preferred)
  vrfKeyPath: "/keys/vrf.skey"  # file path (fallback)
  timezone: "America/New_York"
  timeFormat: "12h"

database:
  driver: "sqlite"           # "sqlite" (default) or "postgres"
  path: "/app/data/goduckbot.db"
  # PostgreSQL settings (driver: postgres)
  host: "postgres-host"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
  password: ""               # or GODUCKBOT_DB_PASSWORD env var
```

All secrets can go in `config.yaml` (gitignored) or as env vars (env vars take precedence). Database password uses `net/url.URL` for URL-safe encoding in connection strings.

## CLI

```bash
goduckbot                       # Start daemon (default)
goduckbot version               # Show version info
goduckbot leaderlog <epoch>     # Calculate leaderlog for single epoch
goduckbot leaderlog <N>-<M>     # Calculate leaderlog for epoch range (max 10)
goduckbot nonce <epoch>         # Show epoch nonce
goduckbot help                  # Show usage
```

CLI subcommands use lightweight init (`cliInit()` in `cli.go`) — reads config, opens DB, sets up Koios client and nonce tracker without starting the daemon.

## Docker Compose

```bash
# Lite mode with SQLite (default)
docker compose up -d

# With PostgreSQL
docker compose --profile postgres up -d
```

Optional `.env` for docker-compose variables only (goduckbot reads config.yaml):
```
GODUCKBOT_VERSION=latest
GODUCKBOT_DB_PASSWORD=your_password  # only for postgres profile, must match database.password
```

## Helm Chart (v0.7.16)

Key values:
- `config.mode` — "lite" (default) or "full"
- `config.leaderlog.vrfKeyValue` — inline CBOR hex VRF key
- `config.database.driver` — "sqlite" (default) or "postgres"
- `persistence.enabled` — creates PVC for SQLite data
- `vrfKey.secretName` — K8s secret containing vrf.skey (alternative to vrfKeyValue)

## Dockerfile

Multi-stage build: `golang:1.24-bookworm` builder + `debian:bookworm-slim` runtime. Supports build args for VERSION, COMMIT_SHA, BUILD_DATE labels.

## Build & Deploy

CI/CD handles Docker images AND helm charts on merge to master. NEVER build locally.

```bash
# Versioned tags require git tag:
git tag v3.0.0 && git push origin v3.0.0

# Deploy via helmfile (from infra repo)
helmfile -e apps -l app=goduckbot apply
```

## Tests

```bash
go test ./... -v    # 30 tests
go vet ./...
helm lint helm-chart/
```

Test files:
- `store_test.go` — SQLite Store operations (in-memory `:memory:` DB)
- `nonce_test.go` — VRF nonce hashing, nonce evolution, genesis seed
- `nonce_koios_test.go` — Nonce verification against Koios API
- `leaderlog_test.go` — SlotToEpoch (all networks), round-trip, formatNumber
- `epoch467_test.go` — End-to-end leaderlog calculation for mainnet epoch 467
- `epoch612_integration_test.go` — Integration test for epoch 612 leaderlog

## Key Dependencies
- `blinklabs-io/adder` v0.37.1-pre (commit 460d03e) — live chain tail with auto-reconnect
- `blinklabs-io/gouroboros` v0.153.1 — VRF, NtN ChainSync, NtC LocalStateQuery, ledger types
- `modernc.org/sqlite` — pure Go SQLite (no CGO required)
- `jackc/pgx/v5` — PostgreSQL driver with COPY protocol support
- `cardano-community/koios-go-client/v3` — Koios API
- `golang.org/x/crypto` — blake2b hashing

## Performance Architecture

**Historical sync pipeline (full mode):**
1. `sync.go` ChainSync reads blocks from cardano-node via gouroboros NtN (pipeline limit 50)
2. Blocks sent to buffered channel (10,000 capacity) — decouples network I/O from DB writes
3. Batch processor goroutine in `main.go` drains channel (1000 blocks or 2-second timeout)
4. `nonce.go` ProcessBatch() evolves nonce in-memory for entire batch
5. `db.go` PgStore.InsertBlockBatch() persists via temp staging table + CopyFrom:
   - `CREATE TEMP TABLE blocks_staging (...) ON COMMIT DROP`
   - CopyFrom into staging (no constraints = no duplicate key failures)
   - `INSERT INTO blocks SELECT ... FROM blocks_staging ON CONFLICT (slot) DO NOTHING`
6. Unlimited retry loop with capped backoff (5s-30s) on keep-alive timeouts
7. Keep-alive tuned to 120s period / 30s timeout

**Measured performance:**
- ~1,800 avg blk/s sustained over full Shelley-to-tip sync (with retries)
- ~8.56M blocks across 406 epochs (208-613)
- ~2 hours total for full historical sync
- ~2 GB PostgreSQL database size

## Current K8s Deployment State

- **Pod**: `goduckbot` on `k3s-control-1`
- **Node address**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:3001`
- **NtC**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:30000`
- **DB**: PostgreSQL `goduckbot_v2` on CNPG cluster (`k3s-postgres-rw.postgres.svc.cluster.local`)
- **Mode**: full, leaderlog enabled, telegram enabled, twitter enabled
- **Image**: `wcatz/goduckbot:3.0.0`
- **Chart**: 0.7.16
- **Duck media**: gif
- **Known issue**: NtC stake queries timeout (Koios fallback working)
