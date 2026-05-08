// Package api defines public contracts for the auth module.
// Other modules import only from this package — never from auth/service or auth/store.
//
// auth owns sessions, JWTs, TOTP, and RBAC: Argon2id password hashing,
// JWT issuance/validation (HS256, access 15 min, refresh 30 days,
// refresh-rotation reuse detection), TOTP enroll/verify, RBAC matrix
// (operator/supervisor/admin), per-IP and per-account rate limiting,
// force-logout-all session revocation.
package api

import (
	"net/netip"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Role enumerates the RBAC roles a user may hold. A user may have multiple roles.
type Role string

const (
	RoleOperator   Role = "operator"
	RoleSupervisor Role = "supervisor"
	RoleAdmin      Role = "admin"
)

// Claims is the decoded JWT payload. The same Claims struct is used for both
// access and refresh tokens; SessionID is stable across refresh-rotation.
type Claims struct {
	UserID    uuid.UUID `json:"sub"`
	TenantID  uuid.UUID `json:"tid"`
	Login     string    `json:"login"`
	Roles     []Role    `json:"roles"`
	SessionID string    `json:"sid"` // stable across access+refresh
	JTI       string    `json:"jti"` // unique per token
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
	TOTPDone  bool      `json:"totp_done,omitempty"`
}

// HasRole reports whether the claims hold the given role.
func (c Claims) HasRole(role Role) bool {
	return slices.Contains(c.Roles, role)
}

// AuthResult is returned from a successful Login or Refresh.
type AuthResult struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             User
	// TOTPRequired is true when Login authenticated the password but the user
	// has TOTP enabled and must complete /login/totp before a full session is issued.
	// In that case AccessToken is a short-lived (5 min) partial token.
	TOTPRequired bool
}

// User is the public projection of a user row.
type User struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Login         string
	FullName      string
	Email         string
	Roles         []Role
	TOTPEnabled   bool
	MustChangePwd bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ArchivedAt    *time.Time
}

// LoginInput is the payload for Authenticator.Login.
type LoginInput struct {
	OrgID     string // public tenant code (e.g. "CC-MOSKVA-01")
	Login     string
	Password  string
	IP        netip.Addr
	UserAgent string
}

// LoginTOTPInput completes a Login that returned TOTPRequired=true.
type LoginTOTPInput struct {
	PartialToken string // short-lived (5 min) partial token returned by Login
	Code         string
	IP           netip.Addr
	UserAgent    string
}

// CreateUserInput is the payload for UserService.Create.
type CreateUserInput struct {
	TenantID uuid.UUID
	Login    string
	FullName string
	Email    string
	Roles    []Role
	ActorID  uuid.UUID
}

// ListUsersInput narrows UserService.List.
type ListUsersInput struct {
	TenantID        uuid.UUID
	IncludeArchived bool
	Limit           int32
	Offset          int32
}

// Action is an RBAC action label, e.g. "user.create", "recording.access".
type Action string

// Resource describes the resource an Action targets.
// ID is optional; the zero UUID means "any instance of Kind".
type Resource struct {
	Kind string    // "user", "project", "call", ...
	ID   uuid.UUID // optional; zero means "any"
}

// TOTPEnrollment is returned ONCE from TOTPService.Enroll. Secret and
// BackupCodes must be displayed to the user immediately and never re-fetched.
type TOTPEnrollment struct {
	Secret      string   // base32, returned ONCE at enroll
	OTPAuthURL  string   // otpauth:// URL for QR-code rendering
	BackupCodes []string // 8-10 single-use codes, returned ONCE
}

// TOTPStatus describes the current TOTP enrolment state for a user.
type TOTPStatus struct {
	Enabled         bool
	EnrolledAt      *time.Time
	LastVerifiedAt  *time.Time
	BackupRemaining int
}

// TOTPState is the persistence-layer projection of an auth_totp row used
// by TOTPService and the auth_totp store. SecretEncrypted holds the
// AES-GCM ciphertext produced by tenancy.KMSResolver.Encrypt — the
// service decrypts on demand. BackupCodeHashes are PHC-encoded Argon2id
// hashes; on use the matching entry is removed from the slice and
// BackupUsedCount is incremented atomically by the store.
type TOTPState struct {
	UserID           uuid.UUID
	TenantID         uuid.UUID
	SecretEncrypted  []byte
	Enrolled         bool
	EnrolledAt       *time.Time
	LastVerifiedAt   *time.Time
	BackupCodeHashes []string
	BackupUsedCount  int
}
