// Package http provides the gin HTTP transport for the auth module.
//
// Handlers are intentionally thin — they bind JSON, call services, and
// render results or errors via the helpers in errors.go. ALL business
// logic lives in internal/auth/service. The transport layer's only
// responsibility is the wire format.
//
// Routes are mounted with Mount(group, deps), which the auth module's
// composition root invokes against the gin engine carried in
// modules.Deps.HTTPRouter.
package http

import (
	"time"

	"github.com/google/uuid"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// LoginRequest is the body of POST /api/auth/login.
type LoginRequest struct {
	OrgID    string `json:"org_id" binding:"required,min=1,max=64"`
	Login    string `json:"login" binding:"required,min=1,max=128"`
	Password string `json:"password" binding:"required,min=1,max=512"`
}

// LoginTOTPRequest is the body of POST /api/auth/login/totp.
type LoginTOTPRequest struct {
	PartialToken string `json:"partial_token" binding:"required"`
	Code         string `json:"code" binding:"required,len=6,numeric"`
}

// RefreshRequest is the body of POST /api/auth/refresh.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// LogoutRequest is the body of POST /api/auth/logout.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// ChangePasswordRequest is the body of POST /api/auth/me/password.
type ChangePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required,min=1,max=512"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=512"`
}

// CreateUserRequest is the body of POST /api/auth/users.
//
// Roles are validated against the canonical set; the matrix layer also
// enforces it but a transport-level guard surfaces the error closer to
// the wire.
type CreateUserRequest struct {
	Login    string   `json:"login" binding:"required,min=1,max=128"`
	FullName string   `json:"full_name" binding:"required,min=1,max=256"`
	Email    string   `json:"email" binding:"omitempty,email,max=256"`
	Roles    []string `json:"roles" binding:"required,min=1,dive,oneof=operator supervisor admin"`
}

// UpdateRoleRequest is the body of PATCH /api/auth/users/:id/roles.
type UpdateRoleRequest struct {
	Roles []string `json:"roles" binding:"required,min=1,dive,oneof=operator supervisor admin"`
}

// TOTPConfirmRequest is the body of POST /api/auth/me/totp/confirm.
type TOTPConfirmRequest struct {
	Code string `json:"code" binding:"required,len=6,numeric"`
}

// LoginResponse is the body of POST /api/auth/login and
// POST /api/auth/login/totp.
//
// On a TOTP-required path, AccessToken carries the 5-minute partial
// token, RefreshToken is empty, and TOTPRequired=true. The client
// completes the flow with /login/totp.
type LoginResponse struct {
	AccessToken      string    `json:"access_token,omitempty"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	AccessExpiresAt  time.Time `json:"access_expires_at,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	TOTPRequired     bool      `json:"totp_required,omitempty"`
	User             UserDTO   `json:"user,omitempty"`
}

// RefreshResponse is the body of POST /api/auth/refresh.
type RefreshResponse struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

// UserDTO is the public projection of an api.User.
//
// The fields mirror api.User one-for-one. Marshalling here (rather
// than letting gin marshal api.User directly) gives us a stable
// transport-level shape that future changes to api.User cannot
// silently break.
type UserDTO struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"tenant_id"`
	Login         string     `json:"login"`
	FullName      string     `json:"full_name"`
	Email         string     `json:"email,omitempty"`
	Roles         []string   `json:"roles"`
	TOTPEnabled   bool       `json:"totp_enabled"`
	MustChangePwd bool       `json:"must_change_pwd"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"`
}

// CreateUserResponse is the body of POST /api/auth/users. The temp
// password is shown ONCE — the caller must surface it to the
// administrator immediately and the server never stores it in
// plaintext.
type CreateUserResponse struct {
	User         UserDTO `json:"user"`
	TempPassword string  `json:"temp_password"`
}

// ResetPasswordResponse is the body of POST /api/auth/users/:id/reset_password.
type ResetPasswordResponse struct {
	TempPassword string `json:"temp_password"`
}

// ListUsersResponse is the body of GET /api/auth/users.
type ListUsersResponse struct {
	Users []UserDTO `json:"users"`
	Total int64     `json:"total"`
}

// TOTPEnrollResponse is the body of POST /api/auth/me/totp/enroll. The
// secret + backup codes are displayed ONCE; the server stores only
// hashes and an encrypted secret, never the plaintext returned here.
type TOTPEnrollResponse struct {
	Secret      string   `json:"secret"`
	OTPAuthURL  string   `json:"otp_auth_url"`
	BackupCodes []string `json:"backup_codes"`
}

// TOTPStatusResponse is the body of GET /api/auth/me/totp/status.
type TOTPStatusResponse struct {
	Enabled         bool       `json:"enabled"`
	EnrolledAt      *time.Time `json:"enrolled_at,omitempty"`
	LastVerifiedAt  *time.Time `json:"last_verified_at,omitempty"`
	BackupRemaining int        `json:"backup_remaining"`
}

// ErrorEnvelope is the JSON shape every 4xx/5xx response uses. It
// matches the envelope rendered by pkg/middleware/auth so the wire
// format stays uniform across modules. The minimal {error,message}
// shape is also deliberately compatible with pkg/httputil's
// ErrorEnvelope (Code/Message) once that lands.
type ErrorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// userToDTO converts an api.User into the transport-level UserDTO.
// uuid.Nil ids serialise as the canonical zero string ("00000000-…")
// — callers should not see Nil ids in practice, but the conversion
// stays total.
func userToDTO(u authapi.User) UserDTO {
	roles := make([]string, len(u.Roles))
	for i, r := range u.Roles {
		roles[i] = string(r)
	}
	return UserDTO{
		ID:            idString(u.ID),
		TenantID:      idString(u.TenantID),
		Login:         u.Login,
		FullName:      u.FullName,
		Email:         u.Email,
		Roles:         roles,
		TOTPEnabled:   u.TOTPEnabled,
		MustChangePwd: u.MustChangePwd,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
		ArchivedAt:    u.ArchivedAt,
	}
}

// usersToDTO is the slice form of userToDTO.
func usersToDTO(in []authapi.User) []UserDTO {
	out := make([]UserDTO, len(in))
	for i, u := range in {
		out[i] = userToDTO(u)
	}
	return out
}

// rolesFromStrings converts a transport-level []string into the typed
// []authapi.Role. Validation (oneof) runs at the binding layer before
// this is reached, so unknown strings here would already have been
// rejected.
func rolesFromStrings(in []string) []authapi.Role {
	out := make([]authapi.Role, len(in))
	for i, s := range in {
		out[i] = authapi.Role(s)
	}
	return out
}

// idString renders a uuid.UUID as a canonical RFC 4122 string. uuid.Nil
// renders as the all-zero UUID rather than an empty string so the JSON
// shape stays predictable.
func idString(id uuid.UUID) string { return id.String() }
