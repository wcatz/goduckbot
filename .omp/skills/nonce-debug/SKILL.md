---
name: nonce-debug
description: Debug epoch nonce calculations and verify against Koios API. Use when nonces don't match expected values or leaderlog calculation fails.
---

# Nonce Debugging

Troubleshoot and verify epoch nonce calculations.

## Quick Diagnosis

```bash
# Check if nonce exists for an epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, final_nonce IS NOT NULL as has_final, source FROM epoch_nonces WHERE epoch = 617;"

# Get nonce hex value
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(final_nonce, 'hex') as nonce_hex, source FROM epoch_nonces WHERE epoch = 617;"

# Compare with Koios
./goduckbot nonce 617  # if running locally
# OR
kubectl -n cardano exec <pod> -- /app/goduckbot nonce 617
```

## Nonce Evolution Fundamentals

### Evolution Formula

**Per block:**
```
vrfNonceValue = BLAKE2b-256(vrfOutput)  # Shelley-Alonzo
              = BLAKE2b-256(BLAKE2b-256(0x4E || vrfOutput))  # Babbage+ (domain-separated)

eta_v = BLAKE2b-256(eta_v || vrfNonceValue)
```

**Epoch boundary (TICKN rule):**
```
epochNonce(N+1) = BLAKE2b-256(η_c(N) || η_ph(N-1))

Where:
  η_c(N)    = candidate nonce (frozen at 60% of epoch N)
  η_ph(N-1) = block hash of last block in epoch N-1
```

### Candidate Nonce Freeze

Stability window = 3k/f slots, where k=2160 (security parameter) and f=0.05 (active slot coefficient).

| Network | Epoch Length | Stability Window | Freeze Slot (relative) |
|---------|--------------|------------------|------------------------|
| Mainnet | 432,000 | 259,200 (60%) | 259,200 |
| Preview | 86,400 | 51,840 (60%) | 51,840 |

## Debugging Workflows

### 1. Verify Nonce Against Koios

```bash
# Get from DB
EPOCH=617
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(final_nonce, 'hex') as db_nonce FROM epoch_nonces WHERE epoch = $EPOCH;"

# Get from Koios
curl -s "https://api.koios.rest/api/v1/epoch_params?_epoch_no=$EPOCH" | \
  jq -r '.[0].nonce'

# Compare (should match exactly)
```

Use the `verify-nonces` skill for automated batch verification.

### 2. Check Nonce Evolution State

```bash
# Current epoch evolving nonce
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(evolving_nonce, 'hex') as evolving, block_count, 
          candidate_nonce IS NOT NULL as has_candidate,
          final_nonce IS NOT NULL as has_final
   FROM epoch_nonces 
   WHERE epoch = (SELECT MAX(epoch) FROM blocks);"

# Check if candidate froze
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, encode(candidate_nonce, 'hex') as candidate_hex 
   FROM epoch_nonces 
   WHERE epoch = 617 AND candidate_nonce IS NOT NULL;"
```

### 3. Trace Nonce Evolution for an Epoch

```bash
# Get all nonce values for an epoch (each block's contribution)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT slot, epoch, encode(nonce_value, 'hex') as contribution 
   FROM blocks 
   WHERE epoch = 617 
   ORDER BY slot 
   LIMIT 20;"

# Count blocks that contributed
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, COUNT(*) as blocks_with_nonce 
   FROM blocks 
   WHERE epoch = 617 
   GROUP BY epoch;"

# Compare with expected block count for epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT block_count FROM epoch_nonces WHERE epoch = 617;"
```

### 4. Verify TICKN Computation

