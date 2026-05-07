//go:build integration

// Integration tests for pkg/outbox.PostgresWriter against a real Postgres 16
// instance booted via testcontainers-go. The test boots PG, applies both
// 000001_init and 000002_outbox migrations, then exercises the writer's
// transactional and non-transactional paths.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./pkg/outbox/...
package outbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPostgresWriter_AppendInsertsRowInTx verifies the writer persists an
// event row inside an existing transaction. The whole insert is wrapped in
// pool.BypassRLS so we exercise the canonical "share the caller's tx"
// path that production modules use (write aggregate state and outbox row
// atomically).
func TestPostgresWriter_AppendInsertsRowInTx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	w := outbox.NewPostgresWriter()

	tenantID := uuid.New()
	aggID := uuid.New()
	payload := mustJSON(t, map[string]any{"from": "ready", "to": "dialing"})

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return w.Append(ctx, tx, outbox.Event{
			TenantID:    &tenantID,
			AggregateID: &aggID,
			Subject:     "tenant.t1.dialer.op.op1.state",
			Payload:     payload,
		})
	}))

	var (
		gotSubject string
		gotPayload []byte
		gotTenant  *uuid.UUID
	)
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT subject, payload, tenant_id FROM event_outbox WHERE aggregate_id = $1`,
			aggID,
		).Scan(&gotSubject, &gotPayload, &gotTenant)
	}))
	require.Equal(t, "tenant.t1.dialer.op.op1.state", gotSubject)
	require.NotNil(t, gotTenant)
	require.Equal(t, tenantID, *gotTenant)

	var p map[string]any
	require.NoError(t, json.Unmarshal(gotPayload, &p))
	require.Equal(t, "ready", p["from"])
	require.Equal(t, "dialing", p["to"])
}

// TestPostgresWriter_AppendRollsBackWithCallerTx asserts that when the
// caller's transaction rolls back, the outbox row goes with it. This is
// the whole point of the transactional outbox pattern.
func TestPostgresWriter_AppendRollsBackWithCallerTx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	w := outbox.NewPostgresWriter()

	aggID := uuid.New()
	payload := mustJSON(t, map[string]any{"k": "v"})

	wantErr := errFakeBoom
	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		if err := w.Append(ctx, tx, outbox.Event{
			AggregateID: &aggID,
			Subject:     "test.subj",
			Payload:     payload,
		}); err != nil {
			return err
		}
		// Force a rollback by returning an error from the WithTenant fn.
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)

	// Row must NOT be present.
	var count int
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1`, aggID,
		).Scan(&count)
	}))
	require.Zero(t, count, "writer must not commit when caller's tx rolls back")
}

// TestPostgresWriter_AppendRejectsEmptySubject is a simple guard against
// silently inserting unroutable events.
func TestPostgresWriter_AppendRejectsEmptySubject(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	w := outbox.NewPostgresWriter()

	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return w.Append(ctx, tx, outbox.Event{
			Subject: "",
			Payload: []byte(`{}`),
		})
	})
	require.Error(t, err)
}

// TestPostgresWriter_AppendRejectsInvalidJSON ensures we never persist a
// payload that the JSONB column would refuse.
func TestPostgresWriter_AppendRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	w := outbox.NewPostgresWriter()

	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return w.Append(ctx, tx, outbox.Event{
			Subject: "test.subj",
			Payload: []byte(`not json {`),
		})
	})
	require.Error(t, err)
}

var errFakeBoom = fakeError("boom")

type fakeError string

func (e fakeError) Error() string { return string(e) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// silence unused warnings for pgx import — kept so future tests that need
// raw pgx escape-hatches don't have to re-add the import.
var _ pgx.Tx
