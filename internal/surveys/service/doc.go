// Package service implements the surveys-module SurveyService.
//
// Composition: the service is built from already-built dependencies in
// the module composition root (internal/surveys/module.go). The
// constructor enforces non-nil for the required deps (panic-on-nil
// pattern, per Plan 05/06 lessons) and accepts a nil eventbus.Publisher
// (Plan 11 owns the NATS wire-up).
//
// Cross-module callers MUST import internal/surveys/api only — depguard
// rejects direct imports of this package from outside the surveys
// module.
package service
