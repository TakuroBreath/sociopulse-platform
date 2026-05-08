package dsl_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// TestRealEvaluator_ParseAndCheck_Accepts covers the canonical
// whitelist forms the validator (and runtime) MUST accept:
//
//   - bare `answer` identifier (legacy convenience).
//   - `q<id>.value` and `q<id>.answered` member access.
//   - boolean & string operators allowed in expr-lang's standard
//     operator set (no extra functions exposed).
func TestRealEvaluator_ParseAndCheck_Accepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
	}{
		{"answered flag", "q1.answered"},
		{"numeric compare", "q1.value > 10"},
		{"string equality", `q1.value == "yes"`},
		{"two values and", "q1.answered && q2.answered"},
		{"in operator", `q1.value in ["a", "b"]`},
		{"negation", "!q1.answered"},
		{"answer identifier", "answer == 1"},
		{"answer member", `answer.value == "x"`},
		{"parenthesised compound", "(q1.answered) && (q2.value > 0)"},
	}
	ev := dsl.NewRealEvaluator(0)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ev.ParseAndCheck(context.Background(), c.expr, nil)
			require.NoErrorf(t, err, "expression %q should be accepted", c.expr)
		})
	}
}

// TestRealEvaluator_ParseAndCheck_Rejects covers the two classes of
// rejection: structural failure (parse error) and identifier-outside-
// whitelist failure (e.g. `os`, `time`, `len`, builtins). Both classes
// MUST wrap [dsl.ErrDSLParse] so callers can errors.Is them.
func TestRealEvaluator_ParseAndCheck_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"forbidden os.Getenv", `os.Getenv("X")`},
		{"forbidden now()", `now()`},
		{"forbidden time.Now", `time.Now()`},
		{"forbidden len builtin", "len(arr) > 0"},
		{"forbidden print", `print("x")`},
		{"unknown bare ident", "foo"},
		{"unknown bare ident dotted", "ghost.value > 1"},
		{"unmatched paren", "((1 + 2)"},
		{"trailing operator", "q1.value =="},
		{"forbidden member", "q1.value.unsafe"},
	}
	ev := dsl.NewRealEvaluator(0)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ev.ParseAndCheck(context.Background(), c.expr, nil)
			require.Errorf(t, err, "expression %q should be rejected", c.expr)
			require.ErrorIsf(t, err, dsl.ErrDSLParse,
				"rejection of %q should wrap ErrDSLParse, got %v", c.expr, err)
		})
	}
}

