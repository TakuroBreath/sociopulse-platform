//go:build smoke

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_AdminCreatesProjectAndImportsRespondents — Plan 21b Task 3.
//
// End-to-end smoke proving the crm module's import pipeline:
//
//	HTTP multipart  ─►  RespondentService.Import (enqueue)
//	                ─►  asynq (Redis-backed queue in cmd/api)
//	                ─►  HandleImportTask (parseCSV + normalise + KMS encrypt)
//	                ─►  postgres respondents.InsertBatch (CopyFrom)
//
// The scenario asserts every contract on that path:
//
//  1. POST /api/projects                                → 201 + ProjectDTO{id}
//  2. POST /api/projects/:pid/respondents/import        → 202 + ImportTicketDTO{job_id}
//  3. Poll GET /api/imports/:jid via WaitForImportStatus → terminal state "succeeded"
//  4. Direct SQL: 2 respondents under tenantA + project (phone_encrypted IS NOT NULL,
//     phone_hash IS NOT NULL — proves the KMS-encrypt + HMAC-pepper path actually ran).
//  5. (Cross-tenant regression net) tenant-B JWT importing into tenant-A's project
//     → 404 via projectSameTenant middleware (RequireSameTenant + BypassRLS resolver).
//
// Verified contracts (from source, BEFORE writing this test):
//
//   - CreateProjectRequest body shape: {code, name, customer?, target_count?, ...}
//     — internal/crm/transport/http/dto.go::CreateProjectRequest (lines 22-30).
//     code + name are required by binding tag; we send those two.
//   - ProjectDTO response: {id: string, tenant_id, code, name, ...} —
//     same file lines 64-79. Status code 201 on success.
//   - Import endpoint: POST /api/projects/:id/respondents/import; multipart
//     form field name is "file" (respondent_handler.go:217); the format
//     hint comes from PostForm("format") or query string (handler line
//     235). Status code 202 on success.
//   - ImportTicketDTO: {job_id, project_id, enqueued, status, started_at}
//     — dto.go:163-170. job_id is the opaque async-job ticket id.
//   - ImportStatusDTO state literal for the terminal happy path:
//     "succeeded" — internal/crm/service/import_progress.go:86. WaitForImportStatus
//     uses that literal as the target.
//   - CSV header column order: phone, full_name, external_ref — matches
//     tests/smoke/respondent_helpers.go::csvImportHeader (verified).
//   - respondents schema: phone_encrypted bytea NOT NULL + phone_hash bytea
//     NOT NULL (migrations/000001_init.up.sql:127-128). We assert non-NULL
//     (NOT the bytes themselves) per Plan-21 retro PII discipline.
//   - Cross-tenant guard: routes.go:83 chains projectSameTenant on the
//     import endpoint → ErrNotFound translates to 404 via the resolver
//     wrapper.
func TestSmoke_AdminCreatesProjectAndImportsRespondents(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	adminA := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-IMP-A", "imp-admin-a", "ImpPassA123!")
	adminB := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-IMP-B", "imp-admin-b", "ImpPassB123!")

	ctx := t.Context()
	cli := &http.Client{Timeout: 15 * time.Second}

	adminAJWT := loginAndAccessToken(ctx, t, cli, httpAddr, adminA)
	adminBJWT := loginAndAccessToken(ctx, t, cli, httpAddr, adminB)

	baseURL := "http://" + httpAddr

	// 1. Admin A creates a project. The crm transport requires the admin
	// role on POST /api/projects (routes.go:71-72); the seeded admin
	// satisfies that gate.
	createBody := `{"code":"smoke-imp","name":"Import smoke"}`
	createStatus, createBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/projects", adminAJWT, createBody)
	require.Equalf(t, http.StatusCreated, createStatus,
		"POST /api/projects must 201 for admin; got %d body=%s", createStatus, string(createBytes))

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(createBytes, &created),
		"decode ProjectDTO id: %s", string(createBytes))
	projectID, err := uuid.Parse(created.ID)
	require.NoErrorf(t, err, "project id must parse as UUID; got %q", created.ID)
	require.NotEqual(t, uuid.Nil, projectID, "project id must be non-nil UUID")

	// 2. Build the CSV body. Column order: phone, full_name, external_ref
	// (csvImportHeader in respondent_helpers.go). NormalizeRussianPhone
	// in internal/crm/service accepts +7 / 8 prefixed numbers; the
	// chosen fixtures are canonical E.164 +7... which the normaliser
	// emits verbatim.
	csvBody := smoke.BuildCSVImport([][]string{
		{"+79001234567", "Alice", "ext-1"},
		{"+79007654321", "Bob", "ext-2"},
	})
	require.NotEmpty(t, csvBody, "CSV body must be non-empty")

	// 3. POST the multipart import. The "format=csv" form field is the
	// explicit hint the handler reads via c.PostForm("format")
	// (respondent_handler.go:235); the filename is carried in the
	// multipart fileHeader (handler line 234) and is also used by
	// inferImportFormat as a fallback.
	importURL := fmt.Sprintf("%s/api/projects/%s/respondents/import",
		baseURL, projectID.String())
	importStatus, importBytes := postMultipartCSV(ctx, t, cli, importURL,
		adminAJWT, "phones.csv", csvBody)
	require.Equalf(t, http.StatusAccepted, importStatus,
		"POST /api/projects/:id/respondents/import must 202; got %d body=%s",
		importStatus, string(importBytes))

	var ticket struct {
		JobID     string `json:"job_id"`
		ProjectID string `json:"project_id"`
		Enqueued  bool   `json:"enqueued"`
		Status    string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(importBytes, &ticket),
		"decode ImportTicketDTO: %s", string(importBytes))
	require.NotEmpty(t, ticket.JobID, "ticket.job_id must be present (drives the status endpoint)")
	assert.Equal(t, projectID.String(), ticket.ProjectID,
		"ticket.project_id must echo the path :id")
	assert.True(t, ticket.Enqueued,
		"ticket.enqueued must be true on the first happy-path POST (subsequent retries return false via asynq.Unique)")

	// 4. Poll until the asynq worker (registered in cmd/api via
	// crm.Module) drives the job to the terminal "succeeded" state.
	// WaitForImportStatus mirrors the literal from
	// import_progress.go::stateSucceeded.
	smoke.WaitForImportStatus(t, httpAddr, adminAJWT, ticket.JobID, "succeeded")

	// 5. Direct SQL: both rows landed AND the KMS-encrypted-phone columns
	// are populated. We do NOT decrypt — the assertion is "the encrypt
	// + hash path actually executed", not "the ciphertext is the right
	// shape" (per Plan-21 retro PII discipline). Querying via the smoke
	// superuser connection bypasses RLS so the count returns regardless
	// of session app.tenant_id.
	assertRespondentsLandedWithEncryptedPhones(t, stack, adminA.TenantID, projectID, 2)

	// 6. (Cross-tenant regression net) Tenant B's admin tries to import
	// into Tenant A's project → 404. The projectSameTenant middleware
	// on routes.go:83 resolves the project's owning tenant via
	// BypassRLS and rejects the mismatched caller before the handler
	// runs. The body is consumed but never reaches the asynq enqueue
	// path — no rogue job is created.
	xtCSV := smoke.BuildCSVImport([][]string{
		{"+79009998877", "Carol", "ext-3"},
	})
	xtStatus, xtBytes := postMultipartCSV(ctx, t, cli, importURL,
		adminBJWT, "phones-tenant-b.csv", xtCSV)
	assert.Equalf(t, http.StatusNotFound, xtStatus,
		"cross-tenant import must 404 (projectSameTenant guard); got %d body=%s",
		xtStatus, string(xtBytes))
}

