//go:build smoke

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_SurveyCreatePreviewActivate — Plan 21b Task 2.
//
// Walks the public HTTP surface of the surveys module end-to-end against
// a real cmd/api + Postgres testcontainer:
//
//  1. POST /api/surveys           → 201 + {id}
//  2. POST /api/surveys/:id/versions          → 201 + VersionDTO (major=1, minor=0)
//  3. POST /api/surveys/:id/versions/:vid/activate → 204
//  4. POST /api/surveys/:id/preview/run                 → 200 + {next_node_id="q1", terminated=false}
//  5. POST a SECOND version + activate it → 204 (regression net)
//  6. SELECT count(*) FROM survey_versions WHERE survey_id=:id AND is_active=true
//     MUST return exactly 1 — the partial-unique index
//     `survey_versions_active_one` (migrations/000001_init.up.sql:183-184)
//     enforces single-active-version-per-survey at the DB level.
//
// Catches the surveys-module wiring failure class: a future refactor
// that drops the transport→service wiring, breaks the version-row
// FK chain, or weakens the activation atomicity (advisory-lock loss,
// dropped DeactivateAll, transaction-isolation drift) surfaces here.
//
// Deviations from Plan 21b Task 2 (verified against actual transport
// source, internal/surveys/transport/http/{dto,handlers}.go):
//
//   - Create body uses {"name": ...} (CreateSurveyRequest in dto.go has
//     name + description + primary_mode — no "code" field; plan text
//     said {"code":"smoke-surv-1","name":"..."} which would silently
//     ignore the code field). We include primary_mode="flow" so it
//     matches the schema fixture (MinimalValidSurveySchema is a
//     flow-mode graph).
//   - Save-version response is the full VersionDTO ({id, survey_id,
//     major:int, minor:int, schema, is_active, created_at, ...}) — not
//     {version_id, semver} as the plan body suggested. We assert
//     major=1 + minor=0 (the canonical first-version tuple computed by
//     SurveyService.computeNextVersion at survey_service.go:514-518).
//   - Activate returns 204 No Content (handlers.go:252:
//     c.Status(http.StatusNoContent)) — not 200 as the plan said.
func TestSmoke_SurveyCreatePreviewActivate(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	admin := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-SURV", "surv-admin", "SurvPass123!")

	ctx := t.Context()
	cli := &http.Client{Timeout: 10 * time.Second}
	adminJWT := loginAndAccessToken(ctx, t, cli, httpAddr, admin)

	baseURL := "http://" + httpAddr

	// 1. Create survey — admin endpoint, returns 201 + {id}.
	createBody := `{"name":"Smoke survey","description":"Plan 21b Task 2 fixture","primary_mode":"flow"}`
	createStatus, createBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/surveys", adminJWT, createBody)
	require.Equalf(t, http.StatusCreated, createStatus,
		"POST /api/surveys must 201 for admin; got %d body=%s", createStatus, string(createBytes))

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(createBytes, &created),
		"decode CreateSurveyResponse: %s", string(createBytes))
	surveyID, err := uuid.Parse(created.ID)
	require.NoErrorf(t, err, "created survey id must be a valid UUID; got %q", created.ID)
	require.NotEqual(t, uuid.Nil, surveyID, "created survey id must be non-nil UUID")

	// 2. SaveVersion — admin endpoint, returns 201 + VersionDTO. The
	// fixture (MinimalValidSurveySchema) is the canonical 3-node graph
	// (start → q1 → end_ok) that the schema validator accepts. Wrapping
	// it inside {"schema": <fixture>, "minor": false} matches
	// SaveVersionRequest in dto.go.
	saveVersionBody := buildSaveVersionBody(t, smoke.MinimalValidSurveySchema(), false)
	saveStatus, saveBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/surveys/"+surveyID.String()+"/versions",
		adminJWT, saveVersionBody)
	require.Equalf(t, http.StatusCreated, saveStatus,
		"POST /api/surveys/:id/versions must 201; got %d body=%s", saveStatus, string(saveBytes))

	firstVersion := decodeVersionDTO(t, saveBytes)
	require.NotEqual(t, uuid.Nil, firstVersion.ID, "version id must be non-nil UUID")
	assert.Equal(t, surveyID, firstVersion.SurveyID, "version.survey_id must match parent survey")
	assert.Equal(t, 1, firstVersion.Major, "first version major must be 1")
	assert.Equal(t, 0, firstVersion.Minor, "first version minor must be 0")
	assert.False(t, firstVersion.IsActive,
		"newly-saved version must not be active until POST .../activate runs")

	// 3. Activate — admin endpoint, returns 204 No Content. No body.
	activateURL := fmt.Sprintf("%s/api/surveys/%s/versions/%s/activate",
		baseURL, surveyID, firstVersion.ID)
	activateStatus, activateBytes := postWithJWT(ctx, t, cli, activateURL, adminJWT, ``)
	require.Equalf(t, http.StatusNoContent, activateStatus,
		"POST .../activate must 204 No Content; got %d body=%s",
		activateStatus, string(activateBytes))
	assert.Empty(t, activateBytes, "204 response must have empty body")

	// 4. Preview run — operator+ endpoint. Admin satisfies the role gate.
	// CurrentNodeID="start" is the canonical preview-start call; the
	// runtime resolves the unconditional edge to "q1" without needing
	// any answers (runtime.NextNode at runtime/runtime.go:76 handles
	// start-edge resolution).
	previewBody := buildPreviewRunBody(t, smoke.MinimalValidSurveySchema(), "start")
	previewStatus, previewBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/surveys/"+surveyID.String()+"/preview/run",
		adminJWT, previewBody)
	require.Equalf(t, http.StatusOK, previewStatus,
		"POST .../preview/run must 200; got %d body=%s",
		previewStatus, string(previewBytes))

	var preview struct {
		NextNodeID string  `json:"next_node_id,omitempty"`
		Terminated bool    `json:"terminated"`
		EndKind    string  `json:"end_kind,omitempty"`
		Progress   float64 `json:"progress"`
	}
	require.NoError(t, json.Unmarshal(previewBytes, &preview),
		"decode PreviewRunResponse: %s", string(previewBytes))
	assert.Equal(t, "q1", preview.NextNodeID,
		"start → q1 is the only unconditional edge in the minimal fixture")
	assert.False(t, preview.Terminated, "preview at start must not terminate")

	// 5. (Regression net) Save a SECOND version + activate it. The
	// schema is the same fixture bytes (mutating the helper output is
	// safe — MinimalValidSurveySchema returns a fresh slice each call —
	// but using the unchanged bytes is enough: SaveVersion only checks
	// validator + uniqueness of (major, minor), not content uniqueness).
	saveVersionBody2 := buildSaveVersionBody(t, smoke.MinimalValidSurveySchema(), false)
	save2Status, save2Bytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/surveys/"+surveyID.String()+"/versions",
		adminJWT, saveVersionBody2)
	require.Equalf(t, http.StatusCreated, save2Status,
		"second SaveVersion must 201; got %d body=%s", save2Status, string(save2Bytes))

	secondVersion := decodeVersionDTO(t, save2Bytes)
	require.NotEqual(t, firstVersion.ID, secondVersion.ID,
		"second version must have distinct id")
	assert.Equal(t, 2, secondVersion.Major,
		"minor=false on a survey with existing versions must bump major: latestMajor+1=2")
	assert.Equal(t, 0, secondVersion.Minor, "major-bump always resets minor to 0")

	activate2URL := fmt.Sprintf("%s/api/surveys/%s/versions/%s/activate",
		baseURL, surveyID, secondVersion.ID)
	activate2Status, activate2Bytes := postWithJWT(ctx, t, cli, activate2URL, adminJWT, ``)
	require.Equalf(t, http.StatusNoContent, activate2Status,
		"second activate must 204; got %d body=%s",
		activate2Status, string(activate2Bytes))

	// 6. Direct SQL: exactly ONE row with is_active=true under this
	// survey. The partial-unique index `survey_versions_active_one`
	// (migrations/000001_init.up.sql:183-184) makes two-active a 23505
	// at the DB level; this assertion plus the successful second activate
	// proves the surveys/service Activate path correctly DeactivateAlls
	// before flipping the new row.
	assertExactlyOneActiveVersion(t, stack, surveyID, secondVersion.ID)
}

