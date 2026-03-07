---
name: perf-profile
description: Performance profiling and optimization for goduckbot. Use when investigating slow sync, high memory usage, or CPU bottlenecks.
---

# Performance Profiling

Diagnose and optimize goduckbot performance.

## Quick Health Check

```bash
# Pod resource usage
kubectl -n cardano top pod -l app.kubernetes.io/name=goduckbot

# Node resource usage
kubectl top node k3s-control-1

# Memory/CPU limits (from values file)
kubectl -n cardano get pod -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].spec.containers[0].resources}'
```

## Built-in Profiling Endpoints

goduckbot exposes pprof endpoints on `:8081`:

```bash
# Port-forward to access locally
kubectl -n cardano port-forward svc/goduckbot 8081:8081

# Or access via pod IP directly (if on same node)
POD_IP=$(kubectl -n cardano get pod -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].status.podIP}')
curl http://$POD_IP:8081/debug/pprof/
```

## CPU Profiling

### 1. Capture CPU Profile

```bash
# 30-second CPU profile
curl -o cpu.prof http://localhost:8081/debug/pprof/profile?seconds=30

# Analyze with pprof
go tool pprof cpu.prof

# pprof commands:
(pprof) top       # top CPU consumers
(pprof) list ProcessBlock  # source code with CPU time
(pprof) web       # visualize (requires graphviz)
(pprof) pdf > cpu.pdf  # export to PDF

# Interactive web UI
go tool pprof -http=:8080 cpu.prof
```

### 2. Common CPU Bottlenecks

**BLAKE2b hashing** (nonce evolution):
```bash
(pprof) top | grep blake2b
# Expected: ~30-40% during historical sync
# High if stalled on single-threaded hashing
```

**VRF proof verification**:
```bash
(pprof) top | grep vrf
# Expected: ~10-15% during leaderlog calculation
```

**Database writes**:
```bash
(pprof) top | grep "Insert\|Upsert"
# Expected: ~10-20% during sync
# High if batch processing not working
```

## Memory Profiling

### 1. Capture Heap Profile

```bash
# Current heap allocation
curl -o heap.prof http://localhost:8081/debug/pprof/heap

# Analyze
go tool pprof heap.prof

# pprof commands:
(pprof) top       # top memory consumers
(pprof) list ProcessBatch  # memory allocations
(pprof) web
(pprof) pdf > heap.pdf

# Interactive
go tool pprof -http=:8080 heap.prof
```

### 2. Alloc vs InUse

```bash
# Total allocated (alloc_space)
go tool pprof -alloc_space heap.prof

# Currently in use (inuse_space)
go tool pprof -inuse_space heap.prof
```

### 3. Common Memory Issues

**Buffered channels not draining**:
```bash
(pprof) list blocksChan
# Check if 10,000-capacity channel is full
```

**Leaking goroutines holding memory**:
```bash
curl http://localhost:8081/debug/pprof/goroutine?debug=1
# Look for goroutines stuck in select{} or channel receive
```

**Large batch allocations**:
```bash
(pprof) top | grep BlockData
# Check InsertBlockBatch slice allocations
```

## Goroutine Profiling

### 1. Check Goroutine Count

```bash
# Current goroutine count
curl -s http://localhost:8081/debug/pprof/goroutine?debug=1 | grep "goroutine profile" | awk '{print $4}'

# Breakdown by state
curl -s http://localhost:8081/debug/pprof/goroutine?debug=1 | grep "^goroutine" | awk '{print $2}' | sort | uniq -c | sort -rn
```

### 2. Find Stuck Goroutines

```bash
# Full goroutine dump
curl http://localhost:8081/debug/pprof/goroutine?debug=2 > goroutines.txt

# Look for patterns
grep -A 10 "chan receive" goroutines.txt
grep -A 10"select" goroutines.txt
grep -A 10 "runtime.gopark" goroutines.txt
```

### 3. Expected Goroutines

| Goroutine | Purpose | Count |
|-----------|---------|-------|
| Main event loop | Adder pipeline | 1 |
| Block processor | Batch drain | 1 |
| WebSocket broadcast | Client handler | 1 |
| HTTP server | Metrics/pprof | ~5 |
| Database pool | pgx connections | ~10 (PostgreSQL) |
| **Total expected** | | **~20-30** |

## Database Performance

### 1. PostgreSQL Connection Pool

```bash
# Check connection pool stats (in Go code)
# Or via pg_stat_activity
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT count(*), state FROM pg_stat_activity WHERE datname = 'goduckbot_v2' GROUP BY state;"

# Expected: mostly idle, few active during sync
```

