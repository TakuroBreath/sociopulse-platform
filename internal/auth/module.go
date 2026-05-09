// Package auth — Module registration entry point.
//
// Plan 05 Task 8/9 (this commit) wires the full DI composition:
//  1. Hashers — bounded password hasher + dedicated backup-code hasher.
//  2. JWT issuer (HS256, signing secret from config).
//  3. Postgres-backed stores (users, totp).
//  4. Redis-backed stores/services (refresh, revoker, rate, lockout).
//  5. UserService, TOTPService, Authenticator.
//  6. Static RBAC matrix.
//  7. Tenant resolver adapter on top of tenancy.TenantService.
//  8. HTTP handlers mounted under /api/auth.
//  9. Service locator entries for cross-module use.
//
// Required Deps:
//
//	d.Logger        — non-nil
//	d.Pool          — non-nil (Postgres pool)
//	d.Redis         — non-nil (Redis client; Plan 05 added the field)
//	d.HTTPRouter    — non-nil (gin engine)
//	d.Locator       — non-nil
//	d.Config        — auth and JWT config populated
//
// Required Locator entries (registered earlier by tenancy module):
//
//	tenancy.TenantService — for org_code -> tenant_id resolution
//	tenancy.KMSResolver   — for TOTP secret encryption
//
// When any required dependency is missing, Register returns an error
// rather than panicking; cmd/api surfaces the error during boot.
package auth

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/pquerna/otp"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	authservice "github.com/sociopulse/platform/internal/auth/service"
	authstore "github.com/sociopulse/platform/internal/auth/store"
	transporthttp "github.com/sociopulse/platform/internal/auth/transport/http"
	"github.com/sociopulse/platform/internal/modules"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/passwords"
)

// Locator keys this module registers. External modules look these up
// to obtain the auth interfaces without crossing into auth/service.
const (
	LocatorAuthenticator   = "auth.Authenticator"
	LocatorUserService     = "auth.UserService"
	LocatorTOTPService     = "auth.TOTPService"
	LocatorRBACChecker     = "auth.RBACChecker"
	LocatorClaimsValidator = "auth.ClaimsValidator"
	LocatorSessionRevoker  = "auth.SessionRevoker"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorTenantService = "tenancy.TenantService"
	locatorKMSResolver   = "tenancy.KMSResolver"
	locatorAuditLogger   = "audit.Logger"
)

// jwtSigningLeeway is the clock-skew tolerance applied during JWT
// validation. 30s matches industry practice (Auth0, AWS Cognito) and
// is small enough that an attacker cannot meaningfully extend a
// stolen token's life by clock manipulation.
const jwtSigningLeeway = 30 * time.Second

// Module is the top-level registration handle for the auth module.
// Stateless; safe to construct as a zero value.
type Module struct{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "auth" }

