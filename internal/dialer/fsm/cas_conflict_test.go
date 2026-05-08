package fsm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestApplyEvent_CASConflictReturnsErrConflict drives casStore against a
// hash whose version has advanced past the expected value (a concurrent
// writer landed first) and asserts the resulting error chains
// api.ErrConflict. Public callers across module boundaries depend on
// errors.Is(err, api.ErrConflict) to detect optimistic-concurrency
// conflicts and decide whether to retry.
//
// The test stays in package fsm (no _test suffix) so it can call the
// package-private casStore directly with an explicit stale version —
// the cleanest way to simulate a CAS race without spawning real
// concurrent goroutines against miniredis.
func TestApplyEvent_CASConflictReturnsErrConflict(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, operatorID := uuid.New(), uuid.New()
	key := opKey(tenantID, operatorID)

	// Plant a hash at version=5 (a concurrent writer's perspective).
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	mr.HSet(key,
		"state", string(api.StateReady),
		"tenant_id", tenantID.String(),
		"state_entered_at", now,
		"heartbeat_at", now,
		"version", "5",
	)

	m := &Machine{
		rdb:     rdb,
		log:     zap.NewNop(),
		clock:   time.Now,
		hashTTL: 24 * time.Hour,
	}

	// CAS with expectedVersion=1 (stale). The Lua script returns -1 →
	// casStore returns errVersionMismatch which wraps api.ErrConflict.
	err := m.casStore(context.Background(), tenantID, operatorID, 1, Snapshot{
		State:          api.StatePause,
		StateEnteredAt: time.Now(),
		HeartbeatAt:    time.Now(),
		TenantID:       tenantID,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrConflict,
		"casStore must wrap api.ErrConflict on optimistic-concurrency mismatch")

	// Sanity: the package-private sentinel itself unwraps to api.ErrConflict.
	require.ErrorIs(t, errVersionMismatch, api.ErrConflict,
		"errVersionMismatch must wrap api.ErrConflict so callers can detect retryable CAS races across module boundaries")

	// Public-surface wrap (mirrors machine.go's `fmt.Errorf("fsm/apply: %w", err)`)
	// must preserve the chain.
	wrapped := fmt.Errorf("fsm/apply: %w", err)
	require.ErrorIs(t, wrapped, api.ErrConflict,
		"public-surface wrap must preserve the api.ErrConflict chain")
}
