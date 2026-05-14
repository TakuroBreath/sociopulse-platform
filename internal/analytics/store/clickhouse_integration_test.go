//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/store"
)

// TestConn_OpenPingClose drives the full Open → Ping → Healthy → Close
// lifecycle against a fresh CH container. Failure here means the
// wrapper does not work end-to-end — the most important
// "is-the-thing-on" test in the package.
//
// The second Close call is explicitly checked to be a no-op (returns
// the same nil it returned the first time). This pins the idempotent
// Close contract that the ingest pipeline depends on for clean
// shutdown sequencing.
func TestConn_OpenPingClose(t *testing.T) {
	t.Parallel()

	dsns := startCH(t)

	conn, err := store.Open(t.Context(), store.Config{
		DSN:           dsns.verify,
		BatchSize:     10,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, conn)

	require.NoError(t, conn.Ping(t.Context()))
	require.NoError(t, conn.Healthy())

	require.NoError(t, conn.Close())
	// Second Close is a no-op; same error (nil) as first.
	require.NoError(t, conn.Close())
}

// TestOpen_PingFailureCleansUp guards the wrapped-error path: a DSN
// that points at nothing listening must come back as a wrapped error
// and the wrapper must not leak a half-initialised *Conn.
//
// We use 127.0.0.1:1 to exercise Open's Ping-on-init path — ParseDSN
// accepts the syntactic form but the dial fails. The short
// DialTimeout keeps the test fast.
func TestOpen_PingFailureCleansUp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	conn, err := store.Open(ctx, store.Config{
		DSN:           "clickhouse://nope:nope@127.0.0.1:1/nope",
		BatchSize:     10,
		FlushInterval: time.Second,
		DialTimeout:   500 * time.Millisecond,
	})
	require.Error(t, err)
	require.Nil(t, conn, "Open must not return a half-init Conn on Ping failure")
}
