package api

import "errors"

// Sentinel errors returned by auth interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrInvalidCredentials is returned when login or password is wrong.
	// Returned with the same delay as the success path to thwart timing attacks.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrAccountLocked is returned when too many failed attempts have been recorded.
	ErrAccountLocked = errors.New("auth: account locked")
	// ErrAccountArchived is returned when the user record has ArchivedAt set.
	ErrAccountArchived = errors.New("auth: account archived")
	// ErrTOTPRequired is returned by Login when the user has TOTP enabled.
	// The caller must complete the flow with /login/totp.
	ErrTOTPRequired = errors.New("auth: TOTP required")
	// ErrTOTPInvalid is returned when the supplied TOTP code is wrong or expired.
	ErrTOTPInvalid = errors.New("auth: TOTP code invalid")
	// ErrPasswordExpired is returned when the user must rotate their password before logging in.
	ErrPasswordExpired = errors.New("auth: password must be changed")
	// ErrTokenInvalid is returned when a JWT cannot be parsed or its signature is bad.
	ErrTokenInvalid = errors.New("auth: token invalid or expired")
	// ErrTokenRevoked is returned when a token's session id is on the revocation list.
	ErrTokenRevoked = errors.New("auth: token revoked")
	// ErrRateLimitExceeded is returned by per-IP / per-account rate limiters.
	ErrRateLimitExceeded = errors.New("auth: rate limit exceeded")
	// ErrInsufficientRole is returned by RBACChecker when the claim's roles are insufficient for the action.
	ErrInsufficientRole = errors.New("auth: insufficient role")
	// ErrRefreshReplay is returned when a refresh token is reused after rotation.
	// Triggers RevokeAllForUser on the affected session.
	ErrRefreshReplay = errors.New("auth: refresh-token replay detected")
)
