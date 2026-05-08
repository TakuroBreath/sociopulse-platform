// Package runtime hosts the pure-function survey evaluator.
//
// The runtime answers three questions about a survey schema, the
// current node, and the answers collected so far:
//
//  1. NextNode      — given an outgoing-edges DSL, which node comes next?
//  2. ValidateAnswer — is the supplied answer well-formed for the node's
//     QuestionType?
//  3. CalculateProgress — coarse [0,1] estimate for UI progress bars.
//
// Stateless. Every public method accepts schema bytes and a fresh
// answers map; the runtime keeps no per-session state. An optional
// LRU cache keyed on sha256(schema) avoids re-parsing the same JSON
// across calls within a session — see [schemaCache].
//
// Designed to compile through TinyGo to WebAssembly (ADR-0008): the
// runtime touches no I/O, no time, no random source, no goroutines,
// and depends only on `encoding/json`, the DSL evaluator (which is
// itself pure), and stdlib data structures.
//
// All methods are safe for concurrent use.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// Runtime is the per-call survey evaluator.
type Runtime struct {
	evaluator dsl.Evaluator
	cache     *schemaCache
}

// Compile-time interface check: *Runtime satisfies api.Runtime.
var _ api.Runtime = (*Runtime)(nil)

// New constructs a Runtime.
//
// evaluator MUST NOT be nil — the runtime always exercises the DSL
// surface (a `when` of empty string is treated as unconditional and
// short-circuits before reaching the evaluator, but every non-empty
// `when` calls Eval). Passing nil panics so misconfiguration is
// caught at composition time, not on the first request.
//
// cacheSize controls the schema-parse LRU. Pass 0 for the project
// default (256), a positive number for a custom cap, or a negative
// number to disable caching entirely (the runtime then re-parses the
// schema on every call — useful for fuzz tests but never in
// production).
func New(evaluator dsl.Evaluator, cacheSize int) *Runtime {
	if evaluator == nil {
		panic("runtime: evaluator required (use dsl.NewRealEvaluator in prod, dsl.NewStubEvaluator in tests that don't exercise Eval)")
	}
	return &Runtime{
		evaluator: evaluator,
		cache:     newSchemaCache(cacheSize),
	}
}

// NextNode evaluates outgoing edges of currentNodeID against answers
// and returns the next node + termination state. See [Runtime.NextNodeCtx]
// for the ctx-aware variant; this method passes context.Background.
//
// Returned errors wrap one of:
//   - api.ErrSchema           — schema bytes couldn't be parsed.
//   - api.ErrNodeNotFound     — currentNodeID not in schema.
//   - api.ErrNoMatchingEdge   — no edge matched (no unconditional default
//     and every conditional `when` evaluated falsy / errored).
//   - dsl.ErrDSLParse / dsl.ErrDSLEval — re-surfaced from the evaluator.
func (r *Runtime) NextNode(schema []byte, currentNodeID string, answers map[string]api.Answer) (api.NodeResult, error) {
	return r.NextNodeCtx(context.Background(), schema, currentNodeID, answers)
}

// NextNodeCtx is the ctx-aware sister of [Runtime.NextNode]. The DSL
// evaluator respects cancellation, so a long-running batch can plumb
// a deadline through this method instead of using the ctx-less form.
func (r *Runtime) NextNodeCtx(ctx context.Context, schema []byte, currentNodeID string, answers map[string]api.Answer) (api.NodeResult, error) {
	doc, ids, err := r.parse(schema)
	if err != nil {
		return api.NodeResult{}, err
	}
	cur, ok := findNode(doc, currentNodeID)
	if !ok {
		return api.NodeResult{}, fmt.Errorf("%w: %s", api.ErrNodeNotFound, currentNodeID)
	}

	// If the current node is itself a terminal, return immediately
	// with the matching EndKind. The runtime is callable on terminals
	// so the UI can re-render without re-asking "what comes next".
	if endKind, terminated := terminalKind(cur.Kind); terminated {
		return api.NodeResult{
			NextNodeID: "",
			Terminated: true,
			EndKind:    endKind,
			Progress:   progressOf(doc, currentNodeID, answers),
		}, nil
	}

	// Build the DSL env once per call. The known-ids slice is the
	// pre-cached sorted list from the schemaCache when present —
	// otherwise parse() returns the same list freshly computed.
	env := dsl.BuildEnv(answers, ids)

	for _, edge := range cur.Next {
		matched, evalErr := r.edgeMatches(ctx, edge.When, env)
		if evalErr != nil {
			return api.NodeResult{}, evalErr
		}
		if !matched {
			continue
		}
		target, ok := findNode(doc, edge.To)
		if !ok {
			// Dangling edge. Schema validation should have caught this
			// at SaveVersion time; surface it here as a structural
			// schema error rather than a runtime no-match.
			return api.NodeResult{}, fmt.Errorf("%w: edge target %q not found", api.ErrSchema, edge.To)
		}
		endKind, terminated := terminalKind(target.Kind)
		return api.NodeResult{
			NextNodeID: target.ID,
			Terminated: terminated,
			EndKind:    endKind,
			Progress:   progressOf(doc, target.ID, answers),
		}, nil
	}

	return api.NodeResult{}, fmt.Errorf("%w: from node %q", api.ErrNoMatchingEdge, currentNodeID)
}

