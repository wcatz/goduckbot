---
name: testing
description: Testing workflows for goduckbot. Use when writing tests, running test suites, or verifying behavior.
---

# Testing

Comprehensive testing guide for goduckbot.

## Test Suite Overview

```bash
# Summary
go test ./... -v | grep -E "^(=== RUN|--- PASS|--- FAIL)"

# Count
go test ./... -v 2>&1 | grep -c "^=== RUN"  # 30 tests total
```

### Test Files

| File | Tests | Coverage |
|------|-------|----------|
| `store_test.go` | 14 | SQLite Store operations (in-memory `:memory:`) |
| `nonce_test.go` | 7 | VRF nonce hashing, evolution, genesis seed |
| `nonce_koios_test.go` | 1 | Nonce verification against Koios API (integration) |
| `leaderlog_test.go` | 7 | SlotToEpoch, epoch math, formatting |
| `comprehensive_test.go` | 30 | End-to-end: slot math, VRF, thresholds, CPraos |

## Running Tests

### All Tests

```bash
# Standard
go test ./... -v

# With coverage
go test ./... -cover -coverprofile=coverage.out

# View coverage in browser
go tool cover -html=coverage.out
```

### Specific Tests

```bash
# By function name
go test -run TestSlotToEpochMainnet -v

# By pattern
go test -run "TestSlot.*" -v
go test -run ".*Nonce.*" -v

# By file
go test -run "" store_test.go -v

# Single package
go test . -v
```

### Integration Tests

```bash
# Koios nonce verification (slow, hits real API)
go test -run TestKoiosEpochNonceVerification -v

# Expected: 15 epochs verified against Koios
# Rate limit: 1.5s between requests (avoid 429s)
# Epochs tested: 210, 225, 250, 280, 300, 320, 340, 360, 370, 400, 450, 500, 550, 600, 615
```

### Performance Tests

```bash
# With benchmarks
go test -bench=. -benchmem

# Specific benchmark
go test -bench=BenchmarkVrfNonceValue -benchmem

# CPU profile
go test -bench=. -cpuprofile=cpu.prof
go tool pprof cpu.prof

# Memory profile
go test -bench=. -memprofile=mem.prof
go tool pprof mem.prof
```

## Writing Tests

### Unit Test Template

```go
func TestFeatureName(t *testing.T) {
    // Arrange
    input := "test-value"
    expected := "expected-result"

    // Act
    result, err := FunctionUnderTest(input)

    // Assert
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if result != expected {
        t.Errorf("got %v, want %v", result, expected)
    }
}
```

### Table-Driven Tests

```go
func TestSlotToEpoch(t *testing.T) {
    tests := []struct {
        name   string
        slot   uint64
        magic  int
        expect int
    }{
        {"mainnet epoch 208", 4_492_800, MainnetNetworkMagic, 208},
        {"mainnet epoch 617", 181_100_000, MainnetNetworkMagic, 617},
        {"preprod epoch 100", 45_000_000, PreprodNetworkMagic, 100},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := SlotToEpoch(tt.slot, tt.magic)
            if got != tt.expect {
                t.Errorf("got %d, want %d", got, tt.expect)
            }
        })
    }
}
```

### Store Tests (In-Memory SQLite)

```go
func TestStoreFeature(t *testing.T) {
    // Use in-memory DB for fast tests
    store, err := NewSqliteStore(":memory:")
    if err != nil {
        t.Fatalf("failed to create store: %v", err)
    }
    defer store.Close()

    ctx := context.Background()

    // Test operations
    err = store.InsertBlock(ctx, 12345678, 285, "abc123", []byte{1,2,3}, []byte{4,5,6})
    if err != nil {
        t.Fatalf("InsertBlock failed: %v", err)
    }

    // Verify
    hash, err := store.GetBlockHash(ctx, 12345678)
    if err != nil {
        t.Fatalf("GetBlockHash failed: %v", err)
    }
    if hash != "abc123" {
        t.Errorf("got %s, want abc123", hash)
    }
}
```

### Nonce Tests

