//go:build smoke

package smoke

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// provisionSharedStreams creates the wildcard JetStream streams cmd/api boot
// expects (tenant.> + trunks.>) ONCE per TestMain. Called from newSharedStack
// after the NATS testcontainer comes up. Returns a teardown closure registered
// via addProcessTeardown so the streams die with the container at process exit.
//
// Why one-shot at Stack construction (not per-bootAPI):
//
// Pre-Plan-22-fix, EnsureSmokeStreams ran per-bootAPI and registered
// t.Cleanup(DeleteStream) per call. Under Go's t.Parallel() — every smoke
// scenario opts in — scenario A's cleanup deleted the streams while scenario
// B's cmd/api was still subscribed, and B's realtime dispatcher errored with
// "nats: stream not found on connection [N] for subscription on tenant.*.X".
// CI failed deterministically on TestSmoke_OperatorReadyAndStateBroadcast on
// the v0.0.28 docs-only commit (run 25958460992) — Plan 21b had captured this
// as a "Phase-1c follow-up" but Plan 22's CI surfaced it as an actual blocker.
//
// Lift to once-per-TestMain via Stack: the streams persist for the entire
// test binary; per-test cleanup is unnecessary because the NATS container
// teardown drops them at process exit (handled by addProcessTeardown).
//
// Why the teardown is registered against addProcessTeardown (not testing.T):
//
// The Stack instance has no *testing.T to register against — it's a
// process-scoped singleton built via sync.Once. addProcessTeardown is the
// existing Stack mechanism for "tear down at process exit", used by every
// testcontainer terminate call.
func provisionSharedStreams(ctx context.Context, natsURL string) error {
	nc, err := nats.Connect(natsURL,
		nats.Timeout(5*time.Second),
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		return fmt.Errorf("provisionSharedStreams: connect to NATS at %s: %w", natsURL, err)
	}
	// Drop the connection at process exit, AFTER the streams + container teardown.
	addProcessTeardown(nc.Close)

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("provisionSharedStreams: open JetStream context: %w", err)
	}

	for _, spec := range sharedStreamSpecs() {
		if _, err := js.AddStream(spec); err != nil {
			if !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
				return fmt.Errorf("provisionSharedStreams: ensure stream %q: %w", spec.Name, err)
			}
			// Idempotent: re-create with potentially-updated config (no-op if identical).
			if _, err := js.UpdateStream(spec); err != nil {
				return fmt.Errorf("provisionSharedStreams: update stream %q: %w", spec.Name, err)
			}
		}
	}

	_ = ctx // reserved for future deadline-bound provision; today the calls are sub-second.
	return nil
}

// sharedStreamSpecs is the canonical set of JetStream streams the smoke
// harness pre-provisions. Today: tenant.> (realtime + dialer + auth +
// recording + analytics + billing fan-out) + trunks.> (telephony health).
// InterestPolicy retention + MemoryStorage so containers don't accumulate
// state across runs.
func sharedStreamSpecs() []*nats.StreamConfig {
	return []*nats.StreamConfig{
		{
			Name:      "TENANT_SMOKE",
			Subjects:  []string{"tenant.>"},
			Retention: nats.InterestPolicy,
			Storage:   nats.MemoryStorage,
			MaxAge:    5 * time.Minute,
		},
		{
			Name:      "TRUNKS_SMOKE",
			Subjects:  []string{"trunks.>"},
			Retention: nats.InterestPolicy,
			Storage:   nats.MemoryStorage,
			MaxAge:    5 * time.Minute,
		},
	}
}

// EnsureSmokeStreams is the Plan-21-era per-bootAPI verify-only shim, retained
// for backwards compatibility with the existing cmd/api/smoke_test.go::bootAPI
// call site (Plan-22 close-out leaves bootAPI's signature untouched).
//
// Today it asserts the shared streams (provisioned by Stack at TestMain)
// exist — a sanity check that the Stack initialiser completed before any test
// reached bootAPI. It does NOT create or delete; that ownership has moved to
// Stack per the Plan-22 fix above. Future callers should drop this call
// entirely once a few more plans have shipped against the new model.
func EnsureSmokeStreams(t *testing.T, natsURL string) {
	t.Helper()

	nc, err := nats.Connect(natsURL,
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(false),
	)
	require.NoError(t, err, "smoke: connect to NATS at %s", natsURL)
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	require.NoError(t, err, "smoke: open JetStream context")

	for _, spec := range sharedStreamSpecs() {
		info, err := js.StreamInfo(spec.Name)
		require.NoErrorf(t, err,
			"smoke: stream %q must be provisioned by Stack at TestMain (not by bootAPI)", spec.Name)
		require.NotNil(t, info,
			"smoke: stream %q info is nil", spec.Name)
	}
}
