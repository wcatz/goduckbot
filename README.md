# duckBot

<img src="logo.jpg" alt="duckBot" width="130" align="right">

We call it duckBot. There is no better name, and the day is coming soon when it will be unleashed.

Cardano stake pool notification bot. Single Go binary, no CGO, talks directly to cardano-node via [gOuroboros](https://github.com/blinklabs-io/gouroboros).

CPraos leader schedule calculation, epoch nonce evolution, block notifications (Telegram, Twitter/X), stake queries, and full history.

## Quick Start

### Prerequisites

- A reachable cardano-node (NtN port 3001)
- Telegram bot token from [@BotFather](https://t.me/BotFather)
- Telegram group/channel ID (add [@RawDataBot](https://t.me/RawDataBot) to your group, grab the chat ID from the JSON response, then remove it)

### Run

```bash
git clone https://github.com/wcatz/goduckbot.git && cd goduckbot
cp .env.example .env
cp config.yaml.example config.yaml
```

Edit `.env`:
```
TELEGRAM_TOKEN=123456789:ABCdef_your_token_here
GODUCKBOT_VERSION=latest
```

Edit `config.yaml`:
```yaml
mode: "lite"
poolId: "YOUR_POOL_ID_HEX"
poolName: "My Pool"
nodeAddress:
  host1: "your-node:3001"
networkMagic: 764824073

telegram:
  enabled: true
  channel: "-100XXXXXXXXXX"
  allowedUsers:
    - 123456789
  allowedGroups:
    - -100XXXXXXXXXX

duck:
  media: "both"
```

```bash
docker compose up -d
docker compose logs -f
```

### Full Mode

Adds full chain sync, self-computed nonces, and local leader schedule calculation. Requires a VRF key and a database (SQLite by default, PostgreSQL optional).

```yaml
mode: "full"
nodeAddress:
  host1: "your-node:3001"       # NtN (chain sync)
  ntcHost: "your-node:30000"    # NtC (stake queries, tip) — or UNIX socket path

leaderlog:
  enabled: true
  vrfKeyValue: "5840..."           # cborHex from vrf.skey
  timezone: "America/New_York"
  timeFormat: "12h"
```

The `vrfKeyValue` is the `cborHex` field from your `vrf.skey` file (`5840` + 128 hex chars = 32-byte private scalar + 32-byte public key). SQLite is used by default with no extra config. For PostgreSQL:

```yaml
database:
  driver: "postgres"
  host: "postgres-host"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
```

Set `GODUCKBOT_DB_PASSWORD` in `.env`.

## How It Works

### Modes

| Mode | Chain Sync | Nonces | Startup |
|------|-----------|--------|---------|
| **lite** | Tail from tip | Koios API | Immediate |
| **full** | Historical NtN ChainSync from Shelley genesis, then live tail | Self-computed from VRF outputs via TICKN rule | Full sync required |

### CPraos Leader Election

Per slot in target epoch:

```text
1. vrfInput    = BLAKE2b-256(slot[8B BE] || epochNonce[32B])
2. _, output   = vrf.Prove(vrfSkey[:32], vrfInput)
3. leaderValue = BLAKE2b-256(0x4C || output)
4. threshold   = 2^256 * (1 - (1-f)^σ)          // f=0.05, σ=poolStake/totalStake
5. isLeader    = leaderValue < threshold
```

The `0x4C` ("L") domain separator is the CPraos distinction from TPraos (which uses a 512-bit comparison space).

### Epoch Nonce Evolution

Full mode streams every block from Shelley genesis, computing nonces per era:

| Era | VRF Nonce Value |
|-----|----------------|
| Shelley–Alonzo (TPraos) | `BLAKE2b-256(vrfOutput)` |
| Babbage+ (CPraos) | `BLAKE2b-256(BLAKE2b-256(0x4E \|\| vrfOutput))` |

Per-block evolution: `η_v = BLAKE2b-256(η_v \|\| nonceValue)`

At the stability window: freeze candidate nonce `η_c` from the current `η_v`.

Epoch transition — TICKN rule for epoch E's nonce:

```text
epochNonce(E) = BLAKE2b-256(η_c(E-1) || η_ph(E-2))
```

Where:
- `η_c(E-1)` = candidate nonce frozen at the stability window of epoch E-1
- `η_ph(E-2)` = `prevHash` of the last block in epoch E-2 (= blockHash of the second-to-last block of epoch E-2)

The `η_ph` component lags by one epoch — at the E-1 → E transition, the `labNonce` (last anchor block nonce) from epoch E-2 is used, while the current epoch's `labNonce` is saved for the next transition.

Epoch 259's nonce is hardcoded because it is the only mainnet epoch where the `extra_entropy` protocol parameter was set. Since duckBot computes nonces purely from VRF outputs in block headers, it cannot account for `extra_entropy` — so the known result is used directly.

### Stability Window

The next epoch's nonce becomes deterministic after the stability window. duckBot triggers leader schedule calculation automatically at this point.

| Era | Margin (`k`/`f`) | Freeze Point | Slots Before Epoch End |
|-----|-------------------|-------------|----------------------|
| Shelley–Babbage | `3k/f` = 129,600 | 302,400 (70%) | 129,600 (~1.5 days) |
| Conway | `4k/f` = 172,800 | 259,200 (60%) | 172,800 (~2 days) |

### Historical Sync Pipeline

```text
sync.go (NtN ChainSync) → blockCh [10,000 buf] → batch writer → InsertBlockBatch (CopyFrom) → ProcessBatch (nonce)
```

- Pipeline limit: 100 concurrent RequestNext messages
- Keepalive disabled during sync (prevents ChainSync stall with gouroboros muxer)
- Unlimited retry with capped backoff (5s–30s) on node timeout
- 100-block overlap window on reconnect prevents block gaps from muxer buffer loss
- `ON CONFLICT (slot) DO NOTHING` deduplicates across retries

### VRF Output Extraction by Era

| Era | Header Field | Output Size |
|-----|-------------|------------|
| Shelley, Allegra, Mary, Alonzo | `header.Body.NonceVrf.Output` | 64 bytes |
| Babbage, Conway | `header.Body.VrfResult.Output` | 64 bytes |
| Byron | N/A | Skipped |

### Stake Snapshots

| Snapshot | Taken At | Used For |
|----------|----------|----------|
| **Mark** | End of epoch N-1 | Next epoch schedule |
| **Set** | End of epoch N-2 | Current epoch |
| **Go** | End of epoch N-3 | Previous epoch |

Queried via NtC `QueryPoolStakeSnapshots` (primary) with Koios API fallback.

### DB Integrity Check

On startup (full mode), validates chain data against cardano-node:

1. **Layer 1**: Block count in `blocks` table vs `epoch_nonces.block_count`
2. **Layer 2**: `FindIntersect` last 50 blocks against node

If blocks are orphaned (CNPG failover): truncate all tables, full resync.
If nonce is stale but blocks valid: recompute from `blocks` table.

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/menu` | Inline keyboard with all commands (primary group chat interface) |
| `/help` | Available commands |
| `/status` | DB sync status |
| `/tip` | Current chain tip |
| `/epoch` | Epoch progress and time remaining |
| `/leaderlog [next\|current\|<epoch>]` | Leader schedule |
| `/nextblock` | Next scheduled slot and time |
| `/nonce [next\|current]` | Epoch nonce |
| `/validate <hash>` | Check block in local DB |
| `/stake` | Pool and network stake |
| `/blocks [epoch]` | Pool block count |
| `/ping` | Node connectivity |
| `/duck [gif\|img]` | Random duck media |
| `/version` | Bot version |

Commands with subcommands show inline keyboard buttons when called without arguments.

## Supported Networks

| Network | Magic | Epoch Length | Active Slot Coeff |
|---------|-------|-------------|-------------------|
| Mainnet | `764824073` | 432,000 slots (5 days) | 5% |
| Preprod | `1` | 432,000 slots (5 days) | 5% |
| Preview | `2` | 86,400 slots (1 day) | 5% |
| Sancho  | `4` | 86,400 slots (1 day) | 5% |

## Environment Variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `TELEGRAM_TOKEN` | Yes | Telegram bot API token |
| `GODUCKBOT_DB_PASSWORD` | No (postgres only) | PostgreSQL password |
| `TWITTER_API_KEY` | No | Twitter/X API key |
| `TWITTER_API_KEY_SECRET` | No | Twitter/X API secret |
| `TWITTER_ACCESS_TOKEN` | No | Twitter/X access token |
| `TWITTER_ACCESS_TOKEN_SECRET` | No | Twitter/X access token secret |

## CLI

```bash
goduckbot                       # Start daemon (default)
goduckbot version               # Version, commit SHA, build date
goduckbot nonce <epoch>         # Show epoch nonce (DB → computed → Koios fallback)
goduckbot leaderlog <epoch>     # Leader schedule for a single epoch
goduckbot leaderlog <N>-<M>     # Leader schedule for epoch range (max 10)
goduckbot history [--force] [--from N]  # Build full historical leaderlog
```

CLI subcommands require `config.yaml` in the working directory. The `leaderlog` and `history` commands require a VRF key and database.

## Build

```bash
CGO_ENABLED=0 go build -o goduckbot .
```

## Docker

```bash
docker pull wcatz/goduckbot:latest
```

Multiarch images (amd64, arm64) published to [Docker Hub](https://hub.docker.com/r/wcatz/goduckbot).

## Helm

```bash
helm install goduckbot oci://ghcr.io/wcatz/helm-charts/goduckbot
```

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | Config, live tail pipeline, block notifications, Telegram/Twitter |
| `sync.go` | Historical chain syncer (NtN ChainSync) |
| `nonce.go` | Nonce evolution, backfill, integrity check, TICKN |
| `leaderlog.go` | CPraos schedule, VRF key parsing, slot/epoch math |
| `cli.go` | CLI subcommands (version, leaderlog, nonce, history) |
| `commands.go` | Telegram bot command handlers |
| `integrity.go` | Startup DB validation (FindIntersect + nonce repair) |
| `localquery.go` | NtC local state queries (QueryTip, QueryPoolStakeSnapshots) |
| `store.go` | Store interface + SQLite implementation |
| `db.go` | PostgreSQL implementation (pgx CopyFrom) |
| `securekey.go` | mmap/mlock/mprotect for VRF key material |

### Key Dependencies

| Library | Purpose |
|---------|---------|
| [blinklabs-io/gouroboros](https://github.com/blinklabs-io/gouroboros) | VRF, NtN ChainSync, ledger types |
| [cardano-community/koios-go-client](https://github.com/cardano-community/koios-go-client) | Koios API (stake data, nonce fallback) |
| [jackc/pgx](https://github.com/jackc/pgx) | PostgreSQL with COPY protocol |
| [modernc.org/sqlite](https://gitlab.com/nicedoc/modernc-sqlite) | Pure Go SQLite |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | BLAKE2b-256 |

## References

- [Ouroboros Praos](https://iohk.io/en/research/library/papers/ouroboros-praos-an-adaptively-secure-semi-synchronous-proof-of-stake-protocol/)
- [IETF VRF Draft](https://datatracker.ietf.org/doc/html/draft-irtf-cfrg-vrf-03)
- [Shelley Ledger Spec](https://github.com/IntersectMBO/cardano-ledger/releases/latest/download/shelley-ledger.pdf)
- [cncli](https://github.com/cardano-community/cncli)
- [Koios API](https://api.koios.rest/)

