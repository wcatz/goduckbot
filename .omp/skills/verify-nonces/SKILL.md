---
name: verify-nonces
description: Verify goduckbot computed epoch nonces against Koios API. Use when checking nonce correctness after a chain sync or nonce fix.
argument-hint: [epoch-range]
---

# Verify Nonces Against Koios

Batch-verify locally computed epoch nonces against the Koios API.

> **Run this after:** any nonce-related code change, chain resync, or DB repair.
> **Koios API:** `https://api.koios.rest/api/v1/epoch_params`

## 1. Get local nonces from DB

```bash
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(final_nonce, 'hex') as nonce, source FROM epoch_nonces WHERE final_nonce IS NOT NULL ORDER BY epoch DESC LIMIT 20;"
```

## 2. Get Koios nonce for specific epoch

```bash
curl -s "https://api.koios.rest/api/v1/epoch_params?_epoch_no=530" | jq '.[0].nonce'
```


## 3. Batch verification script

Compare a range of epochs. Run this from a machine with curl and psql access:

```bash
# Get local nonces
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -t -A -c \
  "SELECT epoch || ',' || encode(final_nonce, 'hex') FROM epoch_nonces WHERE final_nonce IS NOT NULL AND epoch >= 500 ORDER BY epoch;"

# For each epoch, compare with Koios
for EPOCH in 500 501 502 503 504 505; do
  LOCAL=$(kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -t -A -c \
    "SELECT encode(final_nonce, 'hex') FROM epoch_nonces WHERE epoch = $EPOCH;")
  KOIOS=$(curl -s "https://api.koios.rest/api/v1/epoch_params?_epoch_no=$EPOCH" | jq -r '.[0].nonce')
  if [ "$LOCAL" = "$KOIOS" ]; then
    echo "epoch $EPOCH: MATCH"
  else
    echo "epoch $EPOCH: MISMATCH local=$LOCAL koios=$KOIOS"
  fi
  sleep 1  # rate limit
done
```

## 4. Interpreting results

| Result | Meaning | Action |
|---|---|---|
| All MATCH | Nonces are correct | No action needed |
| MISMATCH at specific epoch | Nonce evolution diverged | Check blocks table for that epoch, verify VRF outputs |
| MISMATCH from epoch N onward | Cumulative error from epoch N | Nonce evolution is rolling - one bad block corrupts all subsequent epochs |
| Local is NULL | Nonce not computed yet | Check if sync has reached that epoch |

## 5. Debugging a mismatch

If a specific epoch doesn't match:

```sql
-- Check block count for that epoch
SELECT COUNT(*) FROM blocks WHERE epoch = <N>;

-- Check evolving nonce state
SELECT epoch, encode(evolving_nonce, 'hex'), block_count, source
FROM epoch_nonces WHERE epoch IN (<N-1>, <N>, <N+1>);

-- Check candidate nonce (freezes at 60% epoch progress)
SELECT epoch, encode(candidate_nonce, 'hex') FROM epoch_nonces WHERE epoch = <N>;

-- Check last block hash of previous epoch (for TICKN)
SELECT block_hash FROM blocks WHERE epoch = <N-1> ORDER BY slot DESC LIMIT 1;
```

See `nonce-debug` skill for deeper investigation.

## 6. Known caveats

- **Koios rate limits:** api.koios.rest limits to ~100 req/min. Add `sleep 1` between calls for bulk verification.
- **Rolling accumulation:** Nonce evolution is cumulative. A single wrong VRF output corrupts ALL subsequent epoch nonces. If you find a mismatch, check the earliest mismatched epoch first.
- **Source column:** `tickn` = computed locally via TICKN rule, `koios` = fetched from Koios as fallback. TICKN-sourced nonces that match Koios confirm the entire local chain is correct.
- **Lite mode:** Only has Koios-sourced nonces. Verification is trivial (always matches). Full mode is where TICKN verification matters.
