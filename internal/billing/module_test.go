// Plan 14 Step I — module_test exercises the early-error and degraded-
// boot branches of Module.Register. The "happy-path" wiring requires a
// real *postgres.Pool + a gin engine + a locator + (optionally) a NATS
// subscriber; that path is exercised end-to-end via cmd/api's own boot
// tests and the per-component integration tests (store/pgx +
// transport/http + transport/nats).
package billing_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/billing"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/pkg/config"
)

func TestModule_Name(t *testing.T) {
	t.Parallel()
	require.Equal(t, "billing", billing.Module{}.Name())
}

func TestModule_Register_NilLogger_Errors(t *testing.T) {
	t.Parallel()
	err := billing.Module{}.Register(modules.Deps{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Logger is required")
}

func TestModule_Register_NilConfig_Errors(t *testing.T) {
	t.Parallel()
	err := billing.Module{}.Register(modules.Deps{Logger: zap.NewNop()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Config is required")
}

// TestModule_Register_NilPool_DegradedBoot asserts the worker-only boot
// path (no Postgres pool) succeeds with an INFO log and a no-op
// registration. cmd/worker today does NOT include billing.Module — but
// the degraded path is a free invariant we want to preserve so a future
// cmd/worker that calls billing.Module.Register cleanly skips when the
// pool is absent.
func TestModule_Register_NilPool_DegradedBoot(t *testing.T) {
	t.Parallel()
	err := billing.Module{}.Register(modules.Deps{
		Logger: zap.NewNop(),
		Config: &config.Config{},
		Ctx:    context.Background(),
	})
	require.NoError(t, err, "nil Pool must degrade to no-op, not error")
}

// TestModule_Register_InvalidDefaults_NotReachedWithoutPool documents
// the precedence rule: Pool-nil check fires before Defaults.Validate.
// A test asserting the inverse (Pool real + invalid defaults → error)
// would need a real *postgres.Pool — covered by config-level validator
// tests (pkg/config tests) and the integration test in
// internal/billing/store/pgx.
func TestModule_Register_InvalidDefaults_NotReachedWithoutPool(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Billing.Defaults = billingapi.Tariffs{WagePerSurveyMinor: -1}

	err := billing.Module{}.Register(modules.Deps{
		Logger: zap.NewNop(),
		Config: cfg,
		Ctx:    context.Background(),
	})
	// Pool-nil short-circuits before Validate runs — boot still degrades
	// cleanly. The "Defaults invalid → hard error" path is covered by
	// config.Config.Validate() at load time.
	require.NoError(t, err)
}
