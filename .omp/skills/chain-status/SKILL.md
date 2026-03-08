---
name: chain-status
description: Check Cardano chain sync status across nodes, ogmios, and goduckbot. Use when checking if things are synced and healthy.
argument-hint: [node-name]
---

# Chain Sync Status

Check sync status across the Cardano infrastructure stack.

> **Production namespace:** `cardano` (NOT `duckbot`)
> **PostgreSQL namespace:** `postgres`

## 1. Goduckbot pod status

```bash
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o wide
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot-test -o wide
```

## 2. Goduckbot logs (recent activity)

```bash
# Production
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=30

# Test instance
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot-test --tail=30
```

Look for:
- `[MINTED]` — pool forged a block (healthy)
- `keep-alive timeout` — normal during full-mode sync, auto-retries
- `connection refused` — cardano-node is down
- `intersect not found` — DB/chain mismatch, may need integrity check
- `VRF key loaded` — healthy startup

## 3. Cardano node sync status

```bash
# Check node pods
kubectl -n cardano get pods -l app=cardano-node -o wide

# Node tip via ogmios (if available)
kubectl -n cardano exec deploy/ogmios -- curl -s http://localhost:1337/health | jq '.lastKnownTip'
```

## 4. Database sync position

```bash
# Last synced slot in goduckbot DB
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT MAX(slot) as last_slot, MAX(epoch) as last_epoch FROM blocks;"
```

Compare with chain tip to determine sync lag. Mainnet produces ~1 block every 20 seconds (1 slot/second, ~5% active slot coefficient).

## 5. Mode check

- **Lite mode**: Only sees blocks from first launch. No historical data. Koios fallback for nonces.
- **Full mode**: Historical sync from Shelley genesis. Check sync progress in logs for `historical sync` messages with slot numbers.

## 6. Node addresses

Current K8s config:
- **NtN (chain sync):** `cardano-node-mainnet-az1.cardano.svc.cluster.local:3001`
- **NtC (stake queries):** `cardano-node-mainnet-az1.cardano.svc.cluster.local:30000`

## 7. Common issues

| Symptom | Cause | Fix |
|---|---|---|
| No blocks logging | Wrong mode or node unreachable | Check logs for connection errors |
| `keep-alive timeout` loops | Normal in full-mode historical sync | Retries automatically at ~1800 blk/s |
| Stuck at specific slot | DB integrity issue | Check `integrity.go` startup logs |
| NtC timeout | Known issue with stake queries | Koios fallback is working, no action needed |
| Pod CrashLoopBackOff | Config error or DB connection failure | Check pod events and startup logs |
