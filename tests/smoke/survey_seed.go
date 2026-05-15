//go:build smoke

package smoke

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// minimalValidSurveySchemaJSON is the canonical "smallest survey that
// passes the validator" fixture. Mirrors
// internal/surveys/schemas/testdata/valid-minimal-flat.json — the
// in-tree fixture used by TestSchemaValidator_FixtureDriven — so this
// helper stays in lockstep with the validator's accepted shape. A
// future schema-version bump that breaks valid-minimal-flat.json
// equally fails this helper, surfacing the contract drift in scenario
// 4 instead of an obscure JSON-Schema error in production.
//
// Shape:
//   - 3 nodes: start → q1 (single-choice with 2 options) → end_ok.
//   - primary_mode: "flow" (the canonical version-1.0 flow surface).
//   - All required fields present; no DSL `when` so the graph pass
//     does not need a real DSL evaluator (the stub suffices).
//
// The trailing newline matters less for JSON consumers but is
// preserved so the bytes match a hand-written file format.
const minimalValidSurveySchemaJSON = `{
  "version": "1.0",
  "primary_mode": "flow",
  "metadata": {
    "name": "smoke-minimal",
    "purpose": "smoke harness fixture for scenario 4"
  },
  "nodes": [
    {
      "id": "start",
      "kind": "start",
      "next": [{ "to": "q1" }]
    },
    {
      "id": "q1",
      "kind": "question",
      "title": "Do you support the new policy?",
      "question_type": "single",
      "required": true,
      "options": [
        { "id": "yes", "label": "Yes", "value": "yes" },
        { "id": "no", "label": "No", "value": "no" }
      ],
      "next": [{ "to": "end_ok" }]
    },
    {
      "id": "end_ok",
      "kind": "success-end",
      "title": "Thank you!"
    }
  ]
}
`

// MinimalValidSurveySchema returns the canonical smoke fixture that
// passes the SchemaValidator (JSON-Schema + graph). Scenario 4 POSTs
// these bytes to /api/surveys/:id/versions to produce a valid version
// row. The bytes are returned as a fresh slice on each call —
// callers may safely mutate them (e.g. to inject a deliberate failure
// for negative scenarios) without affecting subsequent calls.
func MinimalValidSurveySchema() []byte {
	out := make([]byte, len(minimalValidSurveySchemaJSON))
	copy(out, minimalValidSurveySchemaJSON)
	return out
}

// SeedSurvey inserts ONE surveys row directly via pgx and returns the
// new survey's id. Designed for scenario 4: the smoke test then drives
// the version-flow over HTTP via /api/surveys/:id/versions —
// SeedSurvey only sets up the parent surveys row so the version path
// has a target.
//
// Columns supplied (mirrors migrations/000001_init.up.sql:163):
//   - id                : generated client-side for deterministic return
//   - tenant_id         : FK key for RLS
//   - name              : NOT NULL
//   - primary_mode      : 'flow' (matches the smoke fixture's primary_mode)
//
// current_version_id stays NULL — it gets populated by
// SaveVersion+Activate, which is exactly what the scenario tests.
//
// The seed bypasses RLS via the testcontainer's superuser connection
// (POSTGRES_USER is granted tenancy_admin via 000001_init grants).
//
// Cleanup deletes the surveys row at test end. survey_versions rows
// the scenario inserts cascade-delete via the FK.
//
// Note: code is NOT a column on surveys (verified — the table only has
// tenant_id / name / current_version_id / primary_mode). The code
// parameter accepted here matches the helper signature documented in
// Plan 21b but is currently unused; we keep it in the API to mirror
// SeedProject's shape and to allow a future "code" column without a
// signature change. We use it as part of the survey name so the row is
// distinguishable in pg_stat queries.
func SeedSurvey(t *testing.T, stack *Stack, tenantID uuid.UUID, code, name string) uuid.UUID {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	surveyID := uuid.New()
	displayName := name
	if displayName == "" {
		displayName = "Smoke Survey " + code
	}
	_, err = conn.Exec(ctx,
		`INSERT INTO surveys (id, tenant_id, name, primary_mode)
		 VALUES ($1, $2, $3, 'flow')`,
		surveyID, tenantID, displayName)
	require.NoError(t, err, "smoke seed: insert survey %s under tenant %s", code, tenantID)

	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM surveys WHERE id = $1`, surveyID)
	})

	return surveyID
}
