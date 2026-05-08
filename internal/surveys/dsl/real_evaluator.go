package dsl

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
)

// ErrDSLEval is the sentinel returned when a syntactically-valid
// expression fails at runtime (e.g. a referenced identifier is not
// present in the env). Callers wrap this with %w so
// errors.Is(err, ErrDSLEval) keeps matching after surfacing.
var ErrDSLEval = errors.New("dsl: eval error")

// RealEvaluator is the production [Evaluator] backed by
// expr-lang/expr. It enforces the DSL whitelist via AST walking
// before compilation and caches compiled programs in an LRU.
//
// Whitelist (per plan-07-surveys.md §11.3):
//
//   - bare `answer` identifier (legacy convenience for nodes that
//     reference the current node's own answer).
//   - `q<id>.value` and `q<id>.answered` member access where <id>
//     matches the survey-1.0.json node-id grammar.
//   - any operator/literal expr-lang's standard parser supports.
//
// Forbidden:
//
//   - any other top-level identifier (`os`, `time`, `now`, `len`,
//     `print`, ...) or call (`time.Now()`, `os.Getenv("X")`, ...).
//   - member access deeper than one level beyond a q<id> identifier
//     (`q1.value.unsafe` → rejected).
//
// All methods are safe for concurrent use.
type RealEvaluator struct {
	cache *programCache
}

// Compile-time interface checks: RealEvaluator implements
// [Evaluator]; both StubEvaluator and RealEvaluator can sit behind
// the same slot. The schema validator binds to the interface so the
// drop-in swap doesn't touch its constructor.
var _ Evaluator = (*RealEvaluator)(nil)

// NewRealEvaluator returns a production evaluator. cacheSize is the
// LRU capacity for the compiled-program cache; zero or negative
// values default to 1024 (sufficient for any reasonable survey-set
// of in-flight predicates without eviction churn).
func NewRealEvaluator(cacheSize int) *RealEvaluator {
	return &RealEvaluator{cache: newProgramCache(cacheSize)}
}

// nodeIDRE pins the q<id> grammar to the same character class
// survey-1.0.json's node-id pattern uses. The leading `q` is
// required (the whitelist is q-prefixed by spec); a future migration
// to a different prefix would update the regex and the doc-string.
var nodeIDRE = regexp.MustCompile(`^q[a-zA-Z0-9_-]+$`)

// allowedMemberProps enumerates the property names a q<id>.<prop>
// reference may use. Anything else is rejected as out-of-whitelist.
var allowedMemberProps = map[string]struct{}{
	"value":    {},
	"answered": {},
}

// allowedTopIdents enumerates the bare identifier forms that don't
// require a q<id> prefix. Currently only `answer` is allowed (legacy
// shorthand the schema validator already accepts).
var allowedTopIdents = map[string]struct{}{
	"answer": {},
}

// ParseAndCheck implements [Evaluator.ParseAndCheck].
//
// Algorithm:
//  1. Trim; reject empty.
//  2. Parse with expr-lang's parser (no full Compile — we don't
//     need the bytecode at this stage; checking the AST is enough).
//  3. Walk the AST: every IdentifierNode and MemberNode that names a
//     top-level binding MUST satisfy the whitelist; otherwise
//     return [ErrDSLParse] wrapped with the offending identifier.
//  4. Compile fully (with [expr.AsBool] when allowedIdents indicates
//     a boolean context — for now we don't pin that; runtime callers
//     can pin it via Eval) and cache.
//
// allowedIdents narrows the whitelist further: when non-nil, only
// the q<id>s listed are accepted (this is the path the
// schemavalidator forward-ref pass uses to pre-restrict identifiers
// to "those reachable from the current node"). Nil leaves the
// whitelist at the default `q<id>` grammar match.
func (e *RealEvaluator) ParseAndCheck(ctx context.Context, expression string, allowedIdents []string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dsl: %w", err)
	}
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return fmt.Errorf("%w: empty expression", ErrDSLParse)
	}

	// Parse-only first. expr-lang's parser surfaces syntax errors
	// without engaging the type-checker; that's what we want at this
	// stage because we haven't given it an env type.
	tree, parseErr := parser.Parse(trimmed)
	if parseErr != nil {
		return fmt.Errorf("%w: %s", ErrDSLParse, parseErr.Error())
	}

	allowSet := buildAllowedSet(allowedIdents)
	// Pre-pass: collect IdentifierNodes that are the receiver of a
	// MemberNode (e.g. `q1` in `q1.value`). expr-lang's ast.Walk
	// visits children before parents, so the IdentifierNode is
	// reported before its enclosing MemberNode. Without this pre-
	// pass we'd reject `q1` standalone before getting a chance to
	// see it's actually `q1.value`. The visitor consults the
	// receivers map and skips identifiers it knows belong to a
	// MemberNode chain (the MemberNode visit handles them).
	receivers := collectMemberReceivers(&tree.Node)
	v := &whitelistVisitor{
		allowedQIDs:     allowSet,
		memberReceivers: receivers,
	}
	ast.Walk(&tree.Node, v)
	if v.err != nil {
		return v.err
	}

	// Compile fully and cache. expr-lang's compile step also runs
	// the optimiser and type-checker; failures here imply a deeper
	// semantic problem that the AST walk didn't catch.
	prog, compileErr := expr.Compile(trimmed)
	if compileErr != nil {
		return fmt.Errorf("%w: %s", ErrDSLParse, compileErr.Error())
	}
	e.cache.Put(trimmed, prog)
	return nil
}

