# duckBot

We call it duckBot. There is no better name, and the day is coming soon when it will be unleashed.

A Cardano stake pool companion. Block notifications, leader schedule, epoch nonces, and stake queries — all from a single Go binary talking directly to your node via [gouroboros](https://github.com/blinklabs-io/gouroboros).

## What It Does

**Block Notifications** — Posts to Telegram and optionally Twitter/X when your pool mints a block. Includes tx count, block size, fill percentage, interval since last block, and running epoch/lifetime tallies.

**Leader Schedule** — Pure Go CPRAOS implementation checking every slot per epoch against your VRF key. Calculates next epoch schedule automatically at the stability window (60% into epoch). On-demand via `/leaderlog`.

**Epoch Nonces** — In full mode, streams every block from Shelley genesis extracting VRF outputs per era, evolving the nonce via BLAKE2b-256, and freezing at the stability window. Backfills ~400 epochs in under 2 minutes.

**Stake Queries** — Direct NtC local state query to your cardano-node for mark/set/go stake snapshots. Falls back to Koios if NtC is unavailable.

**Node Failover** — Multiple node addresses with exponential backoff retry.

## Modes

**lite** (default) — Tails chain from tip via [adder](https://github.com/blinklabs-io/adder). Uses Koios for epoch nonces. Fast startup, minimal resources.

**full** — Historical sync from Shelley genesis via gouroboros NtN ChainSync (~3,300 blocks/sec). Builds complete local nonce history. Self-computes all epoch nonces. Transitions to adder live tail once caught up.

## Supported Networks

Set `networkMagic` in your `config.yaml`. Epoch calculations, Koios API endpoints, and block explorer links adjust automatically.

| Network | Magic | Epoch Length | Active Slot Coeff | Expected Blocks/Epoch |
| ------- | ----- | ------------ | ----------------- | --------------------- |
| Mainnet | `764824073` | 5 days (432,000 slots) | 5% | ~21,600 |
| Preprod | `1` | 5 days (432,000 slots) | 5% | ~21,600 |
| Preview | `2` | 1 day (86,400 slots) | 5% | ~4,320 |

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
| `/duck [gif\|img]` | Random duck media (inline buttons) |
| `/version` | Bot version info |

Commands with subcommands (`/leaderlog`, `/nonce`, `/duck`) show inline keyboard buttons when called without arguments.

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

duck:
  media: "both"    # "gif", "img", or "both"
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

Full mode adds historical chain sync, self-computed nonces, and NtC local state queries. Requires a VRF key and database.

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

database:
  driver: "postgres"
  host: "postgres-host"
  port: 5432
  name: "goduckbot"
  user: "goduckbot"
```

Uncomment the socket volume in `docker-compose.yaml`:
```yaml
volumes:
  - /opt/cardano/ipc:/ipc:ro
```

Place your `vrf.skey` in a `keys/` directory next to the compose file.

### NtC (Node-to-Client) Connection

Full mode uses the NtC mini-protocol for local state queries (stake snapshots). This requires access to the cardano-node UNIX socket, either directly or via socat bridge.

**Direct socket (same host):**
```yaml
ntcHost: "/ipc/node.socket"
```

**Remote via socat bridge:**
```bash
# On the cardano-node host:
socat TCP-LISTEN:30000,fork UNIX-CLIENT:/ipc/node.socket,ignoreeof
```
```yaml
ntcHost: "your-node:30000"
```

Port 3001 is NtN (node-to-node) for chain sync only. Port 30000 (socat) is NtC for local state queries. You cannot do stake queries over NtN.

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

## How It Works

### CPRAOS Leader Election

duckBot implements the Cardano CPRAOS (Certified Praos) leader election algorithm in pure Go. This replaces external tools like cncli for leader schedule calculation. The implementation has been validated against cncli on both preview and mainnet with identical results.

For each slot in the target epoch:

```
1. VRF Input     = BLAKE2b-256(slot[8 bytes BE] || epochNonce[32 bytes])
2. _, vrfOutput  = vrf.Prove(vrfSkey[:32], vrfInput)
3. Leader Value  = BLAKE2b-256("L" || vrfOutput)           // "L" = 0x4C prefix
4. Threshold     = 2^256 * (1 - (1 - f)^sigma)             // f=0.05, sigma=poolStake/totalStake
5. Is Leader     = leaderValue < threshold
```

The "L" prefix in step 3 is the key difference from TPraos. CPRAOS uses a 256-bit comparison space; gouroboros `consensus.IsSlotLeader()` implements TPraos (512-bit) which produces different results. duckBot uses `vrf.Prove()` directly with the CPRAOS algorithm above.

**Performance:** ~86 seconds for a full mainnet epoch (432,000 slots), ~17 seconds for preview (86,400 slots).

### VRF Key Format

The `vrf.skey` file uses Cardano's TextEnvelope format — JSON wrapping CBOR-encoded key material:

```json
{
  "type": "VrfSigningKey_PraosVRF",
  "description": "VRF Signing Key",
  "cborHex": "5840<128 hex chars>"
}
```

The `5840` CBOR prefix indicates a 64-byte byte string. The 64 bytes contain the 32-byte private scalar followed by the 32-byte public key. Only the first 32 bytes (private scalar) are used for `vrf.Prove()`.

### Epoch Nonce

The epoch nonce is a 32-byte hash serving as randomness for VRF leader election. It ensures slot leader selection is unpredictable yet verifiable.

**In full mode**, duckBot self-computes nonces by streaming every block from Shelley genesis:

1. Per block: extract VRF output from header, compute `nonceValue = BLAKE2b-256("N" || vrfOutput)`
2. Evolve: `eta_v = BLAKE2b-256(eta_v || nonceValue)` — rolling accumulation across the epoch
3. At stability window (60% epoch progress): freeze candidate nonce
4. Final nonce: `BLAKE2b-256(candidateNonce || previousEpochNonce)`

**In lite mode**, nonces are fetched from the Koios API.

### Stability Window

The next epoch's nonce becomes available after the stability window — 60% into the current epoch (Conway era, `4k/f` = 172,800 slots). duckBot automatically triggers leader schedule calculation at this point.

| Network | Nonce Available After | Time Before Epoch End |
| ------- | --------------------- | --------------------- |
| Mainnet | 259,200 slots (~3 days) | ~2 days |
| Preprod | 259,200 slots (~3 days) | ~2 days |
| Preview | 51,840 slots (~14.4 hours) | ~9.6 hours |

### Stake Snapshots

Leader schedule calculation requires pool stake and total active stake. duckBot queries these directly from cardano-node via NtC local state query.

| Snapshot | When Taken | Used For |
| -------- | ---------- | -------- |
| **Mark** | End of epoch N-1 | Next epoch schedule |
| **Set** | End of epoch N-2 | Current epoch |
| **Go** | End of epoch N-3 | Previous epoch |

For next epoch's schedule (the most common case), the **mark** snapshot is used. Falls back to Koios `GetPoolInfo` / `GetEpochInfo` if NtC is unavailable.

### VRF Output Extraction by Era

Block headers contain VRF proofs that differ by era:

| Era | Header Field | Output Size |
| --- | ------------ | ----------- |
| Shelley, Allegra, Mary, Alonzo | `header.Body.NonceVrf.Output` | 64 bytes |
| Babbage, Conway | `header.Body.VrfResult.Output` | 64 bytes (combined) |
| Byron | N/A (no VRF) | Skipped |

### Historical Sync Pipeline

Full mode achieves ~3,300 blocks/sec through a pipelined architecture:

1. `sync.go` reads blocks from cardano-node via gouroboros NtN ChainSync
2. Blocks sent to buffered channel (10,000 capacity) — decouples network I/O from DB writes
3. Batch processor drains channel (1,000 blocks or 2-second timeout)
4. `nonce.go` ProcessBatch() evolves nonce in-memory for entire batch
5. `db.go` persists via pgx CopyFrom (PostgreSQL COPY protocol)

Full Shelley-to-tip sync: ~43 minutes for ~8.5M blocks.

## Build From Source

```bash
CGO_ENABLED=0 go build -o goduckbot .
./goduckbot
```

Cross-compile for Docker:
```bash
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .
```

## Docker

```bash
docker pull wcatz/goduckbot:latest
```

Multi-arch images (amd64/arm64) are published to [Docker Hub](https://hub.docker.com/r/wcatz/goduckbot) via GitHub Actions on every merge to master and tagged release.

## Helm Chart

```bash
helm install goduckbot oci://ghcr.io/wcatz/helm-charts/goduckbot
```

Key values:
- `config.mode` — `"lite"` or `"full"`
- `config.duck.media` — `"gif"`, `"img"`, or `"both"`
- `config.leaderlog.enabled` — enables VRF tracking and schedule calculation
- `config.database.driver` — `"sqlite"` or `"postgres"`
- `vrfKey.content` — VRF signing key (mounted as K8s secret)
- `persistence.enabled` — PVC for SQLite data

## Architecture

Single binary, all Go, no CGO. Built on [gouroboros](https://github.com/blinklabs-io/gouroboros) and [adder](https://github.com/blinklabs-io/adder) from [Blink Labs](https://github.com/blinklabs-io).

```
Full mode:  gouroboros NtN ChainSync (historical) → caught up → adder pipeline (live tail)
Lite mode:  adder pipeline only (intersect at tip)
```

| File | Purpose |
| ---- | ------- |
| `main.go` | Config, adder pipeline, block notifications, leaderlog orchestration |
| `sync.go` | Historical chain syncer via NtN ChainSync |
| `commands.go` | Telegram bot command handlers, inline keyboards |
| `leaderlog.go` | CPRAOS schedule math, VRF key parsing, slot/epoch utilities |
| `nonce.go` | Nonce evolution, batch processing, stability window freeze |
| `localquery.go` | NtC local state query client (stake snapshots) |
| `store.go` | Store interface + SQLite implementation |
| `db.go` | PostgreSQL implementation (pgx CopyFrom bulk inserts) |

### gouroboros Usage

[gouroboros](https://github.com/blinklabs-io/gouroboros) provides Go implementations of Cardano's Ouroboros mini-protocols. duckBot uses three subsystems:

**VRF** (`gouroboros/vrf`) — Pure Go ECVRF-ED25519-SHA512-Elligator2 implementation. duckBot calls `vrf.Prove(vrfSkey[:32], vrfInput)` for CPRAOS leader election. This eliminates the need for C dependencies (libsodium) and enables `CGO_ENABLED=0` builds. The VRF output is 64 bytes; duckBot applies CPRAOS post-processing (`BLAKE2b-256("L" || output)`) for the 256-bit leader value.

**NtN ChainSync** (`gouroboros/protocol/chainsync`) — Node-to-Node chain synchronization for historical sync. duckBot creates a connection with `ouroboros.NewConnection()` targeting port 3001, configures `chainsync.WithRollForwardFunc()` and `chainsync.WithPipelineLimit(50)` for high-throughput block streaming. Each block header is type-asserted to its era-specific type (`conway.ConwayBlockHeader`, `babbage.BabbageBlockHeader`, etc.) to extract VRF outputs.

**NtC LocalStateQuery** (`gouroboros/protocol/localstatequery`) — Node-to-Client protocol for stake snapshot queries. duckBot calls `client.GetStakeSnapshots()` to retrieve mark/set/go stake values directly from cardano-node. Configured with `localstatequery.WithQueryTimeout(10*time.Minute)` for mainnet (default 180s is too short). Requires UNIX socket or socat bridge (port 30000), not the NtN port (3001).

### adder Usage

[adder](https://github.com/blinklabs-io/adder) provides a pipeline-based chain follower. duckBot uses it for live chain tail after historical sync completes (or immediately in lite mode):

```go
pipeline.New()
  → chainsync.New(WithAddress(host), WithIntersectTip(true), WithAutoReconnect(false))
  → filter_event.New(WithTypes(["chainsync.block"]))
  → output_embedded.New(WithCallbackFunc(handleEvent))
  → Start()
```

The adder pipeline handles reconnection at the outer level — duckBot wraps it in an infinite restart loop with exponential backoff and host failover. Block events arrive as `event.BlockEvent` with full block data including transaction count, which NtN headers alone don't provide.

### Key Dependencies

| Library | Version | Purpose |
| ------- | ------- | ------- |
| `blinklabs-io/gouroboros` | v0.153.1 | VRF, NtN ChainSync, NtC LocalStateQuery, ledger types |
| `blinklabs-io/adder` | v0.37.0 | Live chain tail pipeline (full block data incl. tx count) |
| `cardano-community/koios-go-client/v3` | | Koios API for stake data and nonce fallback |
| `gopkg.in/telebot.v4` | | Telegram bot framework |
| `michimani/gotwi` | | Twitter/X API v2 client |
| `jackc/pgx/v5` | | PostgreSQL driver with COPY protocol |
| `modernc.org/sqlite` | | Pure Go SQLite (no CGO) |
| `golang.org/x/crypto` | | BLAKE2b-256 hashing |

## References

- [Ouroboros Praos Paper](https://iohk.io/en/research/library/papers/ouroboros-praos-an-adaptively-secure-semi-synchronous-proof-of-stake-protocol/)
- [IETF VRF Draft (ECVRF-ED25519-SHA512-Elligator2)](https://datatracker.ietf.org/doc/html/draft-irtf-cfrg-vrf-03)
- [Shelley Ledger Formal Specification](https://github.com/IntersectMBO/cardano-ledger/releases/latest/download/shelley-ledger.pdf)
- [cncli (Rust reference implementation)](https://github.com/cardano-community/cncli)
- [Koios API Documentation](https://api.koios.rest/)

## License

MIT

---

Early duckBot development by [AstroWa3l](https://github.com/AstroWa3l) — a PoS for not visiting when he's only 3.5 hours away.
