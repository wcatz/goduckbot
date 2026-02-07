# duckBot

We call it duckBot. There is no better name, and the day is coming soon when it will be unleashed.

A zero-dependency Cardano stake pool companion. Block notifications, CPRAOS leader schedule, epoch nonces, and stake queries — all from a single Go binary talking directly to your node via [gouroboros](https://github.com/blinklabs-io/gouroboros). No cncli. No Ogmios. No external APIs.

**Current version: v2.3.5**

## Why duckBot?

Most SPO tooling depends on a stack: cncli for leader schedule, Ogmios or db-sync for queries, Koios/Blockfrost for nonce and stake data. duckBot replaces all of that with a single binary that speaks Ouroboros natively:

| What | Traditional Stack | duckBot |
| ---- | ----------------- | ------- |
| Leader schedule | cncli + cardano-cli | Built-in CPRAOS via gouroboros VRF |
| Stake snapshots | Ogmios / Koios / cardano-cli | Direct NtC local state query |
| Epoch nonces | Koios API / db-sync | Self-computed from local chain data |
| Block notifications | Custom scripts + cron | Real-time via NtN ChainSync |
| Chain sync | cardano-db-sync (100GB+ DB) | gouroboros NtN (lightweight) |

## What It Does

**Block Notifications** — Every time your pool mints a block, duckBot posts to Telegram (with a random duck pic) and optionally Twitter/X. Includes tx count, block size, fill percentage, interval since last block, and running epoch/lifetime tallies.

**Leader Schedule** — Full CPRAOS implementation checks all 432,000 slots per epoch against your VRF key. No cncli required. Calculates next epoch schedule automatically at the stability window (60% into epoch). Supports on-demand `/leaderlog <epoch>` for any historical epoch.

**Direct Node Queries** — Talks to your cardano-node via NtC local state query protocol. Connects directly via UNIX socket (docker-compose) or through a socat TCP bridge for remote nodes. Gets stake snapshots (mark/set/go), chain tip, and epoch number directly from the ledger — no middleware.

**Self-Computed Nonces** — In full mode, streams every block from Shelley genesis, extracting VRF outputs per era (Shelley through Conway), evolving the nonce via BLAKE2b-256, and freezing at the stability window. Backfills the entire nonce history (~400 epochs) in under 2 minutes. Zero external dependency for nonce data.

**Historical Data** — Stores every block header, leader schedule, epoch nonce, and stake snapshot in PostgreSQL or SQLite. Query any past epoch's schedule with `/leaderlog <epoch>`, validate historical blocks with `/validate <hash>`, or look up nonces with `/nonce`. Full audit trail for missed block analysis and performance tracking.

**Node Failover** — Supports multiple node addresses with exponential backoff retry. If the primary goes down, duckBot rolls over to the backup.

## Telegram Commands

| Command | Description |
| ------- | ----------- |
| `/help` | Show available commands |
| `/status` | DB sync status, chain tip distance |
| `/tip` | Current chain tip (slot, block, epoch) |
| `/epoch` | Epoch progress, time remaining, stability window |
| `/leaderlog [next\|current\|<epoch>]` | Calculate or retrieve leader schedule |
| `/nextblock` | Time until your next scheduled block |
| `/nonce [next\|current]` | Epoch nonce (locally computed) |
| `/validate <hash>` | Check if a block is in local DB |
| `/stake` | Pool & network stake from mark snapshot |
| `/blocks [epoch]` | Pool block count for epoch |
| `/ping` | Node connectivity and latency check |
| `/duck` | Random duck pic |

## Operating Modes

**lite** (default) — Tails chain from tip via [adder](https://github.com/blinklabs-io/adder). Uses Koios API for epoch nonces. Fast startup, minimal resources.

**full** — Historical sync from Shelley genesis via gouroboros NtN ChainSync. Builds complete local nonce history with per-block VRF accumulation. Self-computes all epoch nonces. Direct node queries for stake snapshots via NtC. Transitions to live tail once caught up. This is the cncli-replacement mode.

## Node Connectivity

duckBot uses two Ouroboros protocols to talk to your cardano-node:

| Protocol | Purpose |
| -------- | ------- |
| **NtN** (node-to-node) | ChainSync — real-time block streaming and historical sync |
| **NtC** (node-to-client) | Local state query — stake snapshots, chain tip, epoch |

NtC supports two connection methods:

**UNIX socket (recommended)** — Direct connection to the node's socket. Used by docker-compose with a shared volume. No socat needed.

```yaml
nodeAddress:
  host1: "cardano-node:3001"         # NtN (chain sync)
  ntcHost: "/ipc/node.socket"        # NtC (direct UNIX socket)
```

**TCP via socat** — For remote nodes or Kubernetes. Bridges the UNIX socket to a TCP port:

```bash
socat TCP-LISTEN:30000,fork UNIX-CLIENT:/ipc/node.socket,ignoreeof
```

```yaml
nodeAddress:
  host1: "your-node:3001"            # NtN (chain sync)
  host2: "backup-node:3001"          # NtN (optional failover)
  ntcHost: "your-node:30000"         # NtC (via socat TCP bridge)
```

## Quick Start

### Prerequisites

- Go 1.24+
- Access to a Cardano node (NtN port 3001; NtC via UNIX socket or socat for full mode)
- Telegram bot token and channel
- Your pool's `vrf.skey` (for leader schedule)

### Build

```bash
CGO_ENABLED=0 go build -o goduckbot .
```

### Configure

```bash
cp config.yaml.example config.yaml
```

```yaml
mode: "full"  # "full" for zero-dependency SPO mode, "lite" for quick start

poolId: "YOUR_POOL_ID_HEX"
ticker: "DUCK"
poolName: "DuckPool"
nodeAddress:
  host1: "your-node:3001"
  host2: "backup-node:3001"    # optional
  ntcHost: "your-node:30000"   # required for /stake, /leaderlog next
networkMagic: 764824073  # mainnet

telegram:
  enabled: true
  channel: "-100XXXXXXXXXX"
  allowedUsers:
    - 123456789

leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  timezone: "America/New_York"

database:
  driver: "sqlite"        # or "postgres"
  path: "./goduckbot.db"
```

### Environment Variables

| Variable | Purpose |
| -------- | ------- |
| `TELEGRAM_TOKEN` | Telegram bot API token |
| `GODUCKBOT_DB_PASSWORD` | PostgreSQL password |
| `TWITTER_API_KEY` | Twitter/X API key (optional) |
| `TWITTER_API_KEY_SECRET` | Twitter/X API secret (optional) |
| `TWITTER_ACCESS_TOKEN` | Twitter/X access token (optional) |
| `TWITTER_ACCESS_TOKEN_SECRET` | Twitter/X access token secret (optional) |

### Run

```bash
./goduckbot
```

## Docker

```bash
docker build -t goduckbot .

# Multi-arch
docker buildx build --platform linux/amd64,linux/arm64 \
  -t wcatz/goduckbot:latest --push .
```

## Docker Compose

```bash
cp .env.example .env        # set TELEGRAM_TOKEN
cp config.yaml.example config.yaml
# edit config.yaml: set poolId, ticker, host1, mode
docker compose up -d
```

**Lite mode** — Point `host1` at any remote node's NtN port (3001). Koios handles stake and nonces. That's it.

**Full mode** — Run duckBot on the same machine as your node. Uncomment the socket bind-mount in `docker-compose.yaml` and set `ntcHost: "/ipc/node.socket"` in config. Everything computed locally, zero external APIs.

**PostgreSQL** — `docker compose --profile postgres up -d`

## Helm Chart

```bash
helm install goduckbot oci://ghcr.io/wcatz/helm-charts/goduckbot

# Or via helmfile
helmfile -e apps -l app=goduckbot apply
```

## How the Leader Schedule Works

For each of the 432,000 slots in an epoch, duckBot checks if your pool is the leader:

```
VRF input     = BLAKE2b-256(slot || epochNonce)
VRF output    = VRF.Prove(vrfSkey, vrfInput)
Leader value  = BLAKE2b-256("L" || vrfOutput)
Threshold     = 2^256 * (1 - (1 - f)^sigma)
Is leader     = leaderValue < threshold
```

Where `f` = active slot coefficient (0.05 on mainnet), `sigma` = pool stake / total stake, and `"L"` = byte `0x4C`. This is CPRAOS (256-bit output), matching the current Cardano protocol.

The **mark** stake snapshot is used for the next epoch's schedule (not "set" — a common mistake in other implementations).

## How Nonce Computation Works

Each block's VRF output evolves the epoch nonce:

```
eta_v = BLAKE2b-256(eta_v || blockVrfOutput)
```

At the stability window (259,200 slots = 60% into epoch), the candidate nonce freezes:

```
eta_c = eta_v  (frozen at 60%)
```

At the epoch boundary:

```
epoch_nonce = BLAKE2b-256(eta_c || eta_0)
```

duckBot extracts the correct VRF output from each era's block header (Shelley through Conway use different CBOR structures) and processes ~3,300 blocks/sec during backfill.

## Architecture

Single binary, all Go, no CGO required.

| File | Purpose |
| ---- | ------- |
| `main.go` | Config, chain sync pipeline, block notifications, leaderlog orchestration |
| `localquery.go` | NtC local state query client (stake snapshots, chain tip) |
| `commands.go` | Telegram bot command handlers |
| `leaderlog.go` | CPRAOS schedule math, VRF key parsing, epoch/slot calculations |
| `nonce.go` | Nonce evolution, backfill, stability window freeze |
| `sync.go` | Historical chain syncer via NtN ChainSync (Shelley through Conway) |
| `store.go` | Store interface + SQLite implementation |
| `db.go` | PostgreSQL implementation with pgx batch writes |

## Supported Networks

| Network | Magic | Epoch Length |
| ------- | ----- | ------------ |
| Mainnet | `764824073` | 432,000 slots (5 days) |
| Preprod | `1` | 432,000 slots (5 days) |
| Preview | `2` | 86,400 slots (1 day) |

## Dependencies

- [blinklabs-io/gouroboros](https://github.com/blinklabs-io/gouroboros) — Ouroboros protocols, VRF, ledger types, NtN ChainSync, NtC local state query
- [blinklabs-io/adder](https://github.com/blinklabs-io/adder) — Live chain tail pipeline
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — Pure Go SQLite (no CGO)
- [jackc/pgx](https://github.com/jackc/pgx) — PostgreSQL driver
- [cardano-community/koios-go-client](https://github.com/cardano-community/koios-go-client) — Koios API (lite mode fallback only)
- [random-d.uk](https://random-d.uk) — The ducks

## License

MIT
