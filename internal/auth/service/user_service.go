package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/passwords"
	"github.com/sociopulse/platform/pkg/postgres"
)

// userTxRunner is the cross-tenant transaction owner UserService uses
// for write paths. *postgres.Pool satisfies this interface via its
// WithTenant method; tests substitute an in-memory implementation that
// invokes fn with a zero postgres.Tx.
//
// Defined here at the consumer per project convention (07-go-coding
// -standards § Interfaces): the producer (*postgres.Pool) returns a
// concrete struct, the consumer narrows it to the methods it actually
// needs.
type userTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// actorContextKey is the unexported context key UserService uses to
// pull the acting user id when emitting audit rows. Middleware in
// pkg/middleware/auth threads claims through *gin.Context today; once
// that landing surfaces a context.Context-shaped helper, this key is
// what UserService keys on. Until then, tests inject the actor via
// WithActorID directly.
type actorContextKey struct{}

// WithActorID returns a context that carries the supplied actor user
// id. UserService inspects the context for this value when writing
// audit rows; absent value -> nil ActorID (system bootstrap).
func WithActorID(ctx context.Context, actorID uuid.UUID) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actorID)
}

// actorIDFromContext returns the actor id stored on ctx by WithActorID,
// or a nil pointer when no actor is present.
func actorIDFromContext(ctx context.Context) *uuid.UUID {
	v, ok := ctx.Value(actorContextKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return nil
	}
	return &v
}

// UserService implements api.UserService.
//
// Mutating methods open a per-tenant transaction (Pool.WithTenant), run
// the store write, and emit an audit row inside the same transaction
// so the audit log is durable iff the row write committed. List/Get
// open a similar transaction for RLS enforcement; the read paths do
// not need audit.
//
// 152-ФЗ note (pragmatic stance — see CLAUDE.md compliance section):
// full_name is stored as plaintext in the DB. The KMSResolver hook is
// plumbed via DI for forward compatibility but UserService does NOT
// call Encrypt/Decrypt on full_name in this task. Plan 06+ may flip
// the column to bytea + encrypt; the DTO surface stays string-typed.
type UserService struct {
	tx     userTxRunner
	store  authapi.UserStorePort
	hasher passwords.Hasher
	audit  auditapi.Logger
	outbox outbox.Writer // Plan 11.4: appends auth.user.deleted etc. inside same Tx.
	clock  func() time.Time

	// dummyHash is a pre-computed Argon2id hash of a fixed string. We
	// run hasher.Verify against it on the missing-user branch of
	// ChangePassword so that path spends the same wall-clock time as
	// a wrong-password attempt against an existing user. Without it,
	// an attacker could enumerate active user ids by latency.
	dummyHash string

	// TODO Plan 06+: add tenancy.KMSResolver here once full_name encryption
	// is the project-wide standard. Today we keep the DTO plaintext per
	// the 152-ФЗ pragmatic stance documented in CLAUDE.md.
}

// Compile-time assertion: *UserService must satisfy api.UserService.
var _ authapi.UserService = (*UserService)(nil)

