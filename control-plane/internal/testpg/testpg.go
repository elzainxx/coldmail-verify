// Package testpg gives Postgres-backed tests a THROWAWAY, fully migrated database, plus the catalog
// readers needed to assert on verification_raw's partitions.
//
// Why a scratch database and not the shared TM_TEST_DATABASE_URL one: partition tests must be able to
// age the table (drop partitions), break it on purpose (the legacy 0001-only state) and race it, none
// of which is safe against a database other packages are inserting into in parallel. Every call to
// Scratch creates a database nobody else can see and drops it on cleanup.
//
// It needs a superuser DSN (CI's is `postgres`) because it runs CREATE DATABASE. Like every PG-gated
// test here it SKIPS HONESTLY when TM_TEST_DATABASE_URL is unset; once the DSN is set, any other
// failure is a Fatal, never a skip (a green that did not execute is the failure mode this repo
// measures, test-strategy §4).
//
// ROLES ARE CLUSTER-WIDE. Migrations 0005 and 0016 create, rename, alter, comment on and drop roles
// (cp_runtime -> verify_app, cp_admin -> verify_admin, verify_owner), and a role is not per database. `go test ./...`
// runs the packages in parallel, so two Scratch calls applying migrations at once would race on the
// same pg_authid rows ("tuple concurrently updated") and could drop a role the other is about to grant
// to. Every call that applies migrations therefore holds one session-level pg_advisory_lock on its
// ADMIN connection (database `postgres`, so every caller contends on the same key) from before the first
// file until after the last, and a test that reads cluster-wide role state takes the same lock with
// LockMigrations. Never call Scratch or Apply while holding LockMigrations in the same test: they would
// wait on each other.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrationLockKey is the pg_advisory_lock key held while migrations are applied (see the package comment). It is an arbitrary constant:
// advisory locks are scoped to the database they are taken in, and every holder connects to `postgres`.
const migrationLockKey int64 = 7016016016

// migrationLockWait bounds how long a caller waits for another caller's migrations; a holder that hangs fails the waiter instead of hanging the suite.
const migrationLockWait = 5 * time.Minute

func lockMigrations(ctx context.Context, ac *pgx.Conn) error {
	lctx, cancel := context.WithTimeout(ctx, migrationLockWait)
	defer cancel()
	if _, err := ac.Exec(lctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("pg_advisory_lock(migrations): %w", err)
	}
	return nil
}

func unlockMigrations(ctx context.Context, t testing.TB, ac *pgx.Conn) {
	t.Helper()
	var released bool
	if err := ac.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey).Scan(&released); err != nil || !released {
		t.Errorf("testpg: pg_advisory_unlock(migrations) released=%v err=%v", released, err)
	}
}

// LockMigrations takes the migration lock (see the package comment) on its own admin connection and returns the release function, which is also
// registered with t.Cleanup. Use it around reads of cluster-wide role state, so no other Scratch/Apply is half-way through renaming a role.
func LockMigrations(t testing.TB) (unlock func()) {
	t.Helper()
	admin := AdminDSN(t)
	adminDB, err := withDB(admin, "postgres")
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	ctx := context.Background()
	ac, err := pgx.Connect(ctx, adminDB)
	if err != nil {
		t.Fatalf("testpg: connect admin: %v", err)
	}
	if err := lockMigrations(ctx, ac); err != nil {
		ac.Close(ctx)
		t.Fatalf("testpg: %v", err)
	}
	var once sync.Once
	unlock = func() {
		once.Do(func() {
			unlockMigrations(context.Background(), t, ac)
			ac.Close(context.Background())
		})
	}
	t.Cleanup(unlock)
	return unlock
}

// AdminDSN returns the superuser DSN from TM_TEST_DATABASE_URL, or skips the test.
func AdminDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a Postgres superuser DSN to exercise the real store (never faked)")
	}
	return dsn
}

// MigrationsDir is control-plane/migrations, located relative to this source file so it works from
// any test package's working directory.
func MigrationsDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "migrations")
}