// ValidateAnswer checks that ans is well-formed for the node's
// QuestionType. Returns nil on success or an error wrapping
// api.ErrBadAnswer (and api.ErrSchema / api.ErrNodeNotFound for
// structural failures upstream of the answer check).
//
// Validation rules per [docs/references/plan-07-surveys.md]:
//
//   - single  — Answer.SingleChoice must equal one of options[*].id.
//   - multi   — Answer.MultiChoice elements all in options[*].id;
//     non-empty when node.Required.
//   - number  — Answer.Number non-nil; honours optional Min/Max.
//   - text    — Answer.Text non-empty when node.Required.
//   - select  — same rule as single.
//
// Non-question nodes (start, intro, *-end, condition, jump) accept
// any answer (including the zero value); the call-flow never asks
// those nodes for an answer, so the runtime is permissive rather
// than spuriously rejecting.
func (r *Runtime) ValidateAnswer(schema []byte, nodeID string, ans api.Answer) error {
	doc, _, err := r.parse(schema)
	if err != nil {
		return err
	}
	node, ok := findNode(doc, nodeID)
	if !ok {
		return fmt.Errorf("%w: %s", api.ErrNodeNotFound, nodeID)
	}
	if api.NodeKind(node.Kind) != api.NodeQuestion {
		return nil
	}
	return validateQuestionAnswer(node, ans)
}

// CalculateProgress returns a coarse [0,1] progress estimate for the
// UI. The value is "answered question-nodes so far / total question-
// nodes in the schema" — branches make a precise per-path denominator
// expensive to compute (the path is what we're choosing!), so we use
// the static total. Acceptable approximation for a progress bar.
//
// At the start node: 0.0. At a *-end node: 1.0. Inside the body:
// proportional to answered-question count so the bar moves
// monotonically forward when the operator answers questions.
func (r *Runtime) CalculateProgress(schema []byte, currentNodeID string) (float64, error) {
	doc, _, err := r.parse(schema)
	if err != nil {
		return 0, err
	}
	if _, ok := findNode(doc, currentNodeID); !ok {
		return 0, fmt.Errorf("%w: %s", api.ErrNodeNotFound, currentNodeID)
	}
	// CalculateProgress receives no answers map (per the api.Runtime
	// signature), so the runtime falls back to a node-position
	// approximation: the index of currentNodeID in document order
	// among question + terminal nodes. progressOf with a nil answers
	// map collapses to that fallback.
	return progressOf(doc, currentNodeID, nil), nil
}

// CacheLen returns the number of parsed schema documents currently
// stored in the runtime's LRU. Exposed for tests; production callers
// don't need it. Returns 0 when caching is disabled (Runtime
// constructed with a negative cacheSize).
func (r *Runtime) CacheLen() int {
	return r.cache.Len()
}

// parse looks up the parsed schemaDoc in the cache or unmarshals it
// fresh and stores it. Returns the doc, the sorted list of node ids
// (used as the known-ids argument to dsl.BuildEnv), and an error.
func (r *Runtime) parse(schema []byte) (*schemaDoc, []string, error) {
	if len(schema) == 0 {
		return nil, nil, fmt.Errorf("%w: empty schema bytes", api.ErrSchema)
	}
	key := hashSchema(schema)
	if entry, ok := r.cache.get(key); ok {
		return entry.doc, entry.keys, nil
	}
	var doc schemaDoc
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: %s", api.ErrSchema, err.Error())
	}
	if len(doc.Nodes) == 0 {
		return nil, nil, fmt.Errorf("%w: schema has no nodes", api.ErrSchema)
	}
	ids := collectNodeIDs(&doc)
	r.cache.put(key, &doc, ids)
	return &doc, ids, nil
}

