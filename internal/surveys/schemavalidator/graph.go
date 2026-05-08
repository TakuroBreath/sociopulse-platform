package schemavalidator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// GraphValidator runs the semantic checks the JSON-Schema can't
// express. It depends on a [dsl.Evaluator] for the `when`-expression
// parsability check; Plan 07 Task 2 wires the [dsl.StubEvaluator],
// Task 3 swaps it for the real expr-lang implementation without
// touching this file.
type GraphValidator struct {
	dsl dsl.Evaluator
}

// NewGraphValidator builds a validator from the supplied evaluator.
// d MUST NOT be nil — graph checks always exercise the DSL surface
// (a "when" of empty string is normalised to "true" before parsing,
// so every edge runs through ParseAndCheck). Tests inject a fake
// implementation; production injects [dsl.StubEvaluator] until Task 3.
func NewGraphValidator(d dsl.Evaluator) *GraphValidator {
	if d == nil {
		panic("schemavalidator: NewGraphValidator: dsl.Evaluator is required (use a stub in tests, never nil)")
	}
	return &GraphValidator{dsl: d}
}

// Validate runs the six graph-level checks against schemaJSON and
// returns the issues in stable order (issues for earlier checks come
// first; within a check, issues are emitted in node-id order). The
// caller MUST have run the JSON-Schema pass and seen no failures —
// running graphPass on a structurally invalid document is undefined.
//
// ctx is plumbed through to the DSL evaluator so future
// implementations can short-circuit on cancellation; the current code
// only consults ctx via the evaluator, never directly.
func (v *GraphValidator) Validate(ctx context.Context, schemaJSON []byte) []Issue {
	g, parseErr := parseGraph(schemaJSON)
	if parseErr != nil {
		// The JSON-Schema pass should have caught structural problems;
		// reaching this branch means the caller violated the contract
		// or the schema let through something the graph parser can't
		// digest. Surface a single CodeJSONSchemaInvalid so the
		// surfaced error stays in the JSON-Schema bucket rather than
		// lying about being a graph problem.
		return []Issue{{
			Code:    CodeJSONSchemaInvalid,
			Path:    "/nodes",
			Message: fmt.Sprintf("graph re-parse failed: %s", parseErr.Error()),
		}}
	}
	out := make([]Issue, 0, 8)
	out = append(out, g.checkStart()...)
	out = append(out, g.checkDanglingEdges()...)
	out = append(out, g.checkReachability()...)
	out = append(out, g.checkEndsReachable()...)
	out = append(out, g.checkCyclesHaveExit()...)
	out = append(out, g.checkWhenExpressions(ctx, v.dsl)...)
	out = append(out, g.checkForwardReferences()...)
	return out
}

// graph is the parsed-out projection the graph-level checks operate
// on. The map gives O(1) id resolution; order is the deterministic
// iteration order so issue lists are stable across runs.
type graph struct {
	nodes      map[string]*graphNode
	order      []string          // sorted ascending by id
	startID    string            // populated by checkStart when exactly one start exists
	duplicates []duplicateRecord // populated by parseGraph for checkStart
}

// graphNode is the projection of one survey node. Only the fields the
// graph checks need are unpacked — the rest (title, body, ui) is
// invisible to this layer.
type graphNode struct {
	id    string
	kind  api.NodeKind
	qtype api.QuestionType
	edges []graphEdge
}

// graphEdge mirrors one entry of a node's `next` array.
type graphEdge struct {
	to   string
	when string
}