// Register wires the module's components into the composition root.
// See the package-level comment for the full sequence.
//
//nolint:gocognit,gocyclo,cyclop // composition is intentionally linear; splitting it would obscure the boot sequence
func (Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	cfg := d.Config.Auth
	logger := d.Logger.Named("auth")

	// 1. Hashers.
	//    - Bounded password hasher with concurrency cap = NumCPU. Caps
	//      Argon2id memory pressure under concurrent login storms.
	//    - Backup-code hasher uses lighter params (m=1 MiB) so 10
	//      concurrent backup-code Hash() calls during TOTP enrolment
	//      do not flood the host.
	pwdHasher := passwords.NewBoundedHasher(passwords.Default(), runtime.NumCPU())
	backupHasher := passwords.NewHasher(passwords.BackupCodeParams())

	// 2. JWT issuer. Secret comes from config (loaded from Lockbox in
	//    production; from yaml/env in dev).
	jwtCfg := authservice.JWTConfig{
		Issuer:     cfg.JWT.Issuer,
		Secret:     []byte(cfg.JWT.Secret),
		AccessTTL:  cfg.JWT.AccessTTL,
		RefreshTTL: cfg.JWT.RefreshTTL,
		Leeway:     jwtSigningLeeway,
	}
	issuer, err := authservice.NewJWTIssuer(jwtCfg, nil)
	if err != nil {
		return fmt.Errorf("auth: jwt issuer: %w", err)
	}

	// 3. Stores.
	userStore := authstore.NewUserStore(d.Pool)
	totpStore := authstore.NewTOTPStore(d.Pool)
	refreshStore := authstore.NewRefreshStore(d.Redis, cfg.JWT.RefreshTTL)

	// 4. Redis-backed services.
	revoker := authservice.NewSessionRevoker(d.Redis, cfg.JWT.RefreshTTL, nil)
	rate := authservice.NewRateLimiterRedis(
		d.Redis,
		cfg.RateLimit.LoginPerIPPerHour,
		cfg.RateLimit.LoginPerAccountPerHour,
		time.Hour,
		nil,
	)
	lockout := authservice.NewLockoutRedis(
		d.Redis,
		cfg.RateLimit.LockoutAfterFailures,
		cfg.RateLimit.LockoutDuration,
		nil,
	)

	// 5. Cross-module dependencies via the locator.
	tenantSvc, err := lookupTenantService(d.Locator)
	if err != nil {
		return err
	}
	kms, err := lookupKMSResolver(d.Locator)
	if err != nil {
		return err
	}
	auditLogger := lookupAuditLogger(d.Locator, logger)

	tenantResolver := authservice.NewTenantResolverAdapter(tenantSvc)

	// 6. Domain services.
	//    Plan 11.4: UserService.Archive emits a tenant.<t>.auth.user.deleted
	//    outbox row alongside the existing audit row. The PostgresWriter is
	//    stateless (zero-value); the outbox-relay drains pending rows.
	userSvc := authservice.NewUserService(d.Pool, userStore, pwdHasher, auditLogger, outbox.NewPostgresWriter(), nil)

	totpSvc, err := authservice.NewTOTPService(authservice.TOTPDeps{
		Issuer:       cfg.TOTP.Issuer,
		Period:       totpPeriodFromConfig(cfg.TOTP.PeriodSec),
		Pool:         d.Pool,
		Store:        totpStore,
		Users:        userStore,
		KMS:          kms,
		BackupHasher: backupHasher,
		Audit:        auditLogger,
		Clock:        nil,
	})
	if err != nil {
		return fmt.Errorf("auth: totp service: %w", err)
	}

	auth, err := authservice.NewAuthenticator(authservice.AuthenticatorDeps{
		Pool:        d.Pool,
		Users:       userStore,
		Tenants:     tenantResolver,
		Hasher:      pwdHasher,
		Issuer:      issuer,
		Revoker:     revoker,
		Refreshes:   refreshStore,
		RateLimiter: rate,
		Lockout:     lockout,
		TOTP:        totpSvc,
		Audit:       auditLogger,
		Clock:       nil,
	})
	if err != nil {
		return fmt.Errorf("auth: authenticator: %w", err)
	}

	rbac := authservice.NewRBACMatrix()

	// 7. HTTP handlers. The Authenticator implements ClaimsValidator
	//    via ValidateAccessToken; we expose it through a thin adapter
	//    so the contract is explicit at the wire boundary.
	transporthttp.Mount(d.HTTPRouter.Group("/api"), transporthttp.Deps{
		Logger:    logger,
		Auth:      auth,
		Users:     userSvc,
		TOTP:      totpSvc,
		RBAC:      rbac,
		Validator: claimsValidatorAdapter{auth: auth},
	})

	// 8. Locator registration so cross-module callers (e.g.
	//    observability gates) can look up auth interfaces.
	d.Locator.Register(LocatorAuthenticator, authapi.Authenticator(auth))
	d.Locator.Register(LocatorUserService, authapi.UserService(userSvc))
	d.Locator.Register(LocatorTOTPService, authapi.TOTPService(totpSvc))
	d.Locator.Register(LocatorRBACChecker, authapi.RBACChecker(rbac))
	d.Locator.Register(LocatorClaimsValidator, authapi.ClaimsValidator(claimsValidatorAdapter{auth: auth}))
	d.Locator.Register(LocatorSessionRevoker, authapi.SessionRevoker(revoker))

	logger.Info("auth module registered",
		zap.Duration("access_ttl", cfg.JWT.AccessTTL),
		zap.Duration("refresh_ttl", cfg.JWT.RefreshTTL),
	)
	return nil
}

