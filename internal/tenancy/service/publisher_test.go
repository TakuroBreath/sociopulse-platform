package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// fakeBus records every Publish call for assertions. Concurrent-safe so the
// service-layer fakes can use it from any goroutine if a future refactor
// fans out publishes.
type fakeBus struct {
	mu       sync.Mutex
	calls    []fakeBusCall
	errOnPub error
}

type fakeBusCall struct {
	subject string
	payload []byte
}

func (f *fakeBus) Publish(_ context.Context, subject string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnPub != nil {
		return f.errOnPub
	}
	body := make([]byte, len(payload))
	copy(body, payload)
	f.calls = append(f.calls, fakeBusCall{subject: subject, payload: body})
	return nil
}

func (f *fakeBus) snapshot() []fakeBusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeBusCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestEventbusPublisher_PublishCreated_UsesCanonicalSubject(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	p := newPublisher(bus, zaptest.NewLogger(t))

	tn := api.Tenant{ID: uuid.New(), OrgCode: "CC-X", Name: "X", Status: api.TenantStatusActive}
	require.NoError(t, p.PublishCreated(context.Background(), tn))

	got := bus.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, api.SubjectTenantCreatedFor(tn.ID), got[0].subject)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(got[0].payload, &payload))
	require.Equal(t, tn.ID.String(), payload["id"])
}

func TestEventbusPublisher_PublishSuspended_EmitsTenantSuspendedEvent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	p := newPublisher(bus, zaptest.NewLogger(t))

	id := uuid.New()
	require.NoError(t, p.PublishSuspended(context.Background(), id))

	got := bus.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, api.SubjectTenantSuspendedFor(id), got[0].subject)

	var ev api.TenantSuspendedEvent
	require.NoError(t, json.Unmarshal(got[0].payload, &ev))
	require.Equal(t, id, ev.TenantID)
}

func TestEventbusPublisher_PublishArchived_EmitsTenantArchivedEvent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	p := newPublisher(bus, zaptest.NewLogger(t))

	id := uuid.New()
	require.NoError(t, p.PublishArchived(context.Background(), id))

	got := bus.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, api.SubjectTenantArchivedFor(id), got[0].subject)

	var ev api.TenantArchivedEvent
	require.NoError(t, json.Unmarshal(got[0].payload, &ev))
	require.Equal(t, id, ev.TenantID)
}

func TestEventbusPublisher_PublishSettingUpdated_EmitsSettingsUpdatedEvent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	p := newPublisher(bus, zaptest.NewLogger(t))

	id := uuid.New()
	require.NoError(t, p.PublishSettingUpdated(context.Background(), id, "dialer.retry_no_answer_delay"))

	got := bus.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, api.SubjectSettingsUpdatedFor(id), got[0].subject)

	var ev api.SettingsUpdatedEvent
	require.NoError(t, json.Unmarshal(got[0].payload, &ev))
	require.Equal(t, id, ev.TenantID)
	require.Equal(t, "dialer.retry_no_answer_delay", ev.Key)
}

func TestEventbusPublisher_PublishSettingDeleted_EmitsSettingsUpdatedEvent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	p := newPublisher(bus, zaptest.NewLogger(t))

	id := uuid.New()
	require.NoError(t, p.PublishSettingDeleted(context.Background(), id, "missing.key"))

	got := bus.snapshot()
	require.Len(t, got, 1)
	require.Equal(t, api.SubjectSettingsUpdatedFor(id), got[0].subject)
}

func TestEventbusPublisher_PropagatesPublishError(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{errOnPub: errors.New("nats unavailable")}
	p := newPublisher(bus, zaptest.NewLogger(t))

	err := p.PublishCreated(context.Background(), api.Tenant{ID: uuid.New(), OrgCode: "CC-X"})
	require.Error(t, err)
}

func TestEventbusPublisher_NilBus_IsNoOp(t *testing.T) {
	t.Parallel()

	// Production may run with the noop publisher; the SettingsPublisher must
	// degrade gracefully rather than NPE.
	p := newPublisher(nil, zaptest.NewLogger(t))
	require.NoError(t, p.PublishCreated(context.Background(), api.Tenant{ID: uuid.New(), OrgCode: "CC-X"}))
	require.NoError(t, p.PublishSuspended(context.Background(), uuid.New()))
	require.NoError(t, p.PublishArchived(context.Background(), uuid.New()))
	require.NoError(t, p.PublishSettingUpdated(context.Background(), uuid.New(), "k"))
	require.NoError(t, p.PublishSettingDeleted(context.Background(), uuid.New(), "k"))
}
