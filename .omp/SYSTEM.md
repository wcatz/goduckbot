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
    UpsertSlotOutcomes(ctx, epoch, outcomes []SlotOutcome) error
    GetSlotOutcomes(ctx, epoch) ([]SlotOutcome, error)
    IsEpochClassified(ctx, epoch) bool
    MarkEpochClassified(ctx, epoch) error
    DeleteSlotOutcomesBefore(ctx, epoch) (int64, error)
    HasBlockAtSlot(ctx, slot) (bool, error)
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
- Leaderlog history classification (forged/battle/missed) as resumable background job
- Koios REST API calls via `koiosGetWithRetry` with 429/503 backoff and 30s HTTP timeout

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
- `leader_schedules` — calculated schedules with slots JSON, posted flag, history_classified flag
- `slot_outcomes` — per-slot classification (epoch+slot PK, outcome: forged/battle/missed, opponent pool ID for battles)

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
goduckbot history               # Build leaderlog history with slot classification
goduckbot history --force       # Re-classify already-processed epochs
goduckbot history --from N      # Start from epoch N (overrides auto-detect)
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
```env
GODUCKBOT_VERSION=latest
GODUCKBOT_DB_PASSWORD=your_password  # only for postgres profile, overrides database.password
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
go test ./... -v    # 72 tests
go vet ./...
helm lint helm-chart/
```

Test files:
- `comprehensive_test.go` — CPraos algorithm, slot math, VRF, threshold calculations (41 tests)
- `store_test.go` — SQLite Store operations (in-memory `:memory:` DB) (13 tests)
- `nonce_test.go` — VRF nonce hashing, nonce evolution, genesis seed (11 tests)
- `leaderlog_test.go` — SlotToEpoch (all networks), round-trip, formatNumber (6 tests)
- `nonce_koios_test.go` — Nonce verification against Koios API (1 integration test)

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
3. Batch processor goroutine in `main.go` drains channel (2000 blocks or 2-second timeout)
4. `nonce.go` ProcessBatch() evolves nonce in-memory for entire batch
5. `db.go` PgStore.InsertBlockBatch() persists via temp staging table + CopyFrom:
   - `CREATE TEMP TABLE blocks_staging (...) ON COMMIT DROP`
   - CopyFrom into staging (no constraints = no duplicate key failures)
   - `INSERT INTO blocks SELECT ... FROM blocks_staging ON CONFLICT (slot) DO NOTHING`
6. Unlimited retry loop with capped backoff (5s-30s) on keep-alive timeouts
7. Keep-alive tuned to 120s period / 30s timeout

**Projected performance** (based on partial sync data):
- ~1,800 avg blk/s sustained (observed during epochs 208-250)
- ~8.5M blocks estimated for full Shelley-to-tip sync (epochs 208-current)
- ~2-3 hours projected for full historical sync
- ~2 GB PostgreSQL database size (estimated after full sync)

**Note**: These are projections based on observed metrics during partial sync, not complete measurements.

## Koios REST API

Direct REST calls use `koiosRESTBase(networkMagic)` helper (defaults to public koios.rest endpoints, overridable via `koios.url` config) with RPC parameter syntax (`_epoch_no=N`, `_pool_bech32=ID`). All calls go through `koiosGetWithRetry` (max 5 retries, exponential backoff on 429/503) using `koiosHTTPClient` (shared `http.Client` with 30s timeout). The Go client (`koios-go-client/v3`) is still used for startup pool info and some live queries.

## History Classification

Background job (`buildLeaderlogHistory`) that runs after nonce backfill in full mode. Also available as CLI command (`goduckbot history`). Classifies every assigned leader slot from pool registration through current epoch as forged, slot battle, or missed.

