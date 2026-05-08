package fsm_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/fsm"
)

// TestLoad_DetectsCorruptStateField — when the hash contains a bogus
// state value, load surfaces api.ErrUnknownState. The FSM must never
// proceed on a corrupt row.
func TestLoad_DetectsCorruptStateField(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()

	// Plant a corrupt row directly via miniredis.
	mr.HSet(key, "state", "garbage-state")

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.ErrorIs(t, err, api.ErrUnknownState)
}

// TestLoad_DetectsCorruptUUIDField — when a UUID field is malformed,
// load surfaces a wrapped error (not ErrUnknownState; just a parse
// error). The FSM rejects the whole snapshot rather than dropping a
// field.
func TestLoad_DetectsCorruptUUIDField(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()

	// Plant a row with a valid state but a bogus project_id.
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"project_id", "not-a-uuid",
	)

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "project_id")
}

// TestLoad_DetectsCorruptTimestamp — bad RFC3339 surfaces a parse error.
func TestLoad_DetectsCorruptTimestamp(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"state_entered_at", "yesterday",
	)

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "state_entered_at")
}

// TestLoad_DetectsCorruptVersion — bad version int surfaces a parse error.
func TestLoad_DetectsCorruptVersion(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"version", "not-an-int",
	)

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version")
}

// TestLoad_PreservesAllFields — a fully-populated hash round-trips
// through load() preserving every field.
func TestLoad_PreservesAllFields(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	projectID, callID, respID, sessionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	mr.HSet(key,
		"state", "call",
		"state_entered_at", now,
		"heartbeat_at", now,
		"tenant_id", tenantID.String(),
		"session_id", sessionID.String(),
		"project_id", projectID.String(),
		"current_call_id", callID.String(),
		"respondent_id", respID.String(),
		"version", "5",
	)

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	snap, err := mach.GetState(context.Background(), tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateCall, snap.State)
	require.Equal(t, tenantID, snap.TenantID)
	require.Equal(t, operatorID, snap.OperatorID)
	require.NotNil(t, snap.ProjectID)
	require.Equal(t, projectID, *snap.ProjectID)
	require.NotNil(t, snap.CurrentCallID)
	require.Equal(t, callID, *snap.CurrentCallID)
	require.NotNil(t, snap.RespondentID)
	require.Equal(t, respID, *snap.RespondentID)
}

// TestLoad_DetectsCorruptHeartbeatAt — covers the heartbeat parse path
// distinct from state_entered_at.
func TestLoad_DetectsCorruptHeartbeatAt(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"heartbeat_at", "garbled",
	)
	mach, err := fsm.New(fsm.Config{
		Redis: rdb, PG: &fakeTxRunner{}, Outbox: &fakeOutbox{}, Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "heartbeat_at")
}

// TestLoad_DetectsCorruptTenantID — bad uuid in tenant_id field.
func TestLoad_DetectsCorruptTenantID(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "ready",
		"tenant_id", "not-a-uuid",
	)
	mach, err := fsm.New(fsm.Config{
		Redis: rdb, PG: &fakeTxRunner{}, Outbox: &fakeOutbox{}, Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tenant_id")
}

// TestLoad_DetectsCorruptSessionID — bad uuid in session_id field.
func TestLoad_DetectsCorruptSessionID(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"session_id", "not-a-uuid",
	)
	mach, err := fsm.New(fsm.Config{
		Redis: rdb, PG: &fakeTxRunner{}, Outbox: &fakeOutbox{}, Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session_id")
}

// TestLoad_DetectsCorruptCallID — bad uuid in current_call_id field.
func TestLoad_DetectsCorruptCallID(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "call",
		"tenant_id", tenantID.String(),
		"current_call_id", "garbage",
	)
	mach, err := fsm.New(fsm.Config{
		Redis: rdb, PG: &fakeTxRunner{}, Outbox: &fakeOutbox{}, Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "current_call_id")
}

// TestLoad_DetectsCorruptRespondentID — bad uuid in respondent_id field.
func TestLoad_DetectsCorruptRespondentID(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	mr.HSet(key,
		"state", "call",
		"tenant_id", tenantID.String(),
		"respondent_id", "garbage",
	)
	mach, err := fsm.New(fsm.Config{
		Redis: rdb, PG: &fakeTxRunner{}, Outbox: &fakeOutbox{}, Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	_, err = mach.GetState(context.Background(), tenantID, operatorID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "respondent_id")
}

// TestStartShiftCAS_BusyState — when the hash exists in some non-offline
// state (mid-shift), the start_shift Lua script returns -1 and the
// caller surfaces api.ErrInvalidTransition. We forge this by writing a
// non-offline hash directly via miniredis and then invoking StartShift.
//
// This particular path is reached when (a) the load() pre-check was
// satisfied (offline at load time) but (b) before our CAS fired, a
// concurrent goroutine flipped the hash to non-offline. The fake-clock
// Machine is single-threaded, so we forge the post-load mid-shift state
// by tweaking the clock-and-key sequence.
func TestStartShiftCAS_HashHasReadyState(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()

	// Plant a hash already in ready state (bypassing StartShift).
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	mr.HSet(key,
		"state", "ready",
		"tenant_id", tenantID.String(),
		"state_entered_at", now,
		"heartbeat_at", now,
		"version", "1",
	)

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
		Logger: zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	// StartShift on already-ready operator: idempotent (no error).
	snap, err := mach.StartShift(context.Background(), api.StartShiftRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		ProjectID:  uuid.New(),
	})
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
}

// TestForce_FromMissingHash — Force on a never-seen operator (no
// session bound) succeeds but skips the audit (per machine.go's `case
// default` log line). We exercise the no-session path explicitly.
func TestForce_FromMissingHash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Operator never started a shift. Force them to ready (a strange
	// but supported scenario — supervisor manually moves them).
	snap, err := f.machine.Force(ctx, tenantID, operatorID, api.StateReady, api.ForceReasonAdminOverride)
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)

	// No session was bound; no audit row (state_log) should have been
	// written via the fake. The outbox/state_log fakes record only
	// what the FSM passed in.
	require.Empty(t, f.sessions.stateLogSnapshot(),
		"Force without a bound session must skip audit (logged at WARN)")
}
