package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// fakeProjectGetterReturnsNilNil reproduces the (nil, nil) defensive
// branch in projectResolverAdapter.Get. The branch should never fire
// in production (ProjectService.Get returns ErrProjectNotFound on
// miss); a fake that returns (nil, nil) lets the test exercise the
// guard path.
type fakeProjectGetterReturnsNilNil struct{}

func (fakeProjectGetterReturnsNilNil) Get(_ context.Context, _ uuid.UUID) (*crmapi.Project, error) {
	return nil, nil //nolint:nilnil // deliberate test stub for the (nil, nil) guard
}

// TestProjectResolverAdapter_NilNilTicksInconsistentMetric verifies
// the Plan 11.3 Task 4 contract: when crm.ProjectService.Get returns
// (nil, nil) (a service-layer bug), the projectResolverAdapter
// surfaces the bug class via the inconsistent metric callback.
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
		"adapter must surface the (nil, nil) anomaly as an error")
	require.Equal(t, 1, inconsistentTicks,
		"metric callback must tick exactly once on (nil, nil)")
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
