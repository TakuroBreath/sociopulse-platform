package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/passwords"
	"github.com/sociopulse/platform/pkg/postgres"
)

// totpIssuerDefault is the human-readable issuer label embedded in the
// otpauth URL and rendered by every authenticator app on the second-
// factor card. Constant rather than a config knob: changing it would
// orphan every existing secret (the issuer is part of the QR-code
// payload), so we lock it to the project name.
const totpIssuerDefault = "SocioPulse"

// totpPeriodDefault is the rotation period in seconds. RFC 6238 fixes
// the standard at 30 s; deviating breaks compatibility with every
// off-the-shelf authenticator. Plan 05 references this in the
// COMMON.md "Period: 30 s (стандарт)" note.
const totpPeriodDefault uint = 30

// totpSkewDefault is the validation window. ±1 period (30 s) absorbs
// honest clock drift on the user's device while keeping the brute-force
// surface tight. Plan 05 explicitly forbids ±2 — that doubles the
// attack window and still leaves clock-skew failures on slow devices.
const totpSkewDefault uint = 1

// totpBackupCodesCount is the number of backup codes minted at enroll.
// Spec calls for 10 single-use codes; each carries 40 bits of hex
// entropy (10 hex chars). With Argon2id hashing they survive a stolen
// DB dump as long as the user's master password would.
const totpBackupCodesCount = 10

// totpBackupCodeBytes is the raw entropy per backup code in bytes.
// 5 bytes -> 10 hex chars -> 40 bits, matching spec. Driven by
// crypto/rand; never math/rand.
const totpBackupCodeBytes = 5

// TOTPStorePort is the consumer-side projection of *store.TOTPStore the
// TOTPService needs. Keeping the interface small lets unit tests
// substitute hand-rolled fakes without dragging the real store and its
// SQL into the test binary.
type TOTPStorePort interface {
	Upsert(ctx context.Context, tx postgres.Tx, userID, tenantID uuid.UUID, encSecret []byte, backupHashes []string) error
	Get(ctx context.Context, tx postgres.Tx, userID uuid.UUID) (authapi.TOTPState, error)
	GetAny(ctx context.Context, tx postgres.Tx, userID uuid.UUID) (authapi.TOTPState, error)
	Confirm(ctx context.Context, tx postgres.Tx, userID uuid.UUID, at time.Time) error
	Delete(ctx context.Context, tx postgres.Tx, userID uuid.UUID) error
	UpdateLastVerified(ctx context.Context, tx postgres.Tx, userID uuid.UUID, at time.Time) error
	MarkBackupUsed(ctx context.Context, tx postgres.Tx, userID uuid.UUID, hashToRemove string) error
}

// UserTOTPUpdater is the consumer-side narrowing of UserStorePort that
// the TOTPService needs: resolve the user's tenant and login for the
// otpauth URL, and toggle the cached totp_enabled flag on Confirm/
// Disable so the Authenticator's fast-path login fork stays correct.
type UserTOTPUpdater interface {
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (authapi.User, error)
	SetTOTPEnabled(ctx context.Context, tx postgres.Tx, id uuid.UUID, enabled bool) error
}

// totpTxRunner is the cross-tenant transaction owner. *postgres.Pool
// satisfies this interface via WithTenant / BypassRLS; tests substitute
// an in-memory implementation that invokes fn with a zero postgres.Tx.
type totpTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// TOTPDeps captures every dependency NewTOTPService needs. A Deps
// struct keeps the constructor's parameter list manageable and lets
// the composition root build it incrementally.
type TOTPDeps struct {
	// Issuer is the human-readable label embedded in the otpauth URL.
	// When empty, NewTOTPService falls back to totpIssuerDefault.
	Issuer string
	// Period is the TOTP rotation period in seconds. When zero, falls
	// back to totpPeriodDefault (30). Non-default values are valid only
	// when integration with a custom authenticator is required.
	Period uint
	// Skew is the validation window in periods. When zero, falls back
	// to totpSkewDefault (±1).
	Skew uint
	// Pool runs functions inside per-tenant or BypassRLS transactions.
	Pool totpTxRunner
	// Store is the persistence adapter.
	Store TOTPStorePort
	// Users is the user-store narrowing for tenant resolution and
	// totp_enabled toggling.
	Users UserTOTPUpdater
	// KMS encrypts the TOTP secret at rest with the per-tenant DEK.
	KMS tenancyapi.KMSResolver
	// BackupHasher is the Argon2id hasher used for backup codes. The
	// service expects this to be configured with cheap params (single-
	// use tokens, not password equivalents) — the composition root
	// constructs a dedicated low-cost passwords.Hasher for this slot
	// rather than reusing the user-password hasher.
	BackupHasher passwords.Hasher
	// Audit emits "auth.totp.*" rows. May be a no-op fake in tests but
	// MUST NOT be nil — see NewTOTPService.
	Audit auditapi.Logger
	// Clock is injected so tests can advance time deterministically.
	// nil falls back to time.Now.
	Clock func() time.Time
}