```go
func TestNonceEvolution(t *testing.T) {
    // Test data (from Cardano test vectors)
    eta0, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000000")
    contribution1, _ := hex.DecodeString("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
    
    // Evolve
    eta1 := evolveNonce(eta0, contribution1)
    
    // Verify deterministic
    eta1_again := evolveNonce(eta0, contribution1)
    if !bytes.Equal(eta1, eta1_again) {
        t.Error("nonce evolution not deterministic")
    }
    
    // Verify length
    if len(eta1) != 32 {
        t.Errorf("nonce length %d, want 32", len(eta1))
    }
}
```

## Test Data Sources

### Known Good Nonces

```go
// From Koios API (verified against cardano-node)
var knownNonces = map[int]string{
    210: "ddf346732e6a473282f210bb2425a4e27d6a1e86c8aa591eb6610c74415d8e98",
    225: "edb2d417956becb8647cb2c6e3e08d77b2e0e26ca2779f16e5c47f2d5a1a969a",
    250: "8d60fab791ae35349663ae8c9f3484a9e18d90fc35f95a96fad7bd52f6d95be8",
    // ... add more as needed
}
```

### Known Good Leaderlog Slots

```go
// Epoch 612 mainnet (verified against cncli)
var epoch612Slots = []uint64{
    179_755_123,
    179_758_456,
    179_820_789,
    // ... 35 total slots
}
```

## Mocking

### Koios Client Mock

```go
type mockKoiosClient struct {
    poolHistory map[int]*koios.PoolHistoryData
    epochInfo   map[int]*koios.EpochInfoData
}

func (m *mockKoiosClient) GetPoolHistory(ctx context.Context, poolID koios.PoolID, epoch *koios.EpochNo, opts *koios.RequestOptions) (*koios.PoolHistory, error) {
    if data, ok := m.poolHistory[int(*epoch)]; ok {
        return &koios.PoolHistory{Data: []koios.PoolHistoryData{*data}}, nil
    }
    return nil, fmt.Errorf("no data for epoch %d", *epoch)
}
```

### Store Mock

```go
type mockStore struct {
    blocks map[uint64]BlockRecord
    nonces map[int][]byte
}

func (m *mockStore) InsertBlock(ctx context.Context, slot uint64, epoch int, hash string, vrf, nonce []byte) (bool, error) {
    m.blocks[slot] = BlockRecord{Slot: slot, Epoch: epoch, BlockHash: hash}
    return true, nil
}
```

## Regression Tests

### Epoch 467 (Historical Validation)

```go
// Test validates full leaderlog calculation against known good data
func TestEpoch467Integration(t *testing.T) {
    // ... full test in comprehensive_test.go
}
```

### Epoch 612 (Recent Validation)

```go
// Test validates against cncli output
func TestEpoch612Integration(t *testing.T) {
    // ... validates 35 assigned slots match cncli exactly
}
```

## Continuous Testing

### Pre-Commit Hook

```bash
# .git/hooks/pre-commit
#!/bin/bash
set -e

echo "Running tests..."
go test ./... -v

echo "Running vet..."
go vet ./...

echo "Checking formatting..."
if [ -n "$(gofmt -l .)" ]; then
    echo "Code not formatted. Run: go fmt ./..."
    exit 1
fi

echo "All checks passed!"
```

### CI/CD

```yaml
# .github/workflows/test.yaml
name: Test
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go test ./... -v -cover
      - run: go vet ./...
```

## Debugging Failed Tests

### Verbose Output

```bash
# With stack traces
go test ./... -v -failfast

# With timing
go test ./... -v -json | jq -r 'select(.Action=="pass") | "\(.Test) \(.Elapsed)s"'
```

### Interactive Debugging (Delve)

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug test
dlv test -- -test.run TestSlotToEpochMainnet

# Delve commands
(dlv) break TestSlotToEpochMainnet
(dlv) continue
(dlv) print slot
(dlv) next
(dlv) quit
```

### Race Detection

```bash
# Check for race conditions
go test ./... -race

# With verbose
go test ./... -race -v
```

## Next Steps

- Use `local-dev` skill for running tests in development environment
- Use `nonce-debug` skill for verifying nonce calculations
- Use `leaderlog-debug` skill for validating leader schedules
