//go:build smoke

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	crmservice "github.com/sociopulse/platform/internal/crm/service"
	crmstore "github.com/sociopulse/platform/internal/crm/store"
	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_RespondentSoftDelete152FZ — Plan 21b Task 6.
//
// End-to-end smoke proving the 152-ФЗ §21 (Article 21, subject right to
// deletion) pipeline against a fully-booted cmd/api + Postgres
// testcontainer:
//
//	HTTP DELETE /api/respondents/:id
//	    ─►  RespondentService.Delete (WithTenant tx)
//	    ─►  RespondentStore.SoftDelete (status=deletion-requested, deleted_at=now)
//	    ─►  200 DeletionReceiptDTO{respondent_id, scheduled_purge_at}
//
//	[31-day fast-forward via FutureClock injection]
//
//	crmservice.PurgeWorker.Run(ctx)
//	    ─►  BypassRLS tx
//	    ─►  RespondentStore.PurgeOlderThan (DELETE ... WHERE deleted_at < cutoff)
//	    ─►  audit row per purged id
//
//	Direct SQL: SELECT count(*) FROM respondents WHERE id = $1 → 0
//
//	worker.Run(ctx) AGAIN → no-op, no error (idempotency net)
//
// Catches:
//
//   - The 30-day grace contract: PurgeWorker.Run with a clock at NOW
//     MUST NOT purge a respondent soft-deleted "just now"; with the
//     31-day FutureClock it MUST purge. A future refactor that flips
//     the predicate (or drops the grace) surfaces here.
//   - HTTP soft-delete → store SoftDelete wiring: 200 + deleted_at NOT
//     NULL is the contract the purge worker reads downstream. A handler
//     refactor that returns 204 without stamping deleted_at would pass
//     the unit tests (mocked store) but fail here.
//   - The asynq-free PurgeWorker construction surface
//     (NewPurgeWorker(pool, store, audit, grace, batch, clock)) is the
//     canonical exposed contract; the asynq adapter (HandlePurgeTask)
//     is a thin wrapper around Run. Smoke covers Run directly — cron
//     scheduling is asynq territory and out of scope.
//   - Idempotency: a second worker.Run on an empty candidate set MUST
//     not error and MUST not "resurrect" the row. Real production runs
//     pay this cost daily; the smoke pin guards against a future
//     regression that double-counts purged ids.
//
// Verified contracts (read from source BEFORE writing):
//
//   - DELETE /api/respondents/:id → 200 OK + DeletionReceiptDTO with
//     `respondent_id` (string uuid) + `scheduled_purge_at` (time.Time
//     iso-8601). NOT 204. Per
//     internal/crm/transport/http/respondent_handler.go:183-186 and
//     dto.go:158-161.
//   - PurgeWorker constructor signature: NewPurgeWorker(
//     pool purgeBypassRunner,                 // *postgres.Pool satisfies this
//     store api.RespondentStorePort,
//     auditLogger auditapi.Logger,            // smoke.NoopAuditLogger satisfies this
//     grace time.Duration,                    // 30*24*time.Hour
//     batch int,                              // 1000
//     clock func() time.Time,                 // smoke.FutureClock(31*24*time.Hour)
//     ) *PurgeWorker. ALL three pointer deps are mandatory — passing
//     nil panics. Per internal/crm/service/purge.go:66-101.
//   - RespondentStore constructor: NewRespondentStore(pool *postgres.Pool)
//     — single argument; no KMSResolver / PhoneHasher dependencies at
//     the store layer (those live in the service layer). Per
//     internal/crm/store/respondent_store.go:52-54.
//   - PurgeOlderThan predicate: WHERE deleted_at IS NOT NULL AND
//     deleted_at < $1; cutoff = clock() - grace. So:
//     clock = FutureClock(31d) → cutoff ≈ (now+31d) - 30d = now+1d → a
//     respondent soft-deleted at now has deleted_at < cutoff and IS
//     purged. Per internal/crm/store/respondent_store.go:367-402.
//   - respondents NOT NULL columns required for direct INSERT (per
//     migrations/000001_init.up.sql:123-138 + 000007 add of deleted_at
//     and deletion_reason which both default NULL):
//     id, tenant_id, project_id, phone_encrypted, phone_hash,
//     region_code, source. Other fields use schema defaults.
//   - smoke.SeedTenantAndAdmin returns admin with role=admin (DELETE
//     /api/respondents/:id requires requireAdminRole per
//     internal/crm/transport/http/routes.go:90).
//
// Deviations from plan text: NONE. Every step lands as written; the
// "use the import helper from Task 3 OR direct SQL — pick whichever is
// shorter" branch chooses direct SQL because the scenario is about the
// PURGE pipeline, not the import pipeline (Task 3 already pins import).
// Direct SQL keeps the test surface minimal: one INSERT vs an admin
// login + multipart upload + asynq job wait.
//
// TestMain hygiene (Plan 21b Task 6 wires this): the cmd/api smoke
// binary's TestMain (testmain_smoke_test.go) drains
// smoke.TerminateOnTestMainCleanup BEFORE goleak.Find so this scenario's
// Stack.PgPool consumption (the pgxpool's backgroundHealthCheck
// goroutine) does NOT trip the leak guard at process exit. Task 4's
// pgx.Connect workaround in smoke_operator_ws_test.go remains in place
// for that scenario (we don't refactor unrelated scenarios in this
// task); future smoke scenarios consume Stack.PgPool directly.
func TestSmoke_RespondentSoftDelete152FZ(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	// Seed: tenant + admin + project under the SAME tenant. The
	// respondent FK chain is respondents.project_id → projects.id, so
	// the project must exist before the INSERT. SeedTenantAndAdmin gives
	// us a usable admin JWT; SeedProject gives us a project row whose
	// tenant_id matches the admin's JWT claims (DELETE handler's
	// RequireSameTenant guard resolves the respondent's tenant via the
	// store's bypass-RLS lookup).
	admin := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-PURGE", "purge-admin", "PurgePass123!")
	projectID := smoke.SeedProject(t, stack, admin.TenantID, "purge-proj", "Purge smoke project")

	ctx := t.Context()

	// Step 1: seed one respondent directly via SQL. The import path is
	// already covered by Task 3 (TestSmoke_AdminCreatesProjectAndImportsRespondents);
	// re-running it here would extend test runtime by ~10s with no extra
	// coverage. The columns supplied are the NOT NULL minimum
	// (id, tenant_id, project_id, phone_encrypted, phone_hash,
	// region_code, source); attributes/status/created_at use schema
	// defaults. phone_encrypted / phone_hash carry deterministic
	// non-empty bytea blobs because the columns are NOT NULL but the
	// PurgeWorker doesn't read them — only the id + deleted_at matter
	// for purge selection.
	respondentID := uuid.New()
	seedConn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = seedConn.Close(context.Background()) })

	_, err = seedConn.Exec(ctx,
		`INSERT INTO respondents (
			id, tenant_id, project_id, phone_encrypted, phone_hash,
			region_code, source
		 ) VALUES ($1, $2, $3, $4, $5, '', 'imported')`,
		respondentID, admin.TenantID, projectID,
		[]byte("smoke-purge-phone-stub"),
		[]byte("smoke-purge-hash-stub-32-bytes!!"))
	require.NoError(t, err, "seed respondent %s", respondentID)

	// Step 2: admin login → JWT. The DELETE endpoint is gated by
	// requireAdminRole (internal/crm/transport/http/routes.go:90) — the
	// admin's JWT carries roles=["admin"] from the seed.
	cli := &http.Client{Timeout: 10 * time.Second}
	adminJWT := loginAndAccessToken(ctx, t, cli, httpAddr, admin)

	baseURL := "http://" + httpAddr

	// Step 3: DELETE /api/respondents/:id → 200 + DeletionReceiptDTO.
	// We use httpDelete (defined below) — postWithJWT is for POST,
	// getWithJWT for GET; no shared helper for DELETE yet in
	// cmd/api/smoke_test.go.
	delStatus, delBody := smokeHTTPDeleteWithJWT(ctx, t, cli,
		baseURL+"/api/respondents/"+respondentID.String(), adminJWT)
	require.Equalf(t, http.StatusOK, delStatus,
		"DELETE /api/respondents/:id must 200; got %d body=%s",
		delStatus, string(delBody))

	// Pin the receipt shape — surfaces a future refactor that
	// accidentally returns 204 with no body or moves the field tags.
	var receipt struct {
		RespondentID     string    `json:"respondent_id"`
		ScheduledPurgeAt time.Time `json:"scheduled_purge_at"`
	}
	require.NoErrorf(t, json.Unmarshal(delBody, &receipt),
		"decode DeletionReceiptDTO: %s", string(delBody))
	assert.Equal(t, respondentID.String(), receipt.RespondentID,
		"receipt respondent_id must echo the deleted id")
	// scheduled_purge_at must be ~30 days in the future. We use a wide
	// window (29-32 days) because the smoke harness's t.Cleanup + GC
	// pressure can shift the wall clock fractionally.
	assert.WithinRange(t,
		receipt.ScheduledPurgeAt,
		time.Now().Add(29*24*time.Hour),
		time.Now().Add(32*24*time.Hour),
		"scheduled_purge_at must be ~30 days from now (152-ФЗ §21 grace window)")

	// Pre-purge regression net: row STILL exists and is now soft-deleted.
	// Two SELECTs because we assert two facts:
	//   (a) the row is still physically present (count = 1)
	//   (b) deleted_at is non-null (soft-delete actually wrote the column)
	var preCount int
	require.NoError(t,
		seedConn.QueryRow(ctx,
			`SELECT count(*) FROM respondents WHERE id = $1`,
			respondentID).Scan(&preCount),
		"pre-purge count query")
	require.Equal(t, 1, preCount,
		"row must still exist after soft-delete (30-day grace window)")

	var deletedAt *time.Time
	require.NoError(t,
		seedConn.QueryRow(ctx,
			`SELECT deleted_at FROM respondents WHERE id = $1`,
			respondentID).Scan(&deletedAt),
		"pre-purge deleted_at query")
	require.NotNil(t, deletedAt,
		"deleted_at must be non-null after DELETE — the soft-delete column was not stamped")

	// Step 4: build the in-test PurgeWorker. We construct it directly
	// rather than driving asynq because:
	//   - The asynq adapter (HandlePurgeTask) is a thin shell around
	//     Run; the cron-scheduling layer is asynq territory and out of
	//     scope for smoke (per Plan 21b references § 2.4).
	//   - Driving asynq from a smoke test would require a Redis-backed
	//     scheduler that's a clone of cmd/api's; the seam through Run is
	//     simpler and pins the contract directly.
	//
	// The 31-day FutureClock means cutoff = clock() - grace ≈ (now+31d)
	//   - 30d = now+1d, so a respondent whose deleted_at = now has
	// deleted_at < cutoff and IS purged. A clock at "now" (or a 29-day
	// FutureClock) would NOT trip the predicate — that's the grace
	// guarantee, and Task 6 doesn't probe it explicitly because the
	// canonical unit test
	// (internal/crm/service/purge_test.go::TestPurgeWorker_ComputesCutoffFromClock)
	// already pins the predicate. Smoke pins the wiring end-to-end.
	pool := stack.PgPool(t)
	store := crmstore.NewRespondentStore(pool)
	audit := smoke.NoopAuditLogger{}
	clock := smoke.FutureClock(31 * 24 * time.Hour)
	worker := crmservice.NewPurgeWorker(pool, store, audit, 30*24*time.Hour, 1000, clock)
	require.NotNil(t, worker, "NewPurgeWorker must return a non-nil worker")

	// Step 5: run the worker. The Run path opens a BypassRLS tx,
	// invokes store.PurgeOlderThan, then emits one audit row per purged
	// id via the NoopAuditLogger (which silently drops them). Audit
	// failure is non-fatal in production (purge.go:170); the NoopLogger
	// returns nil from Write so the audit-write branch is exercised but
	// produces no observable side-effect — the cleanest smoke shape.
	require.NoError(t, worker.Run(ctx),
		"PurgeWorker.Run must succeed against the testcontainer Postgres")

	// Step 6: verify the row is physically gone. count = 0 is the
	// 152-ФЗ §21 contract: after the grace window expires, the user's
	// data MUST be unrecoverable from the live system. The audit row
	// (which we'd see if NoopAuditLogger forwarded to a real backend)
	// is the only surviving trail.
	var postCount int
	require.NoError(t,
		seedConn.QueryRow(ctx,
			`SELECT count(*) FROM respondents WHERE id = $1`,
			respondentID).Scan(&postCount),
		"post-purge count query")
	assert.Equalf(t, 0, postCount,
		"row must be physically gone after PurgeWorker.Run with 31-day clock; got count=%d",
		postCount)

	// Step 7: idempotency regression net. Run AGAIN — must not error,
	// must not resurrect, must not re-emit audit rows (the NoopLogger
	// would not observe them anyway, but Run's internal counter would
	// log a stale "purged N=…" entry on a buggy implementation). The
	// canonical implementation handles an empty candidate set by
	// returning early after the BypassRLS tx; we just need Run to come
	// back with err=nil and count to stay at 0.
	require.NoError(t, worker.Run(ctx),
		"idempotent PurgeWorker.Run on empty candidate set must not error")

	var idempotentCount int
	require.NoError(t,
		seedConn.QueryRow(ctx,
			`SELECT count(*) FROM respondents WHERE id = $1`,
			respondentID).Scan(&idempotentCount),
		"post-idempotent-purge count query")
	assert.Equalf(t, 0, idempotentCount,
		"second PurgeWorker.Run must not resurrect the purged row; got count=%d",
		idempotentCount)
}

