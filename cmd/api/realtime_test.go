package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProjectGetterReturnsNilNil reproduces the (uuid.Nil, nil)
// defensive branch in projectResolverAdapter.Get. The branch should
// never fire in production (ProjectService.ResolveTenant returns
// ErrProjectNotFound on miss); a fake that returns (uuid.Nil, nil)
// lets the test exercise the guard path.
//
// Plan 13.2.5 Task 1: the realtime resolver now uses ResolveTenant
// (the only sanctioned cross-tenant BypassRLS resolver) instead of
// Get. The fake adapts accordingly.
type fakeProjectGetterReturnsNilNil struct{}

func (fakeProjectGetterReturnsNilNil) ResolveTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil //nolint:nilnil // deliberate test stub for the (uuid.Nil, nil) guard
}

// TestProjectResolverAdapter_NilNilTicksInconsistentMetric verifies
// the Plan 11.3 Task 4 contract: when crm.ProjectService.ResolveTenant
// returns (uuid.Nil, nil) (a service-layer bug), the
// projectResolverAdapter surfaces it via the inconsistent metric
// callback.
func TestProjectResolverAdapter_NilNilTicksInconsistentMetric(t *testing.T) {
	t.Parallel()

	var inconsistentTicks int
	var inconsistentTypes []string
	bumpInconsistent := func(adapterType string) {
		inconsistentTicks++
		inconsistentTypes = append(inconsistentTypes, adapterType)
	}

	adapter := newProjectResolverAdapterWithMetrics(
		fakeProjectGetterReturnsNilNil{},
		bumpInconsistent,
	)
	_, err := adapter.Get(t.Context(), uuid.New().String())
	require.Error(t, err,
		"adapter must surface the (uuid.Nil, nil) anomaly as an error")
	require.Equal(t, 1, inconsistentTicks,
		"metric callback must tick exactly once on (uuid.Nil, nil)")
	assert.Equal(t, []string{"project"}, inconsistentTypes,
		"adapter_type label must be 'project'")
}

// TestProjectResolverAdapter_NilCallbackIsSafe verifies the nil-
// callback fallback (degraded boot when service.Metrics not in locator).
func TestProjectResolverAdapter_NilCallbackIsSafe(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		adapter := newProjectResolverAdapterWithMetrics(
			fakeProjectGetterReturnsNilNil{},
			nil, // no metrics callback
		)
		_, err := adapter.Get(t.Context(), uuid.New().String())
		require.Error(t, err)
	})
}
