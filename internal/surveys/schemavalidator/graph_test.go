package schemavalidator_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/dsl"
	sv "github.com/sociopulse/platform/internal/surveys/schemavalidator"
)

// TestGraphValidator_Cases is the table-driven companion to the
// fixture-driven suite. Each row builds a hand-rolled minimal schema
// that exercises ONE graph check at a time, and asserts that the
// expected issue code appears. The hand-rolled schemas keep the
// test focussed: a fixture file mixes "is a real survey" with "fails
// X check"; here we only care about the failing axis.
//
// Coverage:
//
//   - graph.no-start          (no node has kind=start)
//   - graph.multiple-starts   (two start nodes)
//   - graph.unreachable-node  (orphan in the graph)
//   - graph.dangling-edge     (edge.to references a missing node)
//   - graph.cycle-no-exit     (loop with no terminal reachable)
//   - graph.bad-when          (DSL stub rejects the expression)
//   - graph.forward-ref       (when references a non-dominator)
//   - graph.no-end-reachable  (start has a path but never to a terminal)
//   - graph.duplicate-node-id (two nodes share an id)
//
// The test deliberately does NOT exercise checkWhenExpressions's
// happy path under the stub — the stub is permissive enough that
// almost any non-empty expression is fine, so positive coverage lives
// in evaluator_test.go in the dsl package.
func TestGraphValidator_Cases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		schema   string
		wantCode string
	}{
		{
			name:     "no_start",
			wantCode: sv.CodeGraphNoStart,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "q1", "kind": "question", "question_type": "single",
                     "options": [{"id": "y", "label": "Y", "value": "y"}],
                     "next": [{"to": "e"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "multiple_starts",
			wantCode: sv.CodeGraphMultipleStarts,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "s1", "kind": "start", "next": [{"to": "e"}]},
                    {"id": "s2", "kind": "start", "next": [{"to": "e"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "unreachable_node",
			wantCode: sv.CodeGraphUnreachableNode,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start", "next": [{"to": "e"}]},
                    {"id": "orphan", "kind": "question", "question_type": "text",
                     "next": [{"to": "e"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "dangling_edge",
			wantCode: sv.CodeGraphDanglingEdge,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start", "next": [{"to": "ghost"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "cycle_no_exit",
			wantCode: sv.CodeGraphCycleNoExit,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start", "next": [{"to": "a"}]},
                    {"id": "a", "kind": "question", "question_type": "single",
                     "options": [{"id": "x", "label": "X", "value": "x"}],
                     "next": [{"to": "b"}]},
                    {"id": "b", "kind": "question", "question_type": "single",
                     "options": [{"id": "x", "label": "X", "value": "x"}],
                     "next": [{"to": "a"}]},
                    {"id": "unreachable_end", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "bad_when",
			wantCode: sv.CodeGraphBadWhen,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start", "next": [{"to": "e", "when": "((1+2)"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "forward_ref",
			wantCode: sv.CodeGraphForwardRef,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start",
                     "next": [{"to": "q1", "when": "q3.value == \"yes\""}]},
                    {"id": "q1", "kind": "question", "question_type": "single",
                     "options": [{"id": "y", "label": "Y", "value": "y"}],
                     "next": [{"to": "q3"}]},
                    {"id": "q3", "kind": "question", "question_type": "single",
                     "options": [{"id": "y", "label": "Y", "value": "y"}],
                     "next": [{"to": "e"}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "bad_node_ref",
			wantCode: sv.CodeGraphBadNodeRef,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start",
                     "next": [{"to": "e", "when": "phantom.value == \"x\""}]},
                    {"id": "e", "kind": "success-end"}
                ]
            }`,
		},
		{
			name:     "no_end_reachable",
			wantCode: sv.CodeGraphNoEndReachable,
			schema: `{
                "version": "1.0",
                "primary_mode": "flow",
                "nodes": [
                    {"id": "start", "kind": "start", "next": [{"to": "a"}]},
                    {"id": "a", "kind": "question", "question_type": "text",
                     "next": [{"to": "b"}]},
                    {"id": "b", "kind": "question", "question_type": "text",
                     "next": [{"to": "a"}]}
                ]
            }`,
		},
	}

	js, err := sv.NewJSONSchemaValidator()
	require.NoError(t, err)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Each row is required to pass the JSON-Schema pass.
			// Failing at this point means the row's hand-rolled JSON
			// is wrong, not the GraphValidator under test.
			jsIssues := js.Validate([]byte(c.schema))
			require.Empty(t, jsIssues, "JSON-Schema pass should accept fixture %q; got %+v", c.name, jsIssues)

			gv := sv.NewGraphValidator(dsl.NewStubEvaluator())
			issues := gv.Validate(context.Background(), []byte(c.schema))
			require.NotEmptyf(t, issues, "graph pass expected at least one issue for %q", c.name)

			seen := false
			for _, iss := range issues {
				if iss.Code == c.wantCode {
					seen = true
					break
				}
			}
			require.Truef(t, seen, "fixture %q expected issue with code %q; got %+v", c.name, c.wantCode, issues)
		})
	}
}

// TestGraphValidator_HappyPath checks the no-issue case: a minimal
// valid schema produces an empty slice. Sibling to the cases above.
func TestGraphValidator_HappyPath(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "e"}]},
            {"id": "e", "kind": "success-end"}
        ]
    }`)
	gv := sv.NewGraphValidator(dsl.NewStubEvaluator())
	require.Empty(t, gv.Validate(context.Background(), schema))
}

// TestGraphValidator_DuplicateNodeID exercises the duplicate-id
// detection — the JSON-Schema pass doesn't catch this so we need our
// own. The fixture has the same id appearing twice; the validator
// reports the second occurrence under graph.duplicate-node-id.
func TestGraphValidator_DuplicateNodeID(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "e"}]},
            {"id": "start", "kind": "intro", "next": [{"to": "e"}]},
            {"id": "e", "kind": "success-end"}
        ]
    }`)
	gv := sv.NewGraphValidator(dsl.NewStubEvaluator())
	issues := gv.Validate(context.Background(), schema)
	require.NotEmpty(t, issues)

	var report = sv.ValidationReport{Issues: issues}
	require.True(t, report.HasCode(sv.CodeGraphDuplicateNodeID),
		"expected duplicate-id issue; got %+v", issues)
}

// TestGraphValidator_SelfReferenceInWhen confirms an edge `when` that
// references its OWN node's value is NOT flagged as a forward
// reference. The runtime guarantees the answer is recorded before the
// `next` branches are evaluated.
func TestGraphValidator_SelfReferenceInWhen(t *testing.T) {
	t.Parallel()
	schema := []byte(`{
        "version": "1.0",
        "primary_mode": "flow",
        "nodes": [
            {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
            {"id": "q1", "kind": "question", "question_type": "single",
             "options": [{"id": "y", "label": "Y", "value": "y"}],
             "next": [{"to": "e", "when": "q1.value == \"y\""}]},
            {"id": "e", "kind": "success-end"}
        ]
    }`)
	gv := sv.NewGraphValidator(dsl.NewStubEvaluator())
	issues := gv.Validate(context.Background(), schema)
	report := sv.ValidationReport{Issues: issues}
	require.False(t, report.HasCode(sv.CodeGraphForwardRef),
		"self-reference must not be flagged as forward-ref; got %+v", issues)
}
