# duckBot

We call it duckBot. There is no better name, and the day is coming soon when it will be unleashed.

A Cardano stake pool companion. Block notifications, leader schedule, epoch nonces, and stake queries — all from a single Go binary talking directly to your node via [gouroboros](https://github.com/blinklabs-io/gouroboros).

## What It Does

**Block Notifications** — Posts to Telegram and optionally Twitter/X when your pool mints a block. Includes tx count, block size, fill percentage, interval since last block, and running epoch/lifetime tallies.

**Leader Schedule** — CPRAOS implementation checking all 432,000 slots per epoch against your VRF key. Calculates next epoch schedule automatically at the stability window (60% into epoch). On-demand via `/leaderlog <epoch>`.

**Epoch Nonces** — In full mode, streams every block from Shelley genesis extracting VRF outputs per era, evolving the nonce via BLAKE2b-256, and freezing at the stability window. Backfills ~400 epochs in under 2 minutes.

**Stake Queries** — Direct NtC local state query to your cardano-node for mark/set/go stake snapshots. Falls back to Koios if NtC is unavailable.

**Node Failover** — Multiple node addresses with exponential backoff retry.

## Modes

**lite** (default) — Tails chain from tip via [adder](https://github.com/blinklabs-io/adder). Uses Koios for epoch nonces. Fast startup, minimal resources.

**full** — Historical sync from Shelley genesis via gouroboros NtN ChainSync (~3,300 blocks/sec). Builds complete local nonce history. Self-computes all epoch nonces. Transitions to adder live tail once caught up.

## Telegram Commands

| Command | Description |
| ------- | ----------- |
| `/help` | Show available commands |
| `/status` | DB sync status |
| `/tip` | Current chain tip |
| `/epoch` | Epoch progress and time remaining |
| `/leaderlog [next\|current\|<epoch>]` | Leader schedule (inline buttons) |
| `/nextblock` | Next scheduled block slot and time |
| `/nonce [next\|current]` | Epoch nonce (inline buttons) |
| `/validate <hash>` | Check block in local DB |
| `/stake` | Pool and network stake |
| `/blocks [epoch]` | Pool block count |
| `/ping` | Node connectivity check |
| `/duck [gif\|img]` | Random duck pic (inline buttons) |
| `/version` | Bot version info |

Commands with subcommands (`/leaderlog`, `/nonce`, `/duck`) show inline keyboard buttons when called without arguments.

## Supported Networks

Set `networkMagic` in your `config.yaml` for your network. Epoch calculations, Koios API endpoints, and block explorer links adjust automatically.

| Network | Magic | Epoch Length |
| ------- | ----- | ------------ |
| Mainnet | `764824073` | 5 days (432,000 slots) |
| Preprod | `1` | 5 days (432,000 slots) |
| Preview | `2` | 1 day (86,400 slots) |

## Quick Start (Lite Mode)

### 1. Create a Telegram Bot

1. Open Telegram and message [@BotFather](https://t.me/BotFather)
2. Send `/newbot`, pick a name and username
3. Copy the API token (looks like `123456789:ABCdef...`)

### 2. Get Your Group ID

1. Add [@RawDataBot](https://t.me/RawDataBot) to your Telegram group
2. It replies with a JSON message — find `"chat": { "id": -100XXXXXXXXXX }`
3. That number (including the `-`) is your group ID
4. Remove RawDataBot from the group

### 3. Add Your Bot to the Group

1. Add your new bot to the group
2. Send any message in the group so the bot can see it
3. Optionally make the bot an admin so it can pin messages

### 4. Configure and Run

```bash
git clone https://github.com/wcatz/goduckbot.git
cd goduckbot
cp .env.example .env
cp config.yaml.example config.yaml
```

Edit `.env`:
```
TELEGRAM_TOKEN=123456789:ABCdef_your_token_here
```

Edit `config.yaml`:
```yaml
mode: "lite"
poolId: "YOUR_POOL_ID_HEX"
ticker: "DUCK"
poolName: "My Pool"
nodeAddress:
  host1: "your-node:3001"
networkMagic: 764824073    # mainnet. preprod=1, preview=2

telegram:
  enabled: true
  channel: "-100XXXXXXXXXX"
  allowedUsers:
    - 123456789
  allowedGroups:
    - -100XXXXXXXXXX
```

Start:
```bash
docker compose up -d
```

Check logs:
```bash
docker compose logs -f
```

That's it. Lite mode connects to any reachable cardano-node NtN port and uses Koios for nonces and stake data.

## Full Mode

Full mode adds historical chain sync and self-computed nonces. Requires a VRF key and database.

Edit `config.yaml`:
```yaml
mode: "full"
nodeAddress:
  host1: "your-node:3001"
  ntcHost: "/ipc/node.socket"    # UNIX socket (docker-compose)
  # ntcHost: "your-node:30000"   # or TCP via socat bridge

leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  timezone: "America/New_York"
```

Uncomment the socket volume in `docker-compose.yaml`:
```yaml
volumes:
  - /opt/cardano/ipc:/ipc:ro
```

Place your `vrf.skey` in a `keys/` directory next to the compose file.

For remote nodes, bridge the UNIX socket with socat:
```bash
socat TCP-LISTEN:30000,fork UNIX-CLIENT:/ipc/node.socket,ignoreeof
```

### PostgreSQL

```bash
docker compose --profile postgres up -d
```

Set `database.driver: "postgres"` in config and `GODUCKBOT_DB_PASSWORD` in `.env`.

## Environment Variables

| Variable | Required | Purpose |
| -------- | -------- | ------- |
| `TELEGRAM_TOKEN` | Yes | Telegram bot API token |
| `GODUCKBOT_DB_PASSWORD` | Full mode (postgres) | PostgreSQL password |
| `TWITTER_API_KEY` | No | Twitter/X API key |
| `TWITTER_API_KEY_SECRET` | No | Twitter/X API secret |
| `TWITTER_ACCESS_TOKEN` | No | Twitter/X access token |
| `TWITTER_ACCESS_TOKEN_SECRET` | No | Twitter/X access token secret |

## Build From Source

```bash
CGO_ENABLED=0 go build -o goduckbot .
./goduckbot
```

## Helm Chart

```bash
helm install goduckbot oci://ghcr.io/wcatz/helm-charts/goduckbot
```

## Architecture

Single binary, all Go, no CGO.

| File | Purpose |
| ---- | ------- |
| `main.go` | Config, adder pipeline, block notifications, leaderlog orchestration |
| `sync.go` | Historical chain syncer via NtN ChainSync |
| `commands.go` | Telegram bot command handlers |
| `leaderlog.go` | CPRAOS schedule math, VRF key parsing |
| `nonce.go` | Nonce evolution, backfill, stability window freeze |
| `localquery.go` | NtC local state query client |
| `store.go` | Store interface + SQLite implementation |
| `db.go` | PostgreSQL implementation |

## License

MIT
