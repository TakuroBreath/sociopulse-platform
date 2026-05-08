package dsl

import (
	"github.com/sociopulse/platform/internal/surveys/api"
)

// BuildEnv constructs the expr-lang env map from a set of answers and
// the known question ids in the schema. The returned map is the
// argument passed to [expr.Run] / [RealEvaluator.Eval] when evaluating
// `when`-clause predicates.
//
// Layout (matches the DSL whitelist `q<id>.value`, `q<id>.answered`):
//
//	{
//	    "q1": {"value": <typed-value>, "answered": true},
//	    "q2": {"value": nil,           "answered": false},
//	    ...
//	}
//
// For each id in knownQuestionIDs:
//   - if an answer exists in the answers map: expose
//     {value: typedValue, answered: true}.
//   - otherwise: expose {value: nil, answered: false} so a predicate
//     like `q2.answered` evaluates without throwing on missing keys.
//
// Answers present in the answers map but absent from knownQuestionIDs
// are still exposed — they would only appear there if a respondent
// answered a node not declared by the current schema (a defensive
// path the runtime shouldn't normally hit).
//
// The function is pure: it never reads time, env, files, or the
// network. Safe for concurrent calls.
func BuildEnv(answers map[string]api.Answer, knownQuestionIDs []string) map[string]any {
	out := make(map[string]any, len(knownQuestionIDs)+len(answers))
	// First pass: populate every known id, marking answered iff a
	// matching entry exists in the answers map.
	for _, id := range knownQuestionIDs {
		if ans, ok := answers[id]; ok {
			out[id] = answeredEntry(ans)
			continue
		}
		out[id] = unansweredEntry()
	}
	// Second pass: expose answers for ids NOT in knownQuestionIDs.
	// Cheap defence against schema drift (e.g. an old answer record
	// referencing a removed question).
	for id, ans := range answers {
		if _, already := out[id]; already {
			continue
		}
		out[id] = answeredEntry(ans)
	}
	return out
}

// answeredEntry projects one [api.Answer] into the {value, answered}
// shape exposed to the DSL. The "value" key holds the typed answer
// value chosen by [answerValue]; "answered" is always true here
// because we only build this entry for known answers.
func answeredEntry(ans api.Answer) map[string]any {
	return map[string]any{
		"value":    answerValue(ans),
		"answered": true,
	}
}

// unansweredEntry returns the canonical missing-answer projection.
// `nil` for value lets `q.value == X` evaluate to false (expr-lang
// returns false for nil-typed comparisons against a concrete RHS),
// and `answered: false` lets `!q.answered` short-circuit a branch.
func unansweredEntry() map[string]any {
	return map[string]any{
		"value":    nil,
		"answered": false,
	}
}

// answerValue extracts the typed value from an [api.Answer] for use
// in DSL evaluation. Per the Answer doc-string, exactly one of the
// four value fields is populated per question type:
//
//   - SingleChoice → exposed as string (for select / single).
//   - MultiChoice  → exposed as []string (for multi-select; supports
//     the `in` operator).
//   - Number       → exposed as float64 (dereferenced; for arithmetic
//     comparisons).
//   - Text         → exposed as string (for free-form text).
//
// When all four are zero values, the function returns the empty
// string — the runtime's `answered` flag is sufficient to
// distinguish "answered with empty text" from "not answered" (the
// caller checks `q.answered` first).
func answerValue(ans api.Answer) any {
	switch {
	case ans.MultiChoice != nil:
		return ans.MultiChoice
	case ans.Number != nil:
		return *ans.Number
	case ans.SingleChoice != "":
		return ans.SingleChoice
	default:
		return ans.Text
	}
}
