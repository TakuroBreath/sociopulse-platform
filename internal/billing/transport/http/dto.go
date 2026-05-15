// Package http is the gin-based HTTP transport for the billing module.
//
// It exposes six endpoints:
//
//	GET   /api/finance/dashboard     view  (admin + supervisor)
//	GET   /api/finance/projects      view
//	GET   /api/finance/breakdown     view
//	GET   /api/finance/byMonth       view
//	GET   /api/billing/tariffs       view
//	PATCH /api/billing/tariffs       admin only
//
// All routes derive tenantID from the JWT claims (claims.TenantID); no
// path-:id means the project's RequireSameTenant middleware is not needed
// — see docs/references/plan-14-billing.md §2.11.
//
// PATCH /api/billing/tariffs emits a billing.tariff_updated audit event
// to the outbox AFTER the tariff write commits (best-effort, at-most-once
// — see internal/billing/service.AuditEmitter.EmitTariffUpdated doc).
package http

// ErrorEnvelope is the canonical billing error response. Mirrors the
// envelope used by the reports + dialer transports. Code is a stable
// dotted lowercase identifier; Message is human-readable.
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