### 2. Slow Queries

```bash
# Enable slow query log (PostgreSQL)
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -c \
  "ALTER SYSTEM SET log_min_duration_statement = 1000;"  # 1 second

kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -c \
  "SELECT pg_reload_conf();"

# Check logs
kubectl -n postgres logs k3s-postgres-1 | grep "duration:"
```

### 3. Batch Insert Performance

```bash
# Monitor batch insert rate
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot -f | grep "blk/s"

# Expected during historical sync: ~1,500-2,500 blk/s
# Low if: <500 blk/s → database bottleneck
```

## Historical Sync Performance

### Measured Performance (Mainnet)

**Full Shelley-to-tip sync:**
- **Total blocks**: ~8.56M (epochs 208-613)
- **Duration**: ~2 hours
- **Average rate**: ~1,800 blk/s sustained
- **Database size**: ~2 GB (PostgreSQL)
- **Memory usage**: ~128 MB peak
- **CPU usage**: ~400m (0.4 cores) average

### Bottleneck Analysis

**If sync is slow (<1000 blk/s):**

1. **Database writes** (most common):
   ```bash
   # Check pgx pool stats
   kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep "pool.*max\|pool.*idle"
   
   # Check INSERT performance
   kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
     "SELECT schemaname, tablename, n_tup_ins, n_tup_upd FROM pg_stat_user_tables WHERE tablename IN ('blocks', 'epoch_nonces');"
   ```

2. **Network latency** (NtN ChainSync):
   ```bash
   # Check keep-alive timeouts
   kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep "keep-alive timeout"
   
   # Expected: occasional timeout (auto-retry), not constant
   ```

3. **Nonce computation** (CPU-bound):
   ```bash
   # Profile during sync
   curl -o cpu-sync.prof http://localhost:8081/debug/pprof/profile?seconds=60
   go tool pprof cpu-sync.prof
   (pprof) top | grep blake2b
   ```

## Optimization Tips

### 1. Batch Size Tuning

In `main.go` batch processing:
```go
// Current: 1000 blocks or 2-second timeout
case <-time.After(2 * time.Second):
```

**Increase batch size** if database has high latency:
```go
case <-time.After(5 * time.Second):  // larger batches
```

**Decrease timeout** if real-time responsiveness needed:
```go
case <-time.After(500 * time.Millisecond):  // smaller, faster batches
```

### 2. Database Connection Pool

In `db.go`:
```go
// Current: pool size = number of CPU cores * 2
poolConfig.MaxConns = int32(numCPU * 2)
```

**Increase** for high-throughput sync:
```go
poolConfig.MaxConns = 20  // fixed pool size
```

### 3. Channel Buffer Size

In `main.go`:
```go
// Current: 10,000 capacity
blocksChan := make(chan BlockData, 10000)
```

**Increase** if frequently full (check goroutine profile):
```go
blocksChan := make(chan BlockData, 50000)
```

## Memory Leaks

### 1. Check for Leaks

```bash
# Capture heap before and after
curl -o heap-before.prof http://localhost:8081/debug/pprof/heap
sleep 300  # 5 minutes
curl -o heap-after.prof http://localhost:8081/debug/pprof/heap

# Compare
go tool pprof -base heap-before.prof heap-after.prof
(pprof) top  # growth over 5 minutes
```

### 2. Abandoned Pipelines

goduckbot tracks abandoned adder pipelines (when Stop() times out):

```bash
# Check counter
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot | grep "abandoned"

# Expected: 0
# Non-zero: goroutine leak in adder library
```

## Load Testing

### Simulate Heavy Load

```bash
# Start with empty DB, full mode
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c "TRUNCATE blocks, epoch_nonces, leader_schedules, slot_outcomes CASCADE;"

kubectl -n cardano rollout restart deployment/goduckbot

# Monitor resource usage during sync
watch -n 1 'kubectl -n cardano top pod -l app.kubernetes.io/name=goduckbot'
```

## Metrics (Future Enhancement)

goduckbot currently lacks Prometheus metrics. **Recommended additions:**

```go
// In main.go
var (
    blocksProcessed = prometheus.NewCounter(...)
    nonceEvolutions = prometheus.NewCounter(...)
    syncRate = prometheus.NewGauge(...)
    batchDuration = prometheus.NewHistogram(...)
)
```

## Next Steps

- Use `local-dev` skill to test optimizations locally
- Use `chain-status` skill to verify sync health
- Use `db-ops` skill for database performance tuning
