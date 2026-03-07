---
alwaysApply: true
---

# Goduckbot Project Guardrails

## Architecture

This is a single-binary Go application. All source lives in the root package.

### Critical constraints

1. **main.go is the main file.** Do not split into packages unless explicitly asked. The single-file architecture is intentional.
2. **4 database tables exist: `blocks`, `epoch_nonces`, `leader_schedules`, `slot_outcomes`.** Any change that touches DB operations must account for all four tables.
3. **Nonce computation is cryptographically sensitive.** Changes to nonce calculation, VRF handling, or epoch boundary logic require verification against the Koios API. Use the `verify-nonces` skill after any nonce-related change.
4. **The notification pipeline (Telegram + Twitter) is production-live.** Test changes to notification formatting/logic thoroughly — a bad push sends garbage to real users.

### Testing

5. **`comprehensive_test.go` is the primary test file (1100+ lines).** Run relevant test functions after any change — do not run the entire suite unless asked.
6. **Test files: `comprehensive_test.go`, `leaderlog_test.go`, `nonce_test.go`, `nonce_koios_test.go`, `store_test.go`.** Know which test file covers the code you're modifying.
7. **Run `go vet ./...` and `go build ./...` after any change.** Compilation is not optional verification.

### Deployment

8. **Production namespace is `cardano`, not `duckbot`.** Production label: `app.kubernetes.io/name=goduckbot`. Test label: `app.kubernetes.io/name=goduckbot-test`.
9. **Database is `goduckbot_v2` on pod `k3s-postgres-rw-0` in namespace `postgres`.**
10. **Docker images: `wcatz/goduckbot`. Multi-arch: linux/amd64,linux/arm64.**