// TOTPService implements api.TOTPService. It orchestrates secret
// generation, backup-code minting, KMS-backed encryption, and audit
// emission against pluggable collaborators.
//
// The struct is safe for concurrent use: every collaborator is required
// to be safe for concurrent use, and the service holds no mutable
// state past constructor time.
type TOTPService struct {
	issuer       string
	period       uint
	skew         uint
	digits       otp.Digits
	pool         totpTxRunner
	store        TOTPStorePort
	users        UserTOTPUpdater
	kms          tenancyapi.KMSResolver
	backupHasher passwords.Hasher
	audit        auditapi.Logger
	clock        func() time.Time
}

// Compile-time guarantees the implementation satisfies the public
// contract and the consumer-side TOTPVerifier shape used by the
// Authenticator. Drift in either signature stops the build here
// instead of surfacing as a missing-method runtime error.
var (
	_ authapi.TOTPService  = (*TOTPService)(nil)
	_ authapi.TOTPVerifier = (*TOTPService)(nil)
	_ TOTPVerifier         = (*TOTPService)(nil)
)

// NewTOTPService validates its inputs and returns a ready-to-use
// TOTPService. The constructor returns an error rather than panicking
// for symmetry with NewAuthenticator — the composition root surfaces
// the failure cleanly during cmd/api startup.
func NewTOTPService(deps TOTPDeps) (*TOTPService, error) {
	switch {
	case deps.Pool == nil:
		return nil, errors.New("auth/service: TOTP Pool is required")
	case deps.Store == nil:
		return nil, errors.New("auth/service: TOTP Store is required")
	case deps.Users == nil:
		return nil, errors.New("auth/service: TOTP Users is required")
	case deps.KMS == nil:
		return nil, errors.New("auth/service: TOTP KMS is required")
	case deps.BackupHasher == nil:
		return nil, errors.New("auth/service: TOTP BackupHasher is required")
	case deps.Audit == nil:
		return nil, errors.New("auth/service: TOTP Audit is required (use a no-op fake in tests, never nil)")
	}

	issuer := deps.Issuer
	if issuer == "" {
		issuer = totpIssuerDefault
	}
	period := deps.Period
	if period == 0 {
		period = totpPeriodDefault
	}
	skew := deps.Skew
	if skew == 0 {
		skew = totpSkewDefault
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	return &TOTPService{
		issuer:       issuer,
		period:       period,
		skew:         skew,
		digits:       otp.DigitsSix,
		pool:         deps.Pool,
		store:        deps.Store,
		users:        deps.Users,
		kms:          deps.KMS,
		backupHasher: deps.BackupHasher,
		audit:        deps.Audit,
		clock:        clock,
	}, nil
}

// Enroll implements api.TOTPService.Enroll. It mints a fresh secret
// and 10 single-use backup codes, encrypts the secret with the
// per-tenant DEK, persists the encrypted secret + backup hashes, and
// returns the plaintext secret + backup codes ONCE. Callers must
// display the result immediately; the service holds no plaintext
// after the call returns.
//
// Calling Enroll on an already-confirmed enrolment returns
// ErrTOTPAlreadyEnabled. Calling Enroll on a partial-enrolment row
// (Enroll without a subsequent Confirm) overwrites the row in place,
// re-issuing a fresh secret per the documented Plan 05 Task 6
// pragmatic decision.
func (s *TOTPService) Enroll(ctx context.Context, userID uuid.UUID) (authapi.TOTPEnrollment, error) {
	// Step 1 — resolve tenant + login via BypassRLS so the lookup
	// doesn't require the caller to supply a tenant context up front.
	user, err := s.resolveUser(ctx, userID)
	if err != nil {
		return authapi.TOTPEnrollment{}, err
	}

	// Step 2 — guard against re-enrollment of an already-confirmed user.
	// Read inside per-tenant tx so RLS protects against cross-tenant
	// probes. ErrTOTPNotEnrolled here is expected (no row yet OR partial
	// row) and means we can proceed.
	if err := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		state, gerr := s.store.GetAny(ctx, tx, userID)
		if gerr != nil {
			if errors.Is(gerr, authapi.ErrTOTPNotEnrolled) {
				return nil
			}
			return gerr
		}
		if state.Enrolled {
			return authapi.ErrTOTPAlreadyEnabled
		}
		return nil
	}); err != nil {
		if errors.Is(err, authapi.ErrTOTPAlreadyEnabled) {
			return authapi.TOTPEnrollment{}, err
		}
		return authapi.TOTPEnrollment{}, fmt.Errorf("auth/service: totp enroll preflight: %w", err)
	}

	// Step 3 — generate the otp.Key. pquerna/otp.totp.Generate fills
	// SecretSize=20 by default (the RFC 6238 floor for SHA-1) and reads
	// from crypto/rand internally. Period/Digits come from our config.
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      s.issuer,
		AccountName: user.Login,
		Period:      s.period,
		Digits:      s.digits,
	})
	if err != nil {
		return authapi.TOTPEnrollment{}, fmt.Errorf("auth/service: totp generate: %w", err)
	}
	secret := key.Secret() // base32-encoded plaintext

	// Step 4 — encrypt secret with the tenant DEK. The KMSResolver
	// returns a single ciphertext blob (header + nonce + ciphertext)
	// that goes verbatim into auth_totp.secret_enc.
	enc, err := s.kms.Encrypt(ctx, user.TenantID, []byte(secret))
	if err != nil {
		return authapi.TOTPEnrollment{}, fmt.Errorf("auth/service: totp kms encrypt: %w", err)
	}

	// Step 5 — mint backup codes and hash each one. Plaintext codes are
	// returned to the caller exactly once; the store keeps only hashes.
	codes, hashes, err := s.generateBackupCodes(ctx)
	if err != nil {
		return authapi.TOTPEnrollment{}, err
	}

	// Step 6 — persist. The Upsert resets enrolled=false even when an
	// existing partial row is overwritten (the SQL is explicit).
	if err := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		return s.store.Upsert(ctx, tx, userID, user.TenantID, enc, hashes)
	}); err != nil {
		return authapi.TOTPEnrollment{}, fmt.Errorf("auth/service: totp upsert: %w", err)
	}

	return authapi.TOTPEnrollment{
		Secret:      secret,
		OTPAuthURL:  key.URL(),
		BackupCodes: codes,
	}, nil
}

