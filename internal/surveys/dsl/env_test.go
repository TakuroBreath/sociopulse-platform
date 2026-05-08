package dsl_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// TestBuildEnv_AllUnanswered verifies that BuildEnv pre-populates
// every known question id with `{value: nil, answered: false}` even
// when the answers map is empty. This is the unblocked-eval contract:
// a predicate referencing a not-yet-answered q<id>.answered must
// short-circuit to false rather than blow up the runtime.
func TestBuildEnv_AllUnanswered(t *testing.T) {
	t.Parallel()
	env := dsl.BuildEnv(nil, []string{"q1", "q2"})

	require.Contains(t, env, "q1")
	require.Contains(t, env, "q2")
	requireUnanswered(t, env, "q1")
	requireUnanswered(t, env, "q2")
}

// TestBuildEnv_SingleAnswered exposes the canonical "one answered, one
// not" shape: q1 has been answered with a SingleChoice value, q2 has
// not. The runtime relies on this exact shape (`{value, answered}`)
// for predicates like `q1.value == "yes" && !q2.answered`.
func TestBuildEnv_SingleAnswered(t *testing.T) {
	t.Parallel()
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", SingleChoice: "yes", AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1", "q2"})

	q1 := requireMap(t, env, "q1")
	require.Equal(t, "yes", q1["value"])
	require.Equal(t, true, q1["answered"])

	requireUnanswered(t, env, "q2")
}

// TestBuildEnv_NumberAnswer verifies that Number answers are exposed
// as float64 (the dereferenced value, not the *float64 pointer). This
// is the type expr-lang's checker expects for arithmetic comparisons.
func TestBuildEnv_NumberAnswer(t *testing.T) {
	t.Parallel()
	num := 42.0
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", Number: &num, AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1"})

	q1 := requireMap(t, env, "q1")
	require.InDelta(t, 42.0, q1["value"], 1e-9)
	require.Equal(t, true, q1["answered"])
}

// TestBuildEnv_MultiChoice verifies that MultiChoice answers are
// exposed as []string so the DSL `in` operator works (e.g.
// `"option-a" in q1.value`).
func TestBuildEnv_MultiChoice(t *testing.T) {
	t.Parallel()
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", MultiChoice: []string{"a", "b"}, AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1"})

	q1 := requireMap(t, env, "q1")
	require.Equal(t, []string{"a", "b"}, q1["value"])
	require.Equal(t, true, q1["answered"])
}

// TestBuildEnv_TextAnswer verifies the plain-text answer projection.
func TestBuildEnv_TextAnswer(t *testing.T) {
	t.Parallel()
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", Text: "free-form response", AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1"})

	q1 := requireMap(t, env, "q1")
	require.Equal(t, "free-form response", q1["value"])
	require.Equal(t, true, q1["answered"])
}

// TestBuildEnv_AnswerNotInKnownIDs documents an important invariant:
// answers for ids NOT in knownQuestionIDs are still exposed, because
// the alternative (silently dropping them) would produce
// false-negative branches. The runtime is expected to pass the full
// schema id set, so this path is mostly defensive.
func TestBuildEnv_AnswerNotInKnownIDs(t *testing.T) {
	t.Parallel()
	answers := map[string]api.Answer{
		"q9": {NodeID: "q9", SingleChoice: "yes", AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1"})

	requireUnanswered(t, env, "q1")
	q9 := requireMap(t, env, "q9")
	require.Equal(t, "yes", q9["value"])
	require.Equal(t, true, q9["answered"])
}

// TestBuildEnv_AnswerKey_AlsoExposed_AnsweredFlag verifies that an
// Answer with all four value fields zero (NodeID present but no
// concrete value) is still considered "answered" — the AnsweredAt
// timestamp would have been set by the runtime, and the flag
// reflects the fact that the user moved past the node, not that the
// value is non-zero.
//
// Note: per the api.Answer doc-string, exactly one of the value
// fields is populated per question type. An Answer struct that
// matches a key in the answers map is therefore by definition
// answered, even if the union member happens to be the zero value
// (e.g. an empty Text answer).
func TestBuildEnv_AnswerKey_AlsoExposed_AnsweredFlag(t *testing.T) {
	t.Parallel()
	answers := map[string]api.Answer{
		"q1": {NodeID: "q1", AnsweredAt: 1},
	}
	env := dsl.BuildEnv(answers, []string{"q1"})

	q1 := requireMap(t, env, "q1")
	require.Equal(t, true, q1["answered"])
	// Zero-value text answer ⇒ "" is the canonical projection.
	require.Equal(t, "", q1["value"])
}

// requireMap fetches a map[string]any value at top-level key and
// fails the test with a helpful message if the assertion fails.
func requireMap(t *testing.T, env map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := env[key]
	require.Truef(t, ok, "env missing key %q", key)
	m, ok := raw.(map[string]any)
	require.Truef(t, ok, "env[%q] is not map[string]any (got %T)", key, raw)
	return m
}

// requireUnanswered asserts the canonical "not answered" projection:
// {value: nil, answered: false}.
func requireUnanswered(t *testing.T, env map[string]any, key string) {
	t.Helper()
	q := requireMap(t, env, key)
	require.Nilf(t, q["value"], "%s.value should be nil for unanswered", key)
	require.Equalf(t, false, q["answered"], "%s.answered should be false for unanswered", key)
}
