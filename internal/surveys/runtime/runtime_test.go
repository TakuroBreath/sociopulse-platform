package runtime_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
	"github.com/sociopulse/platform/internal/surveys/runtime"
	"github.com/sociopulse/platform/internal/surveys/schemas/testdata"
)

// loadFixture loads one of the named JSON fixtures from the shared
// testdata embed.FS. Centralised here so each test reads as
// `loadFixture(t, "valid-with-conditions")` rather than repeating the
// path/Read/require dance.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := testdata.Fixtures.ReadFile(name + ".json")
	require.NoErrorf(t, err, "fixture %q must exist under internal/surveys/schemas/testdata/", name)
	return data
}

// newRuntime builds a Runtime with the production DSL evaluator and a
// modest cache cap. Each test gets its own instance so cache state
// doesn't leak across tests.
func newRuntime() *runtime.Runtime {
	return runtime.New(dsl.NewRealEvaluator(64), 16)
}

// TestNextNode_MinimalFlat exercises the linear happy path: start
// → q1 → end_ok.
func TestNextNode_MinimalFlat(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")

	// start → q1 (no answers needed, unconditional edge)
	got, err := r.NextNode(schema, "start", nil)
	require.NoError(t, err)
	require.Equal(t, "q1", got.NextNodeID)
	require.False(t, got.Terminated)
	require.Equal(t, api.EndKindNone, got.EndKind)

	// q1 → end_ok (unconditional edge, but we still record the answer
	// so the progress numerator advances).
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes"},
	}
	got, err = r.NextNode(schema, "q1", answers)
	require.NoError(t, err)
	require.Equal(t, "end_ok", got.NextNodeID)
	require.True(t, got.Terminated)
	require.Equal(t, api.EndKindSuccess, got.EndKind)
	require.InDelta(t, 1.0, got.Progress, 1e-9)
}

// TestNextNode_WithConditions covers the branching fixture where
// q1.value == "yes" → q2 and the default fallback → end_ok. Both
// branches MUST be reachable from the same schema.
func TestNextNode_WithConditions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		answer      string
		wantNextID  string
		wantTermEnd api.EndKind
	}{
		{"yes branch goes to q2", "yes", "q2", api.EndKindNone},
		{"no branch falls through to end_ok", "no", "end_ok", api.EndKindSuccess},
	}
	r := newRuntime()
	schema := loadFixture(t, "valid-with-conditions")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ans := map[string]api.Answer{
				"q1": {NodeID: "q1", SingleChoice: c.answer},
			}
			got, err := r.NextNode(schema, "q1", ans)
			require.NoError(t, err)
			require.Equal(t, c.wantNextID, got.NextNodeID)
			require.Equal(t, c.wantTermEnd, got.EndKind)
		})
	}
}

// TestNextNode_VCIOMElectoral runs the full ВЦИОМ-style flow as a
// smoke test: start → intro → q_voted → (q_party | end_refuse |
// end_ok) depending on the answer. Mirrors the canonical fixture used
// by the schemavalidator suite.
func TestNextNode_VCIOMElectoral(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-vciom-electoral")

	// start → intro
	got, err := r.NextNode(schema, "start", nil)
	require.NoError(t, err)
	require.Equal(t, "intro", got.NextNodeID)

	// intro → q_voted (when=true literal)
	got, err = r.NextNode(schema, "intro", nil)
	require.NoError(t, err)
	require.Equal(t, "q_voted", got.NextNodeID)

	// q_voted=yes → q_party
	ans := map[string]api.Answer{
		"q_voted": {NodeID: "q_voted", SingleChoice: "yes"},
	}
	got, err = r.NextNode(schema, "q_voted", ans)
	require.NoError(t, err)
	require.Equal(t, "q_party", got.NextNodeID)

	// q_voted=refuse → end_refuse
	ans["q_voted"] = api.Answer{NodeID: "q_voted", SingleChoice: "refuse"}
	got, err = r.NextNode(schema, "q_voted", ans)
	require.NoError(t, err)
	require.Equal(t, "end_refuse", got.NextNodeID)
	require.True(t, got.Terminated)
	require.Equal(t, api.EndKindRefusal, got.EndKind)

	// q_voted=no → fall through to end_ok
	ans["q_voted"] = api.Answer{NodeID: "q_voted", SingleChoice: "no"}
	got, err = r.NextNode(schema, "q_voted", ans)
	require.NoError(t, err)
	require.Equal(t, "end_ok", got.NextNodeID)
	require.Equal(t, api.EndKindSuccess, got.EndKind)
}

