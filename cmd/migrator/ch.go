// Package main — ClickHouse target support for the migrator.
//
// The migrator binary applies golang-migrate migration sets to two
// possible targets:
//
//   - postgres  (default) — DATABASE_URL + MIGRATIONS_PATH env vars.
//   - clickhouse           — CLICKHOUSE_DSN + CLICKHOUSE_MIGRATIONS_PATH.
//
// Selection is driven by a --target=<name> CLI flag (default postgres
// for backward compat). Each target uses its own env-var pair so a
// single Helm Job can reuse the same binary with different args/env.
//
// ClickHouse-specific blank imports register the migrate driver and
// the database/sql driver; both are init()-only.
package main

import (
	// Blank import: registers the "clickhouse" SQL driver name with
	// database/sql. Required transitively by golang-migrate's clickhouse
	// driver (it opens *sql.DB internally). Init()-only — no exported
	// symbols we use directly.
	_ "github.com/ClickHouse/clickhouse-go/v2"

	// Blank import: registers golang-migrate's "clickhouse" database
	// driver. DSNs in the form clickhouse://host:port?database=...
	// are routed through this driver. Init()-only, no exported symbols
	// we use directly. Required by revive's blank-imports rule.
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse"
)

// ClickHouse-specific env var names. These are deliberately distinct
// from DATABASE_URL / MIGRATIONS_PATH so a misconfigured deployment
// (forgetting --target=clickhouse but setting CLICKHOUSE_DSN) fails
// loudly with "DATABASE_URL is empty" rather than silently mismatching.
const (
	envClickHouseDSN  = "CLICKHOUSE_DSN"
	envClickHousePath = "CLICKHOUSE_MIGRATIONS_PATH"

	defaultClickHouseMigrationsPath = "file:///etc/sociopulse/migrations/clickhouse"
)

const (
	targetPostgres   = "postgres"
	targetClickHouse = "clickhouse"
	flagTargetPrefix = "--target="
)

// resolveTarget extracts the --target=<name> flag from args (if
// present) and returns the target plus the leftover args (without
// the flag). The flag may appear before or after the sub-command.
// Default target is "postgres" (preserves backward-compat with the
// pre-flag Postgres-only invocation).
//
// Unknown target values return a *usageError so main() routes them
// to exit code 1.
func resolveTarget(args []string) (string, []string, error) {
	target := targetPostgres
	leftover := make([]string, 0, len(args))
	for _, a := range args {
		if v, ok := parseFlag(a, flagTargetPrefix); ok {
			switch v {
			case targetPostgres, targetClickHouse:
				target = v
			default:
				return "", nil, &usageError{msg: "unknown --target value: " + v + " (must be postgres or clickhouse)"}
			}
			continue
		}
		leftover = append(leftover, a)
	}
	return target, leftover, nil
}
