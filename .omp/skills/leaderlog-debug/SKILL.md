---
name: leaderlog-debug
description: Debug leader schedule calculations and compare with cncli/Koios. Use when leaderlog doesn't match expected slots or pool performance is off.
---

# Leaderlog Debugging

Troubleshoot CPraos leader schedule calculations.

## Quick Diagnosis

```bash
# Check if schedule exists
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, slot_count, performance, posted FROM leader_schedules WHERE epoch = 617;"

# Get assigned slots
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, slots FROM leader_schedules WHERE epoch = 617;"

# Calculate fresh schedule
kubectl -n cardano exec <pod> -- /app/goduckbot leaderlog 617

# Compare with cncli (if available)
cncli leaderlog --pool-id c825168836c5bf850dec38567eb4771c2e03eea28658ff291df768ae \
  --pool-vrf-skey vrf.skey \
  --byron-genesis mainnet-byron-genesis.json \
  --shelley-genesis mainnet-shelley-genesis.json \
  --ledger-set current \
  --tz America/New_York
```

## CPraos Algorithm Overview

```
For each slot in epoch:
  1. VRF input = BLAKE2b-256(slot || epochNonce)
  2. VRF output = vrf.Prove(vrfSkey, vrfInput)
  3. Leader value = BLAKE2b-256(0x4C || vrfOutput)  # "L" prefix
  4. Threshold = 2^256 * (1 - (1-0.05)^sigma)
  5. Is leader = (leaderValue < threshold)

Where sigma = poolStake / totalStake
```

**Key difference from gouroboros:** Cardano uses CPraos (256-bit) not TPraos (512-bit).

## Debugging Workflows

### 1. Verify Input Data

```bash
# Epoch nonce
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(final_nonce, 'hex') as nonce, source FROM epoch_nonces WHERE epoch = 617;"

# Compare with Koios
curl -s "https://api.koios.rest/api/v1/epoch_params?_epoch_no=617" | jq -r '.[0].nonce'

# Pool stake (from Koios)
curl -s "https://api.koios.rest/api/v1/pool_history?_pool_bech32=pool1eq33dptvch4lstwuwkzt6atuutp3urh7pry7laudal54y2lsw0x&_epoch_no=617" | \
  jq -r '.[0].active_stake'

# Total stake
curl -s "https://api.koios.rest/api/v1/epoch_info?_epoch_no=617" | \
  jq -r '.[0].active_stake'
```

### 2. Verify VRF Key

```bash
# Check VRF key is loaded
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=100 | grep "VRF key"

# Expected output: "VRF key loaded from vrfKeyValue" or "VRF key loaded from vrfKeyPath"

# Verify key format (should be CBOR envelope: 5840 + 64 bytes)
# In code:
echo "5840<your-vrf-key-hex>" | xxd -r -p | xxd
# First bytes should be: 5840 (CBOR bytestring marker for 64 bytes)
```

### 3. Calculate Sigma and Expected Slots

```bash
# Using Python
python3 << EOF
import math

pool_stake = 15618117000000  # lovelace
total_stake = 21895552138000000  # lovelace

sigma = pool_stake / total_stake
ideal_slots = 432000 * 0.05 * sigma  # epoch_length * f * sigma
std_dev = math.sqrt(ideal_slots * (1 - 0.05))

print(f"Sigma: {sigma:.10f}")
print(f"Ideal slots: {ideal_slots:.2f}")
print(f"Std dev: {std_dev:.2f}")
print(f"Expected range: {ideal_slots - 2*std_dev:.0f} - {ideal_slots + 2*std_dev:.0f} (95% confidence)")
EOF
```

### 4. Test Single Slot Calculation

```bash
# Manually verify leader selection for a specific slot
python3 << EOF
import hashlib
from Crypto.Hash import BLAKE2b

slot = 181000000
epoch_nonce = bytes.fromhex("cd1afa6de579dc65c58e3c93ab12dd2f85ecc4bd3a7b1b9d25ca22f87fc472cf")

# VRF input
slot_bytes = slot.to_bytes(8, 'big')
vrf_input = hashlib.blake2b(slot_bytes + epoch_nonce, digest_size=32).digest()
print(f"VRF input: {vrf_input.hex()}")

# Note: Can't compute VRF output without vrf.skey in Python
# (VRF proof requires private key, use Go code instead)
EOF

# Or use Go test:
go test -run TestIsSlotLeaderCpraos -v
```

