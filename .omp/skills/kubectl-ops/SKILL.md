---
name: kubectl-ops
description: Common kubectl operations for Cardano K8s cluster debugging and monitoring. Use when checking pod status, reading logs, querying the database, or running CLI commands in pods.
argument-hint: [operation]
---

# Kubectl Operations

Common kubectl patterns for the Cardano K8s cluster.

> **Goduckbot namespace:** `cardano`
> **PostgreSQL namespace:** `postgres`
> **Node namespace:** `cardano`

## 1. Pod status

```bash
# Goduckbot pods
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o wide
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot-test -o wide

# All cardano namespace pods
kubectl -n cardano get pods -o wide

# Resource usage
kubectl -n cardano top pods
kubectl top nodes
```

## 2. Logs

```bash
# Recent logs
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=30

# Time-based logs
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --since=30m

# Test instance
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot-test --tail=30

# Follow live
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot -f
```

## 3. CLI commands in pod

```bash
# Get pod name first
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot-test -o name

# Run leaderlog for specific epoch
kubectl -n cardano exec <pod-name> -- /app/goduckbot leaderlog <epoch>

# Run leaderlog for epoch range
kubectl -n cardano exec <pod-name> -- /app/goduckbot leaderlog <start>-<end>

# Check nonce
kubectl -n cardano exec <pod-name> -- /app/goduckbot nonce <epoch>

# Version
kubectl -n cardano exec <pod-name> -- /app/goduckbot version

# History classification
kubectl -n cardano exec <pod-name> -- /app/goduckbot history
kubectl -n cardano exec <pod-name> -- /app/goduckbot history --force
kubectl -n cardano exec <pod-name> -- /app/goduckbot history --from 365
```

## 4. Database queries from kubectl

```bash
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c "<SQL>"
```

See `db-ops` skill for common queries.

## 5. Pod events and debugging

```bash
# Recent events
kubectl -n cardano describe pod <pod-name>

# Previous container logs (after crash)
kubectl -n cardano logs <pod-name> --previous

# Pod restart count
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'
```

## 6. Deployment management

```bash
# Current image version
kubectl -n cardano get pods -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].spec.containers[0].image}'

# Rollout status
kubectl -n cardano rollout status deployment/goduckbot

# Restart pod (triggers new pull if imagePullPolicy=Always)
kubectl -n cardano rollout restart deployment/goduckbot
```

> **NEVER deploy from here.** Use the `deploy` skill and helmfile from the infra repo. These commands are for debugging only.
