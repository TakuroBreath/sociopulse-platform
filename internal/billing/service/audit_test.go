package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeTenantTxRunner satisfies the unexported tenantTxRunner interface used
// by AuditEmitter without spinning up a testcontainer. The closure is invoked
// with a nil Tx — the in-memory AuditWriter ignores the Tx, so this is safe
// for the audit-emit code path (which never reads from the Tx, only passes
// it through).
type fakeTenantTxRunner struct {
	called   bool
	tenantID uuid.UUID
	err      error
}

func (f *fakeTenantTxRunner) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.called = true
	f.tenantID = tenantID
	if f.err != nil {
		return f.err
	}
	// postgres.Tx is a struct, not an interface — pass the zero value. The
	// recording AuditWriter ignores tx (uses _) so it's safe.
	return fn(postgres.Tx{})
}

type recordingAuditWriter struct {
	events []outbox.Event
	err    error
}

func (r *recordingAuditWriter) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func newTestEmitter(t *testing.T, pool *fakeTenantTxRunner, ob *recordingAuditWriter) *service.AuditEmitter {
	t.Helper()
	// Construct via package-internal struct literal so we can inject the
	// fake tenantTxRunner. Production constructor takes *postgres.Pool which
	// satisfies the interface implicitly; tests reach in via the export_test
	// seam (see export_test.go below).
	return service.NewAuditEmitterForTest(pool, ob)
}

func TestAuditEmitter_EmitTariffUpdated_HappyPath(t *testing.T) {
	t.Parallel()
	pool := &fakeTenantTxRunner{}
	ob := &recordingAuditWriter{}
	e := newTestEmitter(t, pool, ob)

	tid := uuid.New()
	actor := uuid.New()
	e.EmitTariffUpdated(context.Background(), tid, actor, 3, 4,
		[]string{"wage_per_survey_minor", "fixed_fees_minor"})

	require.True(t, pool.called)
	require.Equal(t, tid, pool.tenantID)
	require.Len(t, ob.events, 1)
	got := ob.events[0]
	require.NotNil(t, got.TenantID)
	require.Equal(t, tid, *got.TenantID)
	require.Equal(t, "tenant."+tid.String()+".audit.event", got.Subject)

	var ev auditapi.Event
	require.NoError(t, json.Unmarshal(got.Payload, &ev))
	require.Equal(t, tid, ev.TenantID)
	require.NotNil(t, ev.ActorID)
	require.Equal(t, actor, *ev.ActorID)
	require.Equal(t, auditapi.ActorUser, ev.ActorKind)
	require.EqualValues(t, billingapi.AuditActionTariffUpdated, ev.Action)
	require.Equal(t, "tariff:"+tid.String(), ev.Target)
	// JSON numbers decode to float64; InEpsilon for testifylint compliance.
	require.InEpsilon(t, 3.0, ev.Payload["version_before"], 0.0001)
	require.InEpsilon(t, 4.0, ev.Payload["version_after"], 0.0001)
}

func TestAuditEmitter_EmitTariffUpdated_NilActor_OmitsActorID(t *testing.T) {
	t.Parallel()
	pool := &fakeTenantTxRunner{}
	ob := &recordingAuditWriter{}
	e := newTestEmitter(t, pool, ob)

	e.EmitTariffUpdated(context.Background(), uuid.New(), uuid.Nil, 0, 1, []string{"k"})

	require.Len(t, ob.events, 1)
	var ev auditapi.Event
	require.NoError(t, json.Unmarshal(ob.events[0].Payload, &ev))
	require.Nil(t, ev.ActorID, "ActorID must be nil when uuid.Nil was passed")
}

func TestAuditEmitter_EmitTariffUpdated_ZeroTenant_NoEmit(t *testing.T) {
	t.Parallel()
	pool := &fakeTenantTxRunner{}
	ob := &recordingAuditWriter{}
	e := newTestEmitter(t, pool, ob)

	e.EmitTariffUpdated(context.Background(), uuid.Nil, uuid.New(), 1, 2, []string{"k"})

	require.False(t, pool.called, "zero-tenant must skip the WithTenant call")
	require.Empty(t, ob.events)
}

func TestAuditEmitter_EmitTariffUpdated_OutboxError_DoesNotPanicOrPropagate(t *testing.T) {
	t.Parallel()
	pool := &fakeTenantTxRunner{}
	ob := &recordingAuditWriter{err: errors.New("simulated outbox failure")}
	e := newTestEmitter(t, pool, ob)

	// Must not panic — failure path only logs WARN. EmitTariffUpdated
	// returns void, so the assertion is "no panic" via t.Parallel completion.
	e.EmitTariffUpdated(context.Background(), uuid.New(), uuid.New(), 1, 2, []string{"k"})

	require.True(t, pool.called, "WithTenant must be attempted even on outbox failure")
	require.Empty(t, ob.events, "no event recorded when Append errors")
}

func TestAuditEmitter_EmitTariffUpdated_PoolError_DoesNotPanicOrPropagate(t *testing.T) {
	t.Parallel()
	pool := &fakeTenantTxRunner{err: errors.New("simulated pool failure")}
	ob := &recordingAuditWriter{}
	e := newTestEmitter(t, pool, ob)

	e.EmitTariffUpdated(context.Background(), uuid.New(), uuid.New(), 1, 2, []string{"k"})

	require.True(t, pool.called)
	require.Empty(t, ob.events)
}

func TestNewAuditEmitter_PanicsOnNilPool(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"billing.NewAuditEmitter: pool must be non-nil",
		func() { _ = service.NewAuditEmitter(nil, nil, nil) },
	)
}
