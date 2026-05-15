//go:build smoke

package smoke

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/passwords"
)

// smokePepperBytes is the per-tenant phone_hash_pepper used by the seed
// helper. The bytea column has no length constraint at the schema level —
// any non-NULL value satisfies the NOT NULL guard — but production uses
// 32 bytes (HMAC-SHA256 block size) and we mirror that here so the seed
// shape stays representative of real tenants.
//
// Note: the value is a literal byte sequence (NOT a UTF-8 string) and is
// fine for smoke; production peppers are drawn from crypto/rand.
var smokePepperBytes = []byte("smoke-pepper-32bytes-do-not-prod")

// SeededAccount carries the public coordinates of a tenant+user pair
// seeded by SeedTenantAndAdmin. The plaintext Password is retained so
// the caller can drive a login flow (this is fine for the smoke
// test-only build tag; the value lives only in the test process).
type SeededAccount struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	OrgCode  string
	Login    string
	Password string
	Role     string
}

// SeedTenantAndAdmin inserts one tenants row + one admin users row
// directly via pgx, returning the public coordinates. t.Cleanup deletes
// both rows so the shared smoke stack stays clean for sibling tests.
//
// The seed bypasses RLS by connecting as the testcontainer's superuser
// (the Postgres image's POSTGRES_USER, granted tenancy_admin by the
// 000001_init migration's BYPASSRLS grant). No SET app.tenant_id
// needed: the connection identity already carries BYPASSRLS so the
// inserts succeed without an explicit policy context.
//
// KMS path note (Plan 21 Task 6): tenants.kms_kek_id is set to the
// arbitrary "smoke-kek-<org_code>" string. The local KMS client does
// NOT pre-register this id — it only knows keys it minted via
// CreateKey. The auth flow exercised by TestSmoke_AuthFullFlow does
// not decrypt anything keyed off the tenant KEK (login → user lookup
// → argon2id verify is KMS-free), so the dangling kek_id is fine.
// Future smoke scenarios that exercise envelope encryption (recording,
// phone-hash, DEK lifecycle) MUST migrate this helper to either
// (a) generate a real KEK via the smoke-config's LocalKMSClient and
// store the returned id, or (b) extend WriteSmokeConfig to register
// a known-id key. Not in scope for Task 6.
func SeedTenantAndAdmin(t *testing.T, stack *Stack, orgCode, login, plainPwd string) SeededAccount {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	// Use context.Background for close — t.Context() is cancelled by the
	// time cleanup runs, so a ctx-bound Close call would refuse to send
	// the terminate packet and silently leak the backend session.
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	tenantID := uuid.New()
	userID := uuid.New()

	pwdHash, err := passwords.Default().Hash(ctx, plainPwd)
	require.NoError(t, err, "smoke seed: hash password")

	kekID := "smoke-kek-" + orgCode

	// tenants — status='active' is the only value the production code
	// path accepts for login; suspended/archived tenants would refuse to
	// authenticate at the service layer.
	_, err = conn.Exec(ctx,
		`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
		 VALUES ($1, $2, $3, 'active', $4, $5)`,
		tenantID, orgCode, "Smoke Tenant "+orgCode, kekID, smokePepperBytes)
	require.NoError(t, err, "smoke seed: insert tenant %s", orgCode)

	// users — roles is a text[] post-migration 000003. The admin role
	// matches authapi.Role("admin"); the JWT carries it verbatim.
	// must_change_pwd=false so the login succeeds in one step;
	// totp_enabled=false so no second-factor branch fires.
	_, err = conn.Exec(ctx,
		`INSERT INTO users (
			id, tenant_id, login, password_hash, full_name, email,
			roles, must_change_pwd, totp_enabled
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, false, false)`,
		userID, tenantID, login, pwdHash,
		"Admin "+login, login+"@smoke.local",
		[]string{"admin"})
	require.NoError(t, err, "smoke seed: insert user %s", login)

	t.Cleanup(func() {
		bg := context.Background()
		// Delete in FK order: users(tenant_id) → tenants. Errors are
		// swallowed because cleanup must not fail the test, and a row
		// the test already deleted would surface here harmlessly.
		_, _ = conn.Exec(bg, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = conn.Exec(bg, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	return SeededAccount{
		TenantID: tenantID,
		UserID:   userID,
		OrgCode:  orgCode,
		Login:    login,
		Password: plainPwd,
		Role:     "admin",
	}
}
