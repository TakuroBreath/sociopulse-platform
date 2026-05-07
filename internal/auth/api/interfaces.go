package api

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Authenticator is the public login surface mounted under /api/auth.
type Authenticator interface {
	// Login validates org+login+password. If the user has TOTP enabled,
	// returns AuthResult.TOTPRequired=true with a 5-minute partial token.
	Login(ctx context.Context, in LoginInput) (AuthResult, error)
	// LoginTOTP completes a TOTPRequired login by validating the code.
	LoginTOTP(ctx context.Context, in LoginTOTPInput) (AuthResult, error)
	// Refresh rotates a refresh token. Re-use of an already-rotated token
	// returns ErrRefreshReplay and triggers session revocation.
	Refresh(ctx context.Context, refreshToken string, ip netip.Addr) (AuthResult, error)
	// Logout invalidates the session associated with the given refresh token.
	Logout(ctx context.Context, refreshToken string) error
	// ValidateAccessToken decodes an access token and verifies its session is not revoked.
	ValidateAccessToken(ctx context.Context, accessToken string) (Claims, error)
}

// UserService is the public CRUD surface for users (admin endpoints).
type UserService interface {
	// Create creates a user and returns the auto-generated initial password.
	// The user is forced to change it on first login (MustChangePwd=true).
	Create(ctx context.Context, in CreateUserInput) (user User, tempPassword string, err error)
	// List returns one page of users plus the unfiltered total count.
	List(ctx context.Context, in ListUsersInput) (users []User, totalCount int64, err error)
	// Get returns the user with the given ID, or ErrInvalidCredentials-equivalent NotFound.
	Get(ctx context.Context, id uuid.UUID) (User, error)
	// UpdateRole replaces the user's roles atomically.
	UpdateRole(ctx context.Context, id uuid.UUID, roles []Role) (User, error)
	// Archive sets ArchivedAt and revokes all sessions for the user.
	Archive(ctx context.Context, id uuid.UUID) error
	// Restore clears ArchivedAt.
	Restore(ctx context.Context, id uuid.UUID) error
	// ResetPassword issues a new auto-generated password and forces a change on next login.
	ResetPassword(ctx context.Context, id uuid.UUID) (tempPassword string, err error)
	// ChangePassword verifies oldPassword then sets newPassword.
	ChangePassword(ctx context.Context, id uuid.UUID, oldPassword, newPassword string) error
}

// SessionRevoker is the surface used by force-logout-all and admin tooling.
type SessionRevoker interface {
	// RevokeSession marks the session ID as revoked. Subsequent ValidateAccessToken returns ErrTokenRevoked.
	RevokeSession(ctx context.Context, sid string) error
	// RevokeAllForUser revokes every active session for the user.
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
	// IsRevoked reports whether the session id (or specific JTI) is on the revocation list.
	IsRevoked(ctx context.Context, sid, jti string) (bool, error)
}

// RBACChecker enforces the role/action/resource matrix.
type RBACChecker interface {
	// Check returns nil when claims are authorised for action on resource,
	// or ErrInsufficientRole otherwise. Implementations may consult per-tenant
	// custom permissions before falling back to the static role matrix.
	Check(ctx context.Context, claims Claims, action Action, resource Resource) error
}

// JWTIssuer mints and validates JWTs. Implementations use HS256 with the
// global signing secret rotated via tenancy.SettingsCache.
type JWTIssuer interface {
	// IssueAccess produces a 15-minute access token for the claims.
	IssueAccess(c Claims) (token string, expiresAt time.Time, err error)
	// IssueRefresh produces a 30-day refresh token for the claims.
	IssueRefresh(c Claims) (token string, expiresAt time.Time, err error)
	// Validate parses token and ensures its `typ` matches expectedType
	// ("access" or "refresh"). Returns the decoded Claims.
	Validate(token, expectedType string) (Claims, error)
}

// TOTPService manages per-user TOTP enrolment and verification.
type TOTPService interface {
	// Enroll generates a TOTP secret and backup codes for the user. The
	// returned Secret and BackupCodes are persisted hashed and may NOT be
	// retrieved again — callers must display them immediately.
	Enroll(ctx context.Context, userID uuid.UUID) (TOTPEnrollment, error)
	// Confirm finalises enrolment by verifying the first code from the user's authenticator.
	Confirm(ctx context.Context, userID uuid.UUID, code string) error
	// Verify checks a TOTP code (or backup code) against the enrolled secret.
	Verify(ctx context.Context, userID uuid.UUID, code string) error
	// Disable removes the TOTP secret and backup codes for the user.
	Disable(ctx context.Context, userID uuid.UUID) error
	// Status returns the current TOTP state for the user.
	Status(ctx context.Context, userID uuid.UUID) (TOTPStatus, error)
}