// TestRealEvaluator_ParseAndCheck_AllowedIdents_NarrowsWhitelist
// verifies that supplying a non-nil allowedIdents list pins q<id>
// references to that exact set. With nil, any id matching `q<X>` is
// accepted; with []string{"q1"}, q2 must be rejected.
//
// This is the path the schemavalidator uses to pre-restrict
// identifiers to "those reachable from the current node" (the
// dominator pass owns the listing; the evaluator just enforces it).
func TestRealEvaluator_ParseAndCheck_AllowedIdents_NarrowsWhitelist(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	require.NoError(t, ev.ParseAndCheck(context.Background(), "q1.answered", []string{"q1"}))

	err := ev.ParseAndCheck(context.Background(), "q2.answered", []string{"q1"})
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestRealEvaluator_ParseAndCheck_AllowedIdents_NilDefaults verifies
// that nil allowedIdents accepts the q-prefixed grammar generally.
func TestRealEvaluator_ParseAndCheck_AllowedIdents_NilDefaults(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	require.NoError(t, ev.ParseAndCheck(context.Background(), "q1.answered", nil))
	require.NoError(t, ev.ParseAndCheck(context.Background(), "q99.value > 1", nil))
}

// TestRealEvaluator_ParseAndCheck_RespectsContextCancellation
// confirms the cancel fast-fail. Running a real parse on a cancelled
// context MUST return context.Canceled wrapped, not silently parse.
func TestRealEvaluator_ParseAndCheck_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := dsl.NewRealEvaluator(0).ParseAndCheck(ctx, "q1.answered", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestRealEvaluator_Eval_Truthy executes a typical "branch predicate"
// against a populated env and asserts the boolean outcome.
func TestRealEvaluator_Eval_Truthy(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	env := map[string]any{
		"q1": map[string]any{"value": 15.0, "answered": true},
	}
	got, err := ev.Eval(context.Background(), "q1.value > 10", env)
	require.NoError(t, err)
	require.Equal(t, true, got)
}

// TestRealEvaluator_Eval_Falsy mirrors the truthy case for the
// false-outcome path.
func TestRealEvaluator_Eval_Falsy(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	env := map[string]any{
		"q1": map[string]any{"value": 5.0, "answered": true},
	}
	got, err := ev.Eval(context.Background(), "q1.value > 10", env)
	require.NoError(t, err)
	require.Equal(t, false, got)
}

// TestRealEvaluator_Eval_RejectsAtCompileTime confirms that calling
// Eval on a forbidden expression returns ErrDSLParse (compile-time)
// rather than ErrDSLEval (runtime). The whitelist enforcement runs
// before any value lookup.
func TestRealEvaluator_Eval_RejectsAtCompileTime(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	_, err := ev.Eval(context.Background(), `os.Getenv("X")`, map[string]any{})
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestRealEvaluator_Eval_RuntimeError documents the path where the
// expression parses fine but the env lookup fails at runtime —
// returns ErrDSLEval (not parse). The runtime fallback is to take the
// branch's else edge; this test pins the wrapping.
func TestRealEvaluator_Eval_RuntimeError(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	// Reference q2 in expression but only provide q1. expr-lang's
	// strict mode (default when env is supplied) raises a runtime
	// "cannot fetch q2" error.
	_, err := ev.Eval(context.Background(), "q2.value > 1", map[string]any{
		"q1": map[string]any{"value": 1.0, "answered": true},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLEval)
}

// TestRealEvaluator_Eval_Cached verifies that a second Eval of the
// same expression reuses the compiled vm.Program rather than
// recompiling. The contract is observable through the cache size: it
// MUST stay at 1 after the second call.
func TestRealEvaluator_Eval_Cached(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(8)
	env := map[string]any{
		"q1": map[string]any{"value": 11.0, "answered": true},
	}
	const expr = "q1.value > 10"

	_, err := ev.Eval(context.Background(), expr, env)
	require.NoError(t, err)
	require.Equal(t, 1, ev.CacheLen())

	_, err = ev.Eval(context.Background(), expr, env)
	require.NoError(t, err)
	require.Equal(t, 1, ev.CacheLen())

	_, err = ev.Eval(context.Background(), "q1.value > 0", env)
	require.NoError(t, err)
	require.Equal(t, 2, ev.CacheLen())
}

// TestRealEvaluator_Eval_Concurrent stresses the cache under the race
// detector. Fifty goroutines compile and run the same handful of
// expressions; the test passes if no race is detected and every Eval
// produces the expected boolean.
func TestRealEvaluator_Eval_Concurrent(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(64)
	env := map[string]any{
		"q1": map[string]any{"value": 50.0, "answered": true},
		"q2": map[string]any{"value": "yes", "answered": true},
	}
	exprs := []string{
		"q1.value > 0",
		"q1.value > 100",
		"q1.answered",
		`q2.value == "yes"`,
		`q2.value == "no"`,
	}
	expected := []bool{true, false, true, true, false}

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j, expr := range exprs {
				got, err := ev.Eval(context.Background(), expr, env)
				if err != nil {
					t.Errorf("eval %q: %v", expr, err)
					return
				}
				if got != expected[j] {
					t.Errorf("eval %q: got %v, want %v", expr, got, expected[j])
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestRealEvaluator_Eval_RespectsContext fails fast on cancelled
// context (mirrors the ParseAndCheck behaviour).
func TestRealEvaluator_Eval_RespectsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dsl.NewRealEvaluator(0).Eval(ctx, "q1.answered", map[string]any{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestErrDSLEval_StableSentinel guards the new sentinel against
// rename / re-creation drift, mirroring the ErrDSLParse contract.
func TestErrDSLEval_StableSentinel(t *testing.T) {
	t.Parallel()
	require.Error(t, dsl.ErrDSLEval)
	// Trigger a real wrapped instance via the evaluator and confirm
	// errors.Is matches through the wrap.
	_, err := dsl.NewRealEvaluator(0).Eval(t.Context(), "q9.value > 1", map[string]any{})
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLEval)
}

// TestRealEvaluator_ImplementsInterface is a compile-time check
// expressed at runtime: assigning a *RealEvaluator into the
// dsl.Evaluator slot would fail compilation if the interface drifted.
func TestRealEvaluator_ImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ dsl.Evaluator = dsl.NewRealEvaluator(0)
}

// TestRealEvaluator_ParseAndCheck_RejectsAdvancedConstructs covers
// the AST node-types we explicitly reject because they're outside
// the whitelist's pure-expression footprint:
//
//   - PredicateNode: `[1,2].all(# > 0)` → forbidden (collection
//     iteration intrinsics not exposed).
//   - PointerNode: `#` reference (used inside predicate expressions)
//     would be reachable if a predicate slipped past the parser.
//   - VariableDeclaratorNode: `let x = 1; x` → forbidden (no state).
//
// expr-lang's parser may reject some of these at parse-time too;
// where it does, the test asserts the wrapped ErrDSLParse, which is
// what callers care about either way.
func TestRealEvaluator_ParseAndCheck_RejectsAdvancedConstructs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		expr string
	}{
		{"predicate all", "[1, 2, 3].all(# > 0)"},
		{"predicate filter", "[1, 2, 3].filter(# > 1)"},
		{"variable declaration", "let x = 1; x > 0"},
		// Computed property access with a forbidden key — `q1["secret"]`
		// — must be rejected (the whitelist caps property names at
		// .value / .answered regardless of the syntax used).
		{"forbidden computed member", `q1["secret"]`},
		{"chained member access", "q1.value.length"},
	}
	ev := dsl.NewRealEvaluator(0)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ev.ParseAndCheck(context.Background(), c.expr, nil)
			require.Errorf(t, err, "expression %q should be rejected", c.expr)
			require.ErrorIsf(t, err, dsl.ErrDSLParse,
				"rejection of %q should wrap ErrDSLParse, got %v", c.expr, err)
		})
	}
}

// TestRealEvaluator_ParseAndCheck_RejectsBareQIdentifier verifies
// that a bare `q1` (no `.value` / `.answered`) is rejected. This
// closes the gap that would otherwise let an expression do
// `q1 == nil` to detect non-existence, side-stepping the .answered
// flag.
func TestRealEvaluator_ParseAndCheck_RejectsBareQIdentifier(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	err := ev.ParseAndCheck(context.Background(), "q1 == nil", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestRealEvaluator_ParseAndCheck_RejectsForbiddenProperty pins the
// allowedMemberProps whitelist: only `.value` / `.answered` are
// allowed. A property name outside the set (`q1.choice`) MUST be
// rejected at parse-time so a fixture testing the forbidden form
// fails CI rather than silently misreading the answer.
func TestRealEvaluator_ParseAndCheck_RejectsForbiddenProperty(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	err := ev.ParseAndCheck(context.Background(), "q1.choice == 1", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestRealEvaluator_Eval_EmptyExpression returns ErrDSLParse from
// Eval (not ErrDSLEval). The empty-string check happens before we
// even get to compile.
func TestRealEvaluator_Eval_EmptyExpression(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(0)
	_, err := ev.Eval(context.Background(), "   ", map[string]any{})
	require.Error(t, err)
	require.ErrorIs(t, err, dsl.ErrDSLParse)
}

// TestRealEvaluator_Eval_FreshEvaluator verifies the cache-miss
// path: a never-before-seen expression compiles cleanly and the
// program lands in the cache.
func TestRealEvaluator_Eval_FreshEvaluator(t *testing.T) {
	t.Parallel()
	ev := dsl.NewRealEvaluator(8)
	require.Equal(t, 0, ev.CacheLen())
	got, err := ev.Eval(context.Background(), "q1.answered", map[string]any{
		"q1": map[string]any{"value": nil, "answered": false},
	})
	require.NoError(t, err)
	require.Equal(t, false, got)
	require.Equal(t, 1, ev.CacheLen())
}
