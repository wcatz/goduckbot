# Goduckbot — Cardano Stake Pool Notification Bot

## Architecture
- Single main.go architecture
- Image: `wcatz/goduckbot` (ARM64 only, built locally)
- CI/CD handles Docker images AND helm charts on merge to master — NEVER build locally

## Critical Guardrails
- Historical sync: keepalive=false + retry/resume (keepalive=true causes ChainSync 300s stall)
- Nonce correctness: trust `BackfillNonces` output, NOT mid-sync `candidate_nonce` values
- NEVER reuse Docker tags — bump version for redeployments
- NEVER commit directly to master

## Config Patterns
- Use `viper.GetString()` for scalar config values, NOT `GetStringSlice()`
- Koios integration tests are rate-limit sensitive — failures may be 429s, not bugs

## Code Patterns (from audit/cleanup PR #93)
- Stake queries for leaderlog: use `queryStakeForLeaderlog(ctx, targetEpoch)` on `*Indexer`
- Duck media fetch: use `fetchDuckURL(endpoint)` helper
- `StreamBlockNonces`, `GetNonceValuesForEpoch`, `GetLastBlockHashForEpoch` are GONE — do not re-add
- `QueryStakeDistribution` (NtC all-pools) is GONE — use `QueryPoolStakeSnapshots` for single-pool
- `BlockNonceRows` interface + impls are GONE
- `DBIntegrityResult`: only has `Truncated bool` — `Valid` and `Repaired` fields were removed