// parseGraph projects schemaJSON into the [graph] form. It assumes the
// JSON-Schema pass already accepted the document; therefore the
// `nodes` array exists, every node has a non-empty id and kind, and
// every edge has a non-empty `to`. We still defend against a
// malformed input by returning a non-nil error if json.Unmarshal
// fails — that indicates a caller-contract violation upstream.
func parseGraph(schemaJSON []byte) (*graph, error) {
	var raw struct {
		Nodes []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			QType   string `json:"question_type"`
			Options []struct {
				ID string `json:"id"`
			} `json:"options"`
			Next []struct {
				To   string `json:"to"`
				When string `json:"when"`
			} `json:"next"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(schemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal graph: %w", err)
	}
	g := &graph{
		nodes: make(map[string]*graphNode, len(raw.Nodes)),
		order: make([]string, 0, len(raw.Nodes)),
	}
	for i, n := range raw.Nodes {
		// The JSON-Schema rejects empty ids, so this is a defensive
		// guard rather than a real path. We still skip-and-continue
		// instead of erroring so the rest of the graph can be checked
		// in the rare case a hand-built test vector slips through.
		if n.ID == "" {
			continue
		}
		// Record the first occurrence; the second-or-later one is
		// captured for checkStart's duplicate-id pass.
		if _, ok := g.nodes[n.ID]; ok {
			g.duplicates = append(g.duplicates, duplicateRecord{id: n.ID, idx: i})
			continue
		}
		gn := &graphNode{
			id:    n.ID,
			kind:  api.NodeKind(n.Kind),
			qtype: api.QuestionType(n.QType),
		}
		gn.edges = make([]graphEdge, 0, len(n.Next))
		for _, e := range n.Next {
			gn.edges = append(gn.edges, graphEdge{to: e.To, when: e.When})
		}
		g.nodes[n.ID] = gn
		g.order = append(g.order, n.ID)
	}
	sort.Strings(g.order)
	return g, nil
}

// duplicateRecord captures a single duplicate-id occurrence detected
// while iterating the raw nodes array. parseGraph fills this slice;
// checkStart drains it. Empty for any well-formed schema.
type duplicateRecord struct {
	id  string
	idx int
}

// checkStart enforces the "exactly one start node" rule and surfaces
// duplicate-id encounters captured during parsing. It also publishes
// the start id to g.startID so subsequent checks can short-circuit
// when no start exists.
func (g *graph) checkStart() []Issue {
	out := make([]Issue, 0, 2)

	// Surface duplicate-ids collected during parseGraph. These are
	// reported under their own code so the UI can highlight both
	// occurrences (we report only the offending second occurrence
	// here; the first is by definition non-duplicate).
	for _, d := range g.duplicates {
		out = append(out, Issue{
			Code:    CodeGraphDuplicateNodeID,
			Path:    fmt.Sprintf("/nodes/%d/id", d.idx),
			Message: fmt.Sprintf("duplicate node id: %s", d.id),
		})
	}

	starts := make([]string, 0, 1)
	for _, id := range g.order {
		if g.nodes[id].kind == api.NodeStart {
			starts = append(starts, id)
		}
	}
	switch len(starts) {
	case 0:
		out = append(out, Issue{
			Code:    CodeGraphNoStart,
			Path:    "/nodes",
			Message: "graph has no node with kind=start",
		})
	case 1:
		g.startID = starts[0]
	default:
		for _, id := range starts {
			out = append(out, Issue{
				Code:    CodeGraphMultipleStarts,
				Path:    nodePath(id),
				Message: fmt.Sprintf("graph has more than one node with kind=start: %s", id),
			})
		}
	}
	return out
}

// checkDanglingEdges verifies that every edge.to references a node
// that exists in the document. The path uses the synthetic
// "/nodes/<id>/next/<idx>/to" form so the editor can highlight the
// exact edge.
func (g *graph) checkDanglingEdges() []Issue {
	out := make([]Issue, 0, 4)
	for _, id := range g.order {
		n := g.nodes[id]
		for i, e := range n.edges {
			if _, ok := g.nodes[e.to]; !ok {
				out = append(out, Issue{
					Code:    CodeGraphDanglingEdge,
					Path:    fmt.Sprintf("/nodes/%s/next/%d/to", id, i),
					Message: fmt.Sprintf("edge from %s references unknown node: %s", id, e.to),
				})
			}
		}
	}
	return out
}

// checkReachability runs BFS from the start node and reports every
// node not reached. Returns no issues if there's no start (the
// no-start error is reported elsewhere) or if all nodes are reached.
func (g *graph) checkReachability() []Issue {
	if g.startID == "" {
		return nil
	}
	visited := bfs(g, g.startID)
	out := make([]Issue, 0, 4)
	for _, id := range g.order {
		if _, seen := visited[id]; seen {
			continue
		}
		out = append(out, Issue{
			Code:    CodeGraphUnreachableNode,
			Path:    nodePath(id),
			Message: fmt.Sprintf("node %s is not reachable from start", id),
		})
	}
	return out
}

// checkEndsReachable verifies that BFS from start reaches at least
// one *-end node (success-end OR refusal-end). A graph without any
// reachable terminal would never let a respondent finish.
func (g *graph) checkEndsReachable() []Issue {
	if g.startID == "" {
		return nil
	}
	visited := bfs(g, g.startID)
	for id := range visited {
		k := g.nodes[id].kind
		if k == api.NodeSuccessEnd || k == api.NodeRefusalEnd {
			return nil
		}
	}
	return []Issue{{
		Code:    CodeGraphNoEndReachable,
		Path:    "/nodes",
		Message: "no terminal node (success-end or refusal-end) is reachable from start",
	}}
}

// checkCyclesHaveExit reports nodes that sit inside a cycle from
// which no terminal node is reachable. The implementation is the
// "BFS from each node, then verify a terminal is in the reachable
// set" approach from the plan source: O(V*(V+E)) which is fine for
// our envelope (<200 nodes per survey).
func (g *graph) checkCyclesHaveExit() []Issue {
	if g.startID == "" {
		return nil
	}
	out := make([]Issue, 0, 2)
	for _, id := range g.order {
		if !g.inUnescapableCycle(id) {
			continue
		}
		out = append(out, Issue{
			Code:    CodeGraphCycleNoExit,
			Path:    nodePath(id),
			Message: fmt.Sprintf("node %s is in a cycle with no exit to a terminal node", id),
		})
	}
	return out
}

// inUnescapableCycle returns true iff `nodeID` is part of a cycle
// whose reachable set has no terminal node. We tolerate the false
// positive that an isolated dead-end (no outgoing edges, no terminal)
// would also flag — but isolated dead-ends are caught by
// checkEndsReachable, so the redundancy is harmless.
func (g *graph) inUnescapableCycle(nodeID string) bool {
	n := g.nodes[nodeID]
	// A node with no outgoing edges cannot be in a cycle.
	if len(n.edges) == 0 {
		return false
	}
	// A terminal node is by definition not in a cycle-without-exit
	// (it IS the exit).
	if n.kind == api.NodeSuccessEnd || n.kind == api.NodeRefusalEnd {
		return false
	}
	visited := bfs(g, nodeID)
	// If any reachable node is terminal, the cycle has an exit.
	for id := range visited {
		k := g.nodes[id].kind
		if k == api.NodeSuccessEnd || k == api.NodeRefusalEnd {
			return false
		}
	}
	// Confirm there's actually a cycle through nodeID — i.e., some
	// descendant has an edge back to nodeID. Without this confirmation
	// a dead-end with no terminal reachable would false-trigger.
	for d := range visited {
		if d == nodeID {
			continue
		}
		for _, e := range g.nodes[d].edges {
			if e.to == nodeID {
				return true
			}
		}
	}
	return false
}

// bfs runs breadth-first search from `start`, returning the set of
// node ids reachable in the directed graph (including start itself).
// Edges to unknown ids (dangling) are ignored — the dangling-edge
// pass reports them.
func bfs(g *graph, start string) map[string]struct{} {
	out := map[string]struct{}{start: {}}
	q := []string{start}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		n, ok := g.nodes[cur]
		if !ok {
			continue
		}
		for _, e := range n.edges {
			if _, ok := g.nodes[e.to]; !ok {
				continue // dangling — reported by checkDanglingEdges
			}
			if _, seen := out[e.to]; seen {
				continue
			}
			out[e.to] = struct{}{}
			q = append(q, e.to)
		}
	}
	return out
}

// checkWhenExpressions parses every edge.when via the supplied
// evaluator and emits one [Issue] per malformed expression. An empty
// `when` means "always true" and is skipped — sending it through the
// stub would fail the empty-string check.
func (g *graph) checkWhenExpressions(ctx context.Context, ev dsl.Evaluator) []Issue {
	out := make([]Issue, 0, 2)
	for _, id := range g.order {
		for i, e := range g.nodes[id].edges {
			if e.when == "" {
				continue
			}
			if err := ev.ParseAndCheck(ctx, e.when, nil); err != nil {
				out = append(out, Issue{
					Code:    CodeGraphBadWhen,
					Path:    fmt.Sprintf("/nodes/%s/next/%d/when", id, i),
					Message: dslIssueMessage(id, e.to, err),
				})
			}
		}
	}
	return out
}

// dslIssueMessage builds a low-cardinality summary of a DSL parse
// failure. It deliberately drops the original error string when the
// failure is the catch-all (so log-aggregator buckets stay narrow)
// but keeps the wrapped text when it's a user-actionable detail
// (currently only the unmatched-paren classes; see [dsl.StubEvaluator]).
func dslIssueMessage(fromID, toID string, err error) string {
	if errors.Is(err, dsl.ErrDSLParse) {
		return fmt.Sprintf("when-expression on edge %s→%s is malformed: %s", fromID, toID, err.Error())
	}
	return fmt.Sprintf("when-expression on edge %s→%s failed validation: %s", fromID, toID, err.Error())
}

// checkForwardReferences enforces that every q<id>.value /
// q<id>.answered identifier referenced in a `when` clause refers to
// a node that DOMINATES the current node — i.e. that lies on every
// path from start to the current node, so by the time we evaluate
// the predicate the referenced answer is guaranteed to exist.
//
// The Task 2 stub does NOT expose an AST, so we extract identifiers
// here with a regex limited to the canonical "q<id>.value" /
// "q<id>.answered" forms specified in the DSL whitelist (plan source
// §11.3). This is intentionally conservative: an expression that
// uses a syntactic form the regex doesn't recognise (e.g. a future
// "len(q1.options) > 0") simply produces no references and the check
// is a no-op for that edge — Task 3 will replace the regex with an
// AST walk that catches every reference.
func (g *graph) checkForwardReferences() []Issue {
	if g.startID == "" {
		return nil
	}
	dom := computeDominators(g)
	out := make([]Issue, 0, 2)
	for _, id := range g.order {
		for i, e := range g.nodes[id].edges {
			if e.when == "" {
				continue
			}
			refs := extractNodeRefs(e.when)
			for _, ref := range refs {
				switch {
				case ref == id:
					// Self-reference: the node's answer is available
					// once the node has been visited. The runtime
					// guarantees the node fires its `next` evaluation
					// after the answer is recorded, so a self-ref is
					// never a forward reference.
					continue
				case g.nodes[ref] == nil:
					out = append(out, Issue{
						Code:    CodeGraphBadNodeRef,
						Path:    fmt.Sprintf("/nodes/%s/next/%d/when", id, i),
						Message: fmt.Sprintf("when-expression on edge %s→%s references unknown node: %s", id, e.to, ref),
					})
				default:
					if _, dominates := dom[id][ref]; !dominates {
						out = append(out, Issue{
							Code:    CodeGraphForwardRef,
							Path:    fmt.Sprintf("/nodes/%s/next/%d/when", id, i),
							Message: fmt.Sprintf("when-expression on edge %s→%s references %s but %s is not on every path from start", id, e.to, ref, ref),
						})
					}
				}
			}
		}
	}
	return out
}

// nodeRefRE matches the q<id>.value / q<id>.answered identifier
// grammar from the DSL whitelist. The leading `\b` boundary keeps
// the matcher from scooping the trailing characters of a longer
// identifier (e.g. "myq1.value" must NOT match "q1").
//
// The id grammar is the same character class as the survey-1.0.json
// node id pattern: `^[a-zA-Z0-9_-]+$`. We also accept the bare
// identifier prefix (e.g. "q1" with no dot suffix) so a future
// reference shape can be picked up gracefully.
var nodeRefRE = regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9_-]*)\.(value|answered)\b`)

// extractNodeRefs returns the unique node ids referenced from the
// given expression. Order is alphabetical so the issue list stays
// stable across runs.
func extractNodeRefs(expr string) []string {
	matches := nodeRefRE.FindAllStringSubmatch(expr, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		ref := m[1]
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	slices.Sort(out)
	return out
}

// computeDominators returns dom[N] = set of nodes that lie on every
// path from start to N. Iterative algorithm — sufficient for graphs
// with <200 nodes (our envelope). Reference: Cooper / Harvey /
// Kennedy, "A Simple, Fast Dominance Algorithm". We use the textbook
// fixed-point version because the optimised form requires a reverse
// post-order numbering whose construction would dominate this file
// in line count.
func computeDominators(g *graph) map[string]map[string]struct{} {
	dom := initDominators(g)
	preds := buildPredecessors(g)
	for changed := true; changed; {
		changed = false
		for _, id := range g.order {
			if id == g.startID {
				continue
			}
			if updateDominator(id, dom, preds[id]) {
				changed = true
			}
		}
	}
	return dom
}

// initDominators bootstraps the dominator map: dom[start] = {start}
// and dom[N] = {all nodes} for every other node. The "everyone
// dominates everyone" initial state is the canonical starting point
// for the fixed-point iteration.
func initDominators(g *graph) map[string]map[string]struct{} {
	all := make(map[string]struct{}, len(g.order))
	for _, id := range g.order {
		all[id] = struct{}{}
	}
	dom := make(map[string]map[string]struct{}, len(g.order))
	dom[g.startID] = map[string]struct{}{g.startID: {}}
	for _, id := range g.order {
		if id == g.startID {
			continue
		}
		dom[id] = copySet(all)
	}
	return dom
}

// buildPredecessors returns the inverted edge map. Dangling edges
// (edge.to references an unknown node) are skipped — the dangling
// pass reports them, and including them here would corrupt the
// fixed-point.
func buildPredecessors(g *graph) map[string][]string {
	preds := make(map[string][]string, len(g.order))
	for _, id := range g.order {
		for _, e := range g.nodes[id].edges {
			if _, ok := g.nodes[e.to]; !ok {
				continue
			}
			preds[e.to] = append(preds[e.to], id)
		}
	}
	return preds
}

// updateDominator recomputes dom[id] as
// `intersect(dom[p] for p in preds) ∪ {id}`. Returns true iff the
// new set differs from the previous state — caller uses the bool to
// drive the fixed-point convergence loop.
func updateDominator(id string, dom map[string]map[string]struct{}, preds []string) bool {
	var newDom map[string]struct{}
	for _, p := range preds {
		if newDom == nil {
			newDom = copySet(dom[p])
			continue
		}
		newDom = intersect(newDom, dom[p])
	}
	if newDom == nil {
		newDom = make(map[string]struct{})
	}
	newDom[id] = struct{}{}
	if setEqual(newDom, dom[id]) {
		return false
	}
	dom[id] = newDom
	return true
}

func copySet(s map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}

func intersect(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a))
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func setEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// nodePath formats the JSON Pointer for the indexed-by-id node form
// used by graph-layer issues. We can't easily produce the JSON
// document index here (and it's brittle anyway — a re-shuffle of the
// document would shift indices), so the editor expects a synthetic
// "/nodes/<id>" path and resolves it by id lookup.
func nodePath(id string) string {
	return "/nodes/" + id
}
