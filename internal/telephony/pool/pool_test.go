package pool_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/sociopulse/platform/internal/telephony/pool"
)

// TestMain catches goroutine leaks introduced by the pool package. The Task 1
// skeleton spawns no goroutines, so any leak detected here is a regression
// against the contract the real implementation will need to satisfy in Plan
// 09 Task 4.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestNewRejectsEmptyNodes is the one quality gate Task 1 requires. The plan
// references explicitly call out a misconfigured FSNodes list as a high-risk
// boot path; surfacing the error at New is the cheapest possible defence.
func TestNewRejectsEmptyNodes(t *testing.T) {
	t.Parallel()

	_, err := pool.New(context.Background(), pool.Config{Nodes: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one FreeSWITCH node")
}

// TestNewAcceptsConfiguredNodes asserts the happy path: a Config with at
// least one node yields a usable *ESLPool whose HealthyNodes mirror what the
// caller supplied. Pinning this here means the Task 4 rewrite cannot
// accidentally drop the configured list during construction.
func TestNewAcceptsConfiguredNodes(t *testing.T) {
	t.Parallel()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes: []string{"fs-1.local:8021", "fs-2.local:8021"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	assert.True(t, p.AnyHealthy(), "skeleton must report healthy until Task 4 wires real checks")
	assert.ElementsMatch(t,
		[]string{"fs-1.local:8021", "fs-2.local:8021"},
		p.HealthyNodes(),
		"HealthyNodes must reflect the configured list while skeleton stands in")
}

// TestGetNotImplemented documents that Plan 09 Task 4 still owes a real
// Get(). If somebody finishes Task 4 and forgets to update the contract,
// this assertion forces a code change here at the same time.
func TestGetNotImplemented(t *testing.T) {
	t.Parallel()

	p, err := pool.New(context.Background(), pool.Config{Nodes: []string{"fs-1.local:8021"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	_, err = p.Get("fs-1.local:8021")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet implemented")
}

// TestCloseIsIdempotent is a soft guarantee: the composition root's deferred
// pool.Close() must not blow up on a double-close path (e.g. error paths
// during boot that already called Close before falling through to defer).
func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p, err := pool.New(context.Background(), pool.Config{Nodes: []string{"fs-1.local:8021"}})
	require.NoError(t, err)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close(), "double Close must succeed")
}
