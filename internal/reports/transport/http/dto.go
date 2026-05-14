// Package http is the reports module's HTTP transport layer.
//
// It exposes five endpoints under /api/reports:
//
//	GET  /api/reports                       list kinds (6 predefined + custom)
//	POST /api/reports/:kind/export          sync render or auto-route to async
//	POST /api/reports/custom                always async — 202 + JobTicket
//	GET  /api/reports/jobs/:jobID           job status (RequireSameTenant-guarded)
//	GET  /api/reports/jobs/:jobID/download  302 → 24h presigned URL
//
// All routes require admin RBAC via the RouterDeps.RequireAdmin
// middleware injected by cmd/api. Per-jobID routes additionally apply
// a tenant guard that resolves the job's owning tenant via BypassRLS
// and compares with the caller's claims — mismatch returns 404
// reports.job_not_found (existence-probe defence, NOT 403).
package http

// ErrorEnvelope is the canonical {code, message} shape returned by all
// reports endpoints on error. Matches pkg/httputil.ErrorEnvelope and
// the recording-module precedent in internal/recording/transport/http/dto.go.
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