// postMultipartCSV issues a multipart POST against url carrying the
// supplied CSV bytes under form field "file" plus the "format=csv"
// hint the import handler reads via c.PostForm. The JWT is attached as
// a Bearer token; Content-Type is set to the multipart boundary string
// the multipart.Writer wrote (the handler test on contentType[:10] ==
// "multipart/" in respondent_handler.go:216 is the gate we satisfy).
//
// Mirrors postWithJWT's error-handling contract — transport / build /
// read failures fail the test via require.NoError; the caller asserts
// only on the returned status code + body.
func postMultipartCSV(ctx context.Context, t *testing.T, cli *http.Client, url, jwt, filename string, body []byte) (int, []byte) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// "format" form field — the handler reads it via c.PostForm("format")
	// (respondent_handler.go:235). Sending "csv" makes the format-detect
	// branch deterministic regardless of how the filename extension is
	// interpreted on the receiving side.
	require.NoError(t, w.WriteField("format", "csv"),
		"build multipart format field")

	// "file" form field carrying the CSV bytes. The handler reads the
	// file via c.FormFile("file") (respondent_handler.go:217).
	part, err := w.CreateFormFile("file", filename)
	require.NoError(t, err, "build multipart file part")
	_, err = part.Write(body)
	require.NoError(t, err, "write CSV bytes into multipart part")

	require.NoError(t, w.Close(), "close multipart writer")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	require.NoError(t, err, "build POST %s", url)
	// Content-Type carries the multipart boundary the writer chose.
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := cli.Do(req)
	require.NoError(t, err, "POST %s", url)
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read body for POST %s", url)
	return resp.StatusCode, respBody
}

