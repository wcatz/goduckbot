# duckBot

A Cardano stake pool operator's companion. Real-time block notifications, built-in CPRAOS leader schedule calculation, and full chain nonce history — all from a single Go binary syncing directly off your local node.

**Current version: v1.2.3**

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

**Leader Schedule Calculation** — Built-in CPRAOS implementation calculates your upcoming slot assignments for all 432K slots per epoch without needing cncli. VRF-based leadership checking validated against real mainnet data (epoch 500: 19/19 match). Posts the full schedule to Telegram with local timezone support and message chunking for large schedules.

**Full Chain Nonce History** — In full mode, syncs every block from Shelley genesis using gouroboros NtN ChainSync protocol, building a complete local nonce evolution history. Supports all Cardano eras (Shelley/Allegra/Mary/Alonzo/Babbage/Conway) with era-specific VRF extraction. Processes ~3,300 blocks/sec. Enables retroactive leaderlog calculation and missed block detection.

**Dual Database Support** — SQLite (pure Go, no CGO) for development and single-node deployments. PostgreSQL with pgx CopyFrom batch writes for production environments.

**Node Failover** — Supports multiple node addresses with exponential backoff retry. If the primary goes down, duckBot rolls over to the backup.

## Operating Modes

| Mode | Description |
| ---- | ----------- |
| **lite** (default) | Tails chain from tip via adder. Uses Koios API for epoch nonces and stake snapshots. No historical sync. Fast startup. |
| **full** | Historical sync from Shelley genesis via gouroboros NtN ChainSync. Builds complete local nonce history with per-block VRF accumulation. Transitions to live tail once caught up. Requires database (sqlite or postgres). |

## Architecture

Single binary Go app, all code in root package.

| File           | What It Does                                                                |
| -------------- | --------------------------------------------------------------------------- |
| `main.go`      | Config, chain sync (adder), block notifications, leaderlog orchestration, mode/social toggles |
| `leaderlog.go` | CPRAOS schedule math, VRF key parsing, epoch/slot calculations, all 432K slots per epoch |
| `nonce.go`     | Nonce evolution: per-block VRF accumulation via BLAKE2b-256, candidate freeze at stability window |
| `store.go`     | Store interface + SQLite implementation (default, pure Go, no CGO)          |
| `db.go`        | PostgreSQL implementation of Store interface with pgx CopyFrom batch writes |
| `sync.go`      | Historical chain syncer using gouroboros NtN ChainSync, all eras (Shelley→Conway) |

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

duckBot ships with a Helm chart for Kubernetes deployments.

```bash
# Install from OCI registry
helm install goduckbot oci://ghcr.io/wcatz/helm-charts/goduckbot

# Deploy via helmfile
helmfile -e apps -l app=duckbot apply
```

Key chart values:

- `config.mode` — "lite" (default) or "full"
- `config.telegram.enabled` / `config.twitter.enabled` — social network toggles
- `config.leaderlog.enabled` — enable CPRAOS schedule calculation
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
go test ./... -v    # Tests covering store, nonce, and leaderlog
go vet ./...
helm lint helm-chart/
```

Test coverage includes:
- SQLite store operations (insert, query, batch writes)
- CPRAOS VRF calculations and slot assignments
- Nonce evolution and candidate freezing
- Epoch boundary calculations

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