func withDB(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + db
	return u.String(), nil
}

// Scratch creates a fresh database, applies migrations 0001..through (through <= 0 means ALL), and
// returns a DSN to it. The database is dropped on test cleanup.
func Scratch(t testing.TB, through int) string {
	t.Helper()
	return scratch(t, func(n int) bool { return through <= 0 || n <= through }, fmt.Sprintf("through=%d", through))
}

// ScratchSelect is Scratch with an arbitrary subset of migrations: it applies, in order, the ones for which include(number) is true. It exists for
// states that never occur in a real upgrade but that a test needs: after 0016 the store talks to schema verify only, so "the schema before 0014"
// is only reachable as 0001..0013 plus 0016.
func ScratchSelect(t testing.TB, include func(n int) bool) string {
	t.Helper()
	return scratch(t, include, "select")
}

func scratch(t testing.TB, include func(n int) bool, what string) string {
	t.Helper()
	admin := AdminDSN(t)
	ctx := context.Background()

	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("testpg: rand: %v", err)
	}
	name := "cm_scratch_" + hex.EncodeToString(b[:])

	adminDB, err := withDB(admin, "postgres")
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	ac, err := pgx.Connect(ctx, adminDB)
	if err != nil {
		t.Fatalf("testpg: connect admin: %v", err)
	}
	defer ac.Close(ctx)
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testpg: CREATE DATABASE (needs a superuser DSN): %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDB)
		if err != nil {
			t.Logf("testpg: cleanup connect: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("testpg: DROP DATABASE %s: %v", name, err)
		}
	})

	dsn, err := withDB(admin, name)
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("testpg: connect scratch: %v", err)
	}
	defer conn.Close(ctx)

	files, err := filepath.Glob(filepath.Join(MigrationsDir(), "[0-9][0-9][0-9][0-9]_*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("testpg: no migrations found in %s (err=%v)", MigrationsDir(), err)
	}
	sort.Strings(files)

	// Held on the admin connection from before the first file until after the last (see the package comment). The deferred ac.Close also
	// releases it if a Fatalf below unwinds this function.
	if err := lockMigrations(ctx, ac); err != nil {
		t.Fatalf("testpg: %v", err)
	}
	applied := 0
	for _, f := range files {
		n, _ := strconv.Atoi(filepath.Base(f)[:4])
		if !include(n) {
			continue
		}
		sqlText, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("testpg: read %s: %v", f, err)
		}
		// No arguments => pgx uses the simple protocol, so a multi-statement migration file is legal.
		if _, err := conn.Exec(ctx, string(sqlText)); err != nil {
			t.Fatalf("testpg: apply %s: %v", filepath.Base(f), err)
		}
		applied++
	}
	unlockMigrations(ctx, t, ac)
	if applied == 0 {
		t.Fatalf("testpg: applied 0 migrations (%s)", what)
	}
	return dsn
}

// SetRolePassword sets a cluster-wide role's password (TEST passwords only: the literal is quoted, not parameterised) while holding the migration
// lock, and registers a cleanup that clears it again under the same lock. A role is not per database, so this is a cluster-wide write and races
// every Scratch that renames, alters or comments on roles ("tuple concurrently updated"); the lock is what prevents that. The ALTER runs on the
// ADMIN connection (database `postgres`), which is the one holding the lock.
func SetRolePassword(t testing.TB, role, password string) {
	t.Helper()
	admin := AdminDSN(t)
	if err := alterRolePassword(admin, role, password); err != nil {
		t.Fatalf("testpg: set password on role %s: %v", role, err)
	}
	t.Cleanup(func() {
		if err := alterRolePassword(admin, role, ""); err != nil {
			t.Logf("testpg: clear password on role %s: %v", role, err)
		}
	})
}

