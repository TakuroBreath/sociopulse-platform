package schemavalidator_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/dsl"
	"github.com/sociopulse/platform/internal/surveys/schemas/testdata"
	sv "github.com/sociopulse/platform/internal/surveys/schemavalidator"
)

// fixtureMetadata mirrors the optional `metadata` block every
// JSON fixture carries. The validator deliberately ignores metadata at
// runtime (the survey-1.0.json schema marks it free-form), but tests
// drive their behavior off these annotations: which layer is expected
// to fail, and a human-readable summary of the expected diagnostic.
type fixtureMetadata struct {
	Name          string `json:"name"`
	ExpectedError string `json:"expected_error"`
	FailsAt       string `json:"fails_at"`
}

// fixturesNeedingRealDSL lists fixtures whose intended failure can
// only be detected once Task 3 ships the real DSL evaluator. The Task
// 2 stub is permissive on purpose (see [dsl.StubEvaluator] doc) so
// these stay skipped with an explicit pointer to the next task.
var fixturesNeedingRealDSL = map[string]struct{}{
	// `q1.value ===` slips past the stub: parens balance, expression
	// is non-empty. The real expr-lang parser will reject the trailing
	// `===` operator.
	"invalid-bad-when.json": {},
}

// loadFixture reads the embedded fixtures directly off testdata.Fixtures.
// Reading via embed avoids any reliance on the cwd at test time and
// matches the production code path (the validator never touches the OS
// filesystem either).
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := testdata.Fixtures.ReadFile(name)
	require.NoErrorf(t, err, "load fixture %s", name)
	return data
}

