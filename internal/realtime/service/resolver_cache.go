// resolver_cache.go provides a 60s LRU + singleflight wrapper around
// rtapi.UserResolver and rtapi.ProjectResolver. The wrapper absorbs
// the per-frame load that TopicRBAC.Allow would otherwise generate
// on every WS subscribe — production users + projects are O(thousands)
// per tenant and a hot operator UI subscribes to several topics on
// connect.
//
// Why singleflight: a deploy + N WS reconnects produces an N-way
// concurrent miss for the same (user_id, project_id) pair. Without
// coalescing the inner resolver fields N parallel DB hits.
//
// Cache invalidation: there is none. Stale entries TTL out within
// 60s; a deleted user's stale TenantID still validates the in-flight
// JWT (which itself expires within minutes), so the security
// envelope is bounded by the JWT lifetime + cache TTL.
package service

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// defaultResolverTTL is the fallback cache window. Picked so a
// short-lived JWT (default 15min) re-validates at most 15 times,
// keeping the inner resolver load bounded under reconnect storms.
const defaultResolverTTL = 60 * time.Second

// resolverInnerTimeout caps the singleflight closure's inner call so
// a slow inner resolver cannot pin the singleflight key forever.
// Picked at 5s to comfortably cover a healthy DB roundtrip plus a
// retry; longer than handshake/subscribe deadline so a slow inner
// resolver surfaces as the leader's ctx-deadline (the duplicate
// waiters' bound) rather than as a closure timeout.
const resolverInnerTimeout = 5 * time.Second

// cachedResolverEntry is the cache value: the resolved TenantID + an
// expiry deadline checked lazily on read.
type cachedResolverEntry struct {
	tenant    rtapi.ResolvedTenant
	expiresAt time.Time
}

// CachedUserResolver wraps a rtapi.UserResolver with a 60s sync.Map
// cache + a singleflight.Group for concurrent-miss coalescing.
//
// Zero-value not safe — callers must use NewCachedUserResolver. nil
// inner panics at construction time so the wiring bug surfaces at
// boot rather than first subscribe.
type CachedUserResolver struct {
	inner rtapi.UserResolver
	ttl   time.Duration

	cache sync.Map // userID string → *cachedResolverEntry
	group singleflight.Group
}

// NewCachedUserResolver wires a CachedUserResolver. ttl ≤ 0 falls
// back to defaultResolverTTL (60s) — there is intentionally NO
// zero-cache mode; the wrapper exists to bound inner-resolver load.
// A caller wanting to bypass the cache should pass the inner
// resolver directly. nil inner panics at construction time so the
// wiring bug surfaces at boot rather than first subscribe.
func NewCachedUserResolver(inner rtapi.UserResolver, ttl time.Duration) *CachedUserResolver {
	if inner == nil {
		panic("service.NewCachedUserResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	return &CachedUserResolver{
		inner: inner,
		ttl:   ttl,
	}
}

// Get resolves userID via the cache, coalescing concurrent misses
// via singleflight. ctx propagates to the inner resolver and to the
// singleflight Do call so a cancelled subscribe doesn't block on a
// slow DB.
func (c *CachedUserResolver) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	// Fast path: cache hit + not expired.
	if v, ok := c.cache.Load(userID); ok {
		entry, ok2 := v.(*cachedResolverEntry)
		if !ok2 {
			panic("service: CachedUserResolver cache contains unexpected type")
		}
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
		// Expired — fall through to refetch via singleflight.
	}

	// Slow path: miss or expired. Coalesce concurrent calls for the
	// same userID via singleflight.DoChan + select on ctx so a slow
	// inner resolver doesn't pin the caller.
	ch := c.group.DoChan(userID, func() (any, error) {
		// Detach: leader's ctx cancellation must not poison duplicate
		// waiters joining the in-flight call. The outer select on
		// ctx.Done() still bounds *this* caller (returns ctx.Err()
		// immediately to the leader); the closure runs against a
		// detached ctx with a sane upper-bound timeout so duplicate
		// waiters get the real result.
		//
		// singleflight.doCall delivers the closure's outcome to every
		// waiter on c.chans — without WithoutCancel, a leader whose
		// WS connection blips would force every concurrent reconnect
		// for the same user_id to see context.Canceled and fail their
		// RBAC checks (Plan 11.2 Task 3 review IMPORTANT I-1).
		inner, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			resolverInnerTimeout,
		)
		defer cancel()
		got, err := c.inner.Get(inner, userID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(userID, entry)
		return got, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		tenant, ok := res.Val.(rtapi.ResolvedTenant)
		if !ok {
			panic("service: CachedUserResolver singleflight returned unexpected type")
		}
		return tenant, nil
	case <-ctx.Done():
		// Forget the in-flight call so a subsequent retry doesn't
		// inherit this caller's cancelled-ctx error from
		// singleflight's caching of the result.
		c.group.Forget(userID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// CachedProjectResolver mirrors CachedUserResolver for project IDs.
// Behaviour identical; separate type so the resolver-port type
// safety is preserved at call sites.
type CachedProjectResolver struct {
	inner rtapi.ProjectResolver
	ttl   time.Duration

	cache sync.Map
	group singleflight.Group
}

// NewCachedProjectResolver wires a CachedProjectResolver. ttl ≤ 0
// falls back to defaultResolverTTL (60s) — there is intentionally
// NO zero-cache mode; the wrapper exists to bound inner-resolver
// load. A caller wanting to bypass the cache should pass the inner
// resolver directly. nil inner panics at construction time so the
// wiring bug surfaces at boot rather than first subscribe.
func NewCachedProjectResolver(inner rtapi.ProjectResolver, ttl time.Duration) *CachedProjectResolver {
	if inner == nil {
		panic("service.NewCachedProjectResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	return &CachedProjectResolver{
		inner: inner,
		ttl:   ttl,
	}
}

// Get is the project-id mirror of CachedUserResolver.Get.
func (c *CachedProjectResolver) Get(ctx context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	if v, ok := c.cache.Load(projectID); ok {
		entry, ok2 := v.(*cachedResolverEntry)
		if !ok2 {
			panic("service: CachedProjectResolver cache contains unexpected type")
		}
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
	}
	ch := c.group.DoChan(projectID, func() (any, error) {
		// Detach: leader's ctx cancellation must not poison duplicate
		// waiters joining the in-flight call. The outer select on
		// ctx.Done() still bounds *this* caller (returns ctx.Err()
		// immediately to the leader); the closure runs against a
		// detached ctx with a sane upper-bound timeout so duplicate
		// waiters get the real result.
		//
		// singleflight.doCall delivers the closure's outcome to every
		// waiter on c.chans — without WithoutCancel, a leader whose
		// WS connection blips would force every concurrent reconnect
		// for the same project_id to see context.Canceled and fail
		// their RBAC checks (Plan 11.2 Task 3 review IMPORTANT I-1).
		inner, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			resolverInnerTimeout,
		)
		defer cancel()
		got, err := c.inner.Get(inner, projectID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(projectID, entry)
		return got, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		tenant, ok := res.Val.(rtapi.ResolvedTenant)
		if !ok {
			panic("service: CachedProjectResolver singleflight returned unexpected type")
		}
		return tenant, nil
	case <-ctx.Done():
		c.group.Forget(projectID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// Compile-time interface checks. Keeping these next to the
// implementations means a port signature change breaks the build at
// the cache wrapper, not far away in TopicRBAC.
var (
	_ rtapi.UserResolver    = (*CachedUserResolver)(nil)
	_ rtapi.ProjectResolver = (*CachedProjectResolver)(nil)
)