// NewUserService constructs a UserService from already-built deps. The
// caller (the module composition root) owns the lifecycle of every
// dependency. clock may be nil — the constructor falls back to
// time.Now so callers do not have to repeat that boilerplate.
//
// auditLogger MUST NOT be nil: every state-changing UserService method
// emits an audit row inside the same transaction as the data write,
// and a misconfigured composition root that registered nil would
// silently drop those rows. Tests that genuinely don't care about the
// audit trail must inject a no-op fake logger explicitly.
//
// outboxWriter MUST NOT be nil — Plan 11.4 (Archive) writes a
// tenant.<t>.auth.user.deleted row inside the same Tx. Tests use a
// recording fake; production passes outbox.NewPostgresWriter().
func NewUserService(
	pool userTxRunner,
	store authapi.UserStorePort,
	hasher passwords.Hasher,
	auditLogger auditapi.Logger,
	outboxWriter outbox.Writer,
	clock func() time.Time,
) *UserService {
	if pool == nil {
		panic("auth/service: NewUserService: pool is required")
	}
	if store == nil {
		panic("auth/service: NewUserService: store is required")
	}
	if hasher == nil {
		panic("auth/service: NewUserService: hasher is required")
	}
	if auditLogger == nil {
		panic("auth/service: NewUserService: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if outboxWriter == nil {
		panic("auth/service: NewUserService: outboxWriter is required (use a recording fake in tests, never nil)")
	}
	if clock == nil {
		clock = time.Now
	}

	// Pre-bake a dummy Argon2id hash so the timing-safe missing-user
	// branch of ChangePassword can call Verify without paying a Hash
	// per request. We use a context.Background here because Hash is
	// CPU-bound and uncancellable mid-derivation; if it fails (it
	// shouldn't — Argon2 is deterministic) we panic loudly because a
	// UserService without timing protection is a security regression
	// that should not silently start.
	dummyHash, err := hasher.Hash(context.Background(), "auth-service-dummy-hash-input")
	if err != nil {
		panic(fmt.Sprintf("auth/service: NewUserService: precompute dummy hash: %v", err))
	}

	return &UserService{
		tx:        pool,
		store:     store,
		hasher:    hasher,
		audit:     auditLogger,
		outbox:    outboxWriter,
		clock:     clock,
		dummyHash: dummyHash,
	}
}

// Create implements api.UserService.Create. Generates a 16-char temp
// password, hashes it via the project Hasher, inserts the user with
// MustChangePwd=true, and emits a "user.created" audit row inside the
// same transaction as the row write. Returns the saved user and the
// temp password — the caller must surface the temp password to the
// administrator exactly once and never re-fetch it.
func (s *UserService) Create(ctx context.Context, in authapi.CreateUserInput) (authapi.User, string, error) {
	if in.TenantID == uuid.Nil {
		return authapi.User{}, "", fmt.Errorf("auth/service: create user: tenant id required")
	}
	if in.Login == "" {
		return authapi.User{}, "", fmt.Errorf("auth/service: create user: login required")
	}
	if len(in.Roles) == 0 {
		return authapi.User{}, "", fmt.Errorf("%w: create user", authapi.ErrEmptyRoles)
	}

	tempPwd, err := GenerateTempPassword()
	if err != nil {
		return authapi.User{}, "", fmt.Errorf("auth/service: generate temp password: %w", err)
	}
	hash, err := s.hasher.Hash(ctx, tempPwd)
	if err != nil {
		return authapi.User{}, "", fmt.Errorf("auth/service: hash temp password: %w", err)
	}

	candidate := authapi.User{
		TenantID:      in.TenantID,
		Login:         in.Login,
		FullName:      in.FullName,
		Email:         in.Email,
		Roles:         in.Roles,
		MustChangePwd: true,
	}

	var saved authapi.User
	err = s.tx.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		saved, err = s.store.Insert(ctx, tx, candidate, hash)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: saved.TenantID,
			Action:   "user.created",
			Target:   "user:" + saved.ID.String(),
			Payload: map[string]any{
				"login": saved.Login,
				"email": saved.Email,
				"roles": saved.Roles,
			},
		})
	})
	if err != nil {
		// Bubble the sentinel as-is when it's a known error so callers
		// can errors.Is without losing the kind.
		if errors.Is(err, authapi.ErrLoginTaken) {
			return authapi.User{}, "", err
		}
		return authapi.User{}, "", fmt.Errorf("auth/service: create user: %w", err)
	}
	return saved, tempPwd, nil
}

// List implements api.UserService.List. Limit/Offset are clamped to the
// documented 50/500 bounds before the store call so a careless caller
// does not request millions of rows in one shot.
func (s *UserService) List(ctx context.Context, in authapi.ListUsersInput) ([]authapi.User, int64, error) {
	if in.TenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("auth/service: list users: tenant id required")
	}
	if in.Limit <= 0 {
		in.Limit = 50
	}
	if in.Limit > 500 {
		in.Limit = 500
	}
	if in.Offset < 0 {
		in.Offset = 0
	}

	var (
		rows  []authapi.User
		total int64
	)
	err := s.tx.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.store.List(ctx, tx, in)
		return err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("auth/service: list users: %w", err)
	}
	return rows, total, nil
}

// Get implements api.UserService.Get. The lookup uses a BypassRLS
// transaction because the caller has not (necessarily) supplied a
// tenant context — admin tooling routinely needs to resolve a user id
// to its tenant before any per-tenant flow.
func (s *UserService) Get(ctx context.Context, id uuid.UUID) (authapi.User, error) {
	if id == uuid.Nil {
		return authapi.User{}, fmt.Errorf("auth/service: get user: id required")
	}
	var u authapi.User
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		u, err = s.store.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return authapi.User{}, err
		}
		return authapi.User{}, fmt.Errorf("auth/service: get user: %w", err)
	}
	return u, nil
}

