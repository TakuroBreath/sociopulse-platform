// Package main is the СоциоПульс database migration runner.
//
// Usage:
//
//	migrator up                          # apply all pending up migrations
//	migrator down                        # revert all migrations (dev/test)
//	migrator down --steps=1              # revert exactly one step
//	migrator status                      # print current version and dirty flag
//	migrator force <version>             # set version + clear dirty (recover)
//
// Environment:
//
//	DATABASE_URL       Postgres connection string (required)
//	MIGRATIONS_PATH    File URL pointing to the migrations directory
//	                   (default: file:///etc/sociopulse/migrations).
//
// Exit codes:
//
//	0  success
//	1  flag/usage error
//	2  migration error (includes connection error)
//
// The binary is intentionally tiny: it does only what `golang-migrate`
// supports out of the box, plus structured zap logging suitable for k8s.
//
// Design note: this binary uses zap directly per ADR-0012 instead of the
// project-wide observability package, because the migrator is a Kubernetes
// one-shot Job and pulling in pkg/config + the redaction encoder would add
// far more weight than its single responsibility justifies.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	// Blank import: registers golang-migrate's "postgres" database driver
	// (lib/pq-based). DSNs in the form postgres://user:pass@host/db?sslmode=…
	// are handled by this driver. The driver lives entirely in init() — no
	// exported symbols we use directly. Required by revive's blank-imports rule.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	// Blank import: pgx/v5 driver registers under the "pgx5" scheme. Kept in
	// case operators want to opt in via DSN scheme like pgx5://… though we
	// default to postgres:// for compatibility with golang-migrate examples.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	// Blank import: registers the file:// source driver. Same init()-only
	// registration pattern as the database driver above.
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"go.uber.org/zap"
)

const defaultMigrationsPath = "file:///etc/sociopulse/migrations"

// usageError signals that the caller invoked the CLI with bad arguments.
// main() routes these to exit code 1; everything else exits 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

const usageText = `usage: migrator <up|down|status|force> [args]

Subcommands:
  up                   apply all pending migrations
  down                 revert all migrations (dev/test only)
  down --steps=N       revert exactly N steps
  status               print current version and dirty flag
  force <version>      set version + clear dirty flag (manual recovery)

Environment:
  DATABASE_URL         Postgres DSN (required)
  MIGRATIONS_PATH      file:// URL of the migrations directory
                       (default: file:///etc/sociopulse/migrations)

Exit codes:
  0   success
  1   usage error (bad argv, empty DSN)
  2   migration or connection error
`

func main() {
	// --help / -h should print usage cleanly without spinning up zap or
	// touching DATABASE_URL. We special-case before any other work.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Fprint(os.Stdout, usageText)
			return
		}
	}

	dsn := os.Getenv("DATABASE_URL")
	migPath := os.Getenv("MIGRATIONS_PATH")
	if migPath == "" {
		migPath = defaultMigrationsPath
	}

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init zap: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = logger.Sync() }()

	if err := run(os.Args[1:], dsn, migPath, os.Stdout); err != nil {
		var ue *usageError
		if errors.As(err, &ue) {
			logger.Error("migrator usage error", zap.Error(err))
			fmt.Fprint(os.Stderr, usageText)
			os.Exit(1)
		}
		logger.Error("migrator failed", zap.Error(err))
		os.Exit(2)
	}
}

// run is the testable entrypoint. args is the slice without the program name.
//
// Argument-validation errors return *usageError so main() can route them to
// exit code 1. Migration / connection errors return wrapped fmt.Errorf chains
// so callers can errors.Is/errors.As against migrate.ErrNoChange,
// migrate.ErrNilVersion, etc.
func run(args []string, dsn, migrationsPath string, stdout io.Writer) error {
	if dsn == "" {
		return &usageError{msg: "DATABASE_URL is empty"}
	}
	if len(args) == 0 {
		return &usageError{msg: "no subcommand"}
	}

	// Validate sub-command-specific args BEFORE opening a connection so that
	// pure usage errors don't get masked by a migrate.New filesystem or
	// network failure. This makes the exit-code contract crisp:
	//   - exit 1 = the user's fault (bad argv)
	//   - exit 2 = anything else
	if err := validateArgs(args); err != nil {
		return err
	}

	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer func() {
		// migrate.Close returns (sourceErr, dbErr); ignore both — the
		// primary error from the operation has already been returned.
		_, _ = m.Close()
	}()

	return dispatch(m, args, stdout)
}

// validateArgs performs all argv-level checks that must produce *usageError
// (exit code 1). Anything that requires an open migrate.Migrate handle is
// deferred to dispatch().
func validateArgs(args []string) error {
	switch args[0] {
	case "up", "status", "down":
		// down's --steps=N is validated in-line below
	case "force":
		if len(args) < 2 {
			return &usageError{msg: "force requires <version>"}
		}
		if _, err := strconv.Atoi(args[1]); err != nil {
			return &usageError{msg: "force: version must be an integer"}
		}
	default:
		return &usageError{msg: "unknown subcommand: " + args[0]}
	}

	if args[0] == "down" {
		for _, a := range args[1:] {
			if v, ok := parseFlag(a, "--steps="); ok {
				n, err := strconv.Atoi(v)
				if err != nil || n <= 0 {
					return &usageError{msg: "invalid --steps value: " + v}
				}
			}
		}
	}
	return nil
}

// dispatch routes to the per-sub-command migrate.Migrate operation. Argv has
// already been validated by validateArgs at this point, so the type-asserted
// strconv.Atoi calls below cannot fail.
func dispatch(m *migrate.Migrate, args []string, stdout io.Writer) error {
	switch args[0] {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("up: %w", err)
		}
		return printVersion(m, stdout, "applied")

	case "down":
		return runDown(m, args[1:], stdout)

	case "status":
		return printVersion(m, stdout, "current")

	case "force":
		v, _ := strconv.Atoi(args[1])
		if err := m.Force(v); err != nil {
			return fmt.Errorf("force %d: %w", v, err)
		}
		return printVersion(m, stdout, "forced")

	default:
		// Unreachable: validateArgs already filtered unknown sub-commands.
		return &usageError{msg: "unknown subcommand: " + args[0]}
	}
}

// runDown handles the `down` sub-command, optionally honouring --steps=N.
func runDown(m *migrate.Migrate, args []string, stdout io.Writer) error {
	steps := 0
	for _, a := range args {
		if v, ok := parseFlag(a, "--steps="); ok {
			// validateArgs ensured v is a positive integer.
			n, _ := strconv.Atoi(v)
			steps = n
		}
	}
	if steps > 0 {
		if err := m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("down %d steps: %w", steps, err)
		}
	} else {
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("down all: %w", err)
		}
	}
	return printVersion(m, stdout, "reverted")
}

// printVersion writes a one-line status to w. The format is intentionally
// machine-grep-friendly: "<prefix>: version=<N|none> dirty=<bool>".
func printVersion(m *migrate.Migrate, w io.Writer, prefix string) error {
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		fmt.Fprintf(w, "%s: version=none dirty=false\n", prefix)
		return nil
	}
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	fmt.Fprintf(w, "%s: version=%d dirty=%t\n", prefix, v, dirty)
	return nil
}

// parseFlag returns (value, true) if arg is exactly "<prefix><value>", else
// ("", false). Used for the small "--steps=N" flag on `down`.
func parseFlag(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) || arg[:len(prefix)] != prefix {
		return "", false
	}
	return arg[len(prefix):], true
}
