// Package service implements the crm public-API contracts: ProjectService,
// RespondentService, QuotaTracker, DNCManager. The package is private to
// the crm module — cross-module callers go through internal/crm/api,
// the depguard module-boundaries rule rejects any direct import.
//
// The composition root (internal/crm/module.go) wires these services
// from infrastructure (postgres.Pool, audit.Logger, etc.) supplied by
// modules.Deps.
package service
