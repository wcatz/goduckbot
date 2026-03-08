---
name: db-ops
description: Goduckbot database operations - query, inspect, or wipe tables. Use for database management tasks.
argument-hint: [operation]
---

# Database Operations

Query, inspect, and manage goduckbot database tables.

> **PostgreSQL cluster:** `k3s-postgres-rw.postgres.svc.cluster.local` (namespace: `postgres`)
> **Database name:** `goduckbot_v2`
> **User:** `postgres` (for admin), `goduckbot` (for app)
> **Pod for psql:** `k3s-postgres-1`

## 1. Connect to database

```bash
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2
```

## 2. Table overview

### blocks
Per-block VRF data. Primary key: `slot`.
```sql
SELECT MIN(slot) as first_slot, MAX(slot) as last_slot, COUNT(*) as total_blocks FROM blocks;
SELECT COUNT(*) as blocks, epoch FROM blocks GROUP BY epoch ORDER BY epoch DESC LIMIT 10;
```

### epoch_nonces
Evolving, candidate, and final nonces per epoch. Primary key: `epoch`.
```sql
SELECT epoch, encode(final_nonce, 'hex') as nonce, source,
       evolving_nonce IS NOT NULL as has_evolving,
       candidate_nonce IS NOT NULL as has_candidate
FROM epoch_nonces ORDER BY epoch DESC LIMIT 10;
```

### leader_schedules
Calculated leader schedules with slots JSON. Primary key: `epoch`.
```sql
SELECT epoch, posted, history_classified,
       json_array_length(slots::json) as slot_count
FROM leader_schedules ORDER BY epoch DESC LIMIT 10;
```

### slot_outcomes
Per-slot classification. Primary key: `(epoch, slot)`.
```sql
SELECT epoch,
       COUNT(*) FILTER (WHERE outcome='forged') as forged,
       COUNT(*) FILTER (WHERE outcome='missed') as missed,
       COUNT(*) FILTER (WHERE outcome='battle') as battle
FROM slot_outcomes GROUP BY epoch ORDER BY epoch DESC LIMIT 10;
```

## 3. Common queries

### Check nonce completeness
```sql
SELECT epoch, final_nonce IS NOT NULL as has_final, source
FROM epoch_nonces ORDER BY epoch DESC LIMIT 20;
```

### Find gaps in block data
```sql
SELECT epoch, COUNT(*) as blocks FROM blocks
GROUP BY epoch ORDER BY epoch
HAVING COUNT(*) < 100;  -- suspiciously low block count
```

### Check specific epoch nonce
```sql
SELECT epoch, encode(final_nonce, 'hex') as final,
       encode(candidate_nonce, 'hex') as candidate,
       encode(evolving_nonce, 'hex') as evolving,
       block_count, source
FROM epoch_nonces WHERE epoch = 530;
```

### Leaderlog for specific epoch
```sql
SELECT epoch, slots, posted, history_classified
FROM leader_schedules WHERE epoch = 530;
```

## 4. Dangerous operations

### Wipe all tables (full resync required after)
```sql
TRUNCATE blocks, epoch_nonces, leader_schedules, slot_outcomes;
```

### Delete slot outcomes for re-classification
```sql
DELETE FROM slot_outcomes WHERE epoch >= 365;
UPDATE leader_schedules SET history_classified = false WHERE epoch >= 365;
```

### Reset nonces for re-computation
```sql
DELETE FROM epoch_nonces WHERE epoch >= 208;
```

> **WARNING:** After any destructive operation, the bot must be restarted in full mode to rebuild data, or nonces must be backfilled from Koios.
