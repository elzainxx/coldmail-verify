// Command migrate applies module 1's migrations (control-plane/migrations/NNNN_*.sql) exactly once each and in order, and records them in
// verify.schema_migrations. See internal/migrate for the algorithm and control-plane/migrations/README.md for the operator's view.
//
//	migrate up       apply whatever is unapplied (adopting what the old psql loop already ran)
//	migrate status   print one line per file: NNNN applied|adopted|pending <checksum8>
//
// Configuration (environment only; no flag takes a credential, so none can reach `ps`):
//
//	TM_MIGRATE_DATABASE_URL   the MIGRATING identity's DSN: a superuser on stock Postgres, `postgres` on Supabase. NOT the runtime verify_app, which has
//	                          no privilege on the tracker. Needs a direct or session-mode connection (the run holds a session-level advisory lock).
//	TM_MIGRATIONS_DIR         the directory holding NNNN_*.sql; default `migrations` (relative to the working directory)
//
// Exit codes: 0 success; 1 an error (connection, a migration's SQL, the lock wait); 2 a refusal (the runner looked and changed nothing); 64 usage.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/coldmail-verify/control-plane/internal/migrate"
)

const (
	envDSN = "TM_MIGRATE_DATABASE_URL"
	envDir = "TM_MIGRATIONS_DIR"

	exitOK      = 0
	exitError   = 1
	exitRefused = 2
	exitUsage   = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process: arguments, environment and the two output streams are parameters so a test can drive it.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		fmt.Fprintf(stderr, "usage: migrate up|status\n  %s  the migrating identity's DSN (required)\n  %s  directory of NNNN_*.sql (default: migrations)\n", envDSN, envDir)
		return exitUsage
	}
	dsn := getenv(envDSN)
	if dsn == "" {
		fmt.Fprintf(stderr, "migrate: %s is not set (the DSN of the migrating identity, not the runtime verify_app)\n", envDSN)
		return exitUsage
	}
	dir := getenv(envDir)
	if dir == "" {
		dir = "migrations"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := migrate.Config{DSN: dsn, Dir: dir, Out: stdout}

	switch args[0] {
	case "up":
		res, err := migrate.Up(ctx, cfg)
		if err != nil {
			code := report(stderr, err)
			if code == exitError && res.Applied+res.Adopted > 0 {
				// What committed before the failure stays committed and recorded; the next run resumes after it.
				fmt.Fprintf(stderr, "migrate: stopped by the error above after applying %d and adopting %d (those stay committed and recorded)\n", res.Applied, res.Adopted)
			}
			return code
		}
		fmt.Fprintln(stdout, res.Summary())
		return exitOK
	default: // status
		lines, err := migrate.Status(ctx, cfg)
		for _, l := range lines {
			fmt.Fprintln(stdout, l.String())
		}
		if err != nil {
			return report(stderr, err)
		}
		return exitOK
	}
}

// report prints an error (never a DSN: internal/migrate scrubs them at the source) and picks the exit code.
func report(stderr io.Writer, err error) int {
	var refusal *migrate.RefusalError
	if errors.As(err, &refusal) {
		fmt.Fprintf(stderr, "migrate: %v\nmigrate: nothing was changed.\n", err)
		return exitRefused
	}
	fmt.Fprintf(stderr, "migrate: %v\n", err)
	return exitError
}
