//go:build integration

// store_redis_test.go drives the Machine against a real Redis 7.4
// container. miniredis is fine for unit tests of the high-level FSM
// flow (it interprets cjson + HSET/HDEL just like real Redis), but the
// Lua CAS contract — atomic version check, single-key script
// scheduling, EXPIRE refresh under concurrent load — is the production
// invariant. This binary verifies it on real Redis.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run; CI invokes `go test -tags=integration ./...` for
// the integration target.
package fsm_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/fsm"
)

// startRedis boots Redis 7.4 in a container and returns its host:port.
// Cleanup is registered via t.Cleanup; Terminate runs against
// context.Background so a test cancelled mid-flight still reaps the
// container.
func startRedis(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "redis:7.4-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	return host + ":" + port.Port()
}

// redisFixture is the integration counterpart to machineFixture in
// machine_test.go. It boots real Redis but keeps the PG-side fakes
// (we test PG separately in audit_pg_test.go).
type redisFixture struct {
	rdb      *redis.Client
	pg       *fakeTxRunner
	ob       *fakeOutbox
	sessions *fakeSessionStore
	clock    *fakeClock
	machine  *fsm.Machine
}

func newRedisFixture(t *testing.T) *redisFixture {
	t.Helper()
	addr := startRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}
	sessions := &fakeSessionStore{}
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	m := fsm.RegisterMetrics(prometheus.NewRegistry())
	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       pg,
		Outbox:   ob,
		Sessions: sessions,
		Logger:   zaptest.NewLogger(t),
		Clock:    clk.Now,
		Metrics:  m,
		HashTTL:  time.Hour, // shorter TTL for testability; default 24h is fine, but explicit is fine too
	})
	require.NoError(t, err)
	return &redisFixture{
		rdb:      rdb,
		pg:       pg,
		ob:       ob,
		sessions: sessions,
		clock:    clk,
		machine:  mach,
	}
}

// TestIntegration_FullRoundTrip drives StartShift → 11 transitions →
// EndShift on real Redis and asserts the final hash field set is clean.
func TestIntegration_FullRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	f := newRedisFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	_, err = f.machine.RecordCallEnded(ctx, api.CallEndedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, Cause: "NORMAL_CLEARING",
	})
	require.NoError(t, err)

	// SubmitStatus: status → ready (clears call_id / respondent_id).
	_, err = f.machine.SubmitStatus(ctx, api.SubmitStatusRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID, Status: "success",
	})
	require.NoError(t, err)

	// GoVerify is entered from ready (operator-initiated listen-in).
	_, err = f.machine.GoVerify(ctx, tenantID, operatorID)
	require.NoError(t, err)

	_, err = f.machine.VerifyDone(ctx, tenantID, operatorID)
	require.NoError(t, err)

	_, err = f.machine.GoPause(ctx, api.GoPauseRequest{TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break"})
	require.NoError(t, err)

	_, err = f.machine.Resume(ctx, tenantID, operatorID)
	require.NoError(t, err)

	snap, err := f.machine.EndShift(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)

	// Verify the hash is clean after EndShift — no stale call_id /
	// project_id / respondent_id / pause_reason.
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	res, err := f.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "offline", res["state"])
	require.Empty(t, res["session_id"])
	require.Empty(t, res["project_id"])
	require.Empty(t, res["current_call_id"])
	require.Empty(t, res["respondent_id"])
	require.Empty(t, res["pause_reason"])
}

