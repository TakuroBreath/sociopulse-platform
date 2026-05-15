//go:build smoke

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// TestSmoke_HealthAndReadiness — Plan 21 Task 5.
//
// Asserts the gateway exposes a sanity surface on a full-stack boot:
//
//   - /healthz returns 200 unconditionally (liveness, pre-startup-done).
//   - /readyz returns 200 + JSON with status="ok" AND the postgres + nats
//     checks both ok=true. The Redis check is module-owned (auth/dialer)
//     and need not surface here.
//   - /metrics (on the separate listener) returns 200 + at least one
//     well-known process metric (catches a future refactor that drops
//     the Prometheus collector registry from cmd/api boot).
//
// This is the regression net for the Plan 02 gateway middleware path —
// a class of failure where /readyz logic is moved but the underlying
// checker chain is not re-wired.
func TestSmoke_HealthAndReadiness(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, metricsAddr := bootAPI(t, stack)

	cli := &http.Client{Timeout: 5 * time.Second}
	ctx := t.Context()

	// /healthz — liveness, always 200 once the listener is up.
	healthResp := mustSmokeGet(ctx, t, cli, "http://"+httpAddr+"/healthz")
	assert.Equal(t, http.StatusOK, healthResp.StatusCode, "healthz must be 200")
	_ = healthResp.Body.Close()

	// /readyz — must report postgres + nats checks ok against a fully-up
	// testcontainer stack. The Redis check sits inside auth/dialer modules
	// (when they register a check) so we don't assert on its presence.
	readyResp := mustSmokeGet(ctx, t, cli, "http://"+httpAddr+"/readyz")
	require.Equal(t, http.StatusOK, readyResp.StatusCode,
		"readyz must be 200 when Postgres + NATS are reachable")
	body, err := io.ReadAll(readyResp.Body)
	require.NoError(t, err)
	_ = readyResp.Body.Close()

	// Response shape per internal/healthz/readiness.go:
	//   {"status":"ok","checks":{"postgres":{"ok":true,...},"nats":{"ok":true,...}}}
	var ready struct {
		Status string `json:"status"`
		Checks map[string]struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error,omitempty"`
			DurationMS int64  `json:"duration_ms"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(body, &ready),
		"readyz body must be parseable JSON")
	assert.Equal(t, "ok", ready.Status, "readyz top-level status must be ok")
	require.Contains(t, ready.Checks, "postgres",
		"readyz must include a postgres check (built from healthchecks.PostgresCheck at boot)")
	require.Contains(t, ready.Checks, "nats",
		"readyz must include a nats check (built from healthchecks.NATSCheck at boot)")
	assert.True(t, ready.Checks["postgres"].OK,
		"postgres check must be ok on the smoke testcontainer stack")
	assert.True(t, ready.Checks["nats"].OK,
		"nats check must be ok on the smoke testcontainer stack")

	// /metrics — on the separate listener. Must emit at least one
	// canonical Go-runtime metric, proving the Prometheus registry is
	// wired into the metrics server.
	metricsResp := mustSmokeGet(ctx, t, cli, "http://"+metricsAddr+"/metrics")
	require.Equal(t, http.StatusOK, metricsResp.StatusCode, "metrics must be 200")
	mbody, err := io.ReadAll(metricsResp.Body)
	require.NoError(t, err)
	_ = metricsResp.Body.Close()
	mtext := string(mbody)
	assert.True(t,
		strings.Contains(mtext, "go_goroutines") ||
			strings.Contains(mtext, "process_cpu_seconds_total") ||
			strings.Contains(mtext, "go_info"),
		"metrics body must expose at least one well-known process metric; got %d bytes", len(mtext))
}

// mustSmokeGet issues a GET with caller-supplied ctx + http.Client and
// surfaces transport errors via require.NoError so the test fails loudly
// rather than dereferencing a nil response on the next line.
func mustSmokeGet(ctx context.Context, t *testing.T, cli *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := cli.Do(req)
	require.NoError(t, err, "GET %s", url)
	return resp
}

// TestSmoke_AuthFullFlow — Plan 21 Task 6.
//
// Walks the canonical auth lifecycle against a fully-booted cmd/api:
//
//  1. POST /api/auth/login with valid creds → 200 + access + refresh
//  2. POST /api/auth/refresh with the refresh → 200 + fresh tokens
//  3. POST /api/auth/logout with the refresh → 204
//  4. POST /api/auth/refresh with the logged-out refresh → 401
//
// Catches the JWT claims schema drift class (10-end-to-end-testing-gaps.md
// failure scenario #3): any change that breaks login serialisation, JWT
// signing, or Redis-backed refresh invalidation surfaces here.
func TestSmoke_AuthFullFlow(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	admin := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-A", "alice", "AlicePass123!")

	ctx := t.Context()
	cli := &http.Client{Timeout: 10 * time.Second}

	// 1. Login. We read the body bytes once and decode from them so the
	// diagnostic on a non-200 status carries the actual response without
	// racing the Decoder for the same Reader.
	loginBody := fmt.Sprintf(`{"org_id":%q,"login":%q,"password":%q}`,
		admin.OrgCode, admin.Login, admin.Password)
	loginStatus, loginBytes := postJSONAndRead(ctx, t, cli,
		"http://"+httpAddr+"/api/auth/login", loginBody)
	require.Equalf(t, http.StatusOK, loginStatus,
		"login must 200 for seeded admin; got %d body=%s", loginStatus, string(loginBytes))

	var login struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(loginBytes, &login), "decode login response: %s", string(loginBytes))
	require.NotEmpty(t, login.AccessToken, "access_token present")
	require.NotEmpty(t, login.RefreshToken, "refresh_token present")

	// 2. Refresh — must mint a NEW access_token (rotation).
	refreshBody := fmt.Sprintf(`{"refresh_token":%q}`, login.RefreshToken)
	refreshStatus, refreshBytes := postJSONAndRead(ctx, t, cli,
		"http://"+httpAddr+"/api/auth/refresh", refreshBody)
	require.Equalf(t, http.StatusOK, refreshStatus,
		"refresh must 200; got %d body=%s", refreshStatus, string(refreshBytes))

	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(refreshBytes, &refreshed), "decode refresh response: %s", string(refreshBytes))
	require.NotEmpty(t, refreshed.AccessToken)
	assert.NotEqual(t, login.AccessToken, refreshed.AccessToken,
		"refresh must mint a fresh access_token (rotation)")

	// 3. Logout — body carries the most-recent refresh (the rotated one).
	logoutBody := fmt.Sprintf(`{"refresh_token":%q}`, refreshed.RefreshToken)
	logoutStatus, logoutBytes := postJSONAndRead(ctx, t, cli,
		"http://"+httpAddr+"/api/auth/logout", logoutBody)
	require.Truef(t,
		logoutStatus == http.StatusNoContent || logoutStatus == http.StatusOK,
		"logout must 204 (or 200); got %d body=%s", logoutStatus, string(logoutBytes))

	// 4. Refresh with the logged-out token → 401. The body is the same
	// rotated refresh-token payload as the logout step; the session
	// revocation triggered above MUST surface as 401 here.
	revokedStatus, revokedBytes := postJSONAndRead(ctx, t, cli,
		"http://"+httpAddr+"/api/auth/refresh", logoutBody)
	assert.Equalf(t, http.StatusUnauthorized, revokedStatus,
		"refresh after logout must 401 (revoked session); got %d body=%s",
		revokedStatus, string(revokedBytes))
}

// postJSONAndRead issues a POST against url with a JSON body, returning
// the status code and the fully-consumed body bytes. The response body
// is always closed before return so the caller can use the bytes for
// both Decode and diagnostic logging without racing the Reader.
//
// Transport-level failures (build, dial, read) fail the test via
// require.NoError — they are never the test's intent and a nil response
// would only cause a confusing nil-deref one line later.
func postJSONAndRead(ctx context.Context, t *testing.T, cli *http.Client, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err, "build POST %s", url)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	require.NoError(t, err, "POST %s", url)
	defer func() { _ = resp.Body.Close() }()
	buf, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read body for POST %s", url)
	return resp.StatusCode, buf
}
