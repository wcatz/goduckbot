# duckBot

A Cardano stake pool operator's companion. Real-time block notifications, built-in CPRAOS leader schedule calculation, and full chain nonce history — all from a single Go binary syncing directly off your local node.

```text
Quack!(attention)
duckBot notification!

DuckPool
New Block!

Tx Count: 14
Block Size: 42.69 KB
48.53% Full
Interval: 23 seconds

Epoch Blocks: 7
Lifetime Blocks: 1,337
```

## What It Does

**Block Notifications** — Every time your pool mints a block, duckBot fires off a notification to Telegram (with a random duck pic) and optionally Twitter/X. Includes tx count, block size, fill percentage, interval since last block, and running epoch/lifetime tallies.

**Leader Schedule Calculation** — Built-in CPRAOS implementation calculates your upcoming slot assignments without needing cncli as a sidecar. Validated against cncli on both preview and mainnet. Posts the full schedule to Telegram with local timezone support and message chunking for large schedules.

**Full Chain Nonce History** — In full mode, syncs every block from Shelley genesis using gouroboros NtN ChainSync, building a complete local nonce evolution history. Enables retroactive leaderlog calculation and missed block detection. In lite mode, tails the chain from tip and falls back to Koios when needed.

**Nonce Evolution** — Tracks the evolving nonce per-block by accumulating VRF outputs through BLAKE2b-256 hashing. Freezes the candidate nonce at the stability window (70% epoch progress), then triggers leader schedule calculation for the next epoch.

**WebSocket Feed** — Broadcasts all block events over WebSocket for custom integrations and dashboards.

**Node Failover** — Supports multiple node addresses with exponential backoff retry. If the primary goes down, duckBot rolls over to the backup.

## Operating Modes

| Mode | Description |
| ---- | ----------- |
| **lite** (default) | Tails chain from tip via adder. Uses Koios API for epoch nonces. No historical data. |
| **full** | Historical sync from Shelley genesis via gouroboros NtN. Builds complete local nonce history. Transitions to live tail once caught up. |

## Architecture

| File           | What It Does                                                                |
| -------------- | --------------------------------------------------------------------------- |
| `main.go`      | Config, chain sync (adder), block notifications, leaderlog, mode/social toggles |
| `leaderlog.go` | CPRAOS schedule math, VRF key parsing, epoch/slot calculations              |
| `nonce.go`     | Nonce evolution: VRF accumulation, candidate freeze, genesis-seeded or zero-seeded |
| `store.go`     | Store interface + SQLite implementation (default, pure Go, no CGO)          |
| `db.go`        | PostgreSQL implementation of the Store interface                            |
| `sync.go`      | Historical chain syncer using gouroboros NtN ChainSync protocol             |

## Quick Start

### Prerequisites

- Go 1.24+
- Access to a Cardano node (N2N protocol, TCP port 3001)
- Telegram bot token and channel
- Your pool's `vrf.skey` (if using leaderlog)

### Build

```bash
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .
```

### Configure

Copy the example config and fill in your values:

```bash
cp config.yaml.example config.yaml
```

```yaml
mode: "lite"  # "lite" or "full"

poolId: "YOUR_POOL_ID_HEX"
ticker: "DUCK"
poolName: "DuckPool"
nodeAddress:
  host1: "your-node:3001"
  host2: "backup-node:3001"  # optional
networkMagic: 764824073  # mainnet=764824073, preprod=1, preview=2

telegram:
  enabled: true
  channel: "-100XXXXXXXXXX"

twitter:
  enabled: false  # set true + provide env vars to enable

leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  timezone: "America/New_York"

database:
  driver: "sqlite"          # "sqlite" (default) or "postgres"
  path: "./goduckbot.db"    # SQLite file path
  # PostgreSQL settings (driver: postgres)
  host: "localhost"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
  password: ""  # prefer GODUCKBOT_DB_PASSWORD env var
```

### Environment Variables

| Variable                       | Purpose                                            |
| ------------------------------ | -------------------------------------------------- |
| `TELEGRAM_TOKEN`               | Telegram bot API token                             |
| `GODUCKBOT_DB_PASSWORD`        | PostgreSQL password (recommended over config file) |
| `TWITTER_API_KEY`              | Twitter/X API key (optional)                       |
| `TWITTER_API_KEY_SECRET`       | Twitter/X API secret (optional)                    |
| `TWITTER_ACCESS_TOKEN`         | Twitter/X access token (optional)                  |
| `TWITTER_ACCESS_TOKEN_SECRET`  | Twitter/X access token secret (optional)           |

### Run

```bash
./goduckbot
```

## Docker

```bash
# Single arch
docker build -t goduckbot .

# Multi-arch with push
docker buildx build --platform linux/amd64,linux/arm64 \
  -t wcatz/goduckbot:latest --push .
```

SQLite is the default database — no external database required. Data persists in `./goduckbot.db` (or mount a volume to `/data` for containers).

## Helm Chart

duckBot ships with a Helm chart (v0.3.0) for Kubernetes deployments.

```bash
# Package and push
helm package helm-chart/ --version 0.3.0
helm push goduckbot-0.3.0.tgz oci://ghcr.io/wcatz/helm-charts

# Deploy via helmfile
helmfile -e apps -l app=duckbot apply
```

Key chart values:

- `config.mode` — "lite" (default) or "full"
- `config.telegram.enabled` / `config.twitter.enabled` — social network toggles
- `config.leaderlog.enabled` — enable VRF tracking and schedule calculation
- `config.database.driver` — "sqlite" (default) or "postgres"
- `persistence.enabled` — PVC for SQLite data (when using sqlite driver)
- `vrfKey.secretName` — K8s secret containing your `vrf.skey`

## How CPRAOS Works (the short version)

For each slot in the upcoming epoch:

```text
VRF input     = BLAKE2b-256(slot || epochNonce)
VRF output    = VRF.Prove(vrfSkey, vrfInput)
Leader value  = BLAKE2b-256("L" || vrfOutput)
Threshold     = 2^256 * (1 - (1 - f)^sigma)
Is leader     = leaderValue < threshold
```

Where `f` = active slot coefficient (0.05), `sigma` = pool's relative stake, and the `"L"` prefix is literally byte `0x4C`. This is CPRAOS (256-bit), not TPraos (512-bit) — an important distinction from some other implementations.

## Supported Networks

| Network | Magic         | Epoch Length  | Notes                           |
| ------- | ------------- | ------------- | ------------------------------- |
| Mainnet | `764824073`   | 432,000 slots | Byron era offset from epoch 208 |
| Preprod | `1`           | 432,000 slots | Byron era offset from epoch 4   |
| Preview | `2`           | 86,400 slots  | No Byron era                    |

## Tests

```bash
go test ./... -v    # 16 tests covering store, nonce, and leaderlog
go vet ./...
helm lint helm-chart/
```

## Dependencies

Built on the shoulders of:

- [blinklabs-io/adder](https://github.com/blinklabs-io/adder) — Live chain tail pipeline
- [blinklabs-io/gouroboros](https://github.com/blinklabs-io/gouroboros) — Ouroboros protocol, VRF, ledger types, NtN ChainSync
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — Pure Go SQLite (no CGO required)
- [cardano-community/koios-go-client](https://github.com/cardano-community/koios-go-client) — Koios API for stake data and nonce fallback
- [jackc/pgx](https://github.com/jackc/pgx) — PostgreSQL driver
- [random-d.uk](https://random-d.uk) — The ducks

## Credits

Built by [wcatz](https://github.com/wcatz)

## License

MIT