// edgeMatches reports whether the given `when` predicate is truthy.
// An empty `when` is treated as the unconditional default match. A
// non-empty `when` is run through the DSL evaluator; the result is
// cast to bool — anything else (including evaluator errors) bubbles
// up the chain.
func (r *Runtime) edgeMatches(ctx context.Context, when string, env map[string]any) (bool, error) {
	if when == "" {
		return true, nil
	}
	out, err := r.evaluator.Eval(ctx, when, env)
	if err != nil {
		return false, err
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("%w: when-expression returned non-bool: %T", dsl.ErrDSLEval, out)
	}
	return b, nil
}

// findNode returns the named node and whether it was found. Linear
// scan because schemas have ≤200 nodes (per project envelope) and the
// alternative — a map keyed by id — bloats the cached doc with no
// measurable speedup.
func findNode(doc *schemaDoc, id string) (*schemaNode, bool) {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return &doc.Nodes[i], true
		}
	}
	return nil, false
}

// collectNodeIDs returns a sorted slice of every node id in the doc.
// Sorted so the returned slice is deterministic across runs (the
// callers — dsl.BuildEnv — don't care about order, but determinism
// helps tests pin the env shape).
func collectNodeIDs(doc *schemaDoc) []string {
	out := make([]string, 0, len(doc.Nodes))
	for i := range doc.Nodes {
		out = append(out, doc.Nodes[i].ID)
	}
	sort.Strings(out)
	return out
}

// terminalKind reports whether the supplied kind is a terminal and
// returns the matching api.EndKind. Returns ("", false) for
// non-terminal kinds.
func terminalKind(kind string) (api.EndKind, bool) {
	switch api.NodeKind(kind) {
	case api.NodeSuccessEnd:
		return api.EndKindSuccess, true
	case api.NodeRefusalEnd:
		return api.EndKindRefusal, true
	default:
		return api.EndKindNone, false
	}
}

// progressOf computes the coarse [0,1] progress estimate. Algorithm:
//
//   - Denominator: count of question-kind nodes in the document. Empty
//     denominator (no question nodes at all) → return 1.0 if at a
//     terminal, 0.0 otherwise. The 1.0 case is the degenerate "intro
//     → end" survey with no questions.
//   - Numerator (when answers != nil): count of question-kind nodes
//     whose id has an entry in the answers map. Terminals override
//     this to 1.0.
//   - Numerator (when answers == nil, the CalculateProgress path):
//     position of currentNodeID in the document-order list of
//     question + terminal nodes. Same monotonicity as the answers
//     path — proportional to "how far through the survey" — without
//     the answers signal.
func progressOf(doc *schemaDoc, currentNodeID string, answers map[string]api.Answer) float64 {
	if currentNodeID == "" {
		return 0
	}
	cur, ok := findNode(doc, currentNodeID)
	if !ok {
		return 0
	}
	if _, terminated := terminalKind(cur.Kind); terminated {
		return 1
	}
	denom := countQuestions(doc)
	if denom == 0 {
		return 0
	}
	if answers != nil {
		return float64(countAnswered(doc, answers)) / float64(denom)
	}
	// Fallback path: walk doc-order and count question nodes that
	// appear before the current node. The ratio is monotonic in
	// document order — close enough to "how far through the survey
	// am I" for a progress bar.
	pos := 0
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == currentNodeID {
			break
		}
		if api.NodeKind(doc.Nodes[i].Kind) == api.NodeQuestion {
			pos++
		}
	}
	return float64(pos) / float64(denom)
}

// countQuestions returns the number of question-kind nodes in the
// document. The progress denominator.
func countQuestions(doc *schemaDoc) int {
	n := 0
	for i := range doc.Nodes {
		if api.NodeKind(doc.Nodes[i].Kind) == api.NodeQuestion {
			n++
		}
	}
	return n
}

