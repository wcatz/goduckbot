---
name: history-debug
description: Debug leaderlog history classification (forged/missed/battle). Use when monitoring history build progress or investigating missed blocks.
---

# History Classification Debugging

Monitor and troubleshoot the complete leaderlog history classification job.

## Overview

History classification builds a complete record of every assigned leader slot from pool registration through current epoch, classifying each as:
- **forged** — block produced and included in canonical chain
- **battle** — slot battle (Δ=0), another pool's block at our assigned slot
- **missed** — no block produced (or height battle Δ>0, indistinguishable from chain data)

## Quick Status Check

```bash
# Check how many epochs are classified
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT COUNT(*) as classified_epochs FROM leader_schedules WHERE history_classified = 1;"

# Check epoch range
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT MIN(epoch) as first, MAX(epoch) as last, COUNT(*) as total 
   FROM leader_schedules 
   WHERE history_classified = 1;"

# Check latest classified epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT MAX(epoch) as last_classified FROM leader_schedules WHERE history_classified = 1;"

# Summary by outcome
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT outcome, COUNT(*) as total 
   FROM slot_outcomes 
   GROUP BY outcome;"
```

## Monitor Progress

```bash
# Watch daemon logs for classification progress
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot -f | grep -i "history\|classif"

# Expected output:
# - "Starting history classification from epoch X"
# - "Epoch X: Y slots assigned, A forged, B battles, C missed"
# - "History classification complete through epoch X"

# Check background job status
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=100 | grep "buildLeaderlogHistory"
```

## Detailed Analysis

### 1. Per-Epoch Breakdown

```bash
# Outcomes per epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, 
          COUNT(*) FILTER (WHERE outcome='forged') as forged,
          COUNT(*) FILTER (WHERE outcome='missed') as missed,
          COUNT(*) FILTER (WHERE outcome='battle') as battle,
          COUNT(*) as total
   FROM slot_outcomes 
   GROUP BY epoch 
   ORDER BY epoch DESC 
   LIMIT 20;"
```

### 2. Find Missed Blocks

```bash
# All missed slots for an epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, slot 
   FROM slot_outcomes 
   WHERE epoch = 612 AND outcome = 'missed'
   ORDER BY slot;"

# Missed slots with timestamps (requires conversion)
kubectl -n cardano exec <pod> -- /app/goduckbot leaderlog 612 | grep "missed"
```

### 3. Find Slot Battles

```bash
# All battles for an epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, slot, opponent 
   FROM slot_outcomes 
   WHERE epoch = 612 AND outcome = 'battle'
   ORDER BY slot;"

# Battle count per opponent
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT opponent, COUNT(*) as battles 
   FROM slot_outcomes 
   WHERE outcome = 'battle' 
   GROUP BY opponent 
   ORDER BY battles DESC 
   LIMIT 10;"
```

### 4. Lifetime Performance

```bash
# Total stats
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT 
     COUNT(*) as total_slots,
     COUNT(*) FILTER (WHERE outcome='forged') as forged,
     COUNT(*) FILTER (WHERE outcome='missed') as missed,
     COUNT(*) FILTER (WHERE outcome='battle') as battles,
     ROUND(100.0 * COUNT(*) FILTER (WHERE outcome='forged') / NULLIF(COUNT(*), 0), 2) as forge_rate
   FROM slot_outcomes;"
```

## CLI History Command

```bash
# Build complete history (daemon mode runs this automatically after nonce backfill)
./goduckbot history

# Force re-classification of already-processed epochs
./goduckbot history --force

# Start from specific epoch (overrides auto-detect)
./goduckbot history --from 365  # Babbage start

# Output:
# - Estimated time (in daemon: ~75s/epoch, in CLI: ~5s/epoch)
# - Progress per epoch
# - Final summary
```

## Background Job Architecture

History classification runs as a background goroutine after nonce backfill in full mode:

```go
// In main.go
go func() {
    time.Sleep(10 * time.Second)  // let nonce backfill start first
    if err := globalIndexer.buildLeaderlogHistory(ctx); err != nil {
        log.Printf("History classification failed: %v", err)
    }
}()
```

**Key features:**
- **Resumable**: checks `IsEpochClassified` per epoch, skips already-done work
- **CPraos only**: starts from Babbage era (mainnet epoch 365, preprod epoch 12)
- **Own context**: 12-hour timeout (daemon), independent from nonce backfill
- **Rate**: ~75s/epoch in daemon (Koios rate limiting), ~5s/epoch in CLI

