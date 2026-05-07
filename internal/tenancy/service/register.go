package service

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/outbox"
)

// init wires the api.Register seam. cmd/api blank-imports this package so
// the side effect runs before main starts modules.Registry. The seam keeps
// internal/tenancy/api/ free of any service/ or store/ import (api/ is the
// only package other modules may import).
func init() {
	api.Register = registerModule
}

// registerModule is the concrete api.Register implementation. It builds the
// Postgres-backed store, the TenantService, and the KMSResolver, returning
// an *api.Module that the caller (internal/tenancy/module.go) registers in
// the modules.Locator.
//
// The KMSResolver's DEK cache spawns a background eviction goroutine bound
// to ctx — cancelling ctx (cmd/api shutdown) terminates the goroutine.
// Callers should also call mod.Stop() at shutdown for the explicit close
// path; either route stops the goroutine cleanly.
//
// Plan 04 Task 3 added KMSResolver to the wired surface; Task 4 hardens
// the cache to LRU+TTL with a KEK-version-aware key.
func registerModule(ctx context.Context, deps api.Deps) (*api.Module, error) {
	if deps.Logger == nil {
		return nil, errors.New("tenancy/service: logger is required")
	}
	if deps.Pool == nil {
		return nil, errors.New("tenancy/service: pool is required")
	}
	if deps.KMS == nil {
		return nil, errors.New("tenancy/service: kms client is required")
	}

	tenantStore := store.NewPostgresStore(deps.Pool)
	pub := newPublisher(deps.EventBus, deps.Logger)
	outboxWriter := outbox.NewPostgresWriter()
	tenantSvc := NewTenantService(deps.Logger, deps.Pool, tenantStore, deps.KMS, pub, outboxWriter)

	resolverCfg := KMSResolverConfig{
		DEKCacheTTL:  parseTTL(deps.Config.DEKCacheTTL, 5*time.Minute),
		DEKCacheSize: orDefaultInt(deps.Config.DEKCacheSize, 1024),
	}
	kmsResolver := newKMSResolverWithContext(ctx, deps.Logger.Named("kms-resolver"), tenantStore, deps.KMS, resolverCfg)

	mod := api.NewModule(deps, nil /* full Tenancy aggregate lands in a later task */, tenantSvc, &resolverCloser{r: kmsResolver})
	mod.SetKMSResolver(kmsResolver)

	deps.Logger.Info("tenancy module registered",
		zap.Strings("services", []string{
			"tenancy.TenantService",
			"tenancy.KMSResolver",
		}),
		zap.Duration("dek_cache_ttl", resolverCfg.DEKCacheTTL),
		zap.Int("dek_cache_size", resolverCfg.DEKCacheSize),
	)

	return mod, nil
}

// parseTTL parses a duration string from api.Config; on parse error or
// empty input it returns the fallback. Keeps the YAML-shaped string
// field on api.Config without forcing every caller to depend on
// time.ParseDuration error semantics.
func parseTTL(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// orDefaultInt returns v if positive; otherwise fallback.
func orDefaultInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// resolverCloser is the io.Closer the module exposes to the modules.Locator.
// It terminates the KMSResolver's background DEK-cache eviction goroutine
// when the host process shuts down. cmd/api invokes this through the
// modules.Locator's CloseAll.
type resolverCloser struct {
	r *KMSResolverImpl
}

// Close terminates the resolver's eviction goroutine. Always returns nil:
// the resolver's Close is idempotent and panic-free, so there is no error
// surface to propagate.
func (c *resolverCloser) Close() error {
	if c.r != nil {
		c.r.Close()
	}
	return nil
}
