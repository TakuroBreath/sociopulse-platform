package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/tenancy"
	"github.com/sociopulse/platform/internal/tenancy/api"
	_ "github.com/sociopulse/platform/internal/tenancy/service" // install api.Register
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeLocator is a minimal modules.ServiceLocator double the wiring
// test uses to assert "tenancy.TenantService" + "tenancy.KMSResolver"
// are registered after Module.Register.
type fakeLocator struct {
	mu  sync.Mutex
	bag map[string]any
}

func newFakeLocator() *fakeLocator {
	return &fakeLocator{bag: make(map[string]any)}
}

func (l *fakeLocator) Register(name string, svc any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bag[name] = svc
}

func (l *fakeLocator) Lookup(name string) (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.bag[name]
	return v, ok
}

// TestModule_Register_RegistersKMSResolver_WithLocalProvider drives the
// outer tenancy.Module shim end-to-end against the dev defaults: local
// KMS provider, valid hex key, fake pool and locator. After Register
// the locator must hold a "tenancy.KMSResolver" bound to a value
// implementing api.KMSResolver.
func TestModule_Register_RegistersKMSResolver_WithLocalProvider(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultDev()
	require.Equal(t, config.KMSProviderLocal, cfg.KMS.Provider, "precondition: dev default uses local KMS")

	loc := newFakeLocator()
	mod := &tenancy.Module{}
	err := mod.Register(modules.Deps{
		Ctx:     context.Background(),
		Logger:  zaptest.NewLogger(t),
		Config:  &cfg,
		Pool:    &postgres.Pool{}, // not dereferenced before resolver wiring
		Locator: loc,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Stop tears down the KMSResolver's eviction goroutine — required
		// by goleak.VerifyTestMain in main_test.go.
		_ = mod.Stop()
	})

	resolver, ok := loc.Lookup("tenancy.KMSResolver")
	require.True(t, ok, "tenancy.KMSResolver must be registered in the locator")
	_, isResolver := resolver.(api.KMSResolver)
	require.True(t, isResolver, "registered service must satisfy api.KMSResolver")
}

func TestModule_Register_PropagatesUnknownKMSProvider(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultDev()
	cfg.KMS.Provider = "no-such-provider"

	mod := &tenancy.Module{}
	err := mod.Register(modules.Deps{
		Ctx:     context.Background(),
		Logger:  zaptest.NewLogger(t),
		Config:  &cfg,
		Pool:    &postgres.Pool{},
		Locator: newFakeLocator(),
	})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "kms") || errors.Is(err, errPlaceholder()),
		"err should mention KMS provider; got %v", err,
	)
}

// errPlaceholder is here only so the contains-or-is check above stays
// resilient to any sentinel the module may expose later.
func errPlaceholder() error { return errors.New("placeholder") }

func TestModule_Register_PropagatesInvalidLocalKey(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultDev()
	cfg.KMS.LocalKeyHex = "not-hex" // dev key is invalid hex

	mod := &tenancy.Module{}
	err := mod.Register(modules.Deps{
		Ctx:     context.Background(),
		Logger:  zaptest.NewLogger(t),
		Config:  &cfg,
		Pool:    &postgres.Pool{},
		Locator: newFakeLocator(),
	})
	require.Error(t, err)
}