// countAnswered returns the number of question-kind nodes for which
// an answer exists in the supplied map. The progress numerator.
func countAnswered(doc *schemaDoc, answers map[string]api.Answer) int {
	n := 0
	for i := range doc.Nodes {
		if api.NodeKind(doc.Nodes[i].Kind) != api.NodeQuestion {
			continue
		}
		if _, ok := answers[doc.Nodes[i].ID]; ok {
			n++
		}
	}
	return n
}

// validateQuestionAnswer dispatches to the per-QuestionType validator.
func validateQuestionAnswer(node *schemaNode, ans api.Answer) error {
	switch api.QuestionType(node.QuestionType) {
	case api.TypeSingle, api.TypeSelect:
		return validateSingleAnswer(node, ans)
	case api.TypeMulti:
		return validateMultiAnswer(node, ans)
	case api.TypeNumber:
		return validateNumberAnswer(node, ans)
	case api.TypeText:
		return validateTextAnswer(node, ans)
	case "":
		// JSON-Schema enforces question_type is present on question
		// nodes; reaching here means the schema validation pass was
		// skipped. Be defensive rather than blame-the-author.
		return fmt.Errorf("%w: question node %q has no question_type", api.ErrSchema, node.ID)
	default:
		return fmt.Errorf("%w: unknown question_type %q on node %q", api.ErrSchema, node.QuestionType, node.ID)
	}
}

// validateSingleAnswer checks the single-choice / select rules:
// SingleChoice must be set (non-empty when required) and reference an
// existing option id.
func validateSingleAnswer(node *schemaNode, ans api.Answer) error {
	if ans.SingleChoice == "" {
		if node.Required {
			return fmt.Errorf("%w: node %q requires a single-choice answer", api.ErrBadAnswer, node.ID)
		}
		return nil
	}
	if !optionExists(node, ans.SingleChoice) {
		return fmt.Errorf("%w: node %q rejected option id %q", api.ErrBadAnswer, node.ID, ans.SingleChoice)
	}
	return nil
}

// validateMultiAnswer checks the multi-select rules: MultiChoice must
// be non-empty when required, every element must reference an existing
// option id, and duplicates are rejected (a multi-select payload
// containing the same id twice is a UI bug).
func validateMultiAnswer(node *schemaNode, ans api.Answer) error {
	if len(ans.MultiChoice) == 0 {
		if node.Required {
			return fmt.Errorf("%w: node %q requires at least one multi-choice answer", api.ErrBadAnswer, node.ID)
		}
		return nil
	}
	seen := make(map[string]struct{}, len(ans.MultiChoice))
	for _, id := range ans.MultiChoice {
		if id == "" {
			return fmt.Errorf("%w: node %q has an empty multi-choice id", api.ErrBadAnswer, node.ID)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: node %q has duplicate multi-choice id %q", api.ErrBadAnswer, node.ID, id)
		}
		seen[id] = struct{}{}
		if !optionExists(node, id) {
			return fmt.Errorf("%w: node %q rejected option id %q", api.ErrBadAnswer, node.ID, id)
		}
	}
	return nil
}

// validateNumberAnswer checks the numeric rules: Number must be
// non-nil, and respect optional Min / Max bounds. The Required flag
// has no separate behaviour beyond the non-nil check — a number
// answer without a Number value is "no answer" by definition.
func validateNumberAnswer(node *schemaNode, ans api.Answer) error {
	if ans.Number == nil {
		if node.Required {
			return fmt.Errorf("%w: node %q requires a numeric answer", api.ErrBadAnswer, node.ID)
		}
		return nil
	}
	v := *ans.Number
	if node.Min != nil && v < *node.Min {
		return fmt.Errorf("%w: node %q value %v below min %v", api.ErrBadAnswer, node.ID, v, *node.Min)
	}
	if node.Max != nil && v > *node.Max {
		return fmt.Errorf("%w: node %q value %v above max %v", api.ErrBadAnswer, node.ID, v, *node.Max)
	}
	return nil
}

// validateTextAnswer checks the free-form text rules: Text must be
// non-empty when Required. An optional answer may be the zero string.
func validateTextAnswer(node *schemaNode, ans api.Answer) error {
	if node.Required && ans.Text == "" {
		return fmt.Errorf("%w: node %q requires a text answer", api.ErrBadAnswer, node.ID)
	}
	return nil
}

// optionExists reports whether id matches one of node.Options[*].ID.
func optionExists(node *schemaNode, id string) bool {
	for i := range node.Options {
		if node.Options[i].ID == id {
			return true
		}
	}
	return false
}