// versionDTO is a local projection of the survey VersionDTO wire shape
// so the test does not import internal/surveys/transport/http (which
// would tighten the build-tag matrix). The fields we assert on are a
// subset of the full DTO; json decoding ignores the rest.
type versionDTO struct {
	ID       uuid.UUID `json:"id"`
	SurveyID uuid.UUID `json:"survey_id"`
	Major    int       `json:"major"`
	Minor    int       `json:"minor"`
	IsActive bool      `json:"is_active"`
}

// decodeVersionDTO parses a SaveVersion / activate response payload and
// fails the test on a malformed body. Extracted so the scenario steps
// stay readable.
func decodeVersionDTO(t *testing.T, body []byte) versionDTO {
	t.Helper()
	var v versionDTO
	require.NoError(t, json.Unmarshal(body, &v),
		"decode VersionDTO: %s", string(body))
	return v
}

// buildSaveVersionBody renders a SaveVersionRequest JSON body around
// the supplied schema bytes. The schema is embedded verbatim via
// json.RawMessage so the validator sees the same bytes the helper
// returned (no re-marshal — preserves whitespace + key order).
func buildSaveVersionBody(t *testing.T, schema []byte, minor bool) string {
	t.Helper()
	body := struct {
		Schema json.RawMessage `json:"schema"`
		Minor  bool            `json:"minor"`
	}{
		Schema: schema,
		Minor:  minor,
	}
	out, err := json.Marshal(body)
	require.NoError(t, err, "marshal SaveVersionRequest body")
	return string(out)
}

