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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	applied := 0
	for _, f := range files {
		n, _ := strconv.Atoi(filepath.Base(f)[:4])
		if through > 0 && n > through {
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
	if applied == 0 {
		t.Fatalf("testpg: applied 0 migrations (through=%d)", through)
	}
	return dsn
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
		WHERE i.inhparent = 'public.verification_raw'::regclass
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