// Confirm implements api.TOTPService.Confirm. It loads the partial
// enrolment row, validates the code against the decrypted secret, and
// flips enrolled=true + users.totp_enabled=true on success. Idempotent
// on already-enrolled users (returns nil). Wrong code returns
// ErrTOTPInvalid.
func (s *TOTPService) Confirm(ctx context.Context, userID uuid.UUID, code string) error {
	state, err := s.loadAnyState(ctx, userID)
	if err != nil {
		return err
	}
	if state.Enrolled {
		// Idempotent — second Confirm is a no-op so the caller's UX
		// (double-tap on the verify button) does not leak.
		return nil
	}

	secret, err := s.kms.Decrypt(ctx, state.TenantID, state.SecretEncrypted)
	if err != nil {
		return fmt.Errorf("auth/service: totp kms decrypt: %w", err)
	}
	now := s.clock()
	valid, verr := totp.ValidateCustom(code, string(secret), now, totp.ValidateOpts{
		Period:    s.period,
		Skew:      s.skew,
		Digits:    s.digits,
		Algorithm: otp.AlgorithmSHA1,
	})
	if verr != nil {
		return fmt.Errorf("%w: %s", authapi.ErrTOTPInvalid, verr.Error())
	}
	if !valid {
		return authapi.ErrTOTPInvalid
	}

	// Persist confirmation + flip user flag inside one tenant tx so the
	// two writes commit atomically.
	if err := s.pool.WithTenant(ctx, state.TenantID, func(tx postgres.Tx) error {
		if err := s.store.Confirm(ctx, tx, userID, now); err != nil {
			return err
		}
		return s.users.SetTOTPEnabled(ctx, tx, userID, true)
	}); err != nil {
		return fmt.Errorf("auth/service: totp confirm: %w", err)
	}

	s.writeAudit(ctx, authapi.AuditActionTOTPEnrolled, state.TenantID, userID)
	return nil
}