// buildPreviewRunBody renders a PreviewRunRequest JSON body around the
// supplied schema bytes + current node id. Answers stays nil; the
// minimal fixture's start→q1 edge is unconditional.
func buildPreviewRunBody(t *testing.T, schema []byte, currentNodeID string) string {
	t.Helper()
	body := struct {
		Schema        json.RawMessage `json:"schema"`
		CurrentNodeID string          `json:"current_node_id"`
	}{
		Schema:        schema,
		CurrentNodeID: currentNodeID,
	}
	out, err := json.Marshal(body)
	require.NoError(t, err, "marshal PreviewRunRequest body")
	return string(out)
}

// assertExactlyOneActiveVersion opens a direct pgx connection on the
// smoke superuser DSN and asserts that survey :surveyID has exactly one
// is_active=true row in survey_versions, AND that the active row's id
// matches :expectedID. Bypassing cmd/api keeps the assertion close to
// the DB-level invariant (the partial-unique index is what guarantees
// the property).
func assertExactlyOneActiveVersion(t *testing.T, stack *smoke.Stack, surveyID, expectedID uuid.UUID) {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "open pg conn for active-version assertion")
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	var activeCount int
	require.NoError(t,
		conn.QueryRow(ctx,
			`SELECT count(*) FROM survey_versions WHERE survey_id = $1 AND is_active = true`,
			surveyID).Scan(&activeCount),
		"count active versions")
	require.Equalf(t, 1, activeCount,
		"survey_versions_active_one partial-unique constraint must enforce exactly one active row per survey; got %d", activeCount)

	var activeID uuid.UUID
	require.NoError(t,
		conn.QueryRow(ctx,
			`SELECT id FROM survey_versions WHERE survey_id = $1 AND is_active = true`,
			surveyID).Scan(&activeID),
		"fetch active version id")
	assert.Equal(t, expectedID, activeID,
		"the most-recently-activated version (id=%s) must be the active one", expectedID)
}
