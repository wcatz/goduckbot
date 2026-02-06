# goduckbot - Claude Code Context

Cardano stake pool notification bot with chain sync via adder. Calculates leader schedules using CPRAOS and posts to Telegram.

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Core: config, chain sync pipeline, block notifications, leaderlog orchestration |
| `leaderlog.go` | CPRAOS leader schedule calculation, VRF key parsing, epoch/slot math |
| `nonce.go` | Nonce evolution tracker (VRF accumulation per block) |
| `db.go` | PostgreSQL layer (blocks, epoch nonces, leader schedules) |

## Features
- Real-time block notifications via Telegram/Twitter
- Chain sync using blinklabs-io/adder with auto-reconnect and host failover
- Built-in CPRAOS leaderlog calculation (replaces cncli sidecar)
- VRF nonce evolution tracked per block in PostgreSQL
- Koios API integration for stake data and nonce fallback
- WebSocket broadcast for block events

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
Per block: `nonceValue = BLAKE2b-256(0x4E || vrfOutput)`, then `eta_v = BLAKE2b-256(eta_v || nonceValue)`. Candidate nonce freezes at 70% epoch progress (stability window). Koios used as fallback when local nonce unavailable.

### Trigger Flow
1. Every block: extract VRF output from header, update evolving nonce
2. At 70% epoch progress: freeze candidate nonce
3. After freeze: calculate next epoch schedule (mutex-guarded, one goroutine per epoch)
4. Post schedule to Telegram, store in PostgreSQL

### VRF Extraction by Era
- Conway/Babbage: `header.Body.VrfResult.Output` (combined, 64 bytes)
- Shelley/Allegra/Mary/Alonzo: `header.Body.NonceVrf.Output` (separate)
- Must extract from `event.Event` payload BEFORE JSON marshal (`Block` field is `json:"-"`)

### Network Constants

| Network | Magic | Epoch Length | Byron Start Epoch | Byron Epoch Slots |
|---------|-------|-------------|-------------------|-------------------|
| Mainnet | 764824073 | 432,000 | 208 | 21,600 |
| Preprod | 1 | 432,000 | 4 | 21,600 |
| Preview | 2 | 86,400 | N/A (no Byron) | N/A |

### Data Sources

| Data | Source | Notes |
|------|--------|-------|
| VRF signing key | Mounted K8s secret `/keys/vrf.skey` | CBOR envelope, 64-byte key |
| Epoch nonce | Local chain sync (primary), Koios (fallback) | `GetEpochParams` |
| Pool stake | Koios `GetPoolInfo` | `ActiveStake` is `decimal.Decimal` |
| Total stake | Koios `GetEpochInfo` | Use `.IntPart()` for int64 |

### Database Schema (auto-created by `InitDB`)
- `blocks` — per-block VRF data (slot, epoch, block_hash, vrf_output, nonce_value)
- `epoch_nonces` — evolving/candidate/final nonces per epoch with source tracking
- `leader_schedules` — calculated schedules with slots JSONB, posted flag

## Config

```yaml
poolId: "POOL_ID_HEX"
ticker: "TICKER"
poolName: "Pool Name"
nodeAddress:
  host1: "node:3001"
  host2: "backup-node:3001"  # optional failover
networkMagic: 764824073

telegram:
  channel: "CHANNEL_ID"
twitter:
  consumer_key: "..."  # etc.

leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  timezone: "America/New_York"

database:
  host: "postgres-host"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
  password: ""  # Prefer GODUCKBOT_DB_PASSWORD env var
```

## Helm Chart (v0.2.0)

Key values for leaderlog:
- `config.leaderlog.enabled` — enables VRF tracking and schedule calculation
- `config.database.*` — PostgreSQL connection (password via SOPS secret)
- `vrfKey.enabled` / `vrfKey.secretName` / `vrfKey.mountPath` — mounts VRF signing key

## Build & Deploy

```bash
# Build
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .

# Docker
docker buildx build --platform linux/amd64,linux/arm64 -t wcatz/goduckbot:latest --push .

# Helm package + push
helm package helm-chart/ --version 0.2.0
helm push goduckbot-0.2.0.tgz oci://ghcr.io/wcatz/helm-charts

# Deploy via helmfile (from infra repo)
helmfile -e apps -l app=duckbot apply
```

## Key Dependencies
- `blinklabs-io/adder` v0.37.0 — chain sync (must match gouroboros version)
- `blinklabs-io/gouroboros` v0.153.0+ — VRF, block headers, ledger types
- `jackc/pgx/v5` — PostgreSQL driver
- `cardano-community/koios-go-client/v3` — Koios API
- `golang.org/x/crypto` — blake2b hashing