// requireDeps validates that every Register prerequisite is non-nil.
// Returning a structured error (rather than panicking) lets cmd/api
// surface a clean message at boot.
func requireDeps(d modules.Deps) error {
	switch {
	case d.Logger == nil:
		return errors.New("auth: Deps.Logger is required")
	case d.Pool == nil:
		return errors.New("auth: Deps.Pool is required")
	case d.Redis == nil:
		return errors.New("auth: Deps.Redis is required")
	case d.HTTPRouter == nil:
		return errors.New("auth: Deps.HTTPRouter is required")
	case d.Locator == nil:
		return errors.New("auth: Deps.Locator is required")
	case d.Config == nil:
		return errors.New("auth: Deps.Config is required")
	}
	if d.Config.Auth.JWT.Secret == "" {
		return errors.New("auth: Config.Auth.JWT.Secret is required (load from Lockbox)")
	}
	return nil
}

// lookupTenantService pulls tenancy.TenantService out of the locator
// and asserts its concrete type. Returns a clean error when the slot
// is missing or holds the wrong type — both are composition-root bugs
// that should surface during cmd/api boot rather than at first login.
func lookupTenantService(loc modules.ServiceLocator) (tenancyapi.TenantService, error) {
	raw, ok := loc.Lookup(locatorTenantService)
	if !ok {
		return nil, fmt.Errorf("auth: %s not registered (tenancy module must register before auth)", locatorTenantService)
	}
	svc, ok := raw.(tenancyapi.TenantService)
	if !ok {
		return nil, fmt.Errorf("auth: %s registered with wrong type %T", locatorTenantService, raw)
	}
	return svc, nil
}

// lookupKMSResolver pulls tenancy.KMSResolver out of the locator.
func lookupKMSResolver(loc modules.ServiceLocator) (tenancyapi.KMSResolver, error) {
	raw, ok := loc.Lookup(locatorKMSResolver)
	if !ok {
		return nil, fmt.Errorf("auth: %s not registered (tenancy module must register before auth)", locatorKMSResolver)
	}
	res, ok := raw.(tenancyapi.KMSResolver)
	if !ok {
		return nil, fmt.Errorf("auth: %s registered with wrong type %T", locatorKMSResolver, raw)
	}
	return res, nil
}

// lookupAuditLogger pulls audit.Logger out of the locator. Audit is
// optional in early plans (Plan 03 stubs the module), so a missing
// entry falls back to a noop logger and a one-line warning.
func lookupAuditLogger(loc modules.ServiceLocator, log *zap.Logger) auditapi.Logger {
	raw, ok := loc.Lookup(locatorAuditLogger)
	if !ok {
		log.Warn("audit.Logger not in locator — falling back to noop logger; audit rows will be silently dropped until audit module registers")
		return noopAuditLogger{}
	}
	logger, ok := raw.(auditapi.Logger)
	if !ok {
		log.Error("audit.Logger registered with wrong type — falling back to noop logger",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return noopAuditLogger{}
	}
	return logger
}

// totpPeriodFromConfig converts the int seconds knob from config into
// the unsigned period the TOTP service expects. A zero or negative
// configured value falls back to the standard 30 s period.
func totpPeriodFromConfig(seconds int) uint {
	if seconds <= 0 {
		return 0
	}
	return uint(seconds)
}

// claimsValidatorAdapter is the thin adapter that exposes
// Authenticator.ValidateAccessToken under the api.ClaimsValidator
// interface. Splitting this out keeps the dependency graph explicit:
// HTTP middleware consumes only ClaimsValidator, never Authenticator.
type claimsValidatorAdapter struct {
	auth authapi.Authenticator
}

// Validate parses an access token, verifies its signature, and
// confirms the session is not revoked.
func (a claimsValidatorAdapter) Validate(ctx context.Context, accessToken string) (authapi.Claims, error) {
	return a.auth.ValidateAccessToken(ctx, accessToken)
}

// noopAuditLogger is the fallback audit.Logger used when the audit
// module hasn't registered yet. It silently drops every event;
// auth bootstraps to a working state, and once Plan 03's audit
// module wires its real Logger this fallback is never selected.
type noopAuditLogger struct{}

// Write satisfies auditapi.Logger.
func (noopAuditLogger) Write(_ context.Context, _ auditapi.Event) error { return nil }

// Compile-time compliance with the api.ClaimsValidator surface.
var _ authapi.ClaimsValidator = claimsValidatorAdapter{}

// _ ensures the otp.Digits import stays referenced even if a future
// edit removes the explicit Digits param above. The TOTP service
// itself defaults to six digits when zero is passed, so this is a
// lint-stability hook rather than a feature flag.
var _ = otp.DigitsSix