// UpdateRole implements api.UserService.UpdateRole. The role list must
// be non-empty; the DB enforces a CHECK constraint, but we surface a
// clearer error here.
func (s *UserService) UpdateRole(ctx context.Context, id uuid.UUID, roles []authapi.Role) (authapi.User, error) {
	if id == uuid.Nil {
		return authapi.User{}, fmt.Errorf("auth/service: update role: id required")
	}
	if len(roles) == 0 {
		return authapi.User{}, authapi.ErrEmptyRoles
	}

	tenantID, err := s.resolveTenant(ctx, id)
	if err != nil {
		return authapi.User{}, err
	}

	var refreshed authapi.User
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		refreshed, err = s.store.UpdateRoles(ctx, tx, id, roles)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: refreshed.TenantID,
			Action:   "user.roles_updated",
			Target:   "user:" + refreshed.ID.String(),
			Payload:  map[string]any{"roles": refreshed.Roles},
		})
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return authapi.User{}, err
		}
		return authapi.User{}, fmt.Errorf("auth/service: update role: %w", err)
	}
	return refreshed, nil
}

// Archive implements api.UserService.Archive. Idempotent: archiving a
// user whose archived_at is already set returns nil (the store-level
// idempotency); the audit row is emitted on every call so a re-archive
// is still observable. Plan 11.4: also publishes the
// tenant.<t>.auth.user.deleted outbox event so downstream subscribers
// (realtime resolver cache) can drop stale entries. The outbox row is
// appended INSIDE the same WithTenant Tx as the store mutation + audit
// write — a publish failure rolls all three back together (canonical
// transactional-outbox semantics).
func (s *UserService) Archive(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("auth/service: archive: id required")
	}
	tenantID, err := s.resolveTenant(ctx, id)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.store.Archive(ctx, tx, id); err != nil {
			return err
		}
		if err := s.writeAudit(ctx, auditapi.Event{
			TenantID: tenantID,
			Action:   "user.archived",
			Target:   "user:" + id.String(),
		}); err != nil {
			return err
		}
		payload, err := json.Marshal(authapi.UserDeletedEvent{
			UserID:    id,
			TenantID:  tenantID,
			DeletedAt: now.Unix(),
			Reason:    "archived",
		})
		if err != nil {
			return fmt.Errorf("marshal user_deleted payload: %w", err)
		}
		return s.outbox.Append(ctx, tx, outbox.Event{
			TenantID:    &tenantID,
			AggregateID: &id,
			Subject:     authapi.SubjectUserDeletedFor(tenantID),
			Payload:     payload,
		})
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return err
		}
		return fmt.Errorf("auth/service: archive: %w", err)
	}
	return nil
}

// Restore implements api.UserService.Restore. Returns
// ErrUserNotArchived when the user is currently active so callers
// distinguish that from a transparent no-op.
func (s *UserService) Restore(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("auth/service: restore: id required")
	}
	tenantID, err := s.resolveTenant(ctx, id)
	if err != nil {
		return err
	}
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.store.Restore(ctx, tx, id); err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: tenantID,
			Action:   "user.restored",
			Target:   "user:" + id.String(),
		})
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) || errors.Is(err, authapi.ErrUserNotArchived) {
			return err
		}
		return fmt.Errorf("auth/service: restore: %w", err)
	}
	return nil
}

// ResetPassword implements api.UserService.ResetPassword. Generates a
// fresh 16-char temp password, hashes it, swaps the stored hash, and
// flips MustChangePwd to true so the user is forced to rotate on next
// login. Returns the temp password — single-use, must be displayed to
// the admin once and never persisted by the caller.
func (s *UserService) ResetPassword(ctx context.Context, id uuid.UUID) (string, error) {
	if id == uuid.Nil {
		return "", fmt.Errorf("auth/service: reset password: id required")
	}
	tenantID, err := s.resolveTenant(ctx, id)
	if err != nil {
		return "", err
	}

	tempPwd, err := GenerateTempPassword()
	if err != nil {
		return "", fmt.Errorf("auth/service: reset password: %w", err)
	}
	hash, err := s.hasher.Hash(ctx, tempPwd)
	if err != nil {
		return "", fmt.Errorf("auth/service: hash reset password: %w", err)
	}

	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.store.UpdatePassword(ctx, tx, id, hash, true); err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: tenantID,
			Action:   "user.password_reset",
			Target:   "user:" + id.String(),
		})
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return "", err
		}
		return "", fmt.Errorf("auth/service: reset password: %w", err)
	}
	return tempPwd, nil
}