### 5. Compare with cncli

```bash
# Install cncli
cargo install cncli --locked

# Get Byron and Shelley genesis files
curl -O https://book.world.dev.cardano.org/environments/mainnet/byron-genesis.json
curl -O https://book.world.dev.cardano.org/environments/mainnet/shelley-genesis.json

# Run cncli leaderlog
cncli leaderlog \
  --pool-id c825168836c5bf850dec38567eb4771c2e03eea28658ff291df768ae \
  --pool-vrf-skey vrf.skey \
  --byron-genesis byron-genesis.json \
  --shelley-genesis shelley-genesis.json \
  --ledger-set current \
  --tz America/New_York

# Compare slot lists (should match exactly)
```

## Known Issues

### Stake Data Fallback

goduckbot tries multiple epochs for stake data:
1. Target epoch
2. Current epoch
3. Current - 1
4. Current - 2

If stake data is unavailable for the target epoch (pool not registered yet, or epoch in future), it falls back to recent epochs. This is correct behavior.

```bash
# Check logs for stake fallback
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep "Using stake from epoch"
```

### Slot Count Mismatch

**Expected**: Actual assigned slots should be within 2 standard deviations of ideal slots 95% of the time.

**If way off:**
1. Check nonce (most common issue)
2. Check stake data (wrong epoch?)
3. Check VRF key (corrupted?)
4. Verify network magic (mainnet=764824073, preprod=1, preview=2)

### Performance Calculation

Performance = (actual forged blocks) / (assigned slots) * 100

Requires history classification to compute. See `history-debug` skill.

## Testing

```bash
# Run leaderlog tests
go test -run TestCalculateLeaderSchedule -v
go test -run TestIsSlotLeaderCpraos -v
go test -run TestCalculateThreshold -v

# End-to-end test (epoch 467)
go test -run TestEpoch467 -v

# Recent epoch test (epoch 612)
go test -run TestEpoch612 -v
```

## Validation Test Vectors

### Epoch 444 (Preview)

**Validation**: 35 assigned slots, all 35 forged (100% performance)

```bash
go test -run TestCalculateLeaderScheduleEpoch444 -v  # if test exists
```

### Epoch 612 (Mainnet)

**Validation**: Matches cncli output exactly (35 assigned slots)

```bash
go test -run TestEpoch612Integration -v
```

## CLI Leaderlog Command

```bash
# Single epoch
./goduckbot leaderlog 617

# Epoch range (max 10)
./goduckbot leaderlog 610-619

# Output format:
# ━━━ Epoch 617 ━━━
# Nonce: cd1afa6de579dc65c58e3c93ab12dd2f85ecc4bd3a7b1b9d25ca22f87fc472cf
# Pool Active Stake: 15,618,117 ADA
# Network Active Stake: 21,895,552,138 ADA
# Sigma: 0.0000007134
# Ideal Slots: 15.41
# 
# Assigned Slots (America/New_York):
#   03/10/2026 02:15:33 AM - Slot: 181234567 - B: 1
#   ...
# 
# Total Scheduled Blocks: 16
# Assigned Epoch Performance: 0.00% (0/16 forged)
```

## Database Schema

```sql
CREATE TABLE leader_schedules (
    epoch          INTEGER PRIMARY KEY,
    pool_stake     INTEGER NOT NULL,
    total_stake    INTEGER NOT NULL,
    epoch_nonce    TEXT NOT NULL,
    sigma          REAL,
    ideal_slots    REAL,
    slot_count     INTEGER DEFAULT 0,
    performance    REAL,
    slots          TEXT DEFAULT '[]',  -- JSON array of slot numbers
    posted         INTEGER DEFAULT 0,
    history_classified INTEGER DEFAULT 0,
    calculated_at  TEXT DEFAULT (datetime('now'))
);
```

## Telegram Formatting

goduckbot formats schedules for Telegram with:
- Timezone conversion (configurable: `leaderlog.timezone`)
- 12h/24h time format (configurable: `leaderlog.timeFormat`)
- Number formatting with commas (e.g., "15,618,117")
- Emoji indicators (🦆 for duck pools, obviously)

## Next Steps

- Use `nonce-debug` skill if nonce is incorrect
- Use `chain-status` skill to verify sync state
- Use `history-debug` skill for performance analysis
- Use `verify-nonces` skill for batch validation