// Verify implements api.TOTPService.Verify. The algorithm:
//  1. Load the enrolled row (ErrTOTPNotEnrolled if absent / partial).
//  2. Try the code against the decrypted TOTP secret first — the
//     common path. On match, stamp last_verified_at and return.
//  3. On TOTP mismatch, walk the unused backup-code hashes; on a
//     hit, atomically remove the matched hash via MarkBackupUsed and
//     return. The store layer is the single source of single-use
//     truth (no double-spend).
//  4. Else return (false, nil) — wrong code, but not a "service
//     down" failure.
//
// The (bool, error) shape matches the consumer-side TOTPVerifier the
// Authenticator depends on. Wrong-code is (false, nil); ErrTOTPNotEnrolled
// is (false, error) so callers can distinguish.
func (s *TOTPService) Verify(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	state, err := s.loadEnrolledState(ctx, userID)
	if err != nil {
		return false, err
	}

	secret, err := s.kms.Decrypt(ctx, state.TenantID, state.SecretEncrypted)
	if err != nil {
		return false, fmt.Errorf("auth/service: totp kms decrypt: %w", err)
	}
	now := s.clock()
	valid, verr := totp.ValidateCustom(code, string(secret), now, totp.ValidateOpts{
		Period:    s.period,
		Skew:      s.skew,
		Digits:    s.digits,
		Algorithm: otp.AlgorithmSHA1,
	})
	// ValidateCustom returns an error only on malformed inputs (wrong
	// digit count, etc.). Treat that as a wrong code rather than a
	// service failure — the user typed something we can't parse.
	if verr == nil && valid {
		if uerr := s.pool.WithTenant(ctx, state.TenantID, func(tx postgres.Tx) error {
			return s.store.UpdateLastVerified(ctx, tx, userID, now)
		}); uerr != nil {
			return false, fmt.Errorf("auth/service: totp update last verified: %w", uerr)
		}
		s.writeAudit(ctx, authapi.AuditActionTOTPVerified, state.TenantID, userID)
		return true, nil
	}

	// TOTP mismatch — try backup codes. We Verify each unused hash;
	// passwords.Hasher already runs constant-time on the underlying
	// Argon2 key compare, so an attacker cannot probe individual
	// backup codes by timing.
	for _, hash := range state.BackupCodeHashes {
		ok, herr := s.backupHasher.Verify(ctx, hash, code)
		if herr != nil {
			// Malformed hash in the DB is a server-side bug, not a wrong
			// code — surface so we don't silently accept the next try.
			return false, fmt.Errorf("auth/service: totp backup verify: %w", herr)
		}
		if !ok {
			continue
		}
		// Atomically consume the matched hash. The store guarantees
		// single-use even under concurrent verifies of the same code.
		if merr := s.pool.WithTenant(ctx, state.TenantID, func(tx postgres.Tx) error {
			return s.store.MarkBackupUsed(ctx, tx, userID, hash)
		}); merr != nil {
			if errors.Is(merr, authapi.ErrTOTPInvalid) {
				// Race: another verifier beat us to it. Treat as wrong
				// code so the user retries with a different one.
				return false, nil
			}
			return false, fmt.Errorf("auth/service: totp mark backup used: %w", merr)
		}
		s.writeAudit(ctx, authapi.AuditActionTOTPBackupUsed, state.TenantID, userID)
		return true, nil
	}
	return false, nil
}

// Disable implements api.TOTPService.Disable. The Delete + flip-flag
// + audit triplet runs inside a single tenant tx so a crash between
// the row delete and the user-flag toggle cannot leave the system in
// the "row gone but user still flagged TOTP-enabled" state.
//
// Idempotent: calling on a user who never enrolled is a no-op (row
// already absent, flag already false). The audit row is still emitted
// so re-disable is observable.
func (s *TOTPService) Disable(ctx context.Context, userID uuid.UUID) error {
	user, err := s.resolveUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		if derr := s.store.Delete(ctx, tx, userID); derr != nil {
			return derr
		}
		return s.users.SetTOTPEnabled(ctx, tx, userID, false)
	}); err != nil {
		return fmt.Errorf("auth/service: totp disable: %w", err)
	}
	s.writeAudit(ctx, authapi.AuditActionTOTPDisabled, user.TenantID, userID)
	return nil
}

// Status implements api.TOTPService.Status. A user with no row is
// reported as Enabled=false / zero values; a partial-enrolment row is
// also reported as Enabled=false (consistent with the Verify path's
// view).
func (s *TOTPService) Status(ctx context.Context, userID uuid.UUID) (authapi.TOTPStatus, error) {
	user, err := s.resolveUser(ctx, userID)
	if err != nil {
		return authapi.TOTPStatus{}, err
	}
	var state authapi.TOTPState
	terr := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		var inner error
		state, inner = s.store.GetAny(ctx, tx, userID)
		return inner
	})
	if terr != nil {
		if errors.Is(terr, authapi.ErrTOTPNotEnrolled) {
			return authapi.TOTPStatus{Enabled: false}, nil
		}
		return authapi.TOTPStatus{}, fmt.Errorf("auth/service: totp status: %w", terr)
	}
	return authapi.TOTPStatus{
		Enabled:         state.Enrolled,
		EnrolledAt:      state.EnrolledAt,
		LastVerifiedAt:  state.LastVerifiedAt,
		BackupRemaining: len(state.BackupCodeHashes),
	}, nil
}

