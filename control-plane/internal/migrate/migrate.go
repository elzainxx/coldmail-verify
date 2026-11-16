// Package migrate is module 1's migration runner. It applies control-plane/migrations/NNNN_*.sql exactly once each and in order, and records what it did
// in verify.schema_migrations (created by migration 0017), so that:
//
//   - running the whole chain twice is a no-op (the second run applies nothing and changes no row);
//   - a database built from nothing and a database upgraded from an older state end in the same state;
//   - a migration file edited after it was applied, a database that is ahead of the checkout, and a history with a hole in it are REFUSED before a
//     single statement runs (nothing is changed on a refusal).
//
// Owner ruling (verbatim): "M1 migration tracker: add one so migrations run exactly once and in order. Running the full chain twice must be a no-op,
// and a fresh install must end in the same state as an upgraded one. Prove both on the runner. Keep 0016's refusal guard." (Docs/future-work.md FW-3.)
//
// THE ALGORITHM (Up). Every step that can refuse happens before any change.
//
//  1. Read NNNN_*.sql from the directory. The numbers must be a contiguous 0001.. sequence with no duplicate, no stray *.sql that does not match the
//     pattern (it would silently never run) and at least one file (a gate whose subject is empty is red). Each file is hashed (sha256) after CRLF->LF.
//  2. Take a SESSION-level pg_advisory_lock (LockKey) on a dedicated connection for the whole run. A second runner waits (bounded) and then fails with
//     a message that says so; it never runs a migration concurrently.
//  3. Read the state under the lock, then decide (buildPlan):
//     a. tracker table present with rows: refuse on (i) a recorded version with no file, (ii) a recorded checksum that differs from the file on disk,
//     (iii) recorded versions that are not a prefix of the file list (a hole). Otherwise apply each unrecorded file in order.
//     b. tracker table present but EMPTY: the old psql loop took this database through 0017 (it created the table and nothing recorded anything). Adopt:
//     record 0001..0017 as 'adopted' without running anything, then continue as (a). Never replay 0001..0016 here.
//     c. tracker table absent and verify.verifications present: the old loop ran 0016. Run 0017 and, in the same transaction, record 0001..0016 as
//     'adopted' and 0017 as 'applied'. Never replay 0001..0015 here: they are unqualified and would create empty tables in public (the F23 hazard).
//     d. tracker table absent and verify.verifications absent: an empty database, or one the old loop took to 0015 or earlier. Run every file in
//     order, each in its own transaction and without tracker rows (the table does not exist yet); in the same transaction as 0017 record 0001..0017
//     as 'applied'.
//     In (c) and (b) the runner also refuses if public.verifications exists next to verify.verifications: that is 0001..0015 re-run on a migrated database.
//  4. Every file runs in ITS OWN transaction together with its tracker row, so a file is either fully applied and recorded, or not at all. The first
//     error stops the run (the failed file's transaction rolls back; nothing after it runs). A re-run reports the same error and re-applies nothing.
//
// REPLAY SAFETY IN BRANCH (d). Replaying 0001..0015 on a database the old loop already took to 0015 is safe BY CONSTRUCTION: every CREATE in them is
// IF NOT EXISTS (tables, partitions, indexes), every ADD COLUMN is IF NOT EXISTS, the four widened CHECK constraints are DROP ... IF EXISTS followed by
// ADD (each constraint is rebuilt once, with its final definition), the two functions are CREATE OR REPLACE, the roles are created only if missing, and
// every GRANT / REVOKE / COMMENT is idempotent. The files run in the same order, so the grants and revokes land on the same final ACLs; no earlier file
// grants a privilege that a later one revokes (read from 0005..0015), so a replay never widens anything even between two files. It is NOT safe once 0016
// has run, which is why branches (b) and (c) never replay. Branch (d) is also what a crash before 0017 commits leaves behind: an untracked, replay-safe
// database. A crash after 0016 commits but before 0017 is picked up by branch (c) on the next run.
//
// The runner connects as the migrating identity (a superuser on stock Postgres, `postgres` on Supabase, which 0016 makes a member of verify_owner). It
// does not SET ROLE and it is not the runtime login verify_app, which has no privilege on the tracker.
package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// LockKey is the session-level pg_advisory_lock key every run holds for its whole duration. Advisory locks are scoped to the DATABASE they are taken
	// in, so this cannot collide with another database's runner. It is a constant of module 1 on purpose: the platform owns 7_xxx_xxx_xxx, module 3 8_xxx_xxx_xxx,
	// module 2 9_xxx_xxx_xxx, and module 1 is 6_000_000_001. (testpg's own cluster-wide lock, 7016016016, lives in the `postgres` database and is unrelated.)
	LockKey int64 = 6000000001

	// TrackerVersion is the migration that creates verify.schema_migrations. Everything before it ran without a tracker, so the runner records those
	// versions in the same transaction that applies this one.
	TrackerVersion = "0017"

	// DefaultLockWait bounds how long a second runner waits for the first.
	DefaultLockWait = 10 * time.Minute

	// AppName is the application_name of both runner connections; it is how an operator (and the tests) find a runner in pg_stat_activity.
	AppName = "tm-migrate"
)

