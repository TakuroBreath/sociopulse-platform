//go:build smoke

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_HarnessBootsAndHealthz is the Plan 21 Task 4 shakedown.
//
// It proves that the smoke harness can stand up the full backing stack —
// Postgres + Redis + NATS testcontainers, with PG migrations applied and
// JetStream streams pre-provisioned — and that cmd/api boots cleanly
// against it and serves /healthz.
//
// Every subsequent smoke scenario (Plan 21 Tasks 5-7) reuses the
// smoke.SharedStack + the bootAPI(t, stack) wiring established here.
//
// Why the test lives under cmd/api (not tests/smoke):
//
// cmd/api is package main and main.run() — the composition root — is
// unexported. The plan (docs/superpowers/plans/2026-05-15-21-e2e-smoke-foundation.md)
// allows either:
//
//	(a) extract run() into an importable internal/runner package, or
//	(b) place smoke tests under cmd/api so they can call run() directly.
//
// (a) cascades ~1700 LOC across 12 files (every helper in postgres.go,
// redis.go, eventbus.go, server.go, providers.go, modules.go, realtime.go,
// recording.go, recording_resolver.go is referenced by run() and would
// migrate alongside). (b) keeps the seam intact and matches the existing
// pattern in cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly which
// also drives run() directly. The Plan 21 references file (§ 2.4) confirms
// (b) is the intended path.
//
// The reusable testcontainer-stack lifecycle lives in tests/smoke/ as a
// library package so the build-tagged tests under cmd/api/ stay thin.
func TestSmoke_HarnessBootsAndHealthz(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	cli := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+httpAddr+"/healthz", nil)
	require.NoError(t, err)

	resp, err := cli.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// bootAPI writes a smoke config pointing at the testcontainer DSNs, picks
// free 127.0.0.1 ports for the HTTP + metrics listeners, and runs cmd/api's
// composition root (main.run) in a goroutine. It returns the bound HTTP +
// metrics addresses and registers a t.Cleanup that cancels the boot context
// and waits for run() to drain.
//
// Mirrors cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly's seam
// usage, with two adaptations for smoke:
//
//  1. The config DSNs point at the testcontainer stack (real PG / Redis /
//     NATS), not at the localhost defaults.
//  2. The listener-ready timeout is 30s (vs 10s in the unit-level boot
//     test) because cmd/api Register() does real work against PG/Redis on
//     a cold stack — tenancy/auth/crm migrate-time queries can take a
//     beat on a freshly-booted container.
func bootAPI(t *testing.T, stack *smoke.Stack) (httpAddr, metricsAddr string) {
	t.Helper()

	httpAddr = smoke.PickFreeAddr(t)
	metricsAddr = smoke.PickFreeAddr(t)
	configDir := smoke.WriteSmokeConfig(t, stack, httpAddr, metricsAddr)

	// Pre-provision the wildcard JetStream streams cmd/api boot expects:
	// without TENANT_SMOKE + TRUNKS_SMOKE, the realtime dispatcher's
	// JetStream subscriber fails Start with "no stream matches subject"
	// and trips the errgroup before /healthz is wired (see
	// docs/references/plan-21-e2e-smoke-foundation.md § 2.9).
	smoke.EnsureSmokeStreams(t, stack.NATSURL)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, configDir)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("smoke: run() returned: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("smoke: run() did not exit within 10s of cancel")
		}
	})

	select {
	case err := <-errCh:
		// run() failed before the listener came up — surface immediately.
		t.Fatalf("smoke: run() returned before listener was ready: %v", err)
	case <-smoke.ListenerReadyChan(httpAddr, 30*time.Second):
		// listener accepted a TCP connection — boot succeeded
	}
	return httpAddr, metricsAddr
}
