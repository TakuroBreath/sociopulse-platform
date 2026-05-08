package dsl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// TestStubEvaluator_ParseAndCheck_Accepts covers the inputs the Task 2
// stub deliberately accepts. The stub is deliberately permissive: it
// only catches the structural errors enumerated in evaluator.go's doc
// comment, so anything that's non-empty and balanced parens-wise is
// considered "syntactically OK" until the real DSL lands in Task 3.
func TestStubEvaluator_ParseAndCheck_Accepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
	}{
		{"plain identifier", "q1"},
		{"binary equality", `q1.value == "yes"`},
		{"balanced parens", "(q1.value == 1) && (q2.answered)"},
		{"true literal", "true"},
		{"nested parens", "((q1.value > 0) || (q2.value > 0))"},
	}
	ev := dsl.NewStubEvaluator()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, ev.ParseAndCheck(context.Background(), c.expr, nil))
		})
	}
}

// TestStubEvaluator_ParseAndCheck_Rejects covers the inputs the Task 2
// stub catches today. Each rejection MUST wrap [dsl.ErrDSLParse] so
// the caller can errors.Is its way to a stable error class.
func TestStubEvaluator_ParseAndCheck_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"tab and newlines", "\t \n"},
		{"unmatched closing paren", "1) + 2"},
		{"unmatched opening paren", "((1 + 2)"},
	}
	ev := dsl.NewStubEvaluator()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ev.ParseAndCheck(context.Background(), c.expr, nil)
			require.Error(t, err)
			require.ErrorIs(t, err, dsl.ErrDSLParse)
		})
	}
}

// TestStubEvaluator_RespectsContextCancellation verifies the
// fast-fail path: if the caller cancels before invoking ParseAndCheck,
// the stub returns the context's error wrapped with the dsl prefix
// rather than silently ignoring cancellation. This matches the
// project-wide contract (07-go-coding-standards § Context).
func TestStubEvaluator_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := dsl.NewStubEvaluator().ParseAndCheck(ctx, "q1.value == 1", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestStubEvaluator_IgnoresAllowedIdents documents the gap that the real
// DSL in Task 3 will close: the stub does not enforce allowedIdents at
// all, so passing nil or a wrong list never changes the verdict on
// well-formed expressions. The test pins this behaviour so a Task 3
// implementer noticing the regression knows it's intentional.
func TestStubEvaluator_IgnoresAllowedIdents(t *testing.T) {
	t.Parallel()
	ev := dsl.NewStubEvaluator()
	require.NoError(t, ev.ParseAndCheck(context.Background(), "ghost.value == 1", []string{"q1"}))
}

// TestErrDSLParse_StableSentinel guards the public sentinel against
// rename / re-creation drift. A consumer doing errors.Is on a wrapped
// instance MUST keep matching across stub-message changes.
func TestErrDSLParse_StableSentinel(t *testing.T) {
	t.Parallel()
	require.Error(t, dsl.ErrDSLParse)

	// Trigger a real wrapped error via the stub and confirm errors.Is
	// matches through the wrap. This is the contract callers depend
	// on (the schemavalidator graph layer does exactly this).
	wrapped := dsl.NewStubEvaluator().ParseAndCheck(t.Context(), "", nil)
	require.ErrorIs(t, wrapped, dsl.ErrDSLParse)
}
