package dsl

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrDSLParse is the sentinel returned when an expression is malformed.
// Callers wrap this with %w when surfacing field-pathed validation
// issues so errors.Is(err, ErrDSLParse) keeps matching after wrapping.
var ErrDSLParse = errors.New("dsl: parse error")

// Evaluator is the conditional-DSL surface the schema validator depends
// on. The Plan 07 Task 2 implementation is a stub ([StubEvaluator]) that
// only catches obviously malformed inputs (empty string after trimming,
// unmatched parens) so the rest of the validation pipeline can be
// written and tested against real fixtures. Task 3 will replace the
// stub with an expr-lang/expr-backed implementation that whitelists
// identifiers and exposes the AST.
//
// All methods MUST be safe to call concurrently from multiple
// goroutines.
type Evaluator interface {
	// ParseAndCheck verifies that expression is syntactically valid
	// and that every identifier it references is in allowedIdents (or
	// matches the q<id>.value / q<id>.answered grammar that the real
	// DSL knows about). The stub ignores allowedIdents entirely; the
	// real implementation in Task 3 will use it.
	//
	// Returns nil on acceptance, or an error wrapping [ErrDSLParse] on
	// rejection so callers can errors.Is(err, ErrDSLParse).
	ParseAndCheck(ctx context.Context, expression string, allowedIdents []string) error
}

// StubEvaluator is the placeholder Plan 07 Task 2 ships. It accepts any
// non-empty expression whose parentheses balance and rejects everything
// else with [ErrDSLParse]. Concrete checks the stub catches:
//
//   - empty / whitespace-only expression (e.g. "   ")
//   - unmatched ')'  (e.g. "1) + 2")
//   - unmatched '('  (e.g. "((1 + 2)")
//
// What the stub does NOT catch (and Task 3 will): unbalanced quotes,
// invalid operators, identifiers outside the whitelist, type-checked
// references to unknown nodes. The bad-when fixture
// (`q1.value ===`) lands in this gap and is therefore expected to pass
// the stub; the related test skips it explicitly with a TODO referencing
// Task 3.
type StubEvaluator struct{}

// NewStubEvaluator returns the Task 2 placeholder.
func NewStubEvaluator() *StubEvaluator { return &StubEvaluator{} }

// Compile-time interface check.
var _ Evaluator = (*StubEvaluator)(nil)

// ParseAndCheck implements [Evaluator].
//
// ctx is accepted for forward-compatibility with the Task 3 evaluator
// (which will respect cancellation while parsing large expressions);
// the stub never blocks so it ignores ctx aside from a fast-fail
// against an already-cancelled context.
func (StubEvaluator) ParseAndCheck(ctx context.Context, expression string, _ []string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dsl: %w", err)
	}
	expr := strings.TrimSpace(expression)
	if expr == "" {
		return fmt.Errorf("%w: empty expression", ErrDSLParse)
	}
	depth := 0
	for _, r := range expr {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return fmt.Errorf("%w: unmatched ')'", ErrDSLParse)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%w: unmatched '('", ErrDSLParse)
	}
	return nil
}
