// Package observability is the project-wide telemetry toolkit: the zap
// logger factory, the OTel tracer/meter constructors, and the gin
// middleware that ties them together at the HTTP edge.
//
// Modules pull a *zap.Logger out of their Deps and a tracer/meter from
// the global OTel providers; they never construct telemetry primitives
// directly. The contract surfaces, log fields, metric names, span
// names, and PII redaction rules are documented in
// docs/architecture/06-observability.md.
//
// Concrete wiring (zap encoder + sampler, OTLP exporter, redaction
// regex compilation, gin instrumentation) lands in Plan 02 Task 2.
package observability

import "go.uber.org/zap"

// NewLogger returns a *zap.Logger configured per
// docs/architecture/06-observability.md.
//
//   - env selects the encoder ("development" → console, anything else
//     → JSON) and the sampler.
//   - level is the minimum level ("debug" / "info" / "warn" / "error").
//   - The returned logger has the redaction encoder layered over zap's
//     own JSON encoder (see Plan 02 Task 2).
//
// Callers SHOULD attach the standard fields (service, service.environment,
// module, request_id, ...) immediately via .With(...).
func NewLogger(env, level string) (*zap.Logger, error) {
	panic("not implemented: see Plan 02 Task 2")
}

// MaskPhone returns a phone number rendered for safe logging:
// "+7 (***) ***-**-12". Use this for intentionally logged phone
// values; the encoder's redaction filter is a safety net for
// accidents.
func MaskPhone(phone string) string {
	panic("not implemented: see Plan 02 Task 2")
}