// TestNextNode_NoMatchingEdge fires when none of the outgoing edges
// match. We synthesise a schema with a single conditional edge whose
// `when` is false; the runtime MUST surface ErrNoMatchingEdge.
func TestNextNode_NoMatchingEdge(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {
                "id": "q1",
                "kind": "question",
                "question_type": "single",
                "required": true,
                "options": [{"id": "yes", "label": "Yes"}],
                "next": [{"to": "end_ok", "when": "q1.value == \"never_matches\""}]
            },
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	ans := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes"},
	}
	_, err := r.NextNode(schema, "q1", ans)
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrNoMatchingEdge)
}

// TestNextNode_MalformedSchema verifies that an unparseable schema
// surfaces ErrSchema (and not a panic / lower-level json error). The
// HTTP layer relies on errors.Is(err, api.ErrSchema) to render a 422.
func TestNextNode_MalformedSchema(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	cases := []struct {
		name   string
		schema []byte
	}{
		{"empty bytes", nil},
		{"truncated json", []byte("{")},
		{"empty nodes array", []byte(`{"version":"1.0","primary_mode":"flow","nodes":[]}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.NextNode(c.schema, "start", nil)
			require.Error(t, err)
			require.ErrorIs(t, err, api.ErrSchema)
		})
	}
}

// TestNextNode_UnknownCurrentNode surfaces ErrNodeNotFound when the
// caller supplies an id that doesn't exist in the schema. Matches the
// pre-condition for /api/surveys/{id}/preview/run guarding against
// stale UI state.
func TestNextNode_UnknownCurrentNode(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")
	_, err := r.NextNode(schema, "does-not-exist", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrNodeNotFound)
}

// TestNextNode_AtTerminalReturnsTerminated covers the post-end UI
// re-render path: NextNode called on a terminal MUST short-circuit
// rather than scanning outgoing edges (terminals have none).
func TestNextNode_AtTerminalReturnsTerminated(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")
	got, err := r.NextNode(schema, "end_ok", nil)
	require.NoError(t, err)
	require.Empty(t, got.NextNodeID)
	require.True(t, got.Terminated)
	require.Equal(t, api.EndKindSuccess, got.EndKind)
	require.InDelta(t, 1.0, got.Progress, 1e-9)
}

// TestNextNode_DanglingEdgeSurfacesAsErrSchema verifies the runtime's
// defence against malformed schemas slipping past SaveVersion. A
// dangling edge target → ErrSchema, not a silent jump-to-nowhere.
func TestNextNode_DanglingEdgeSurfacesAsErrSchema(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "ghost"}]},
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	_, err := r.NextNode(schema, "start", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrSchema)
}

// TestValidateAnswer covers each QuestionType, valid + invalid cases.
// Table-driven so adding a new rule is one row, not a new test func.
func TestValidateAnswer(t *testing.T) {
	t.Parallel()

	// We synthesise schemas inline so the assertions stay compact and
	// self-contained. Each fixture's nodeID is the answer node we
	// validate.
	cases := []struct {
		name      string
		schema    []byte
		nodeID    string
		ans       api.Answer
		wantErrIs error // nil means "must succeed"
	}{
		{
			name:   "single accepts a known option id",
			schema: schemaWithSingleQuestion(true),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", SingleChoice: "yes"},
		},
		{
			name:      "single rejects an unknown option id",
			schema:    schemaWithSingleQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", SingleChoice: "ghost"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:      "single rejects empty when required",
			schema:    schemaWithSingleQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:   "single accepts empty when optional",
			schema: schemaWithSingleQuestion(false),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1"},
		},
		{
			name:   "select uses single rules",
			schema: schemaWithSelectQuestion(true),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", SingleChoice: "er"},
		},
		{
			name:      "select rejects unknown id",
			schema:    schemaWithSelectQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", SingleChoice: "ghost"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:   "multi accepts a subset of known ids",
			schema: schemaWithMultiQuestion(true),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", MultiChoice: []string{"a", "b"}},
		},
		{
			name:      "multi rejects an unknown id",
			schema:    schemaWithMultiQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", MultiChoice: []string{"a", "ghost"}},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:      "multi rejects duplicates",
			schema:    schemaWithMultiQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", MultiChoice: []string{"a", "a"}},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:      "multi rejects empty when required",
			schema:    schemaWithMultiQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:   "multi accepts empty when optional",
			schema: schemaWithMultiQuestion(false),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1"},
		},
		{
			name:   "number accepts a value within bounds",
			schema: schemaWithNumberQuestion(true, floatp(0), floatp(100)),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", Number: floatp(42)},
		},
		{
			name:      "number rejects below min",
			schema:    schemaWithNumberQuestion(true, floatp(0), floatp(100)),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", Number: floatp(-1)},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:      "number rejects above max",
			schema:    schemaWithNumberQuestion(true, floatp(0), floatp(100)),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1", Number: floatp(101)},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:      "number rejects nil when required",
			schema:    schemaWithNumberQuestion(true, floatp(0), floatp(100)),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:   "number accepts nil when optional",
			schema: schemaWithNumberQuestion(false, floatp(0), floatp(100)),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1"},
		},
		{
			name:   "number with no bounds accepts any value",
			schema: schemaWithNumberQuestion(true, nil, nil),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", Number: floatp(-9999)},
		},
		{
			name:   "number with only min rejects below but accepts large",
			schema: schemaWithNumberQuestion(true, floatp(10), nil),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", Number: floatp(99999)},
		},
		{
			name:   "text accepts non-empty when required",
			schema: schemaWithTextQuestion(true),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1", Text: "hello"},
		},
		{
			name:      "text rejects empty when required",
			schema:    schemaWithTextQuestion(true),
			nodeID:    "q1",
			ans:       api.Answer{NodeID: "q1"},
			wantErrIs: api.ErrBadAnswer,
		},
		{
			name:   "text accepts empty when optional",
			schema: schemaWithTextQuestion(false),
			nodeID: "q1",
			ans:    api.Answer{NodeID: "q1"},
		},
		{
			name:   "non-question node accepts any answer",
			schema: loadMinimalFlat(t),
			nodeID: "start",
			ans:    api.Answer{NodeID: "start"},
		},
	}
	r := newRuntime()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := r.ValidateAnswer(c.schema, c.nodeID, c.ans)
			if c.wantErrIs == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, c.wantErrIs)
		})
	}
}

// TestValidateAnswer_MultiRejectsEmptyID covers the empty-id branch
// in validateMultiAnswer: the runtime MUST reject a multi answer
// containing the empty string even when the rest of the payload is
// valid (defense against UI bugs that send "" alongside real ids).
func TestValidateAnswer_MultiRejectsEmptyID(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := schemaWithMultiQuestion(true)
	err := r.ValidateAnswer(schema, "q1", api.Answer{
		NodeID:      "q1",
		MultiChoice: []string{"a", ""},
	})
	require.ErrorIs(t, err, api.ErrBadAnswer)
}

// TestValidateAnswer_UnknownQuestionTypeSurfacesErrSchema covers the
// "unknown question_type" path of validateQuestionAnswer. The runtime
// surfaces this as ErrSchema (not ErrBadAnswer) because the cause is
// a malformed schema — a well-formed survey would have rejected the
// unknown enum at SaveVersion time.
func TestValidateAnswer_UnknownQuestionTypeSurfacesErrSchema(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {
                "id": "q1",
                "kind": "question",
                "question_type": "rorschach",
                "next": [{"to": "end_ok"}]
            },
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	err := r.ValidateAnswer(schema, "q1", api.Answer{NodeID: "q1", Text: "foo"})
	require.ErrorIs(t, err, api.ErrSchema)
}

// TestValidateAnswer_QuestionNodeMissingTypeSurfacesErrSchema covers
// the "question_type empty" branch of validateQuestionAnswer.
func TestValidateAnswer_QuestionNodeMissingTypeSurfacesErrSchema(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {"id": "q1", "kind": "question", "next": [{"to": "end_ok"}]},
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	err := r.ValidateAnswer(schema, "q1", api.Answer{NodeID: "q1"})
	require.ErrorIs(t, err, api.ErrSchema)
}

// TestNextNode_NonBoolWhenSurfacesErrDSLEval pins the cast-to-bool
// guard in edgeMatches: a `when` expression that evaluates to a
// string / number MUST surface ErrDSLEval rather than silently
// treating it as truthy.
func TestNextNode_NonBoolWhenSurfacesErrDSLEval(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	// `when` returns the answer string itself, not a bool.
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {
                "id": "q1",
                "kind": "question",
                "question_type": "single",
                "required": true,
                "options": [{"id": "yes", "label": "Yes"}],
                "next": [{"to": "end_ok", "when": "q1.value"}]
            },
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	ans := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes"},
	}
	_, err := r.NextNode(schema, "q1", ans)
	require.ErrorIs(t, err, dsl.ErrDSLEval)
}

// TestNextNode_DSLParseErrorSurfacesErrDSLParse pins the parse-error
// pass-through: a syntactically broken `when` MUST surface
// dsl.ErrDSLParse.
func TestNextNode_DSLParseErrorSurfacesErrDSLParse(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {
                "id": "q1",
                "kind": "question",
                "question_type": "single",
                "required": true,
                "options": [{"id": "yes", "label": "Yes"}],
                "next": [{"to": "end_ok", "when": "q1.value =="}]
            },
            {"id": "end_ok", "kind": "success-end"}
        ]
    }`)
	ans := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes"},
	}
	_, err := r.NextNode(schema, "q1", ans)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestValidateAnswer_UnknownNodeID surfaces ErrNodeNotFound when the
// nodeID isn't in the schema.
func TestValidateAnswer_UnknownNodeID(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")
	err := r.ValidateAnswer(schema, "nope", api.Answer{NodeID: "nope"})
	require.ErrorIs(t, err, api.ErrNodeNotFound)
}

// TestValidateAnswer_MultiFromMultiFixture exercises ValidateAnswer
// against the canonical multi-select fixture (so the inline test
// fixtures don't mask a regression in the real schema bytes).
func TestValidateAnswer_MultiFromMultiFixture(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-with-multi")

	// Valid: subset of three known ids.
	err := r.ValidateAnswer(schema, "q_topics", api.Answer{
		NodeID:      "q_topics",
		MultiChoice: []string{"economy", "healthcare"},
	})
	require.NoError(t, err)

	// Invalid: empty when required.
	err = r.ValidateAnswer(schema, "q_topics", api.Answer{NodeID: "q_topics"})
	require.ErrorIs(t, err, api.ErrBadAnswer)

	// Invalid: unknown id.
	err = r.ValidateAnswer(schema, "q_topics", api.Answer{
		NodeID:      "q_topics",
		MultiChoice: []string{"economy", "ghost"},
	})
	require.ErrorIs(t, err, api.ErrBadAnswer)
}

// TestCalculateProgress walks several fixed positions and asserts the
// computed progress matches expectations. The minimal-flat fixture
// has exactly one question node, so:
//   - start    → 0 (numerator pos = 0, denom = 1)
//   - q1       → 0 (numerator pos = 0; q1 is current, not before)
//   - end_ok   → 1.0 (terminal short-circuit)
func TestCalculateProgress(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")

	got, err := r.CalculateProgress(schema, "start")
	require.NoError(t, err)
	require.InDelta(t, 0.0, got, 1e-9)

	got, err = r.CalculateProgress(schema, "q1")
	require.NoError(t, err)
	require.InDelta(t, 0.0, got, 1e-9)

	got, err = r.CalculateProgress(schema, "end_ok")
	require.NoError(t, err)
	require.InDelta(t, 1.0, got, 1e-9)
}

// TestCalculateProgress_VCIOMIntermediate covers the multi-question
// fixture (q_voted + q_party = 2 questions). Progress at q_party is
// 0.5 because exactly one question precedes it in document order.
func TestCalculateProgress_VCIOMIntermediate(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-vciom-electoral")

	got, err := r.CalculateProgress(schema, "q_party")
	require.NoError(t, err)
	require.InDelta(t, 0.5, got, 1e-9)
}

// TestCalculateProgress_UnknownNode surfaces ErrNodeNotFound.
func TestCalculateProgress_UnknownNode(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-minimal-flat")
	_, err := r.CalculateProgress(schema, "nope")
	require.ErrorIs(t, err, api.ErrNodeNotFound)
}

// TestCalculateProgress_MalformedSchema surfaces ErrSchema.
func TestCalculateProgress_MalformedSchema(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	_, err := r.CalculateProgress([]byte("{"), "start")
	require.ErrorIs(t, err, api.ErrSchema)
}

// TestNew_PanicsOnNilEvaluator pins the panic-on-nil contract called
// out in plan-05/06's "panic on nil deps" lesson.
func TestNew_PanicsOnNilEvaluator(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(
		t,
		"runtime: evaluator required (use dsl.NewRealEvaluator in prod, dsl.NewStubEvaluator in tests that don't exercise Eval)",
		func() { runtime.New(nil, 16) },
	)
}

// TestNextNode_Concurrent fires 50 goroutines through NextNode against
// the same schema bytes. Survival under -race verifies the cache and
// evaluator are correctly mutex-guarded.
func TestNextNode_Concurrent(t *testing.T) {
	t.Parallel()
	r := newRuntime()
	schema := loadFixture(t, "valid-with-conditions")
	ans := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes"},
	}
	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				got, err := r.NextNode(schema, "q1", ans)
				if err != nil {
					errCh <- err
					return
				}
				if got.NextNodeID != "q2" {
					errCh <- errors.New("unexpected next node: " + got.NextNodeID)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
}

// TestSchemaCache_HitMiss exercises the hit/miss path: the first call
// is a miss (cache size grows to 1), and subsequent calls with the
// same schema bytes don't grow the cache further.
func TestSchemaCache_HitMiss(t *testing.T) {
	t.Parallel()
	// Use a hand-rolled evaluator with no compile cache so we can
	// observe the runtime cache in isolation.
	r := runtime.New(dsl.NewRealEvaluator(8), 4)
	schema := loadFixture(t, "valid-minimal-flat")

	// First call: parse + insert.
	_, err := r.NextNode(schema, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 1, r.CacheLen(), "first NextNode should populate the cache")

	// Second call with the SAME bytes: hit, no growth.
	_, err = r.NextNode(schema, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 1, r.CacheLen(), "second NextNode with identical schema should hit the cache")

	// Distinct fixture: miss, growth to 2.
	other := loadFixture(t, "valid-with-conditions")
	_, err = r.NextNode(other, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 2, r.CacheLen(), "different schema should produce a second cache entry")
}

// TestSchemaCache_Disabled covers the negative-cap path: passing a
// negative size disables caching, so r.CacheLen always reports 0.
func TestSchemaCache_Disabled(t *testing.T) {
	t.Parallel()
	r := runtime.New(dsl.NewRealEvaluator(0), -1)
	schema := loadFixture(t, "valid-minimal-flat")
	_, err := r.NextNode(schema, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 0, r.CacheLen())
}

// TestSchemaCache_Eviction exercises the LRU-eviction path by
// configuring a tiny cache (cap=1) and pushing two distinct schemas
// through it. The cache MUST evict the first entry on the second
// insert and end up with len=1.
func TestSchemaCache_Eviction(t *testing.T) {
	t.Parallel()
	r := runtime.New(dsl.NewRealEvaluator(0), 1)
	flat := loadFixture(t, "valid-minimal-flat")
	cond := loadFixture(t, "valid-with-conditions")

	_, err := r.NextNode(flat, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 1, r.CacheLen())

	_, err = r.NextNode(cond, "start", nil)
	require.NoError(t, err)
	require.Equal(t, 1, r.CacheLen(), "cap=1 cache should evict the prior entry")
}

// schemaWithSingleQuestion synthesises a one-question survey of type
// `single` with two options (yes/no) and a configurable `required`
// flag. Used by TestValidateAnswer to keep the table compact.
func schemaWithSingleQuestion(required bool) []byte {
	return mustMarshal(map[string]any{
		"version":      "1.0",
		"primary_mode": "flow",
		"nodes": []any{
			map[string]any{"id": "start", "kind": "start", "next": []any{map[string]any{"to": "q1"}}},
			map[string]any{
				"id":            "q1",
				"kind":          "question",
				"question_type": "single",
				"required":      required,
				"options": []any{
					map[string]any{"id": "yes", "label": "Yes"},
					map[string]any{"id": "no", "label": "No"},
				},
				"next": []any{map[string]any{"to": "end_ok"}},
			},
			map[string]any{"id": "end_ok", "kind": "success-end"},
		},
	})
}

// schemaWithSelectQuestion is the same shape as schemaWithSingleQuestion
// but uses question_type=select and a different option set so we can
// pin the "select uses single rules" branch.
func schemaWithSelectQuestion(required bool) []byte {
	return mustMarshal(map[string]any{
		"version":      "1.0",
		"primary_mode": "flow",
		"nodes": []any{
			map[string]any{"id": "start", "kind": "start", "next": []any{map[string]any{"to": "q1"}}},
			map[string]any{
				"id":            "q1",
				"kind":          "question",
				"question_type": "select",
				"required":      required,
				"options": []any{
					map[string]any{"id": "er", "label": "ER"},
					map[string]any{"id": "kprf", "label": "KPRF"},
				},
				"next": []any{map[string]any{"to": "end_ok"}},
			},
			map[string]any{"id": "end_ok", "kind": "success-end"},
		},
	})
}

// schemaWithMultiQuestion produces a multi-select question with three
// options a/b/c.
func schemaWithMultiQuestion(required bool) []byte {
	return mustMarshal(map[string]any{
		"version":      "1.0",
		"primary_mode": "form",
		"nodes": []any{
			map[string]any{"id": "start", "kind": "start", "next": []any{map[string]any{"to": "q1"}}},
			map[string]any{
				"id":            "q1",
				"kind":          "question",
				"question_type": "multi",
				"required":      required,
				"options": []any{
					map[string]any{"id": "a", "label": "A"},
					map[string]any{"id": "b", "label": "B"},
					map[string]any{"id": "c", "label": "C"},
				},
				"next": []any{map[string]any{"to": "end_ok"}},
			},
			map[string]any{"id": "end_ok", "kind": "success-end"},
		},
	})
}

// schemaWithNumberQuestion produces a numeric question with optional
// min/max bounds. Pass nil for "absent" so the helper omits the
// bound from the document — exercising the runtime's "no bound"
// path through the shared validator.
func schemaWithNumberQuestion(required bool, min, max *float64) []byte {
	q := map[string]any{
		"id":            "q1",
		"kind":          "question",
		"question_type": "number",
		"required":      required,
		"next":          []any{map[string]any{"to": "end_ok"}},
	}
	if min != nil {
		q["min"] = *min
	}
	if max != nil {
		q["max"] = *max
	}
	return mustMarshal(map[string]any{
		"version":      "1.0",
		"primary_mode": "form",
		"nodes": []any{
			map[string]any{"id": "start", "kind": "start", "next": []any{map[string]any{"to": "q1"}}},
			q,
			map[string]any{"id": "end_ok", "kind": "success-end"},
		},
	})
}

// schemaWithTextQuestion produces a free-form text question.
func schemaWithTextQuestion(required bool) []byte {
	return mustMarshal(map[string]any{
		"version":      "1.0",
		"primary_mode": "form",
		"nodes": []any{
			map[string]any{"id": "start", "kind": "start", "next": []any{map[string]any{"to": "q1"}}},
			map[string]any{
				"id":            "q1",
				"kind":          "question",
				"question_type": "text",
				"required":      required,
				"next":          []any{map[string]any{"to": "end_ok"}},
			},
			map[string]any{"id": "end_ok", "kind": "success-end"},
		},
	})
}

// loadMinimalFlat is a thin wrapper over loadFixture so a table case
// can call it without taking a *testing.T inside its struct literal.
func loadMinimalFlat(t *testing.T) []byte {
	t.Helper()
	return loadFixture(t, "valid-minimal-flat")
}

// mustMarshal panics on json.Marshal failure. Used inside helper
// builders where the failure mode is "the test fixture is malformed",
// which we want to surface immediately rather than silently skip.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("test: json.Marshal failed: " + err.Error())
	}
	return b
}

// floatp returns a pointer to v. Local helper to keep table-driven
// tests readable (`floatp(42)` vs `func() *float64 { v := 42.0; return &v }()`).
func floatp(v float64) *float64 { return &v }