func alterRolePassword(admin, role, password string) error {
	ctx := context.Background()
	adminDB, err := withDB(admin, "postgres")
	if err != nil {
		return err
	}
	ac, err := pgx.Connect(ctx, adminDB)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer ac.Close(ctx)
	if err := lockMigrations(ctx, ac); err != nil {
		return err
	}
	lit := "NULL"
	if password != "" {
		lit = "'" + strings.ReplaceAll(password, "'", "''") + "'"
	}
	if _, err := ac.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD "+lit); err != nil {
		return err
	}
	_, err = ac.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	return err
}

// Apply runs migration number n (e.g. 16) against the database at dsn, exactly as Scratch does (simple protocol, one implicit transaction),
// under the same migration lock. It is how a test snapshots a database at 0015 and then applies 0016 itself.
func Apply(t testing.TB, dsn string, n int) {
	t.Helper()
	admin := AdminDSN(t)
	ctx := context.Background()
	matches, err := filepath.Glob(filepath.Join(MigrationsDir(), fmt.Sprintf("%04d_*.sql", n)))
	if err != nil || len(matches) != 1 {
		t.Fatalf("testpg: migration %04d: want exactly one file in %s, got %v (err=%v)", n, MigrationsDir(), matches, err)
	}
	sqlText, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("testpg: read %s: %v", matches[0], err)
	}
	adminDB, err := withDB(admin, "postgres")
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	ac, err := pgx.Connect(ctx, adminDB)
	if err != nil {
		t.Fatalf("testpg: connect admin: %v", err)
	}
	defer ac.Close(ctx)
	if err := lockMigrations(ctx, ac); err != nil {
		t.Fatalf("testpg: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("testpg: connect to apply migration %04d: %v", n, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, string(sqlText)); err != nil {
		t.Fatalf("testpg: apply %s: %v", filepath.Base(matches[0]), err)
	}
	unlockMigrations(ctx, t, ac)
}

// Part is one partition of verification_raw: its name and the instants it covers, [From, To).
type Part struct {
	Name     string
	From, To time.Time
}

var boundRE = regexp.MustCompile(`FROM \('([^']+)'\) TO \('([^']+)'\)`)

// Parts reads verification_raw's partitions from the catalog. The session is pinned to UTC so the
// bound text is unambiguous.
func Parts(ctx context.Context, dsn string) ([]Part, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET TIME ZONE 'UTC'"); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'verify.verification_raw'::regclass
		ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var name, expr string
		if err := rows.Scan(&name, &expr); err != nil {
			return nil, err
		}
		m := boundRE.FindStringSubmatch(expr)
		if m == nil {
			return nil, fmt.Errorf("partition %s: cannot parse bound %q", name, expr)
		}
		from, err1 := time.Parse("2006-01-02 15:04:05-07", m[1])
		to, err2 := time.Parse("2006-01-02 15:04:05-07", m[2])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("partition %s: bad timestamps in %q", name, expr)
		}
		out = append(out, Part{Name: name, From: from.UTC(), To: to.UTC()})
	}
	return out, rows.Err()
}

// MonthStart is the first instant of t's month, in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// HeadroomError is the CLIFF GUARD. It returns nil iff, for every month from the one containing `now`
// through `now`'s month + n, there is a partition whose bounds are EXACTLY that UTC calendar month. It
// returns an error naming each missing month otherwise. Returning an error (rather than failing a
// *testing.T) is deliberate: it lets a test prove the guard is not vacuous by handing it a state that
// must fail it.
func HeadroomError(parts []Part, now time.Time, n int) error {
	have := map[time.Time]Part{}
	for _, p := range parts {
		have[p.From] = p
	}
	var missing []string
	for i := 0; i <= n; i++ {
		m := MonthStart(now).AddDate(0, i, 0)
		next := m.AddDate(0, 1, 0)
		if p, ok := have[m]; !ok || !p.To.Equal(next) {
			missing = append(missing, m.Format("2006-01"))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("verification_raw has no exact-month partition for %s (need this month + %d ahead, as of %s): inserts dated then will fail with SQLSTATE 23514",
			strings.Join(missing, ", "), n, now.UTC().Format("2006-01-02"))
	}
	return nil
}