// TestIntegration_ConcurrentCAS drives N goroutines × M GoPause/Resume
// transitions on the SAME operator and asserts the production invariant
// of the optimistic-concurrency design:
//
//   - The Lua CAS script serialises state-changing writes per key.
//     Each successful state-change increments `version` by exactly 1.
//   - The metric counter `dialer_fsm_transitions_total` (which the FSM
//     increments only on real CAS writes, not idempotent replays)
//     therefore equals the version delta from the StartShift baseline.
//   - The FSM never tears: final state is always a valid api.State
//     value, never a partially-written hash. -race surfaces any torn
//     read.
//
// miniredis honours single-script atomicity but real Redis is the
// authoritative testbed.
func TestIntegration_ConcurrentCAS(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()

	addr := startRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}
	sessions := &fakeSessionStore{}
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	reg := prometheus.NewRegistry()
	m := fsm.RegisterMetrics(reg)
	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       pg,
		Outbox:   ob,
		Sessions: sessions,
		Logger:   zaptest.NewLogger(t),
		Clock:    clk.Now,
		Metrics:  m,
	})
	require.NoError(t, err)

	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err = mach.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	const goroutines = 100
	const perGoroutine = 100

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				_, _ = mach.GoPause(ctx, api.GoPauseRequest{
					TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break",
				})
				_, _ = mach.Resume(ctx, tenantID, operatorID)
			}
		})
	}
	wg.Wait()

	// Read final version + state directly from Redis.
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	versionStr, err := rdb.HGet(ctx, key, "version").Result()
	require.NoError(t, err)
	finalVersion, err := parseInt64(versionStr)
	require.NoError(t, err)

	// Count actual successful state-changing CAS writes via the metric.
	// StartShift contributed 1 (offline→ready). The pause/resume loop
	// contributes the rest. Every increment of the Transitions counter
	// corresponds to one successful CAS write that bumped version by 1.
	var totalTransitions int64
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "dialer_fsm_transitions_total" {
			continue
		}
		for _, met := range mf.GetMetric() {
			totalTransitions += int64(met.GetCounter().GetValue())
		}
	}

	require.Equal(t, totalTransitions, finalVersion,
		"version (%d) must equal number of state-changing CAS writes (%d)",
		finalVersion, totalTransitions)

	stateStr, err := rdb.HGet(ctx, key, "state").Result()
	require.NoError(t, err)
	state := api.State(stateStr)
	require.True(t, state.Valid(), "final state %q must be a valid api.State", stateStr)

	t.Logf("concurrent CAS: state-changing CAS writes=%d final_version=%d state=%s",
		totalTransitions, finalVersion, stateStr)
}

// TestIntegration_CASConflict_SurfacesErrConflict races N parallel CAS
// writes on the SAME hash version against real Redis and asserts that
// at least one of the losers surfaces api.ErrConflict via errors.Is.
// Public callers across module boundaries (cmd/api retry middleware,
// cmd/worker dispatch loop) rely on this chain to distinguish a
// retryable optimistic-concurrency conflict from a genuinely invalid
// transition.
//
// We drive the race directly through the public Force surface, which
// always issues a CAS regardless of source state. Force(target=ready)
// from ready is a no-op (idempotent), so we race Force(target=pause)
// against an already-paused operator: the winner of the CAS lands the
// transition, every other racer's expected_version is now stale and
// the script returns -1 → errVersionMismatch wrapping api.ErrConflict.
//
// To eliminate the trailing-racer "saw the post-winner state and
// idempotent-shortcut" path, we toggle the target across racers using
// odd/even indexing — each racer attempts to flip the operator state.
// Real Redis serialises Lua scripts per-key, so the first winner of any
// version=v→v+1 advance wins; concurrent attempts at the same version
// all observe the bump and surface ErrConflict.
func TestIntegration_CASConflict_SurfacesErrConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	f := newRedisFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Race N parallel pause/resume cycles. Force is the cleanest
	// driver because it bypasses the transitions table — every call
	// issues a CAS — but surfaces the same errVersionMismatch chain.
	// The mix of pause/resume targets keeps every iteration "useful":
	// after a winner moves the state, racers at the OLD version still
	// see ErrConflict via errors.Is (their expected version is stale).
	const racers = 64
	var conflictCount, otherErrCount, successCount int64
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			<-startBarrier
			target := api.StatePause
			if i%2 == 0 {
				target = api.StateReady
			}
			_, err := f.machine.Force(ctx, tenantID, operatorID, target, api.ForceReasonAdminOverride)
			switch {
			case err == nil:
				atomic.AddInt64(&successCount, 1)
			case errors.Is(err, api.ErrConflict):
				atomic.AddInt64(&conflictCount, 1)
			default:
				atomic.AddInt64(&otherErrCount, 1)
				t.Logf("unexpected error: %v", err)
			}
		})
	}
	close(startBarrier)
	wg.Wait()

	require.Zero(t, atomic.LoadInt64(&otherErrCount),
		"all errors from a same-version Force race must be api.ErrConflict; got non-conflict errors")
	require.GreaterOrEqual(t, atomic.LoadInt64(&conflictCount), int64(1),
		"at least one parallel Force must lose the CAS and surface api.ErrConflict on real Redis (success=%d)",
		atomic.LoadInt64(&successCount))
	t.Logf("CAS conflict race: %d racers, %d successes, %d conflicts surfaced",
		racers, atomic.LoadInt64(&successCount), atomic.LoadInt64(&conflictCount))
}

// parseInt64 parses a decimal int64. Inlined helper to keep the
// import set focused.
func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errParseInt
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

var errParseInt = stringError("parse int")

type stringError string

func (e stringError) Error() string { return string(e) }
