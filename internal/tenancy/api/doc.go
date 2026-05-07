// Package api defines the public surface of the tenancy module.
//
// Only this package may be imported by other modules. Per the depguard
// rule in .golangci.yml, internal/tenancy/{service,store,events,transport}
// are off-limits to anything outside internal/tenancy/.
//
// The aggregate interface Tenancy embeds the four primary interfaces:
//
//   - TenantService    — CRUD over tenants (Service-Owner level)
//   - SettingsCache    — per-tenant key/value (cached, NATS-invalidated)
//   - KMSResolver      — per-tenant KEK lifecycle + DEK envelope ops
//   - PhoneHasher      — HMAC-SHA256 with per-tenant pepper
//
// Construct one via Module.Register(ctx, deps). The Register seam is a
// package-level variable function so api/ never imports service/; the
// service package's init() supplies the implementation at startup.
//
// Method-name reconciliation: TenantService.Get and the historical
// SettingsCache.Get/GetWithDefault/GetAll collide on the "Get" name with
// different signatures, which Go disallows when embedding directly. The
// SettingsCache surface uses Lookup/LookupWithDefault/LookupAll instead so
// the Tenancy aggregate composes cleanly.
package api
