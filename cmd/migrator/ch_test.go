package main

import (
	"errors"
	"testing"
)

// TestResolveTarget_DefaultsToPostgres verifies that, with no --target flag,
// the migrator falls back to the existing Postgres path. This is the
// backward-compat guarantee for every pre-flag invocation.
func TestResolveTarget_DefaultsToPostgres(t *testing.T) {
	t.Parallel()

	target, rest, err := resolveTarget([]string{"up"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target != "postgres" {
		t.Fatalf("expected postgres, got %q", target)
	}
	if len(rest) != 1 || rest[0] != "up" {
		t.Fatalf("expected [up], got %v", rest)
	}
}

// TestResolveTarget_AcceptsClickHouse verifies the new --target=clickhouse
// flag routes to the CH path and is stripped from leftover args.
func TestResolveTarget_AcceptsClickHouse(t *testing.T) {
	t.Parallel()

	target, rest, err := resolveTarget([]string{"--target=clickhouse", "up"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target != "clickhouse" {
		t.Fatalf("expected clickhouse, got %q", target)
	}
	if len(rest) != 1 || rest[0] != "up" {
		t.Fatalf("expected [up], got %v", rest)
	}
}

// TestResolveTarget_RejectsUnknown verifies an unknown --target= value yields
// a *usageError so main() routes it to exit code 1 (user fault).
func TestResolveTarget_RejectsUnknown(t *testing.T) {
	t.Parallel()

	_, _, err := resolveTarget([]string{"--target=mysql", "up"})
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *usageError, got %v", err)
	}
}

// TestResolveTarget_FlagAfterSubcommand verifies the flag may appear AFTER
// the sub-command (operator-friendly: tab-completion of the verb first).
func TestResolveTarget_FlagAfterSubcommand(t *testing.T) {
	t.Parallel()

	target, rest, err := resolveTarget([]string{"up", "--target=clickhouse"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target != "clickhouse" {
		t.Fatalf("expected clickhouse, got %q", target)
	}
	if len(rest) != 1 || rest[0] != "up" {
		t.Fatalf("expected [up], got %v", rest)
	}
}
