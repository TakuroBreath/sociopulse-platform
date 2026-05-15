package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sociopulse/platform/internal/analytics"
	"github.com/sociopulse/platform/internal/auth"
	"github.com/sociopulse/platform/internal/billing"
	"github.com/sociopulse/platform/internal/crm"
	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/recording"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/internal/reports"
	"github.com/sociopulse/platform/internal/surveys"
	"github.com/sociopulse/platform/internal/telephony"
	"github.com/sociopulse/platform/internal/tenancy"
)

// buildProvidersDeps carries the per-boot module instances that
// buildProviders needs but cannot construct itself (because they
// depend on resources opened in run() — Prometheus registries, the
// recording ObjectStore wired off cfg.Recording, etc.).
//
// Plan 21 Task 1 introduced this seam so cmd/api/main_test.go can
// assert the providers list at unit-test time without booting the
// composition root. Tests pass the zero value; production passes the
// real instances out of run().
type buildProvidersDeps struct {
	// Dialer is the *dialer.Module produced at run-time so its
	// pointer-receiver Stop() can be deferred in the composition
	// root. Tests that only inspect Module.Name() may pass nil;
	// tests that invoke Register MUST supply a real instance. Note:
	// registerModules skips interface-nil entries, but a typed-nil
	// *dialer.Module wrapped in modules.Module is NOT == nil and
	// would dereference at Register time.
	Dialer *dialer.Module

	// Recording is the *recording.Module produced via recording.New
	// against per-boot Config (gRPC listener gating, DEKUnwrapper,
	// ObjectStore). Same nil semantics as Dialer above — safe for
	// Name()-only inspection, unsafe for Register invocation.
	Recording *recording.Module

	// Crm is the *crm.Module produced at run-time so run() can defer
	// its Stop() before the Redis client closes. crm.Module.Register
	// spawns an asynq.Server goroutine when Redis is non-nil; without
	// a deferred Stop the asynq subscriber races rdb.Close at
	// shutdown and panics on a nil pubsub message (asynq@v0.26.0
	// subscriber.go:83). Tests that only inspect Module.Name() may
	// pass the zero value — Name() doesn't dereference its receiver,
	// so a typed-nil *crm.Module wrapped in modules.Module is safe
	// to walk. Tests that invoke Register MUST supply a real instance:
	// registerModules' `mod == nil` guard catches interface-nil but
	// NOT typed-nil pointers (Go interface-nil-vs-typed-nil gotcha).
	Crm *crm.Module

	// MetricsRegistry is the *prometheus.Registry that analytics +
	// any other module-built-with-New() collectors register against.
	// Nil is tolerated by analytics.New / reports.New.
	MetricsRegistry prometheus.Registerer

	// RecordingObjects is the ObjectStore wired off cfg.Recording's
	// local_keks. Reports.New reuses it for the async export path;
	// nil → reports falls through to a WARN at Register time.
	RecordingObjects storage.ObjectStore
}

// buildProviders returns the modules.Registry cmd/api walks at
// startup. The list ordering is load-bearing — see the comments on
// each entry for the rules a future edit must preserve.
//
// Pure function: no I/O, no globals, no infra-touching. Tests can
// call it with the zero buildProvidersDeps to assert presence of
// specific modules (e.g. Plan 21's tenancy wiring).
func buildProviders(deps buildProvidersDeps) modules.Registry {
	return modules.Registry{Modules: []modules.Module{
		// Plan 21 Task 1 — tenancy MUST be FIRST. Its Register
		// publishes tenancy.TenantService / KMSResolver / PhoneHasher
		// / Tenancy into the locator; auth/crm/surveys (Plan 21
		// Task 2+) consume those keys at their own Register time.
		// Reordering this entry breaks every downstream consumer
		// silently — the test TestRun_TenancyModuleInProvidersList
		// pins both presence AND first-position.
		&tenancy.Module{},

		// Plan 21 Task 2 — auth MUST come immediately after tenancy
		// and BEFORE any module that consumes auth locator keys
		// (crm, surveys, billing, future analytics). Consumes
		// tenancy.{TenantService, KMSResolver}; publishes
		// auth.{Authenticator, UserService, TOTPService,
		// RBACChecker, ClaimsValidator, SessionRevoker}.
		//
		// Closes Plan 14 Production Lesson #1: billing's requireRBAC
		// today falls through to its role-fast-path because
		// auth.RBACChecker is not in the locator. Wiring auth here
		// makes the matrix-checker the active gate.
		auth.Module{},

		telephony.Module{},
		deps.Dialer,
		deps.Recording,
		analytics.New(analytics.Config{Registerer: deps.MetricsRegistry}),
		reports.New(reports.Config{ObjectStore: deps.RecordingObjects}),
		billing.Module{},

		// Plan 21 Task 2 — crm and surveys are siblings (independent
		// of each other); each one consumes auth's locator keys so
		// both MUST come after auth in this slice. crm is passed in
		// via deps so the composition root can defer its Stop()
		// AHEAD of rdb.Close() — see buildProvidersDeps.Crm for the
		// asynq-shutdown rationale. surveys is stateless so the
		// value-receiver inline construction suffices.
		deps.Crm,
		surveys.Module{},
	}}
}
