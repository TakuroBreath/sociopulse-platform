// Package api is the public façade of the surveys module.
//
// External callers (gateway HTTP handlers, the dialer, the worker) MUST
// import only this package — never the implementation packages
// (service, store, runtime, dsl, schemavalidator). The depguard linter
// enforces this rule.
//
// What lives here:
//   - SurveyService and VersionStore interfaces (CRUD + version pinning)
//   - Runtime interface (pure-function survey execution)
//   - DTOs that cross module boundaries (Survey, Version, Answer, NodeResult)
//   - Sentinel errors (ErrNotFound, ErrValidation, ErrSchema, ErrCycle, ...)
//
// Spec: §FR-C, §11.1–11.7, ADR-008.
package api
