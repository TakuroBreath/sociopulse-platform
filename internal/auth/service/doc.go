// Package service implements the auth module's service layer.
//
// Plan 05 builds this package up over several tasks:
//
//   - Task 2 (this file): JWTIssuer — HS256 minting and validation,
//     rotation-friendly claims (sid stable across pairs, jti unique
//     per token), strict alg/iss/exp/typ checks.
//   - Task 3 (later): PasswordHasher (Argon2id) and BoundedHasher.
//   - Task 4 (later): Authenticator — Login / Refresh /
//     LoginTOTP / Logout, refresh-token rotation with replay detection.
//   - Task 5 (later): per-IP / per-account rate limiting.
//   - Task 6 (later): TOTPService.
//   - Task 7 (later): UserService CRUD.
//   - Task 8 (later): RBAC enforcement.
//
// Other modules consume only the interfaces in
// github.com/sociopulse/platform/internal/auth/api; depguard's
// module-boundaries rule blocks direct imports of this package.
package service
