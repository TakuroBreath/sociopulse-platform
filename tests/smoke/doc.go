//go:build smoke

// Package smoke is the СоциоПульс end-to-end smoke harness. It boots a
// real Postgres + Redis + NATS testcontainer stack, applies migrations,
// pre-provisions JetStream streams, and writes a smoke config that
// cmd/api can be booted against in-process.
//
// Smoke tests run via:
//
//	make test-smoke
//	# OR
//	go test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/... ./cmd/api/...
//
// # Architecture
//
// One container stack per `TestMain` (shared across every smoke test in
// the package). Per-test isolation is achieved by truncating per-tenant
// rows via Stack.Reset(t) when a scenario needs a clean slate; the
// default is per-test additive seeding (each test seeds its own rows
// and t.Cleanup tears them down).
//
// cmd/api runs as a goroutine in the same process as the test (no
// os/exec). The boot helpers live under cmd/api/ — they call the
// unexported main.run() directly via the existing seam at
// cmd/api/main.go::run(ctx, configDir). This package only provides the
// reusable testcontainer-stack lifecycle + the config writer + HTTP
// helpers; the smoke tests themselves live under cmd/api/smoke_test.go
// (and Plan 21 Task 5-7 follow-ups) so they retain in-package access
// to run().
//
// # Build tag
//
// The //go:build smoke tag gates the entire package. Untagged builds
// (go build ./..., make ci) ignore everything in tests/smoke/, so the
// default unit + integration suites are unaffected.
//
// # CI
//
// Plan 21 Task 8 adds a dedicated `smoke` job to .github/workflows/ci.yml
// that runs on every push to main and on every v* tag push. Tag-push
// deploys gate on smoke green.
//
// Gotchas
//
//   - TESTCONTAINERS_RYUK_DISABLED=true is recommended on macOS where
//     the ryuk reaper container has had Docker-version compatibility
//     trouble. See docs/references/plan-21-e2e-smoke-foundation.md § 4.1.
//   - testcontainers-go API has churned through 2025; if dial errors
//     surface, run `make test-smoke` from a freshly-pulled
//     postgres:16-alpine / redis:7 / nats:2.10-alpine.
package smoke