// Config is one run's input.
type Config struct {
	DSN      string        // the MIGRATING identity (TM_MIGRATE_DATABASE_URL), never the runtime verify_app
	Dir      string        // the directory holding NNNN_*.sql (TM_MIGRATIONS_DIR)
	LockWait time.Duration // how long to wait for another runner; 0 means DefaultLockWait
	Out      io.Writer     // progress lines; nil discards them
}

// Result is what a run did. Applied counts files whose SQL the runner executed, Adopted counts rows it recorded without executing, AlreadyApplied
// counts files the tracker already held when the run started, Total is the number of migration files.
type Result struct {
	Applied, Adopted, AlreadyApplied, Total int
}

// Summary is the one-line report both `up` and the tests read.
func (r Result) Summary() string {
	return fmt.Sprintf("migrate: applied %d, adopted %d, already applied %d, total %d", r.Applied, r.Adopted, r.AlreadyApplied, r.Total)
}

// RefusalError means the runner looked, found the migrations directory or the database in a state it will not act on, and changed NOTHING.
type RefusalError struct{ Problems []string }

func (e *RefusalError) Error() string {
	if len(e.Problems) == 1 {
		return "refused: " + e.Problems[0]
	}
	return "refused:\n  - " + strings.Join(e.Problems, "\n  - ")
}

func refuse(format string, args ...any) error {
	return &RefusalError{Problems: []string{fmt.Sprintf(format, args...)}}
}

// LockTimeoutError means another runner held LockKey for the whole bounded wait. Nothing was changed.
type LockTimeoutError struct{ Wait time.Duration }

func (e *LockTimeoutError) Error() string {
	return fmt.Sprintf("another migration runner holds the advisory lock %d on this database and did not release it within %s: giving up, nothing was changed", LockKey, e.Wait)
}

// File is one migration file as the runner sees it.
type File struct {
	Version  string // "0017"
	Name     string // "0017_schema_migrations.sql"
	Checksum string // hex sha256 of SQL
	SQL      string // the file's text with CRLF normalised to LF: exactly what is hashed and exactly what is executed
}

// Short is the first eight hex digits of the checksum, as printed in messages and by `status`.
func (f File) Short() string { return short(f.Checksum) }

func short(sum string) string {
	if len(sum) > 8 {
		return sum[:8]
	}
	return sum
}

var fileRE = regexp.MustCompile(`^([0-9]{4})_[A-Za-z0-9][A-Za-z0-9_.-]*\.sql$`)