- **Resumable**: checks `IsEpochClassified` per epoch, skips already-done work
- **CPraos only**: starts from Babbage era (mainnet epoch 365, preprod epoch 12)
- **Data sources**: Koios REST for pool-specific forged slots (`fetchPoolForgedSlots` via `pool_blocks` endpoint), pool/total stake; local DB for nonces and `HasBlockAtSlot` (battle detection)
- **Slot classification**: forged (in Koios pool_blocks for our pool), battle (different pool's block exists at slot via `HasBlockAtSlot`), missed (no block at slot from any pool)
- **Own context**: 12-hour timeout (daemon mode), independent from the nonce backfill context
- **CLI mode**: shows estimated time before starting (~5s/epoch), no timeout
- **Rate**: ~75s/epoch in daemon (Koios rate limiting on api.koios.rest), ~5s/epoch in CLI (direct Koios API)

### Classification Limitations

- **Slot battles** (Δ=0): correctly detected — another pool's block at our assigned slot
- **Height battles** (Δ>0): NOT detectable from chain data — our block was orphaned, winner at different slot. These appear as "missed" in classification. Only visible via external orphan data (e.g., AdaStat, Pooltool)
- **Lifetime stats (OTG, 251 epochs)**: 4,454 forged, 115 slot battles, 19 known height battles (from AdaStat), 35 truly missed. 96.34% forge rate

## Current K8s Deployment State

- **Pod**: `goduckbot` on `k3s-control-1`
- **Node address**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:3001`
- **NtC**: `cardano-node-mainnet-az1.cardano.svc.cluster.local:30000`
- **DB**: PostgreSQL `goduckbot_v2` on CNPG cluster (`k3s-postgres-rw.postgres.svc.cluster.local`)
- **Mode**: full, leaderlog enabled, telegram enabled, twitter enabled
- **Image**: `wcatz/goduckbot:3.0.17`
- **Chart**: 0.7.16
- **Duck media**: gif
- **Known issue**: NtC stake queries timeout (Koios fallback working)


## Debugging Workflows

### Chain Sync Issues
1. Check pod is running: `kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o wide`
2. Check logs for sync progress: `kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=30`
3. Look for: `keep-alive timeout` (normal, retries automatically), `connection refused` (node down), `intersect not found` (DB integrity issue)
4. If intersect fails: check DB integrity with `SELECT MAX(slot) FROM blocks;` and compare with chain tip

### Nonce Verification
After any nonce-related change, verify against Koios API using the `verify-nonces` skill.
Manual check: `kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c "SELECT epoch, encode(final_nonce, 'hex') as nonce, source FROM epoch_nonces ORDER BY epoch DESC LIMIT 5;"`

### Leaderlog Issues
1. Check VRF key is loaded: look for `VRF key loaded` in startup logs
2. Check nonce availability: leaderlog requires `GetNonceForEpoch(N)` to return data
3. Manual trigger: `kubectl -n cardano exec <pod> -- /app/goduckbot leaderlog <epoch>`
4. Compare with cncli or Koios for validation

### Database Health
```bash
# Block count and range
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT MIN(slot) as first_slot, MAX(slot) as last_slot, COUNT(*) as total_blocks FROM blocks;"

# Nonce completeness
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, final_nonce IS NOT NULL as has_final, source FROM epoch_nonces ORDER BY epoch DESC LIMIT 10;"

# Slot outcome summary
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, COUNT(*) FILTER (WHERE outcome='forged') as forged, COUNT(*) FILTER (WHERE outcome='missed') as missed, COUNT(*) FILTER (WHERE outcome='battle') as battle FROM slot_outcomes GROUP BY epoch ORDER BY epoch DESC LIMIT 10;"
```

### Common Log Patterns
| Pattern | Meaning | Action |
|---------|---------| --------|
| `keep-alive timeout` | NtN connection dropped | Normal — auto-retries with backoff |
| `intersect not found` | DB/chain mismatch | Check DB integrity, may need wipe |
| `429 Too Many Requests` | Koios rate limit | Normal — `koiosGetWithRetry` handles it |
| `nonce mismatch` | Computed vs Koios nonce differs | Run verify-nonces skill |
| `VRF key loaded` | Startup success | Expected on healthy start |
| `leaderlog calculated` | Schedule computed | Check epoch number matches expected |

## Skills Reference

See `.omp/skills/` for operational runbooks:

### Development & Testing
- **local-dev** — Local development setup, running outside K8s, SQLite/PostgreSQL config
- **testing** — Test suite overview, writing tests, table-driven tests, mocking

### Debugging & Troubleshooting
- **chain-status** — Check Cardano chain sync status across nodes, ogmios, goduckbot
- **nonce-debug** — Debug epoch nonce calculations, verify against Koios, TICKN computation
- **leaderlog-debug** — Debug leader schedule calculations, compare with cncli/Koios
- **history-debug** — Monitor history classification progress, find missed blocks/battles
- **perf-profile** — Performance profiling (CPU, memory, goroutines), optimization tips

### Operations & Deployment
- **db-ops** — Database operations (query, inspect, wipe tables)
- **deploy** — Build and deploy goduckbot (Docker + Helm)
- **kubectl-ops** — Common kubectl patterns for debugging and monitoring
- **verify-nonces** — Batch verify computed nonces against Koios API