// Eval compiles (with cache) and runs expression against env.
// Returns the typed result (typically bool for branch predicates) or
// [ErrDSLParse] on compile failure / [ErrDSLEval] on runtime
// failure.
//
// env is the same map shape [BuildEnv] produces. Calling Eval with a
// nil env on an expression that references q<id> identifiers will
// return [ErrDSLEval] because expr-lang fails the lookup at runtime.
func (e *RealEvaluator) Eval(ctx context.Context, expression string, env map[string]any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("dsl: %w", err)
	}
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrDSLParse)
	}

	prog, err := e.compileForEval(ctx, trimmed)
	if err != nil {
		return nil, err
	}

	out, runErr := expr.Run(prog, env)
	if runErr != nil {
		return nil, fmt.Errorf("%w: %s", ErrDSLEval, runErr.Error())
	}
	return out, nil
}

// CacheLen returns the number of compiled programs currently stored
// in the LRU. Exposed for tests; production callers don't need it.
func (e *RealEvaluator) CacheLen() int {
	return e.cache.Len()
}

// compileForEval reuses a cached program when present and otherwise
// runs the full ParseAndCheck → Compile → cache pipeline. The
// no-allowedIdents call from Eval keeps the whitelist at the default
// q<id> grammar (Eval is the runtime path; the schemavalidator owns
// the narrower per-edge identifier list).
func (e *RealEvaluator) compileForEval(ctx context.Context, expression string) (*vm.Program, error) {
	if prog, ok := e.cache.Get(expression); ok {
		return prog, nil
	}
	if err := e.ParseAndCheck(ctx, expression, nil); err != nil {
		return nil, err
	}
	// ParseAndCheck cached the program on success.
	prog, ok := e.cache.Get(expression)
	if !ok {
		// Should never happen: ParseAndCheck just put it. Guard
		// against an LRU-cap=0 misconfiguration that would silently
		// prevent caching. We re-Compile in that case so the runtime
		// still moves forward.
		var compileErr error
		prog, compileErr = expr.Compile(expression)
		if compileErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrDSLParse, compileErr.Error())
		}
	}
	return prog, nil
}

// collectMemberReceivers walks the tree once and collects the
// IdentifierNodes that appear as the receiver (Node field) of a
// MemberNode. These identifiers are deliberately skipped by the
// main whitelist walk because the MemberNode-level check already
// validates them as part of the q<id>.<prop> shape; without
// skipping, the bare-identifier branch would reject `q1` before we
// could see it's `q1.value`.
//
// The function uses a separate visitor so the main walk stays
// linear and side-effect-free in its happy path.
func collectMemberReceivers(root *ast.Node) map[*ast.IdentifierNode]struct{} {
	out := make(map[*ast.IdentifierNode]struct{})
	v := &memberReceiverCollector{out: out}
	ast.Walk(root, v)
	return out
}

// memberReceiverCollector implements ast.Visitor and records the
// IdentifierNode receiver of every MemberNode it encounters.
type memberReceiverCollector struct {
	out map[*ast.IdentifierNode]struct{}
}

// Visit implements ast.Visitor.Visit.
func (c *memberReceiverCollector) Visit(node *ast.Node) {
	mn, ok := (*node).(*ast.MemberNode)
	if !ok {
		return
	}
	if id, isIdent := mn.Node.(*ast.IdentifierNode); isIdent {
		c.out[id] = struct{}{}
	}
}

// buildAllowedSet projects the allowedIdents slice into a quick-
// lookup map; nil → nil (signal: any q<id> grammar match is OK).
func buildAllowedSet(allowed []string) map[string]struct{} {
	if allowed == nil {
		return nil
	}
	out := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		out[id] = struct{}{}
	}
	return out
}

// whitelistVisitor walks an expr-lang AST, recording the first
// identifier-related violation in `err`. The visitor doesn't short-
// circuit (ast.Walk has no return-false hook), so subsequent visits
// are no-ops once err is non-nil — fine, because the first
// rejection is the user-actionable one.
type whitelistVisitor struct {
	// allowedQIDs is non-nil iff the caller pinned a specific id set;
	// nil means any q<id>-grammar match is OK.
	allowedQIDs map[string]struct{}
	// memberReceivers contains the IdentifierNodes that are the
	// receiver of a MemberNode in the same tree (pre-collected by
	// collectMemberReceivers). The visitor skips these to avoid
	// rejecting the bare `q1` identifier in a `q1.value` chain
	// before the MemberNode is visited.
	memberReceivers map[*ast.IdentifierNode]struct{}
	err             error
}