// LoadFiles reads, validates and hashes the migration files in dir. It returns a *RefusalError for a directory the runner will not act on: no files, a
// gap in the numbering, two files with one number, a *.sql whose name does not match NNNN_name.sql (it would never run), or a sequence that does not start at 0001.
func LoadFiles(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory %q: %w", dir, err)
	}
	byVersion := map[string][]string{}
	var stray []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			continue
		}
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil {
			stray = append(stray, e.Name())
			continue
		}
		byVersion[m[1]] = append(byVersion[m[1]], e.Name())
	}

	var problems []string
	if len(stray) > 0 {
		sort.Strings(stray)
		problems = append(problems, fmt.Sprintf("%s: not named NNNN_name.sql, so it would silently never run (rename it or move it out of %s)", strings.Join(stray, ", "), dir))
	}
	versions := make([]string, 0, len(byVersion))
	for v, names := range byVersion {
		versions = append(versions, v)
		if len(names) > 1 {
			sort.Strings(names)
			problems = append(problems, fmt.Sprintf("migration %s has %d files: %s (a number must be used once)", v, len(names), strings.Join(names, ", ")))
		}
	}
	sort.Strings(versions)
	if len(versions) == 0 && len(stray) == 0 {
		problems = append(problems, fmt.Sprintf("no migration files (NNNN_*.sql) in %s: a run over nothing proves nothing", dir))
	}
	for i, v := range versions {
		if want := fmt.Sprintf("%04d", i+1); v != want {
			problems = append(problems, fmt.Sprintf("migration %s is missing: the numbers must run 0001.. without a gap, and %s comes where %s is expected", want, v, want))
			break
		}
	}
	if len(problems) > 0 {
		return nil, &RefusalError{Problems: problems}
	}

	files := make([]File, 0, len(versions))
	for _, v := range versions {
		name := byVersion[v][0]
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		norm := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		sum := sha256.Sum256(norm)
		files = append(files, File{Version: v, Name: name, Checksum: hex.EncodeToString(sum[:]), SQL: string(norm)})
	}
	return files, nil
}

// row is one verify.schema_migrations row (without the volatile columns).
type row struct{ Version, Checksum, How string }

func rowsFor(files []File, how string) []row {
	out := make([]row, 0, len(files))
	for _, f := range files {
		out = append(out, row{Version: f.Version, Checksum: f.Checksum, How: how})
	}
	return out
}

// dbState is everything the plan depends on, read under the lock.
type dbState struct {
	TrackerExists       bool // verify.schema_migrations exists (0017 ran, by the runner or by the old loop)
	VerifyExists        bool // verify.verifications exists: 0016 ran
	PublicVerifications bool // public.verifications exists: 0001..0015 ran (or ran AGAIN after 0016)
	Rows                []row
}

// op is one transaction: optionally run a file, then insert tracker rows.
type op struct {
	File File
	Run  bool  // execute File.SQL (false = an adoption that only records)
	Rows []row // inserted in the same transaction, after the file
}

type plan struct {
	Ops            []op
	AlreadyApplied int
}

const clashMsg = "verify.verifications and public.verifications both exist: migrations 0001..0015 were re-run on a database that already ran 0016, which created empty module-1 tables in public. " +
	"Check that the public copy is empty, drop it, then re-run (0016 has its own guard with the same advice)"

