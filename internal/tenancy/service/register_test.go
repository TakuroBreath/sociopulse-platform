package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	// blank import is what cmd/api will do — it triggers the init() that
	// installs api.Register.
	_ "github.com/sociopulse/platform/internal/tenancy/service"
	"github.com/sociopulse/platform/pkg/postgres"
)

func TestRegisterSeam_IsInstalledByInit(t *testing.T) {
	t.Parallel()

	require.NotNil(t, api.Register, "service.init must install api.Register")
}

func TestRegisterSeam_RejectsMissingLogger(t *testing.T) {
	t.Parallel()

	_, err := api.Register(context.Background(), api.Deps{})
	require.Error(t, err)
}

func TestRegisterSeam_RejectsMissingPool(t *testing.T) {
	t.Parallel()

	_, err := api.Register(context.Background(), api.Deps{
		Logger: zaptest.NewLogger(t),
	})
	require.Error(t, err)
}

func TestRegisterSeam_WiresKMSResolver_WhenAllDepsPresent(t *testing.T) {
	t.Parallel()

	// fakeKMS is the test-local api.KMSClient double used elsewhere in
	// this package. Construct via the same pattern as the TenantService
	// tests so we can drive Register without any real KMS endpoint.
	mod, err := api.Register(context.Background(), api.Deps{
		Logger: zaptest.NewLogger(t),
		Pool:   &postgres.Pool{},
		KMS:    &fakeKMS{},
	})
	require.NoError(t, err)
	require.NotNil(t, mod, "register must return a Module")
	t.Cleanup(func() {
		// Stop releases the KMSResolver's eviction goroutine so goleak
		// stays clean. Plan 04 Task 4 wired the resolver's lifecycle into
		// the module's Closer.
		_ = mod.Stop()
	})
	require.NotNil(t, mod.KMSResolver(),
		"Plan 04 Task 3 requires Register to wire the KMSResolver onto the Module")
	require.NotNil(t, mod.TenantService(),
		"the existing TenantService surface must remain intact")
}

func TestRegisterSeam_RejectsMissingKMSClient(t *testing.T) {
	t.Parallel()

	// A zero-value *postgres.Pool is non-nil but unusable. That is enough
	// for the missing-pool guard not to trigger; the test exercises the
	// missing-KMS guard introduced by Plan 04 Task 3 — Register must
	// short-circuit before the pool is dereferenced.
	_, err := api.Register(context.Background(), api.Deps{
		Logger: zaptest.NewLogger(t),
		Pool:   &postgres.Pool{},
	})
	require.Error(t, err)
}
