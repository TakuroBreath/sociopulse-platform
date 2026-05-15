//go:build smoke

package smoke

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// ensureStream provisions a JetStream stream for the given subjects on
// natsURL.
//
// Why we provision: cmd/api's realtime dispatcher SUBSCRIBES to
// "tenant.*.dialer.op.*.state" via the JetStream-backed eventbus before
// any module PUBLISHES — the JetStream broker returns "no stream matches
// subject" if the stream doesn't yet exist, and the realtime
// dispatcher's Start treats that as a hard error in the errgroup. In
// production the nats-bridge sidecar auto-creates streams from a
// config inventory; the smoke testcontainer has NATS up but no streams,
// so this helper bridges the gap. Mirrors cmd/api/main_test.go::ensureTestStream
// minus the skip-on-no-NATS branch (smoke ALWAYS has NATS).
//
// The stream uses InterestPolicy retention + memory storage so no
// on-disk artefacts accumulate; cleanup deletes it.
func ensureStream(t *testing.T, natsURL, name string, subjects []string) {
	t.Helper()

	nc, err := nats.Connect(natsURL,
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(false),
	)
	require.NoError(t, err, "smoke: connect to NATS at %s", natsURL)
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	require.NoError(t, err, "smoke: open JetStream context")

	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    5 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "smoke: ensure stream %q", name)
	}
	t.Cleanup(func() {
		// Best-effort delete — a parallel test holding the same stream
		// name would have skipped re-creation above, so DeleteStream
		// might race. Swallow the error rather than fail cleanup.
		_ = js.DeleteStream(name)
	})
}

// EnsureSmokeStreams pre-provisions the wildcard streams cmd/api boot
// expects: tenant.> (the realtime dispatcher's "tenant.*.dialer.op.*.state"
// + dialer pubsub subjects) and trunks.> (the trunks replicator's
// "trunks.health" cross-tenant fan-out). Call this AFTER NATS is up
// (Stack.NATSURL is set) and BEFORE the cmd/api goroutine starts.
//
// Mirrors PROJECT_STATUS.md:342's lesson from Plan 14:
// "cmd/api/main_test.go::ensureTestStream provisions a `tenant.>` +
// `trunks.>` JetStream stream pair BEFORE run() starts".
func EnsureSmokeStreams(t *testing.T, natsURL string) {
	t.Helper()
	ensureStream(t, natsURL, "TENANT_SMOKE", []string{"tenant.>"})
	ensureStream(t, natsURL, "TRUNKS_SMOKE", []string{"trunks.>"})
}
