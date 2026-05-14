package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeOutboxWriter is a minimal recording AuditWriter fake. The Tx
// parameter is the zero-value postgres.Tx — the emitter does not touch
// it, it merely forwards to Append.
type fakeOutboxWriter struct {
	appended []outbox.Event
	nextErr  error
}

func (f *fakeOutboxWriter) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	if f.nextErr != nil {
		return f.nextErr
	}
	f.appended = append(f.appended, ev)
	return nil
}

// Compile-time assertion: the fake satisfies the AuditWriter contract.
var _ reportsvc.AuditWriter = (*fakeOutboxWriter)(nil)

func TestAuditEmit_PublishesAuditEventToTenantSubject(t *testing.T) {
	t.Parallel()

	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	actorID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	jobID := "job-abc-123"
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	fw := &fakeOutboxWriter{}
	emitter := reportsvc.NewAuditEmitter(fw)

	err := emitter.EmitTx(context.Background(), postgres.Tx{}, reportsvc.AuditExport{
		TenantID:  tenantID,
		ActorID:   actorID,
		ActorKind: auditapi.ActorUser,
		JobID:     jobID,
		Kind:      "operator_efficiency",
		Format:    "xlsx",
		BytesSize: 12_345,
		Window: reportsvc.AuditWindow{
			From: ts.Add(-24 * time.Hour),
			To:   ts,
		},
		Params:    map[string]any{"project_id": "33333333-3333-3333-3333-333333333333"},
		Timestamp: ts,
	})
	require.NoError(t, err)

	require.Len(t, fw.appended, 1)
	ev := fw.appended[0]
	require.Equal(t, "tenant.11111111-1111-1111-1111-111111111111.audit.event", ev.Subject)
	require.NotNil(t, ev.TenantID)
	require.Equal(t, tenantID, *ev.TenantID)
	require.NotEmpty(t, ev.Payload)

	var unmarshaled auditapi.Event
	require.NoError(t, json.Unmarshal(ev.Payload, &unmarshaled))
	require.Equal(t, tenantID, unmarshaled.TenantID)
	require.NotNil(t, unmarshaled.ActorID)
	require.Equal(t, actorID, *unmarshaled.ActorID)
	require.Equal(t, auditapi.ActorUser, unmarshaled.ActorKind)
	require.Equal(t, "reports.export", unmarshaled.Action)
	require.Equal(t, "report:job-abc-123", unmarshaled.Target)
	require.True(t, unmarshaled.Timestamp.Equal(ts))

	// Spot-check the four expected payload keys are present.
	require.Contains(t, unmarshaled.Payload, "job_id")
	require.Contains(t, unmarshaled.Payload, "kind")
	require.Contains(t, unmarshaled.Payload, "format")
	require.Contains(t, unmarshaled.Payload, "bytes_size")
	require.Equal(t, "job-abc-123", unmarshaled.Payload["job_id"])
	require.Equal(t, "operator_efficiency", unmarshaled.Payload["kind"])
	require.Equal(t, "xlsx", unmarshaled.Payload["format"])
}

// TestAuditEmit_SystemActorOmitsActorID asserts that when ActorID is the
// zero uuid (system-initiated export), the marshalled event omits the
// actor_id field via *uuid.UUID + omitempty — auditapi.Event consumers
// reading the payload must see actor_id ABSENT, not "00000000-...-0".
func TestAuditEmit_SystemActorOmitsActorID(t *testing.T) {
	t.Parallel()

	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	fw := &fakeOutboxWriter{}
	emitter := reportsvc.NewAuditEmitter(fw)

	err := emitter.EmitTx(context.Background(), postgres.Tx{}, reportsvc.AuditExport{
		TenantID:  tenantID,
		ActorID:   uuid.Nil, // system-initiated
		ActorKind: auditapi.ActorSystem,
		JobID:     "job-sys",
		Kind:      "calls_by_status",
		Format:    "csv",
		Window: reportsvc.AuditWindow{
			From: ts.Add(-time.Hour),
			To:   ts,
		},
		Timestamp: ts,
	})
	require.NoError(t, err)

	require.Len(t, fw.appended, 1)
	var unmarshaled auditapi.Event
	require.NoError(t, json.Unmarshal(fw.appended[0].Payload, &unmarshaled))
	require.Nil(t, unmarshaled.ActorID, "ActorID must be absent (omitempty) when in.ActorID == uuid.Nil")
	require.Equal(t, auditapi.ActorSystem, unmarshaled.ActorKind)
}

func TestAuditEmit_PropagatesAppendError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom-from-outbox")
	fw := &fakeOutboxWriter{nextErr: sentinel}
	emitter := reportsvc.NewAuditEmitter(fw)

	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	err := emitter.EmitTx(context.Background(), postgres.Tx{}, reportsvc.AuditExport{
		TenantID:  tenantID,
		ActorID:   uuid.New(),
		ActorKind: auditapi.ActorUser,
		JobID:     "job-x",
		Kind:      "finance",
		Format:    "csv",
		BytesSize: 0,
		Window: reportsvc.AuditWindow{
			From: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
		},
		Timestamp: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel, "wrapped sentinel must survive %%w")
	require.Empty(t, fw.appended, "the fake must not record on error")
}