```bash
# Get candidate nonce from epoch N
EPOCH=616
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT encode(candidate_nonce, 'hex') FROM epoch_nonces WHERE epoch = $EPOCH;"

# Get last block hash from epoch N-1
PREV_EPOCH=$((EPOCH - 1))
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT block_hash FROM blocks WHERE epoch = $PREV_EPOCH ORDER BY slot DESC LIMIT 1;"

# Compute TICKN manually (Python)
python3 << EOF
import hashlib
candidate_hex = "<candidate_nonce_from_above>"
prev_hash_hex = "<block_hash_from_above>"

candidate = bytes.fromhex(candidate_hex)
prev_hash = bytes.fromhex(prev_hash_hex)

tickn = hashlib.blake2b(candidate + prev_hash, digest_size=32).hexdigest()
print(f"TICKN nonce for epoch {$EPOCH + 1}: {tickn}")
EOF

# Compare with stored final nonce for epoch N+1
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT encode(final_nonce, 'hex') FROM epoch_nonces WHERE epoch = $((EPOCH + 1));"
```

### 5. Check for Nonce Corruption

```bash
# Look for gaps in block sequence
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, MIN(slot) as first_slot, MAX(slot) as last_slot, COUNT(*) as block_count
   FROM blocks
   WHERE epoch BETWEEN 615 AND 617
   GROUP BY epoch
   ORDER BY epoch;"

# Look for duplicate blocks (should be none)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT slot, COUNT(*) FROM blocks GROUP BY slot HAVING COUNT(*) > 1;"

# Check for NULL nonce values (should be none)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT COUNT(*) FROM blocks WHERE nonce_value IS NULL;"
```

## Known Issues

### Epoch 259 (Extra Entropy)

Mainnet epoch 259 had `extra_entropy` protocol parameter set. Our implementation doesn't track this, so we use a known-good override:

```go
// In nonce.go
var knownEpochNonces = map[int]string{
    259: "0022cfa563a5328c4fb5c8017121329e964c26ade5d167b1bd9b2ec967772b60",
}
```

### Babbage Transition (Epoch 365 Mainnet, 12 Preprod)

Babbage introduced domain-separated VRF nonces (0x4E prefix). Check era transition logic:

```bash
# Verify Babbage start epoch constants
grep -n "BabbageStartEpoch" leaderlog.go

# Check blocks around transition
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT epoch, slot, length(vrf_output), length(nonce_value) 
   FROM blocks 
   WHERE epoch IN (364, 365, 366) 
   ORDER BY slot 
   LIMIT 30;"
```

## Repair Workflows

### Recalculate Nonce from Scratch

```bash
# Clear stored nonces for an epoch
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "UPDATE epoch_nonces SET evolving_nonce = NULL, candidate_nonce = NULL, final_nonce = NULL WHERE epoch = 617;"

# Restart goduckbot (will recompute from block data)
kubectl -n cardano rollout restart deployment/goduckbot

# Watch logs for nonce evolution
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot -f | grep -i nonce
```

### Backfill Missing Nonces

```bash
# Run nonce backfill from CLI (full mode only)
kubectl -n cardano exec <pod> -- /app/goduckbot history --from 208

# Or use the NonceTracker.BackfillNonces method in code
# (automatically runs on startup in full mode)
```

## Testing Nonce Calculations

```bash
# Run nonce-specific tests
go test -run TestVrfNonceValue -v
go test -run TestEvolveNonce -v
go test -run TestInitialNonce -v

# Test against Koios (integration test, slow)
go test -run TestKoiosEpochNonceVerification -v

# Test TICKN computation
go test -run TestComputeEpochNonce -v  # if test exists
```

## CLI Nonce Command

```bash
# Show nonce for an epoch
./goduckbot nonce 617

# Output includes:
# - Final nonce (hex)
# - Source (chain_sync, koios, backfill)
# - Candidate nonce (if available)
# - Evolving nonce state
```

## Reference: Nonce Types

| Nonce Type | Purpose | When Set | Used For |
|------------|---------|----------|----------|
| `evolving_nonce` | Rolling eta_v | Every block | Interim state |
| `candidate_nonce` | Frozen eta_c | At 60% epoch | TICKN computation |
| `final_nonce` | Epoch boundary nonce | End of epoch | Leader election |

## Next Steps

- Use `verify-nonces` skill for automated batch verification
- Use `leaderlog-debug` skill if nonce is correct but leaderlog fails
- Use `chain-status` skill to verify sync state
