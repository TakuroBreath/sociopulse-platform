package store

import "errors"

// Sentinel errors specific to the auth store layer.
//
// These intentionally live alongside the store implementation rather than
// in internal/auth/api: they describe persistence-layer outcomes that the
// service layer maps to api sentinels (ErrRefreshNotFound -> ErrTokenInvalid,
// ErrRefreshAlreadyRotated -> ErrRefreshReplay). Cross-package callers
// inside the auth module use errors.Is on these to drive the refresh-rotation
// reuse-detection branch.
var (
	// ErrRefreshNotFound is returned by RefreshStore.Lookup when the supplied
	// jti is absent from the whitelist (key expired, deleted, or never saved).
	ErrRefreshNotFound = errors.New("auth/store: refresh not found")
	// ErrRefreshAlreadyRotated is returned by RefreshStore.Rotate when the
	// supplied oldJTI was already rotated. The Authenticator service layer
	// maps this to api.ErrRefreshReplay and revokes the entire session, per
	// the OAuth Best Current Practice for refresh-token reuse detection.
	ErrRefreshAlreadyRotated = errors.New("auth/store: refresh already rotated (reuse)")
)