// ChangePassword implements api.UserService.ChangePassword.
//
// Timing-safety: an attacker probing this endpoint must NOT learn whether
// the user id exists from the response time. We achieve this by ALWAYS
// running hasher.Verify exactly once on the request — against the real
// stored hash if the user exists, against a pre-computed dummy hash
// otherwise. Both paths surface api.ErrInvalidCredentials so the
// observable response is identical (modulo the audit row written on
// success, which is server-side-only). The new-password Argon2id hash
// is computed AFTER Verify succeeds so a flood of wrong-old-password
// attempts costs one Argon2 (verify) instead of two (verify + hash).
func (s *UserService) ChangePassword(ctx context.Context, id uuid.UUID, oldPassword, newPassword string) error {
	if id == uuid.Nil {
		return fmt.Errorf("auth/service: change password: id required")
	}
	if newPassword == "" {
		return fmt.Errorf("auth/service: change password: new password must be non-empty")
	}

	tenantID, resolveErr := s.resolveTenant(ctx, id)
	if resolveErr != nil && !errors.Is(resolveErr, authapi.ErrUserNotFound) {
		return resolveErr
	}
	userMissing := errors.Is(resolveErr, authapi.ErrUserNotFound)

	if userMissing {
		// Spend the same Argon2 cost on a dummy hash so the response
		// time matches the wrong-password path exactly. The hash is
		// pre-baked in the constructor (see s.dummyHash) so we don't
		// pay an extra Hash here on every miss.
		_, _ = s.hasher.Verify(ctx, s.dummyHash, oldPassword)
		return authapi.ErrInvalidCredentials
	}

	err := s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.applyPasswordChange(ctx, tx, id, tenantID, oldPassword, newPassword)
	})
	if err != nil {
		if errors.Is(err, authapi.ErrInvalidCredentials) || errors.Is(err, authapi.ErrUserNotFound) {
			return err
		}
		return fmt.Errorf("auth/service: change password: %w", err)
	}
	return nil
}

// applyPasswordChange is the inner closure of ChangePassword. Extracted
// so ChangePassword stays under the gocognit threshold and so the
// timing-equalization branches are easier to audit independently.
func (s *UserService) applyPasswordChange(ctx context.Context, tx postgres.Tx, id, tenantID uuid.UUID, oldPassword, newPassword string) error {
	oldHash, err := s.store.GetPasswordHash(ctx, tx, id)
	if err != nil {
		// Race: the user was archived/deleted between resolveTenant
		// and this read. Equalize timing with the missing-user branch
		// above (run Verify against the dummy hash) and surface the
		// same sentinel.
		if errors.Is(err, authapi.ErrUserNotFound) {
			_, _ = s.hasher.Verify(ctx, s.dummyHash, oldPassword)
			return authapi.ErrInvalidCredentials
		}
		return err
	}
	ok, err := s.hasher.Verify(ctx, oldHash, oldPassword)
	if err != nil {
		return fmt.Errorf("verify old password: %w", err)
	}
	if !ok {
		return authapi.ErrInvalidCredentials
	}
	// Only compute the new hash when the old one is verified —
	// rejecting wrong-old fast saves the second Argon2 derivation.
	newHash, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	if err := s.store.UpdatePassword(ctx, tx, id, newHash, false); err != nil {
		return err
	}
	return s.writeAudit(ctx, auditapi.Event{
		TenantID: tenantID,
		Action:   "user.password_changed",
		Target:   "user:" + id.String(),
	})
}

// resolveTenant returns the TenantID for the given user id by reading
// the row through a BypassRLS transaction. The mutation methods need
// the tenant id to call WithTenant, but the caller surface (api.User
// Service) does not include it for ergonomics. A single read up front
// keeps the public surface tight at the cost of one extra round-trip
// per write — acceptable given users-CRUD is an admin path.
func (s *UserService) resolveTenant(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var u authapi.User
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		u, err = s.store.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("auth/service: resolve tenant: %w", err)
	}
	return u.TenantID, nil
}

// writeAudit fills in the boilerplate fields (timestamp, actor) and
// invokes the audit Logger. A nil Logger short-circuits — the auth
// module composition root may register a no-op logger in tests, and we
// don't want a missing audit dependency to take down user CRUD.
func (s *UserService) writeAudit(ctx context.Context, ev auditapi.Event) error {
	if s.audit == nil {
		return nil
	}
	if ev.ActorKind == "" {
		ev.ActorKind = auditapi.ActorUser
	}
	if ev.ActorID == nil {
		ev.ActorID = actorIDFromContext(ctx)
		if ev.ActorID == nil {
			ev.ActorKind = auditapi.ActorSystem
		}
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = s.clock()
	}
	if err := s.audit.Write(ctx, ev); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}