// Visit implements ast.Visitor.Visit. The visitor is invoked once per
// node post-walk (see expr-lang/ast/visitor.go); we only inspect
// IdentifierNode (top-level identifiers) and MemberNode (the
// q<id>.<prop> shape). Everything else is structural and per-spec
// safe in expr-lang's standard surface (operators, literals, slices,
// arrays, ...).
func (v *whitelistVisitor) Visit(node *ast.Node) {
	if v.err != nil {
		return
	}
	switch n := (*node).(type) {
	case *ast.IdentifierNode:
		if _, isReceiver := v.memberReceivers[n]; isReceiver {
			// The MemberNode visit owns whitelist enforcement for
			// this identifier; skip the bare-identifier check.
			return
		}
		v.checkIdentifier(n)
	case *ast.MemberNode:
		v.checkMember(n)
	case *ast.CallNode:
		// All function calls are forbidden — the whitelist exposes no
		// functions. expr-lang's builtins (`len`, `now`, `print`...)
		// reach this branch as CallNode wrappers around a bare
		// IdentifierNode callee; the IdentifierNode branch will also
		// reject them, but we surface the call-context message first
		// for clarity.
		v.err = fmt.Errorf("%w: function calls are not allowed", ErrDSLParse)
	case *ast.BuiltinNode:
		v.err = fmt.Errorf("%w: builtin functions are not allowed", ErrDSLParse)
	case *ast.PointerNode:
		// Pointer references (#) are part of expr-lang's predicate
		// shorthand — disallow because we don't expose any
		// collection-iteration intrinsics.
		v.err = fmt.Errorf("%w: pointer references are not allowed", ErrDSLParse)
	case *ast.PredicateNode:
		v.err = fmt.Errorf("%w: predicate expressions are not allowed", ErrDSLParse)
	case *ast.VariableDeclaratorNode:
		v.err = fmt.Errorf("%w: variable declarations are not allowed", ErrDSLParse)
	}
}

// checkIdentifier validates a top-level identifier. It rejects bare
// references that don't appear in allowedTopIdents — the q<id>.value
// shape is checked at the MemberNode level so a bare `q1` identifier
// (no `.value` / `.answered`) is also rejected here.
func (v *whitelistVisitor) checkIdentifier(n *ast.IdentifierNode) {
	if _, ok := allowedTopIdents[n.Value]; ok {
		return
	}
	// q<id> bare references are intentionally rejected — the
	// whitelist requires `.value` or `.answered` for every q-prefixed
	// id. This avoids accidentally exposing the whole map under a
	// future `expr.Env(map[string]any)` wiring.
	if nodeIDRE.MatchString(n.Value) {
		v.err = fmt.Errorf("%w: bare q-identifier %q must be accessed via .value or .answered", ErrDSLParse, n.Value)
		return
	}
	v.err = fmt.Errorf("%w: identifier %q is not in the whitelist", ErrDSLParse, n.Value)
}

// checkMember validates the q<id>.<prop> form. The receiver MUST be
// a bare IdentifierNode whose Value matches the q-grammar (and the
// allowedIdents pin if supplied), and Property MUST be a StringNode
// whose Value is one of "value" / "answered".
//
// expr-lang represents `a.b` as MemberNode{Node: a, Property: "b"}
// where Property is a *ast.StringNode for property access (not a
// computed index). MemberNode is also emitted for nested chains
// (`a.b.c` → MemberNode{Node: MemberNode{Node:a, P:"b"}, P:"c"}); we
// reject the nested form because the whitelist caps at one level.
func (v *whitelistVisitor) checkMember(n *ast.MemberNode) {
	prop, isString := n.Property.(*ast.StringNode)
	if !isString {
		v.err = fmt.Errorf("%w: computed member access is not allowed", ErrDSLParse)
		return
	}
	receiver, isIdent := n.Node.(*ast.IdentifierNode)
	if !isIdent {
		// e.g. `q1.value.x` — Node is a MemberNode, not an
		// IdentifierNode. Reject (one level of access only).
		v.err = fmt.Errorf("%w: nested member access is not allowed", ErrDSLParse)
		return
	}
	// `answer.<prop>` legacy shorthand: accept any prop name (the
	// runtime decides what to expose under `answer`).
	if _, ok := allowedTopIdents[receiver.Value]; ok {
		return
	}
	if !nodeIDRE.MatchString(receiver.Value) {
		v.err = fmt.Errorf("%w: identifier %q is not in the whitelist", ErrDSLParse, receiver.Value)
		return
	}
	if v.allowedQIDs != nil {
		if _, allowed := v.allowedQIDs[receiver.Value]; !allowed {
			v.err = fmt.Errorf("%w: identifier %q is not in the allowed-idents list", ErrDSLParse, receiver.Value)
			return
		}
	}
	if _, ok := allowedMemberProps[prop.Value]; !ok {
		v.err = fmt.Errorf("%w: property %q is not allowed (only .value / .answered)", ErrDSLParse, prop.Value)
	}
}
