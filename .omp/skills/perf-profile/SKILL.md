---
name: perf-profile
description: Performance monitoring and optimization for goduckbot. Use when investigating slow sync, high memory usage, or resource bottlenecks.
---

# Performance Profiling

Monitor and optimize goduckbot performance during chain sync and operations.

## Quick Health Check

```bash
# Pod resource usage
kubectl -n cardano top pod -l app.kubernetes.io/name=goduckbot

# Node resource usage
kubectl top node k3s-control-1

# Memory/CPU limits (from values file)
kubectl -n cardano get pod -l app.kubernetes.io/name=goduckbot -o jsonpath='{.items[0].spec.containers[0].resources}'

# Current memory usage
kubectl -n cardano exec -it <pod> -- ps aux | grep goduckbot
```

## Built-in Monitoring

goduckbot exposes basic HTTP endpoints on `:8081`:

```bash
# WebSocket endpoint for real-time block events
kubectl -n cardano port-forward <pod> 8081:8081

# Test WebSocket connection
websocat ws://localhost:8081/ws
```

**Note**: goduckbot does NOT have pprof endpoints. For detailed profiling, use Go's built-in race detector, memory profiler, or external tools like Delve.

## Database Performance

### 1. PostgreSQL Connection Pool

```bash
# Check connection pool stats via pg_stat_activity
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
# Monitor batch insert rate from logs
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot -f | grep "blk/s"

# Expected during historical sync: ~1,500-2,500 blk/s
# Low if: <500 blk/s → database bottleneck
```

## Historical Sync Performance

### Projected Performance (Based on Partial Sync)

**Extrapolated from observed metrics (epochs 208-250):**
- **Estimated total blocks**: ~8.5M (Shelley to current tip)
- **Projected duration**: ~2-3 hours for full sync
- **Observed average rate**: ~1,800 blk/s sustained
- **Expected database size**: ~2 GB (PostgreSQL after full sync)
- **Memory usage**: ~128 MB peak
- **CPU usage**: ~400m (0.4 cores) average

**Note**: These are estimates based on partial sync data, not complete measurements.

### Current Sync Progress

```bash
# Check database state
kubectl -n postgres exec k3s-postgres-1 -- psql -U postgres -d goduckbot_v2 -c \
  "SELECT MIN(epoch) as first, MAX(epoch) as last, COUNT(DISTINCT epoch) as epochs, COUNT(*) as total_blocks FROM blocks;"

# Check current slot/epoch
kubectl -n cardano logs -l app.kubernetes.io/name=goduckbot --tail=10 | grep "slot\|epoch"
```

### Bottleneck Analysis

**If sync is slow (<1000 blk/s):**

1. **Database writes** (most common):
   ```bash
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

3. **Memory pressure**:
   ```bash
   # Check OOM kills
   kubectl -n cardano get events | grep OOM
   
   # Check swap usage on node
   kubectl top node k3s-control-1
   ```

## Optimization Tips

### 1. Batch Processing Tuning

In `main.go`, the batch processor drains the channel:

```go
// Current: 2000 blocks or 2-second timeout
const maxBatchSize = 2000
case <-time.After(2 * time.Second):
```

**Increase batch size** if database has high latency:
```go
const maxBatchSize = 5000  // larger batches, fewer DB round-trips
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

**Note**: More connections = more memory usage. Monitor with `pg_stat_activity`.

### 3. Channel Buffer Size

In `main.go`:
```go
// Current: 10,000 capacity
blocksChan := make(chan BlockData, 10000)
```

**Increase** if channel is frequently full (check logs for blocking):
```go
blocksChan := make(chan BlockData, 50000)
```

**Warning**: Larger buffer = more memory usage (~1 KB per BlockData entry).

## Resource Limits

### Kubernetes Resource Configuration

From `values.yaml`:
```yaml
resources:
  limits:
    cpu: 500m
    memory: 128Mi
  requests:
    cpu: 100m
    memory: 64Mi
```

**Increase if needed**:
```yaml
resources:
  limits:
    cpu: 1000m      # 1 core
    memory: 256Mi   # more headroom
  requests:
    cpu: 200m
    memory: 128Mi
```

### Monitor Resource Usage

```bash
# Real-time pod metrics
kubectl -n cardano top pod -l app.kubernetes.io/name=goduckbot --containers

# Check for throttling (CPU) or OOM (memory)
kubectl -n cardano describe pod <pod> | grep -A 10 "State:\|Last State:"

# Check resource utilization percentage
kubectl -n cardano get pod <pod> -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{.resources}{"\n"}{end}'
```

## Common Performance Issues

### Slow Historical Sync

**Symptoms**: <500 blk/s, logs show slow batch processing

**Check**:
1. Database disk I/O (PostgreSQL on NVMe vs HDD)
2. Network latency to cardano-node
3. Memory limits causing swapping

**Fix**:
- Increase batch size (reduce DB round-trips)
- Increase resource limits
- Optimize PostgreSQL config (work_mem, shared_buffers)

### High Memory Usage

**Symptoms**: Pod restarting with OOM, memory usage >128 MB

**Check**:
1. Channel buffer full (10K blocks × ~1KB each = ~10 MB)
2. Large batches held in memory
3. Goroutine leaks

**Fix**:
- Increase memory limit to 256 MB
- Reduce channel buffer size
- Check for stuck goroutines (see logs for "goroutine" or "deadlock")

### Connection Timeouts

**Symptoms**: Frequent "keep-alive timeout" in logs, sync rate drops to 0

**Check**:
1. Network path to cardano-node (cross-AZ = slower)
2. Node under heavy load
3. Firewall/routing issues

**Fix**:
- Configure host2 failover in config
- Use same-node cardano-node for NtC queries
- Increase timeout in sync.go (keep-alive period)

## Monitoring Best Practices

1. **Watch sync rate** — `kubectl logs -f` and grep for "blk/s"
2. **Monitor database size** — `SELECT pg_database_size('goduckbot_v2');`
3. **Track epoch progress** — `SELECT MAX(epoch) FROM blocks;`
4. **Check for errors** — `kubectl logs | grep -i error`
5. **Resource trends** — Use Prometheus/Grafana for long-term tracking

## Advanced Profiling (Manual)

Since goduckbot doesn't have built-in pprof, use these methods:

### CPU Profiling

```bash
# Build with pprof support (add import in main.go first)
# import _ "net/http/pprof"

# Or use external profilers
go test -cpuprofile=cpu.prof -bench=.
go tool pprof cpu.prof
```

### Memory Profiling

```bash
# Go race detector
go build -race -o goduckbot .
./goduckbot

# Memory trace
go build -o goduckbot .
GODEBUG=gctrace=1 ./goduckbot
```

### Delve Debugger

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug live process
kubectl -n cardano exec -it <pod> -- sh
dlv attach $(pgrep goduckbot)

# Set breakpoints, inspect variables
(dlv) break main.ProcessBatch
(dlv) continue
(dlv) print blocksChan
```

## Related Skills

- **local-dev** — Local development and testing setup
- **testing** — Run tests including benchmarks
- **db-ops** — Database query and analysis
- **chain-status** — Check chain sync health
