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
// KMS path note (Plan 21b Task 1): tenants.kms_kek_id is set to the
// deterministic "smoke-kek-default" id. WriteSmokeConfig publishes the
// matching 32-byte KEK under recording.local_keks, so cmd/api's
// recwire.LocalPorts builds a LocalDEKUnwrapper that recognises the id.
// Recording-touching scenarios (Plan 21b Task 5) reuse the same id via
// BuildRecordingFixture; tenancy-touching scenarios are unaffected
// (the KMSResolver path goes through pkg/encryption, not the recording
// LocalDEKUnwrapper).
//
// Earlier (Plan 21 Task 6) the value was the per-tenant
// "smoke-kek-<org_code>" string; the LocalDEKUnwrapper did NOT know
// that id, but the auth flow exercised by TestSmoke_AuthFullFlow does
// not decrypt anything keyed off the tenant KEK (login → user lookup
// → argon2id verify is KMS-free), so the dangling kek_id was fine.
// Plan 21b's recording-stream scenario forces the harness to register
// a known KEK; standardising on one id across every tenant keeps the
// smoke config minimal.
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

	kekID := smokeKEKID

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

// SeedOperator inserts an operator-role users row under the existing
// tenant identified by tenantID. Returns the public coordinates the
// caller needs to drive a login flow.
//
// Designed to be paired with SeedTenantAndAdmin so the
// TestSmoke_RbacEnforcement scenario can issue both an admin and an
// operator JWT under the SAME tenant — that scenario is about RBAC
// gating, not cross-tenant isolation, so reusing the tenant keeps the
// surface minimal.
//
// Like SeedTenantAndAdmin, this helper bypasses RLS via the
// testcontainer's superuser connection and runs a cleanup that deletes
// the row at test end (best-effort; sibling test cleanup may have
// already taken the row down).
func SeedOperator(t *testing.T, stack *Stack, tenantID uuid.UUID, login, plainPwd string) SeededAccount {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	userID := uuid.New()

	pwdHash, err := passwords.Default().Hash(ctx, plainPwd)
	require.NoError(t, err, "smoke seed: hash password")

	// Look up the org_code for the existing tenant so the returned
	// SeededAccount carries everything the login DTO needs. The
	// tenants row is owned by SeedTenantAndAdmin's cleanup — we only
	// read it here, never delete it.
	var orgCode string
	require.NoError(t,
		conn.QueryRow(ctx, `SELECT org_code FROM tenants WHERE id = $1`, tenantID).Scan(&orgCode),
		"smoke seed: lookup tenant org_code for %s", tenantID)

	// roles = {'operator'} per migration 000003's users_roles_valid
	// check constraint (subset of {operator,supervisor,admin}). The
	// JWT carries the role verbatim and the RBAC matrix gates against
	// it; "operator" is the canonical lowercase form authapi.Role uses.
	_, err = conn.Exec(ctx,
		`INSERT INTO users (
			id, tenant_id, login, password_hash, full_name, email,
			roles, must_change_pwd, totp_enabled
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, false, false)`,
		userID, tenantID, login, pwdHash,
		"Operator "+login, login+"@smoke.local",
		[]string{"operator"})
	require.NoError(t, err, "smoke seed: insert operator user %s", login)

	t.Cleanup(func() {
		// Best-effort delete — bg context because t.Context() is
		// cancelled by the time cleanup runs.
		_, _ = conn.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	return SeededAccount{
		TenantID: tenantID,
		UserID:   userID,
		OrgCode:  orgCode,
		Login:    login,
		Password: plainPwd,
		Role:     "operator",
	}
}

// SeedProject inserts a projects row under tenantID with the supplied
// code + name and returns the new project's id. status='active' is
// the only value compatible with both the 000001 check constraint
// ('active','paused','archived') and the live ProjectStatus enum.
//
// The columns we supply:
//   - id           : generated client-side for the deterministic return
//   - tenant_id    : FK → tenants.id
//   - code         : tenant-unique (UNIQUE(tenant_id, code) per 000001)
//   - name         : NOT NULL
//   - customer     : nullable at the schema level, but the crm
//     ProjectStore reads it into a plain `string`
//     (internal/crm/api/dto.go) — a NULL value would
//     fail pgx.Scan when the BypassRLS resolver reads
//     the row. Supply an empty-string sentinel so the
//     cross-tenant guard's lookup succeeds and the
//     middleware can compare tenants.
//   - status       : NOT NULL, check-constrained; 'active' is safe
//   - target_count : NOT NULL DEFAULT 0 — supplied explicitly so the
//     row shape is self-documenting
//
// All other columns are nullable / have defaults; survey_id stays NULL
// (no survey is needed for the TenantIsolation scenario — the project
// row's tenant_id is what the RequireSameTenant guard reads).
//
// Cleanup deletes the projects row at test end; the tenants/users rows
// belong to the SeedTenantAndAdmin cleanup chain.
func SeedProject(t *testing.T, stack *Stack, tenantID uuid.UUID, code, name string) uuid.UUID {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	projectID := uuid.New()
	_, err = conn.Exec(ctx,
		`INSERT INTO projects (id, tenant_id, code, name, customer, status, target_count)
		 VALUES ($1, $2, $3, $4, '', 'active', 0)`,
		projectID, tenantID, code, name)
	require.NoError(t, err, "smoke seed: insert project %s under tenant %s", code, tenantID)

	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID)
	})

	return projectID
}

// SeedCall inserts a calls row pointing at projectID under tenantID
// and returns the new call's id. The row is shaped to satisfy the
// 000001 schema's NOT NULL constraints and check predicates without
// pulling in respondents / operators / survey versions:
//   - id            : generated client-side
//   - tenant_id     : NOT NULL
//   - project_id    : FK → projects.id, NOT NULL
//   - status        : 'in-progress' is in the check set and the
//     default value; we set it explicitly so the row
//     shape is self-documenting
//
// Other columns the schema permits to be NULL stay NULL:
//
//	respondent_id, operator_id, survey_version_id, answered_at,
//	ended_at, duration_sec, hangup_cause, trunk_used, sip_call_id,
//	freeswitch_node, comment
//
// This is enough for the TenantIsolation scenario: the
// callTenantResolveFn in internal/dialer/transport/http/routes.go
// reads ONLY tenant_id from this row to gate the cross-tenant hangup
// attempt. The hangup handler itself is not reached because the
// RequireSameTenant middleware short-circuits with 404 first.
//
// Cleanup deletes the calls row at test end. FK cascades from
// projects/users are not exercised — the rows are taken down by their
// own seeders.
func SeedCall(t *testing.T, stack *Stack, tenantID, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	callID := uuid.New()
	_, err = conn.Exec(ctx,
		`INSERT INTO calls (id, tenant_id, project_id, status)
		 VALUES ($1, $2, $3, 'in-progress')`,
		callID, tenantID, projectID)
	require.NoError(t, err, "smoke seed: insert call under project %s tenant %s", projectID, tenantID)

	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM calls WHERE id = $1`, callID)
	})

	return callID
}