// loadFixtureMeta extracts the `metadata` object from one fixture.
// The metadata is what tells us whether we expect Valid=true, or
// Valid=false with a specific failure layer.
func loadFixtureMeta(t *testing.T, name string) fixtureMetadata {
	t.Helper()
	var wrap struct {
		Metadata fixtureMetadata `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(loadFixture(t, name), &wrap))
	return wrap.Metadata
}

// listFixtureNames returns every *.json file under
// schemas/testdata, sorted. Sorting matters because the test runner
// names subtests after fixtures; a stable order keeps CI logs
// diff-able.
func listFixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := testdata.Fixtures.ReadDir(".")
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

func newValidator(t *testing.T) *sv.SchemaValidator {
	t.Helper()
	v, err := sv.New(dsl.NewStubEvaluator())
	require.NoError(t, err)
	return v
}

// TestSchemaValidator_FixtureDriven walks every fixture in
// `schemas/testdata` and asserts behaviour from the metadata block:
//
//   - When `expected_error` is empty, the document is valid and the
//     validator MUST return Valid=true with no issues.
//   - When `fails_at` is "json-schema", at least one Issue MUST carry
//     a `json-schema.*` code (the structural pass caught it).
//   - When `fails_at` is "graph", at least one Issue MUST carry a
//     `graph.*` code (the semantic pass caught it).
//
// Two fixtures (invalid-bad-when, invalid-forward-ref) require the
// real DSL — bad-when slips past the stub's paren balancer because the
// expression is structurally fine; forward-ref happens to be caught by
// the regex extractor we install in graph.go, so it passes today.
// We tag bad-when explicitly with t.Skip pointing at Task 3.
func TestSchemaValidator_FixtureDriven(t *testing.T) {
	t.Parallel()
	v := newValidator(t)
	for _, name := range listFixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, skip := fixturesNeedingRealDSL[name]; skip {
				t.Skipf("DSL parser not yet wired (Task 3): fixture %s requires the real expr-lang parser to fail", name)
			}
			meta := loadFixtureMeta(t, name)
			report := v.Validate(context.Background(), loadFixture(t, name))

			if meta.ExpectedError == "" {
				// Valid fixture: must pass cleanly.
				require.Truef(t, report.Valid, "fixture %s expected Valid=true; got issues: %+v", name, report.Issues)
				require.Emptyf(t, report.Issues, "fixture %s expected no issues; got %+v", name, report.Issues)
				return
			}

			// Invalid fixture: expect Valid=false plus at least one
			// Issue whose code matches the layer recorded in
			// metadata.fails_at.
			require.Falsef(t, report.Valid, "fixture %s expected Valid=false (%s)", name, meta.ExpectedError)
			require.NotEmptyf(t, report.Issues, "fixture %s expected at least one issue", name)

			var wantPrefix string
			switch meta.FailsAt {
			case "json-schema":
				wantPrefix = sv.CodeJSONSchemaPrefix
			case "graph":
				wantPrefix = "graph."
			default:
				t.Fatalf("fixture %s has unknown metadata.fails_at: %q", name, meta.FailsAt)
			}
			require.Truef(t, report.HasCodePrefix(wantPrefix),
				"fixture %s expected at least one issue with code prefix %q; got %+v",
				name, wantPrefix, report.Issues)
		})
	}
}

// TestSchemaValidator_NotJSON guards the JSON-Schema pass against
// completely malformed input. The validator never crashes — the
// caller gets a single CodeJSONSchemaInvalid issue.
func TestSchemaValidator_NotJSON(t *testing.T) {
	t.Parallel()
	v := newValidator(t)
	rep := v.Validate(context.Background(), []byte("not json at all"))
	require.False(t, rep.Valid)
	require.NotEmpty(t, rep.Issues)
	require.True(t, rep.HasCode(sv.CodeJSONSchemaInvalid),
		"expected json-schema.invalid issue; got %+v", rep.Issues)
}

// TestSchemaValidator_EmptyDocument guards against the degenerate
// case where the caller passes `{}` — a syntactically valid JSON
// document missing every required field.
func TestSchemaValidator_EmptyDocument(t *testing.T) {
	t.Parallel()
	v := newValidator(t)
	rep := v.Validate(context.Background(), []byte(`{}`))
	require.False(t, rep.Valid)
	require.NotEmpty(t, rep.Issues)
	// Every issue should belong to the JSON-Schema pass (we don't run
	// the graph pass when the structural check fails).
	for _, iss := range rep.Issues {
		require.Truef(t, strings.HasPrefix(iss.Code, sv.CodeJSONSchemaPrefix) || iss.Code == sv.CodeJSONSchemaInvalid,
			"unexpected non-json-schema code in empty-document failure: %s", iss.Code)
	}
}

// TestValidationReport_ErrorInterface pins the report's
// implementation of the error interface so callers can return it
// through an `error` slot from SaveVersion.
func TestValidationReport_ErrorInterface(t *testing.T) {
	t.Parallel()
	valid := sv.ValidationReport{Valid: true}
	require.Empty(t, valid.Error())

	invalid := sv.ValidationReport{
		Valid: false,
		Issues: []sv.Issue{
			{Code: sv.CodeGraphNoStart, Path: "/nodes", Message: "no start"},
			{Code: sv.CodeGraphNoStart, Path: "/nodes", Message: "no start"},
			{Code: sv.CodeGraphUnreachableNode, Path: "/nodes/x", Message: "unreachable"},
		},
	}
	msg := invalid.Error()
	require.Contains(t, msg, "schemavalidator: 3 issue(s)")
	// Codes appear once each, deduplicated.
	require.Contains(t, msg, sv.CodeGraphNoStart)
	require.Contains(t, msg, sv.CodeGraphUnreachableNode)
	// And it satisfies the error interface so errors.Is/As works on
	// callers using a returned `error`.
	var asErr error = invalid
	require.Error(t, asErr)
	// Ensure we didn't accidentally make Error() satisfy a stranger
	// sentinel via implicit comparison rules.
	stranger := errors.New("anything")
	require.NotErrorIs(t, asErr, stranger)
}

// TestNewSchemaValidator_PanicsOnNil mirrors the project-wide
// composition-root contract: misconfigured constructors panic at
// startup so the bug surfaces in CI rather than per request.
func TestNewSchemaValidator_PanicsOnNil(t *testing.T) {
	t.Parallel()
	js, err := sv.NewJSONSchemaValidator()
	require.NoError(t, err)
	gv := sv.NewGraphValidator(dsl.NewStubEvaluator())

	require.Panics(t, func() { sv.NewSchemaValidator(nil, gv) })
	require.Panics(t, func() { sv.NewSchemaValidator(js, nil) })
	require.NotPanics(t, func() { _ = sv.NewSchemaValidator(js, gv) })
}

// TestNewGraphValidator_PanicsOnNil locks the same contract on the
// graph-only constructor.
func TestNewGraphValidator_PanicsOnNil(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { sv.NewGraphValidator(nil) })
	require.NotPanics(t, func() { _ = sv.NewGraphValidator(dsl.NewStubEvaluator()) })
}

// TestNew_DefaultsToStubEvaluator confirms that omitting the
// evaluator wires the Task 2 stub instead of panicking. This keeps
// tests and the composition root concise — they don't need to thread
// a stub through unless they want a custom evaluator.
func TestNew_DefaultsToStubEvaluator(t *testing.T) {
	t.Parallel()
	v, err := sv.New(nil)
	require.NoError(t, err)
	rep := v.Validate(context.Background(), loadFixture(t, "valid-minimal-flat.json"))
	require.True(t, rep.Valid)
}