## Data Sources

### Forged Slots (Koios)

```bash
# Direct Koios query for pool-specific forged slots
curl -s "https://koios.tosidrop.me/api/v1/pool_blocks?_pool_bech32=pool1eq33dptvch4lstwuwkzt6atuutp3urh7pry7laudal54y2lsw0x&_epoch_no=612" | \
  jq -r '.[] | .abs_slot'

# Returns array of slots where pool forged blocks
```

### Slot Battles (Local DB)

```bash
# Check if a block exists at a slot (any pool)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT EXISTS(SELECT 1 FROM blocks WHERE slot = 181234567);"

# If TRUE and slot is assigned to us but not in Koios pool_blocks → slot battle
```

## Classification Limitations

### Height Battles (Δ>0)

**Cannot detect from chain data alone.**

Height battles occur when our block was orphaned by a longer chain at a different slot. These appear as "missed" in classification. Only detectable via external orphan data (AdaStat, Pooltool).

**Example** (OTG lifetime stats over 251 epochs):
- 4,454 forged
- 115 slot battles (Δ=0) ✓ correctly classified
- 19 height battles (Δ>0) ✗ appear as "missed"
- 35 truly missed
- 96.34% forge rate

### Data Availability

- **Koios API**: Only has recent data (typically last ~2 years)
- **Historical epochs**: May not have complete pool_blocks data
- **Solution**: Best-effort classification, mark unavailable epochs as skipped

## Troubleshooting

### Classification Stalled

```bash
# Check if background job is running
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=200 | grep "history"

# Look for errors
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep -i "error.*history"

# Check Koios rate limiting
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep "429"

# Restart if stuck
kubectl -n cardano rollout restart deployment/goduckbot
```

### Koios API Errors

```bash
# Check tosidrop Koios status
curl -s https://koios.tosidrop.me/api/v1/tip | jq

# Switch to official Koios (change koiosRestBase in code)
# Or add retry logic with longer backoff
```

### Missing Classifications

```bash
# Find epochs with schedules but no classification
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT ls.epoch 
   FROM leader_schedules ls 
   LEFT JOIN slot_outcomes so ON ls.epoch = so.epoch 
   WHERE ls.history_classified = 0 AND so.epoch IS NULL 
   ORDER BY ls.epoch;"

# Re-run classification for specific epoch range
./goduckbot history --from 365 --force
```

## Performance Calculation

```bash
# Per-epoch performance
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT ls.epoch, ls.slot_count as assigned,
          COUNT(*) FILTER (WHERE so.outcome='forged') as forged,
          ROUND(100.0 * COUNT(*) FILTER (WHERE so.outcome='forged') / NULLIF(ls.slot_count, 0), 2) as performance
   FROM leader_schedules ls
   LEFT JOIN slot_outcomes so ON ls.epoch = so.epoch
   WHERE ls.history_classified = 1
   GROUP BY ls.epoch, ls.slot_count
   ORDER BY ls.epoch DESC
   LIMIT 20;"

# Update performance field in leader_schedules
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "UPDATE leader_schedules ls
   SET performance = (
     SELECT ROUND(100.0 * COUNT(*) FILTER (WHERE outcome='forged') / NULLIF(ls.slot_count, 0), 2)
     FROM slot_outcomes so
     WHERE so.epoch = ls.epoch
   )
   WHERE history_classified = 1;"
```

## Database Schema

```sql
CREATE TABLE slot_outcomes (
    epoch    INTEGER NOT NULL,
    slot     INTEGER NOT NULL,
    outcome  TEXT NOT NULL,      -- "forged", "battle", "missed"
    opponent TEXT,               -- bech32 pool ID (battle only)
    PRIMARY KEY (epoch, slot)
);

-- leader_schedules.history_classified flag
ALTER TABLE leader_schedules ADD COLUMN history_classified INTEGER DEFAULT 0;
```

## Cleanup Old Data

```bash
# Delete outcomes before a certain epoch (after archiving)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "DELETE FROM slot_outcomes WHERE epoch < 365;"

# Returns count of deleted rows
```

## Next Steps

- Use `nonce-debug` skill if nonces are incorrect
- Use `leaderlog-debug` skill if schedules don't match expectations
- Use `chain-status` skill to verify sync completeness
- Use `db-ops` skill for advanced database queries
