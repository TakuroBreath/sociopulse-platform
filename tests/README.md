# tests/

Cross-package E2E, load, and smoke tests.

- `smoke/` — Go end-to-end smoke harness (Plan 21). Boots cmd/api as a goroutine against a real Postgres+Redis+NATS testcontainer stack. Run via `make test-smoke`. See `smoke/README.md`.
- `e2e/` — Playwright (filled in Plan 15+).
- `load/` — k6 scripts (filled later).
