# duckBot

A Cardano stake pool operator's companion. Real-time block notifications, built-in CPRAOS leader schedule calculation, and full chain nonce history — all from a single Go binary syncing directly off your local node.

**Current version: v2.1.0**

```text
Epoch: 612
Nonce: c28960bf04eaa6b73c94eadeec9d74d80da6bbdbe25494538c4f1c9cfbfb6147
Pool Active Stake:  15,039,733,916,912₳
Network Active Stake: 21,333,861,050,004,229₳
Ideal Blocks: 15.10

Assigned Slots Times in 12-hour Format (America/New_York):
  02/09/2026 02:07:03 AM - Slot: 54532  - B: 1
  02/09/2026 02:16:00 AM - Slot: 55069  - B: 2
  02/09/2026 05:21:58 AM - Slot: 66227  - B: 3
  ...

Total Scheduled Blocks: 15
Assigned Epoch Performance: 99.37 %
```

## What It Does

**Block Notifications** — Every time your pool mints a block, duckBot fires off a notification to Telegram (with a random duck pic) and optionally Twitter/X. Includes tx count, block size, fill percentage, interval since last block, and running epoch/lifetime tallies.

**Leader Schedule Calculation** — Built-in CPRAOS implementation calculates your upcoming slot assignments for all 432K slots per epoch without needing cncli. VRF-based leadership checking validated against real mainnet data. Posts the full schedule to Telegram with 12-hour AM/PM timestamps and local timezone support.

**Direct Node Queries** — Queries your cardano-node directly via the Ouroboros local state query mini-protocol (NtC). Gets stake snapshots (mark/set/go), chain tip, epoch number, and protocol parameters without needing Ogmios or any other middleware.

**Full Chain Nonce History** — In full mode, syncs every block from Shelley genesis using gouroboros NtN ChainSync protocol, building a complete local nonce evolution history. Supports all Cardano eras (Shelley/Allegra/Mary/Alonzo/Babbage/Conway) with era-specific VRF extraction. Processes ~3,300 blocks/sec. Enables retroactive leaderlog calculation and missed block detection.

**Dual Database Support** — SQLite (pure Go, no CGO) for development and single-node deployments. PostgreSQL with pgx CopyFrom batch writes for production environments.

**Node Failover** — Supports multiple node addresses with exponential backoff retry. If the primary goes down, duckBot rolls over to the backup.

**Telegram Bot Commands** — Interactive `/commands` for querying chain state, stake info, leader schedule, epoch nonce, block validation, and node connectivity.

## Telegram Commands

| Command | Description |
| ------- | ----------- |
| `/help` | Show available commands |
| `/status` | DB sync status, chain tip distance |
| `/tip` | Current chain tip (slot, block, epoch) |
| `/epoch` | Epoch progress, time remaining, stability window |
| `/leaderlog [next\|current]` | Calculate or retrieve leader schedule |
| `/nonce [next\|current]` | Epoch nonce (locally computed) |
| `/validate <hash>` | Check if a block is in local DB |
| `/stake` | Pool & network stake from mark snapshot |
| `/blocks [epoch]` | Pool block count for epoch |
| `/ping` | Node connectivity and latency check |

## Operating Modes

| Mode | Description |
| ---- | ----------- |
| **lite** (default) | Tails chain from tip via adder. Uses Koios API for epoch nonces and stake snapshots. No historical sync. Fast startup. |
| **full** | Historical sync from Shelley genesis via gouroboros NtN ChainSync. Builds complete local nonce history with per-block VRF accumulation. Queries node directly for stake snapshots via NtC local state query. Transitions to live tail once caught up. Requires database (sqlite or postgres). |

## Architecture

Single binary Go app, all code in root package.

| File             | What It Does                                                                |
| ---------------- | --------------------------------------------------------------------------- |
| `main.go`        | Config, chain sync (adder), block notifications, leaderlog orchestration, mode/social toggles |
| `localquery.go`  | Node query client via gouroboros NtC local state query (stake snapshots, chain tip, epoch) |
| `commands.go`    | Telegram bot /command handlers with user authorization |
| `leaderlog.go`   | CPRAOS schedule math, VRF key parsing, epoch/slot calculations, all 432K slots per epoch |
| `nonce.go`       | Nonce evolution: per-block VRF accumulation via BLAKE2b-256, candidate freeze at stability window |
| `store.go`       | Store interface + SQLite implementation (default, pure Go, no CGO)          |
| `db.go`          | PostgreSQL implementation of Store interface with pgx CopyFrom batch writes |
| `sync.go`        | Historical chain syncer using gouroboros NtN ChainSync, all eras (Shelley→Conway) |

## Quick Start

### Prerequisites

- Go 1.24+
- Access to a Cardano node (NtN on TCP port 3001, NtC on same port for local state queries)
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
  allowedUsers:
    - 123456789  # Telegram user IDs allowed to use bot commands

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
helmfile -e apps -l app=goduckbot apply
```

Key chart values:

- `config.mode` — "lite" (default) or "full"
- `config.telegram.enabled` / `config.twitter.enabled` — social network toggles
- `config.telegram.allowedUsers` — list of Telegram user IDs for bot commands
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
- [blinklabs-io/gouroboros](https://github.com/blinklabs-io/gouroboros) — Ouroboros protocol, VRF, ledger types, NtN ChainSync, NtC local state query
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — Pure Go SQLite (no CGO required)
- [cardano-community/koios-go-client](https://github.com/cardano-community/koios-go-client) — Koios API for stake data and nonce fallback (lite mode)
- [jackc/pgx](https://github.com/jackc/pgx) — PostgreSQL driver
- [random-d.uk](https://random-d.uk) — The ducks

## Credits

Built by [wcatz](https://github.com/wcatz)

## License

MIT