// buildPlan is pure: it turns the files and the database state into the list of transactions to run, or refuses. It never touches a database.
func buildPlan(files []File, st dbState) (plan, error) {
	if len(files) == 0 {
		return plan{}, refuse("no migration files: a plan over nothing proves nothing")
	}
	idx := make(map[string]int, len(files))
	for i, f := range files {
		idx[f.Version] = i
	}
	ti, ok := idx[TrackerVersion]
	if !ok {
		return plan{}, refuse("the tracker migration %s_*.sql is not in the migrations directory (%d file(s), %s..%s): this checkout is older than the runner",
			TrackerVersion, len(files), files[0].Version, files[len(files)-1].Version)
	}
	clash := st.VerifyExists && st.PublicVerifications

	var p plan
	// rest appends every file after `from` as a normal, recorded, applied step.
	rest := func(from int) {
		for _, f := range files[from:] {
			p.Ops = append(p.Ops, op{File: f, Run: true, Rows: rowsFor([]File{f}, "applied")})
		}
	}

	switch {
	case st.TrackerExists && len(st.Rows) > 0: // (a) tracked
		recorded := make(map[string]row, len(st.Rows))
		var ahead, changed, holes []string
		for _, r := range st.Rows {
			i, known := idx[r.Version]
			if !known {
				ahead = append(ahead, fmt.Sprintf("migration %s is recorded in verify.schema_migrations but there is no file for it: the database is ahead of this checkout", r.Version))
				continue
			}
			if r.Checksum != files[i].Checksum {
				changed = append(changed, fmt.Sprintf("migration %s changed after it was applied: recorded %s, on disk %s", r.Version, short(r.Checksum), files[i].Short()))
			}
			recorded[r.Version] = r
		}
		var missing []File
		for _, f := range files {
			if _, done := recorded[f.Version]; !done {
				missing = append(missing, f)
				continue
			}
			for _, m := range missing {
				holes = append(holes, fmt.Sprintf("migration %s is not applied but a later one (%s) is", m.Version, f.Version))
			}
			missing = nil
		}
		if problems := append(append(ahead, changed...), holes...); len(problems) > 0 {
			return plan{}, &RefusalError{Problems: problems}
		}
		p.AlreadyApplied = len(recorded)
		for _, f := range files {
			if _, done := recorded[f.Version]; !done {
				p.Ops = append(p.Ops, op{File: f, Run: true, Rows: rowsFor([]File{f}, "applied")})
			}
		}

	case st.TrackerExists: // (b) the old loop created the table and recorded nothing
		if !st.VerifyExists {
			return plan{}, refuse("verify.schema_migrations exists but verify.verifications does not: this is neither an empty database nor one the old loop migrated; fix it by hand")
		}
		if clash {
			return plan{}, refuse("%s", clashMsg)
		}
		p.Ops = append(p.Ops, op{File: files[ti], Run: false, Rows: rowsFor(files[:ti+1], "adopted")})
		rest(ti + 1)

	case st.VerifyExists: // (c) the old loop ran 0016 and stopped
		if clash {
			return plan{}, refuse("%s", clashMsg)
		}
		rows := append(rowsFor(files[:ti], "adopted"), rowsFor([]File{files[ti]}, "applied")...)
		p.Ops = append(p.Ops, op{File: files[ti], Run: true, Rows: rows})
		rest(ti + 1)

	default: // (d) empty, or at 0015 or earlier: replay-safe, untracked until 0017
		for _, f := range files[:ti] {
			p.Ops = append(p.Ops, op{File: f, Run: true})
		}
		p.Ops = append(p.Ops, op{File: files[ti], Run: true, Rows: rowsFor(files[:ti+1], "applied")})
		rest(ti + 1)
	}
	return p, nil
}

// StatusLine is one line of `status`.
type StatusLine struct {
	Version, Name, State, Short, Note string
}

// String is the documented format: `NNNN applied|adopted|pending <checksum8>`, with an explanation appended only when something is wrong.
func (l StatusLine) String() string {
	s := fmt.Sprintf("%s %s %s", l.Version, l.State, l.Short)
	if l.Note != "" {
		s += "  " + l.Note
	}
	return s
}