// resolveUser fetches the user via BypassRLS so the lookup doesn't
// require the caller to supply a tenant context. The TOTP service is
// the only consumer of the user's tenant id for routing subsequent
// per-tenant transactions.
func (s *TOTPService) resolveUser(ctx context.Context, id uuid.UUID) (authapi.User, error) {
	var u authapi.User
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		var inner error
		u, inner = s.users.GetByID(ctx, tx, id)
		return inner
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return authapi.User{}, err
		}
		return authapi.User{}, fmt.Errorf("auth/service: resolve user: %w", err)
	}
	return u, nil
}

// loadAnyState resolves the tenant and reads the auth_totp row even if
// the row is partial-enrollment. Returns ErrTOTPNotEnrolled when the
// row is absent. Used by Confirm.
func (s *TOTPService) loadAnyState(ctx context.Context, userID uuid.UUID) (authapi.TOTPState, error) {
	user, err := s.resolveUser(ctx, userID)
	if err != nil {
		return authapi.TOTPState{}, err
	}
	var state authapi.TOTPState
	terr := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		var inner error
		state, inner = s.store.GetAny(ctx, tx, userID)
		return inner
	})
	if terr != nil {
		if errors.Is(terr, authapi.ErrTOTPNotEnrolled) {
			return authapi.TOTPState{}, terr
		}
		return authapi.TOTPState{}, fmt.Errorf("auth/service: load totp state: %w", terr)
	}
	return state, nil
}

// loadEnrolledState reads the auth_totp row and rejects partial-
// enrollment rows. Used by Verify — a partial row is "not enrolled"
// for verification purposes.
func (s *TOTPService) loadEnrolledState(ctx context.Context, userID uuid.UUID) (authapi.TOTPState, error) {
	user, err := s.resolveUser(ctx, userID)
	if err != nil {
		return authapi.TOTPState{}, err
	}
	var state authapi.TOTPState
	terr := s.pool.WithTenant(ctx, user.TenantID, func(tx postgres.Tx) error {
		var inner error
		state, inner = s.store.Get(ctx, tx, userID)
		return inner
	})
	if terr != nil {
		if errors.Is(terr, authapi.ErrTOTPNotEnrolled) {
			return authapi.TOTPState{}, terr
		}
		return authapi.TOTPState{}, fmt.Errorf("auth/service: load enrolled totp state: %w", terr)
	}
	return state, nil
}

// generateBackupCodes mints 10 single-use codes from crypto/rand and
// hashes each one with the configured backup hasher. Returns the
// plaintext codes (for the caller to display once) and the hashes
// (for storage).
//
// The hashes are not password-equivalents — they are one-shot tokens
// — so the configured hasher is expected to use cheap Argon2 params.
// Using the user-password hasher here would push enroll-time over a
// second on commodity hardware; a dedicated low-cost hasher in the
// composition root keeps the call fast.
func (s *TOTPService) generateBackupCodes(ctx context.Context) ([]string, []string, error) {
	codes := make([]string, totpBackupCodesCount)
	hashes := make([]string, totpBackupCodesCount)
	for i := range totpBackupCodesCount {
		var raw [totpBackupCodeBytes]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, nil, fmt.Errorf("auth/service: read backup code: %w", err)
		}
		code := hex.EncodeToString(raw[:])
		hash, err := s.backupHasher.Hash(ctx, code)
		if err != nil {
			return nil, nil, fmt.Errorf("auth/service: hash backup code: %w", err)
		}
		codes[i] = code
		hashes[i] = hash
	}
	return codes, hashes, nil
}

// writeAudit emits an audit row with a low-cardinality action label.
// All TOTP audits are tagged with the actor user (themselves) and the
// row's tenant; the caller never sees the underlying secret. The
// payload is currently always empty for TOTP actions — only the
// (action, target, actor) triple matters — but the parameter is kept
// for future event-specific context (e.g. backup-remaining count).
func (s *TOTPService) writeAudit(ctx context.Context, action string, tenantID, userID uuid.UUID) {
	if s.audit == nil {
		return
	}
	id := userID
	ev := auditapi.Event{
		TenantID:  tenantID,
		ActorID:   &id,
		ActorKind: auditapi.ActorUser,
		Action:    action,
		Target:    "user:" + userID.String(),
		Timestamp: s.clock(),
	}
	_ = s.audit.Write(ctx, ev)
}
