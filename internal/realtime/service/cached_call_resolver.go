// cached_call_resolver.go is the call-id mirror of CachedUserResolver
// + CachedProjectResolver — same TTL + sync.Map + singleflight +
// ctx-detached closure pattern. Lives in its own file because
// resolver_cache.go is already 287 lines and a third copy would push
// past 400. The semantics are identical; see resolver_cache.go for the
// in-depth concurrency commentary.
//
// Plan 11.4 Task 3.
package service

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// CachedCallResolver wraps a rtapi.CallResolver with a 60s sync.Map
// cache + a singleflight.Group for concurrent-miss coalescing.
//
// Zero-value not safe — callers must use NewCachedCallResolver. nil
// inner panics at construction time so the wiring bug surfaces at
// boot rather than first subscribe.
type CachedCallResolver struct {
	inner rtapi.CallResolver
	ttl   time.Duration

	cache sync.Map // callID string → *cachedResolverEntry (defined in resolver_cache.go)
	group singleflight.Group
}

// NewCachedCallResolver wires a CachedCallResolver. ttl ≤ 0 falls back
// to defaultResolverTTL (60s). nil inner panics — wiring bug surfaces
// at boot, not first subscribe. See resolver_cache.go::NewCachedUserResolver
// for the full design rationale.
func NewCachedCallResolver(inner rtapi.CallResolver, ttl time.Duration) *CachedCallResolver {
	if inner == nil {
		panic("service.NewCachedCallResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	return &CachedCallResolver{
		inner: inner,
		ttl:   ttl,
	}
}

// Get resolves callID via the cache, coalescing concurrent misses via
// singleflight. ctx propagates to the inner resolver and the
// singleflight Do call so a cancelled subscribe doesn't block on a
// slow DB; the inner closure runs against a detached ctx so a leader
// whose subscribe cancels does NOT poison concurrent duplicate waiters
// (Plan 11.2 Task 3 review IMPORTANT I-1).
func (c *CachedCallResolver) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	if v, ok := c.cache.Load(callID); ok {
		entry, ok2 := v.(*cachedResolverEntry)
		if !ok2 {
			panic("service: CachedCallResolver cache contains unexpected type")
		}
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
	}
	ch := c.group.DoChan(callID, func() (any, error) {
		inner, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			resolverInnerTimeout,
		)
		defer cancel()
		got, err := c.inner.Get(inner, callID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(callID, entry)
		return got, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		tenant, ok := res.Val.(rtapi.ResolvedTenant)
		if !ok {
			panic("service: CachedCallResolver singleflight returned unexpected type")
		}
		return tenant, nil
	case <-ctx.Done():
		c.group.Forget(callID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// Invalidate drops the cached entry for callID. Idempotent — no error
// if the key was never cached. Calls singleflight.Forget so any
// in-flight inner call (the leader) is uncached for future joiners —
// they re-query rather than inheriting the leader's (possibly stale)
// result. Used by the events-package cache invalidator (Plan 11.4
// Task 6) to drop entries on tenant.<t>.recording.call.deleted events.
//
// Concurrency: see CachedUserResolver.Invalidate for the full
// concurrency contract.
func (c *CachedCallResolver) Invalidate(callID string) {
	c.cache.Delete(callID)
	c.group.Forget(callID)
}

// Compile-time interface check. Mirrors the pattern at the bottom of
// resolver_cache.go.
var _ rtapi.CallResolver = (*CachedCallResolver)(nil)
