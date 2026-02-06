# goduckbot - Claude Code Context

Cardano stake pool notification bot with chain sync via adder.

## Current Features
- Real-time block notifications via Telegram/Twitter
- Chain sync using blinklabs-io/adder
- Epoch/block tracking
- Koios API integration
- WebSocket broadcast

## Leaderlog Integration (TODO)

### Goal
Calculate and post leader schedule to Telegram, eliminating need for separate cncli container.

### CPRAOS Algorithm (Verified)
The VRF calculation has been validated against cncli - see `/home/wayne/git/infra/research/cardano-vrf-go/`.

**Key insight:** gouroboros `consensus.IsSlotLeader()` uses TPraos (512-bit) but Cardano uses CPRAOS:
```go
// 1. VRF input
vrfInput := blake2b256(slot || epochNonce)

// 2. VRF prove
_, vrfOutput, _ := vrf.Prove(vrfSkey[:32], vrfInput)

// 3. Leader value (CPRAOS specific!)
leaderValue := blake2b256([]byte{0x4C} || vrfOutput)  // "L" prefix

// 4. Threshold (256-bit, not 512-bit)
sigma := float64(poolStake) / float64(totalStake)
phi := 1.0 - math.Pow(1.0-0.05, sigma)
threshold := 2^256 * phi

// 5. Check
isLeader := leaderValueAsInt < threshold
```

### Data Sources

| Data | Source | Storage |
|------|--------|---------|
| VRF signing key | Mounted secret `/keys/vrf.skey` | Parse CBOR at startup |
| Epoch nonce | Koios API `epoch_params` | Cache per epoch |
| Pool stake | Koios API `pool_info` | Query at 65% epoch |
| Total stake | Koios API `epoch_info` | Query at 65% epoch |
| Genesis params | Config or `/opt/cardano/config/{network}/shelley-genesis.json` | Static |

### Epoch Nonce from Koios

```go
// GET https://api.koios.rest/api/v1/epoch_params?_epoch_no=611
type EpochParams struct {
    Nonce string `json:"nonce"`  // 64-char hex
    // ...
}
```

### Stake from Koios

```go
// POST https://api.koios.rest/api/v1/pool_info
// Body: {"_pool_bech32_ids": ["pool1..."]}
type PoolInfo struct {
    ActiveStake string `json:"active_stake"`  // pool stake in lovelace
}

// GET https://api.koios.rest/api/v1/epoch_info?_epoch_no=611
type EpochInfo struct {
    ActiveStake string `json:"active_stake"`  // network total stake
}
```

### New Fields for Indexer

```go
type Indexer struct {
    // ... existing fields ...

    // Leaderlog
    vrfSkey         []byte            // 64-byte VRF key
    expectedSlots   map[uint64]int    // slot -> block number in epoch
    epochNonces     map[int][]byte    // epoch -> 32-byte nonce
    leaderlogMu     sync.RWMutex
    leaderlogPosted map[int]bool      // epochs already posted
}
```

### Trigger: 65% Epoch Progress

```go
func (i *Indexer) checkLeaderlogTrigger() {
    progress := i.getEpochProgress()
    nextEpoch := i.epoch + 1

    if progress >= 65.0 && !i.leaderlogPosted[nextEpoch] {
        go i.calculateAndPostLeaderlog(nextEpoch)
    }
}
```

### Telegram Message Format

```
🦆 DUCKPOOL Leader Schedule

Epoch: 612
Pool Stake: 15,221,834 ADA
Network Stake: 21,570,932,611 ADA

Assigned Slots: 14
Expected: 15.24
Performance: 91.87%

Schedule (UTC):
  02/09 03:15:42 - Slot 179012345
  02/09 08:22:17 - Slot 179030567
  ...

Quack! 🦆
```

### Config Additions

```yaml
leaderlog:
  enabled: true
  vrfKeyPath: "/keys/vrf.skey"
  postToTelegram: true
  postToTwitter: false  # Twitter has char limit
  timezone: "America/New_York"
```

### Dependencies to Add

```go
import (
    "github.com/blinklabs-io/gouroboros/vrf"
    "golang.org/x/crypto/blake2b"
)
```

### Files to Modify

| File | Changes |
|------|---------|
| `main.go` | Add leaderlog calculation, nonce caching, Telegram posting |
| `config.yaml.example` | Add leaderlog config section |
| `go.mod` | Add gouroboros/vrf, blake2b dependencies |
| `helm-chart/values.yaml` | Add VRF key secret mount |
| `helm-chart/templates/deployment.yaml` | Mount VRF key, add env vars |

### Reference Implementation

See `/home/wayne/git/infra/research/cardano-vrf-go/prototype/main.go` for validated CPRAOS algorithm.

## Existing Code Reference

### Epoch Calculation (lines 89-104)
```go
func getCurrentEpoch() int {
    shelleyStartTime, _ := time.Parse(time.RFC3339, ShelleyEpochStart)
    elapsedSeconds := time.Since(shelleyStartTime).Seconds()
    epochsElapsed := int(elapsedSeconds / (EpochDurationInDays * SecondsInDay))
    return StartingEpoch + epochsElapsed
}
```

### Koios Client Setup (lines 174-199)
Already initializes Koios client based on `networkMagic`.

### Telegram Notifications (lines 243-246)
```go
i.bot.Send(&telebot.Chat{ID: channelID}, message)
```

## Build & Deploy

```bash
# Build
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .

# Docker
docker build -t wcatz/goduckbot:latest .
docker push wcatz/goduckbot:latest

# Deploy via helmfile
cd /home/wayne/git/infra/helmfile-app
helmfile -e apps -l app=duckbot apply
```