// statusLines is pure. State comes from the tracker (applied / adopted) or is "pending"; the checksum shown is the recorded one when there is one, so a
// changed file is visible as a note rather than silently replacing the recorded value.
func statusLines(files []File, st dbState) []StatusLine {
	rec := make(map[string]row, len(st.Rows))
	for _, r := range st.Rows {
		rec[r.Version] = r
	}
	out := make([]StatusLine, 0, len(files))
	for _, f := range files {
		l := StatusLine{Version: f.Version, Name: f.Name, State: "pending", Short: f.Short()}
		if r, ok := rec[f.Version]; ok {
			l.State, l.Short = r.How, short(r.Checksum)
			if r.Checksum != f.Checksum {
				l.Note = "CHANGED: the file on disk is " + f.Short()
			}
			delete(rec, f.Version)
		}
		out = append(out, l)
	}
	extra := make([]string, 0, len(rec))
	for v := range rec {
		extra = append(extra, v)
	}
	sort.Strings(extra)
	for _, v := range extra {
		r := rec[v]
		out = append(out, StatusLine{Version: v, State: r.How, Short: short(r.Checksum), Note: "NO FILE: the database is ahead of this checkout"})
	}
	return out
}

// Status reports, per file, whether the database has it. It takes no lock and changes nothing. When the history is one `up` would refuse, it still
// returns the lines (so the operator can see which) together with the *RefusalError.
func Status(ctx context.Context, cfg Config) ([]StatusLine, error) {
	files, err := LoadFiles(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if cfg.DSN == "" {
		return nil, errors.New("no database connection string given (TM_MIGRATE_DATABASE_URL)")
	}
	conn, err := connect(ctx, cfg.DSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	st, err := readState(ctx, conn)
	if err != nil {
		return nil, err
	}
	_, perr := buildPlan(files, st)
	return statusLines(files, st), perr
}

// Up applies whatever is unapplied, in order (see the package comment). On a refusal or an error it returns what it did before stopping.
func Up(ctx context.Context, cfg Config) (Result, error) {
	files, err := LoadFiles(cfg.Dir)
	if err != nil {
		return Result{}, err
	}
	res := Result{Total: len(files)}
	if cfg.DSN == "" {
		return res, errors.New("no database connection string given (TM_MIGRATE_DATABASE_URL)")
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	wait := cfg.LockWait
	if wait <= 0 {
		wait = DefaultLockWait
	}

	work, err := connect(ctx, cfg.DSN)
	if err != nil {
		return res, err
	}
	defer func() { _ = work.Close(context.WithoutCancel(ctx)) }()

	lock, err := acquireLock(ctx, cfg.DSN, wait)
	if err != nil {
		return res, err
	}
	defer release(lock)

	// State is read only AFTER the lock is held: a runner that waited must see what the one before it did.
	st, err := readState(ctx, work)
	if err != nil {
		return res, err
	}
	p, err := buildPlan(files, st)
	if err != nil {
		return res, err
	}
	res.AlreadyApplied = p.AlreadyApplied

	for _, o := range p.Ops {
		if err := lockHeld(ctx, lock); err != nil {
			return res, err
		}
		if o.Run {
			fmt.Fprintf(out, "migrate: applying %s\n", o.File.Name)
		}
		if err := runOp(ctx, work, o); err != nil {
			return res, err
		}
		if o.Run {
			res.Applied++
		}
		for _, r := range o.Rows {
			if r.How == "adopted" {
				res.Adopted++
				fmt.Fprintf(out, "migrate: adopted %s (already applied by the old loop)\n", r.Version)
			}
		}
	}
	return res, nil
}

const insertRowSQL = `INSERT INTO verify.schema_migrations (version, checksum, how) VALUES ($1, $2, $3)`

// runOp runs one transaction: the file's SQL (unless this is a pure adoption), then its tracker rows, then COMMIT. Any failure rolls the whole thing back.
func runOp(ctx context.Context, conn *pgx.Conn, o op) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the transaction for %s: %w", o.File.Name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	// No arguments => pgx uses the simple protocol, so a multi-statement migration file is legal (exactly how testpg and psql run it).
	if o.Run {
		if _, err := tx.Exec(ctx, o.File.SQL); err != nil {
			return fmt.Errorf("migration %s failed and was rolled back (nothing after it ran): %w", o.File.Name, err)
		}
	}
	for _, r := range o.Rows {
		if _, err := tx.Exec(ctx, insertRowSQL, r.Version, r.Checksum, r.How); err != nil {
			return fmt.Errorf("recording migration %s as %s failed and the transaction for %s was rolled back: %w", r.Version, r.How, o.File.Name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit of %s failed: %w", o.File.Name, err)
	}
	committed = true
	return nil
}

func readState(ctx context.Context, conn *pgx.Conn) (dbState, error) {
	var st dbState
	// to_regclass is NULL (not an error) when the schema or the table is absent, which is exactly the question.
	if err := conn.QueryRow(ctx, `SELECT to_regclass('verify.schema_migrations') IS NOT NULL,
	                                     to_regclass('verify.verifications') IS NOT NULL,
	                                     to_regclass('public.verifications') IS NOT NULL`).
		Scan(&st.TrackerExists, &st.VerifyExists, &st.PublicVerifications); err != nil {
		return st, fmt.Errorf("read database state: %w", err)
	}
	if !st.TrackerExists {
		return st, nil
	}
	rows, err := conn.Query(ctx, `SELECT version, checksum, how FROM verify.schema_migrations ORDER BY version`)
	if err != nil {
		return st, fmt.Errorf("read verify.schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Version, &r.Checksum, &r.How); err != nil {
			return st, fmt.Errorf("read verify.schema_migrations: %w", err)
		}
		st.Rows = append(st.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("read verify.schema_migrations: %w", err)
	}
	return st, nil
}

// connect opens one connection as the migrating identity. The DSN never appears in an error: pgx v5.10.0 embeds the whole connection string in its parse
// error and its own redaction misses a password carried as a query parameter or spelled `password = x` (ADR-0016, store.scrubDSNError), so a parse
// failure is reported without the driver's text, and a connect failure is suppressed if it carries the password.
func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("connection string rejected by the driver (message suppressed: it can embed the DSN; check TM_MIGRATE_DATABASE_URL)")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = AppName
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		if cfg.Password != "" && strings.Contains(err.Error(), cfg.Password) {
			return nil, errors.New("could not connect to the database (message suppressed: it embedded the credential)")
		}
		return nil, fmt.Errorf("could not connect to the database: %w", err)
	}
	return conn, nil
}

// acquireLock opens the dedicated lock connection and blocks in pg_advisory_lock until it holds LockKey or `wait` has passed. The wait is bounded by the
// SERVER (statement_timeout), so the blocked backend shows up in pg_locks as an ungranted advisory lock and in pg_stat_activity with wait_event_type = 'Lock'.
func acquireLock(ctx context.Context, dsn string, wait time.Duration) (*pgx.Conn, error) {
	c, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pgx.Conn, error) {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", max(wait.Milliseconds(), 1))); err != nil {
		return fail(fmt.Errorf("set statement_timeout on the lock connection: %w", err))
	}
	if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", LockKey); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "57014" { // query_canceled: statement_timeout fired while waiting
			return fail(&LockTimeoutError{Wait: wait})
		}
		return fail(fmt.Errorf("pg_advisory_lock: %w", err))
	}
	if _, err := c.Exec(ctx, "SET statement_timeout = 0"); err != nil {
		return fail(fmt.Errorf("reset statement_timeout on the lock connection: %w", err))
	}
	return c, nil
}

func release(c *pgx.Conn) {
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.Exec(bg, "SELECT pg_advisory_unlock($1)", LockKey)
	_ = c.Close(bg)
}

// lockHeld proves, before every transaction, that the lock connection still holds LockKey: if that connection died the lock died with it and another
// runner may already be working.
func lockHeld(ctx context.Context, c *pgx.Conn) error {
	var held bool
	err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
	                                       WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid() AND objsubid = 1
	                                         AND ((classid::bigint << 32) | objid::bigint) = $1)`, LockKey).Scan(&held)
	if err != nil {
		return fmt.Errorf("lost the lock connection (stopping before the next migration): %w", err)
	}
	if !held {
		return errors.New("the advisory lock is no longer held (stopping before the next migration)")
	}
	return nil
}