// assertRespondentsLandedWithEncryptedPhones opens a pgx connection on
// the smoke superuser DSN and asserts the import produced exactly
// :expected respondents under (tenantID, projectID), AND that every
// row's phone_encrypted + phone_hash columns are non-NULL.
//
// The superuser connection bypasses RLS so the query returns rows
// regardless of session app.tenant_id — the (tenant_id, project_id)
// predicate is what scopes the count to the test's fixture.
//
// We intentionally do NOT inspect the ciphertext bytes:
//   - The smoke test is a wiring regression net, not a crypto test;
//     unit/integration coverage in internal/crm/service/import_test.go
//     already round-trips encrypt/decrypt.
//   - PII discipline (CLAUDE.md golang-security): phone bytes are PII,
//     and even ciphertext shouldn't pollute the test stdout on failure.
func assertRespondentsLandedWithEncryptedPhones(t *testing.T, stack *smoke.Stack, tenantID, projectID uuid.UUID, expected int) {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "open pg conn for respondent assertion")
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	var totalCount int
	require.NoError(t,
		conn.QueryRow(ctx,
			`SELECT count(*) FROM respondents WHERE tenant_id = $1 AND project_id = $2`,
			tenantID, projectID).Scan(&totalCount),
		"count respondents under tenant+project")
	require.Equalf(t, expected, totalCount,
		"import must produce %d respondents under tenant %s / project %s; got %d",
		expected, tenantID, projectID, totalCount)

	// Phone columns are bytea NOT NULL at the schema level
	// (migrations/000001_init.up.sql:127-128); the count below proves the
	// IMPORT path populated them (a failed encrypt would have aborted the
	// batch with a non-nil error before InsertBatch ran).
	var encryptedCount int
	require.NoError(t,
		conn.QueryRow(ctx,
			`SELECT count(*) FROM respondents
			 WHERE tenant_id = $1 AND project_id = $2
			   AND phone_encrypted IS NOT NULL
			   AND octet_length(phone_encrypted) > 0
			   AND phone_hash IS NOT NULL
			   AND octet_length(phone_hash) > 0`,
			tenantID, projectID).Scan(&encryptedCount),
		"count respondents with non-empty phone_encrypted + phone_hash")
	assert.Equalf(t, expected, encryptedCount,
		"every imported respondent must carry non-empty phone_encrypted + phone_hash (KMS encrypt + HMAC pepper path executed); got %d of %d",
		encryptedCount, expected)
}