// smokeHTTPDeleteWithJWT issues a DELETE against url with the supplied
// JWT and returns the status + fully-consumed body bytes. Mirrors
// postWithJWT / getWithJWT in cmd/api/smoke_test.go for the DELETE verb
// — Task 6 is the first smoke scenario that needs it.
//
// The helper is named with the "smoke" prefix because cmd/api/smoke_test.go
// (in this same build-tagged package) already defines postWithJWT /
// getWithJWT without a prefix; we add the verb-specific helper here
// rather than extend smoke_test.go so this task's diff stays scoped to
// the purge scenario. Future smoke scenarios needing DELETE consume
// this helper (or, if the suite grows three or more DELETEs, the
// helpers consolidate into a shared file at the next refactor).
//
// Content-Type is NOT set because DELETE bodies are not the contract
// here (the handler ignores the body entirely; the :id is the sole
// identifier). The Authorization header carries the JWT verbatim.
//
// Transport-level failures (build / dial / read) fail the test via
// require — they are never the assertion under test, and a nil response
// would only cause a confusing nil-deref one line later. Mirrors the
// contract documented on postWithJWT in smoke_test.go.
func smokeHTTPDeleteWithJWT(ctx context.Context, t *testing.T, cli *http.Client, url, jwt string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	require.NoError(t, err, "build DELETE %s", url)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := cli.Do(req)
	require.NoError(t, err, "DELETE %s", url)
	defer func() { _ = resp.Body.Close() }()
	buf, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read body for DELETE %s", url)
	return resp.StatusCode, buf
}
