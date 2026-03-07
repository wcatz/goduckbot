---
name: local-dev
description: Local development setup and workflows for goduckbot. Use when setting up a dev environment, running locally, or debugging without K8s.
---

# Local Development

Quick setup for running goduckbot locally (outside K8s) for development and testing.

## Prerequisites

```bash
# Go 1.24+
go version

# PostgreSQL (via Docker for local testing)
docker run -d --name goduckbot-postgres \
  -e POSTGRES_PASSWORD=testpass \
  -e POSTGRES_USER=goduckbot \
  -e POSTGRES_DB=goduckbot_v2 \
  -p 15432:5432 \
  postgres:17-alpine

# Or use SQLite (no external dependencies)
# Just configure database.driver: "sqlite" in config.yaml
```

## Config Setup

Create `config.yaml` in project root (gitignored):

```yaml
mode: "lite"  # or "full" for historical sync

poolId: "c825168836c5bf850dec38567eb4771c2e03eea28658ff291df768ae"
ticker: "OTG"
poolName: "Star Forge"

nodeAddress:
  host1: "relays-new.cardano-mainnet.iohk.io:3001"
  # For NtC queries (optional, for local stake queries)
  # ntcHost: "localhost:30000"  # requires socat bridge to local node socket

networkMagic: 764824073  # mainnet

telegram:
  enabled: false  # disable for local dev
  token: ""
  channel: ""
  allowedUsers: []

twitter:
  enabled: false  # disable for local dev

duck:
  media: "gif"

leaderlog:
  enabled: true
  # VRF key inline (CBOR hex from pool cold keys)
  vrfKeyValue: "5840<64-byte-hex-key>"
  # OR file path
  # vrfKeyPath: "/path/to/vrf.skey"
  timezone: "America/New_York"
  timeFormat: "12h"

database:
  # SQLite (easiest for local dev)
  driver: "sqlite"
  path: "./goduckbot-dev.db"
  
  # PostgreSQL (if you need to test DB-specific behavior)
  # driver: "postgres"
  # host: "localhost"
  # port: 15432
  # name: "goduckbot_v2"
  # user: "goduckbot"
  # password: "testpass"  # or set GODUCKBOT_DB_PASSWORD env var
```

## Build & Run

```bash
# Build
go build -o goduckbot .

# Run daemon
./goduckbot

# Run CLI subcommands
./goduckbot version
./goduckbot leaderlog 617
./goduckbot nonce 617
./goduckbot history  # builds complete history

# With PostgreSQL connection string override
DATABASE_URL="postgres://goduckbot:testpass@localhost:15432/goduckbot_v2?sslmode=disable" \
  ./goduckbot
```

## Development Workflow

### 1. Make Changes

```bash
# Edit code
vim main.go nonce.go leaderlog.go

# Format
go fmt ./...

# Vet
go vet ./...
```

### 2. Run Tests

```bash
# All tests
go test ./... -v

# Specific test
go test -run TestSlotToEpochMainnet -v

# With coverage
go test ./... -cover

# Koios integration tests (slow, hits real API)
go test -run TestKoiosEpochNonceVerification -v
```

### 3. Test Locally

```bash
# Lite mode (quick start, no historical sync)
./goduckbot

# Full mode (historical sync from Shelley genesis)
# Edit config.yaml: mode: "full"
./goduckbot

# Test specific epoch leaderlog
./goduckbot leaderlog 500

# Test nonce calculation
./goduckbot nonce 500

# Test history classification
./goduckbot history --from 365  # Babbage start
```

### 4. Debug Database

```bash
# SQLite
sqlite3 goduckbot-dev.db
> .tables
> SELECT COUNT(*) FROM blocks;
> SELECT epoch, final_nonce IS NOT NULL FROM epoch_nonces ORDER BY epoch DESC LIMIT 10;
> .quit

# PostgreSQL
psql postgres://goduckbot:testpass@localhost:15432/goduckbot_v2
\dt
SELECT COUNT(*) FROM blocks;
SELECT epoch, final_nonce IS NOT NULL FROM epoch_nonces ORDER BY epoch DESC LIMIT 10;
\q
```

## Common Local Dev Tasks

### Test Nonce Evolution

```bash
# Start with empty DB, run in lite mode
rm goduckbot-dev.db
./goduckbot

# Check first block processed
sqlite3 goduckbot-dev.db "SELECT * FROM blocks LIMIT 1;"

# Check evolving nonce
sqlite3 goduckbot-dev.db "SELECT epoch, hex(evolving_nonce), block_count FROM epoch_nonces WHERE epoch = 617;"
```

### Test Leaderlog Calculation

```bash
# Calculate for past epoch (requires nonce)
./goduckbot leaderlog 612

# Calculate for epoch range
./goduckbot leaderlog 610-615

# Force recalculation (delete cached schedule first)
sqlite3 goduckbot-dev.db "DELETE FROM leader_schedules WHERE epoch = 612;"
./goduckbot leaderlog 612
```

### Test Historical Sync

```bash
# Full mode from genesis
# 1. Edit config.yaml: mode: "full"
# 2. Clear DB
rm goduckbot-dev.db
# Watch progress in logs
tail -f <log-file>  # or check terminal output
```

### Monitor Resources

```bash
# CPU and memory usage
top -p $(pgrep goduckbot)

# Or use htop for better visualization
htop -p $(pgrep goduckbot)

# Watch goroutine count (requires code changes to expose metrics)
# goduckbot does not have built-in pprof endpoints
```

## Troubleshooting

### Cannot connect to node

```bash
# Test connectivity
telnet relays-new.cardano-mainnet.iohk.io 3001

# Try backup relay
# Edit config.yaml: host1: "backbone.cardano.iog.io:3001"
```

### VRF key issues

```bash
# Verify CBOR format
# Key should start with 5840 (CBOR envelope for 64-byte bytestring)
echo "5840<your-key>" | xxd -r -p | xxd | head

# Check secure memory allocation (should see mmap/mlock in strace)
strace -e mmap,mprotect,munmap ./goduckbot 2>&1 | grep -A 2 mmap
```

### Database locked (SQLite)

```bash
# Check for stale locks
fuser goduckbot-dev.db

# Force close
rm goduckbot-dev.db-wal goduckbot-dev.db-shm

# Switch to PostgreSQL if concurrent access needed
```

### Out of memory during full sync

```bash
# Monitor memory
watch -n 1 'ps aux | grep goduckbot'

# Reduce batch size in code (db.go: InsertBlockBatch)
# Or increase available memory

# goduckbot does not expose profiling endpoints by default
# Use Go's built-in tools for goroutine analysis:
#   go build -race  # race detector
#   GODEBUG=schedtrace=1000 ./goduckbot  # scheduler trace
```

## Next Steps

After local dev is working:
1. Use `testing` skill for comprehensive test workflows
2. Use `db-ops` skill for advanced database queries
3. Use `deploy` skill to build and push to production
