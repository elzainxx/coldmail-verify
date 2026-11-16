package migrate

// The runner against a REAL Postgres. These tests have no build tag and RUN whenever TM_TEST_DATABASE_URL is set (they skip only when it is unset,
// like every PG-gated test here; CI always sets it and check-test-execution.sh fails on any skip). Each builds its own throwaway databases with
// testpg, so none touches the shared one.
//
// ROLES ARE CLUSTER-WIDE. Migrations 0005 and 0016 create, rename, alter and drop roles, and `go test ./...` runs packages in parallel, so every run of
// the runner here happens under testpg.LockMigrations (the runner itself locks per database, which is a different thing). A database migrated only
// through 0015 still depends on the role cp_runtime, which tests in other packages assert is gone, so such a database is created AND finished inside
// one hold of the lock (testpg.ScratchUnderLock).
//
// What each test proves is in its own comment; every one that asserts a refusal, a no-op or an equality has a paired control that shows the check can fail.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/coldmail-verify/control-plane/internal/testpg"
)

func mustLoad(t testing.TB, dir string) []File {
	t.Helper()
	files, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles(%s): %v", dir, err)
	}
	return files
}

func testConn(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func dbExec(t testing.TB, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func scalar[T any](t testing.TB, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// runUp runs the runner under the cluster-wide migration lock.
func runUp(t testing.TB, dsn, dir string) (Result, error) {
	t.Helper()
	unlock := testpg.LockMigrations(t)
	defer unlock()
	return Up(context.Background(), Config{DSN: dsn, Dir: dir, LockWait: time.Minute})
}

func mustUp(t testing.TB, dsn, dir string) Result {
	t.Helper()
	res, err := runUp(t, dsn, dir)
	if err != nil {
		t.Fatalf("Up(%s): %v", dir, err)
	}
	return res
}

// freshFingerprint is F: the fingerprint of a database the runner built from nothing with the repository's own migrations.
func freshFingerprint(t testing.TB) fingerprint {
	t.Helper()
	dsn := testpg.Empty(t)
	mustUp(t, dsn, testpg.MigrationsDir())
	return fingerprintOf(t, dsn)
}

// upgradeFrom0015 builds a database the OLD loop took to 0015 (module 1's tables in public) and runs the runner on it, all inside one hold of the
// migration lock. `seed` runs on the 0015 database before the runner does.
func upgradeFrom0015(t testing.TB, dir string, seed func(conn *pgx.Conn)) (string, Result, error) {
	t.Helper()
	unlock := testpg.LockMigrations(t)
	defer unlock()
	dsn := testpg.ScratchUnderLock(t, 15)
	conn := testConn(t, dsn)
	if !scalar[bool](t, conn, `SELECT to_regclass('public.verifications') IS NOT NULL AND to_regnamespace('verify') IS NULL AND to_regclass('verify.schema_migrations') IS NULL`) {
		t.Fatal("the starting database is not a 0015 database (module 1 in public, no schema verify, no tracker)")
	}
	if seed != nil {
		seed(conn)
	}
	res, err := Up(context.Background(), Config{DSN: dsn, Dir: dir, LockWait: time.Minute})
	return dsn, res, err
}

func versionsOf(rows []trackerRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Version
	}
	return out
}

func wantVersions(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Version
	}
	return out
}

func requireNothingInPublic(t testing.TB, dsn string) {
	t.Helper()
	if left := publicLeftovers(t, dsn); len(left) != 0 {
		t.Errorf("schema public still holds %d object(s) of module 1: %v", len(left), left)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fresh install, and the full chain twice
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrate_FreshInstall: on an EMPTY database the runner applies every file, records one tracker row per file with how='applied' and the checksum
// of that file, and leaves nothing of module 1 in public. It also reports how many objects it fingerprinted (zero would be red).
func TestMigrate_FreshInstall(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)

	res := mustUp(t, dsn, dir)
	if want := (Result{Applied: len(files), Total: len(files)}); res != want {
		t.Fatalf("fresh install reported %+v, want %+v", res, want)
	}
	if got, want := res.Summary(), fmt.Sprintf("migrate: applied %d, adopted 0, already applied 0, total %d", len(files), len(files)); got != want {
		t.Errorf("summary line = %q, want %q", got, want)
	}

	rows := trackerSnapshot(t, dsn)
	if !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Fatalf("tracker versions = %v, want %v", versionsOf(rows), wantVersions(files))
	}
	for i, r := range rows {
		if r.How != "applied" || r.Checksum != files[i].Checksum || r.AppliedBy == "" || r.AppliedAtMicro == 0 {
			t.Errorf("tracker row %s = %+v, want how=applied, checksum %s, applied_by and applied_at set", r.Version, r, files[i].Checksum)
		}
	}
	if len(rows) < 17 {
		t.Errorf("only %d tracker rows: want at least 17 (0001..0017)", len(rows))
	}

	fp := fingerprintOf(t, dsn)
	t.Logf("fresh install: fingerprint F covers %d objects (relations + functions) in %d facts; catalog=%s tracker=%s", fp.Objects, len(fp.Facts), fp.Catalog[:12], fp.Tracker[:12])
	requireNothingInPublic(t, dsn)
}

// TestMigrate_TwiceIsANoOp: the second full run applies nothing, adopts nothing, reports every file as already applied, rewrites no tracker row (same
// transaction id, same applied_at), and leaves the catalog fingerprint unchanged. Control: a database that really did change (one table added) gets a
// different fingerprint, so "unchanged" above is a finding and not a property of the function.
func TestMigrate_TwiceIsANoOp(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)
	mustUp(t, dsn, dir)
	rowsBefore, fpBefore := trackerSnapshot(t, dsn), fingerprintOf(t, dsn)

	for run := 2; run <= 3; run++ {
		res := mustUp(t, dsn, dir)
		if want := (Result{AlreadyApplied: len(files), Total: len(files)}); res != want {
			t.Fatalf("run %d reported %+v, want %+v", run, res, want)
		}
		if got, want := res.Summary(), fmt.Sprintf("migrate: applied 0, adopted 0, already applied %d, total %d", len(files), len(files)); got != want {
			t.Errorf("run %d summary = %q, want %q", run, got, want)
		}
		if rowsAfter := trackerSnapshot(t, dsn); !reflect.DeepEqual(rowsBefore, rowsAfter) {
			t.Errorf("run %d changed the tracker:\n before %+v\n after  %+v", run, rowsBefore, rowsAfter)
		}
		fpAfter := fingerprintOf(t, dsn)
		requireSameCatalog(t, "after run 1", fmt.Sprintf("after run %d", run), fpBefore, fpAfter)
		requireSameTracker(t, "after run 1", fmt.Sprintf("after run %d", run), fpBefore, fpAfter)
		requireNothingInPublic(t, dsn)
	}

	dbExec(t, testConn(t, dsn), `CREATE TABLE verify.zz_twice_control (a int)`)
	if fingerprintOf(t, dsn).Catalog == fpBefore.Catalog {
		t.Error("CONTROL: a database with one more table has the same fingerprint, so 'unchanged' above proves nothing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// upgrades from what the OLD psql loop left behind
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrate_UpgradeFrom0015: a database the old loop took to 0015 (module 1 in public, a row of data in one table) is upgraded by replaying 0001..0015
// (safe by construction: IF NOT EXISTS), 0016 and 0017. It ends in the same state as a fresh install, and the data row survives (moved to verify).
func TestMigrate_UpgradeFrom0015(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)

	dsn, res, err := upgradeFrom0015(t, dir, func(conn *pgx.Conn) {
		dbExec(t, conn, `INSERT INTO public.disposable_domains (domain, source, added_at) VALUES ('survives.example', 'migrate-test', now())`)
	})
	if err != nil {
		t.Fatalf("Up on a 0015 database: %v", err)
	}
	if want := (Result{Applied: len(files), Total: len(files)}); res != want {
		t.Fatalf("upgrade from 0015 reported %+v, want %+v", res, want)
	}
	conn := testConn(t, dsn)
	if got := scalar[string](t, conn, `SELECT source FROM verify.disposable_domains WHERE domain = 'survives.example'`); got != "migrate-test" {
		t.Errorf("the data row did not survive the upgrade (source = %q)", got)
	}
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.disposable_domains`); n != 1 {
		t.Errorf("verify.disposable_domains holds %d row(s), want exactly the one we put there", n)
	}
	rows := trackerSnapshot(t, dsn)
	if !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Fatalf("tracker versions = %v, want %v", versionsOf(rows), wantVersions(files))
	}
	for i, r := range rows {
		if r.How != "applied" || r.Checksum != files[i].Checksum {
			t.Errorf("tracker row %+v, want applied with %s's checksum", r, files[i].Name)
		}
	}
	requireNothingInPublic(t, dsn)

	f, up := freshFingerprint(t), fingerprintOf(t, dsn)
	requireSameCatalog(t, "fresh install", "upgrade from 0015", f, up)
	requireSameTracker(t, "fresh install", "upgrade from 0015", f, up)

	// The upgrade is itself a no-op the second time.
	if res2 := mustUp(t, dsn, dir); res2.Applied != 0 || res2.AlreadyApplied != len(files) {
		t.Errorf("a second run after the upgrade reported %+v", res2)
	}
}

// TestMigrate_UpgradeFrom0016OldLoop: a database the old loop took to 0016 has NO tracker. The runner applies only 0017 and records 0001..0016 as
// 'adopted' (0017 as 'applied'); the end state equals a fresh install's.
func TestMigrate_UpgradeFrom0016OldLoop(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Scratch(t, 16)
	conn := testConn(t, dsn)
	if !scalar[bool](t, conn, `SELECT to_regclass('verify.verifications') IS NOT NULL AND to_regclass('verify.schema_migrations') IS NULL`) {
		t.Fatal("the starting database is not a 0016 database without a tracker")
	}

	res := mustUp(t, dsn, dir)
	if want := (Result{Applied: len(files) - 16, Adopted: 16, Total: len(files)}); res != want {
		t.Fatalf("upgrade from 0016 reported %+v, want %+v (apply 0017 and later; adopt 0001..0016)", res, want)
	}
	rows := trackerSnapshot(t, dsn)
	if !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Fatalf("tracker versions = %v, want %v", versionsOf(rows), wantVersions(files))
	}
	for i, r := range rows {
		want := "adopted"
		if files[i].Version >= TrackerVersion {
			want = "applied"
		}
		if r.How != want || r.Checksum != files[i].Checksum {
			t.Errorf("tracker row %+v, want how=%s with %s's checksum", r, want, files[i].Name)
		}
	}
	requireNothingInPublic(t, dsn)

	f, up := freshFingerprint(t), fingerprintOf(t, dsn)
	requireSameCatalog(t, "fresh install", "upgrade from 0016", f, up)
	requireSameTracker(t, "fresh install", "upgrade from 0016", f, up)
}

// TestMigrate_UpgradeFromOldLoopThrough0017: a database the old loop took through 0017 has the tracker table but NO rows (0017 created it and nothing
// recorded anything). A runner that only asked "does the table exist?" would replay 0001..0016 here. It adopts all 17 instead and ends equal to a fresh install.
func TestMigrate_UpgradeFromOldLoopThrough0017(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Scratch(t, 17) // the old loop through 0017, exactly
	conn := testConn(t, dsn)
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.schema_migrations`); n != 0 {
		t.Fatalf("the old loop left %d tracker row(s), want an empty tracker", n)
	}

	res := mustUp(t, dsn, dir)
	if want := (Result{Applied: len(files) - 17, Adopted: 17, Total: len(files)}); res != want {
		t.Fatalf("reported %+v, want %+v (0001..0017 adopted; only files after 0017, if any, applied)", res, want)
	}
	for i, r := range trackerSnapshot(t, dsn) {
		want := "adopted"
		if files[i].Version > TrackerVersion {
			want = "applied"
		}
		if r.Version != files[i].Version || r.How != want || r.Checksum != files[i].Checksum {
			t.Errorf("tracker row %+v, want %s %s with that file's checksum", r, files[i].Name, want)
		}
	}
	f, up := freshFingerprint(t), fingerprintOf(t, dsn)
	requireSameCatalog(t, "fresh install", "adopted old-loop database", f, up)
	requireSameTracker(t, "fresh install", "adopted old-loop database", f, up)
}

// tripwireDir copies the migrations and replaces every file up to and including `upTo` with a statement that RAISES, so any run that executes one of
// them fails loudly instead of "working".
func tripwireDir(t testing.TB, upTo int) string {
	t.Helper()
	dir := copyMigrations(t)
	for _, f := range mustLoad(t, dir) {
		n, err := strconv.Atoi(f.Version)
		if err != nil || n > upTo {
			continue
		}
		body := fmt.Sprintf("DO $$ BEGIN RAISE EXCEPTION 'tripwire: migration %s must never be executed on this database'; END $$;\n", f.Version)
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMigrate_NeverReplaysAfter0016 is the F23 hazard, proved by execution and not by inspecting the end state: on a database that already ran 0016,
// 0001..0015 are unqualified and every CREATE is IF NOT EXISTS, so replaying them creates EMPTY module-1 tables in public. Here 0001..0016 (and, for
// the second database, 0017) are replaced by a statement that raises. If the runner executed any of them the run would fail with 'tripwire'; it adopts
// them instead and succeeds, and public stays empty. Control: the same tripwire directory on an EMPTY database fails at 0001, so the tripwire does fire.
func TestMigrate_NeverReplaysAfter0016(t *testing.T) {
	t.Run("0016 by the old loop, no tracker", func(t *testing.T) {
		dsn := testpg.Scratch(t, 16)
		dir := tripwireDir(t, 16)
		files := mustLoad(t, dir)
		res := mustUp(t, dsn, dir)
		if want := (Result{Applied: len(files) - 16, Adopted: 16, Total: len(files)}); res != want {
			t.Fatalf("reported %+v, want %+v", res, want)
		}
		requireNothingInPublic(t, dsn)
	})
	t.Run("0017 by the old loop, empty tracker", func(t *testing.T) {
		dsn := testpg.Scratch(t, 17)
		dir := tripwireDir(t, 17)
		files := mustLoad(t, dir)
		res := mustUp(t, dsn, dir)
		if want := (Result{Applied: len(files) - 17, Adopted: 17, Total: len(files)}); res != want {
			t.Fatalf("reported %+v, want %+v", res, want)
		}
		requireNothingInPublic(t, dsn)
	})
	t.Run("control: the tripwire fires on a database that has not run them", func(t *testing.T) {
		_, err := runUp(t, testpg.Empty(t), tripwireDir(t, 16))
		if err == nil || !strings.Contains(err.Error(), "tripwire: migration 0001 must never be executed") {
			t.Fatalf("an empty database must execute 0001 and hit the tripwire, got %v", err)
		}
	})
}

// TestMigrate_0017IsHarmlessUnderTheOldLoop: CI and the platform harness still apply the files with one psql run each. 0017 applied by that loop, and
// applied again, changes nothing (no error, no new object, the tracker stays empty), and it refuses, naming 0016, on a database where 0016 has not run.
func TestMigrate_0017IsHarmlessUnderTheOldLoop(t *testing.T) {
	dsn := testpg.Scratch(t, 17) // 0001..0017, one file at a time
	before := fingerprintOf(t, dsn)
	testpg.Apply(t, dsn, 17)
	testpg.Apply(t, dsn, 17)
	after := fingerprintOf(t, dsn)
	requireSameCatalog(t, "after 0017 once", "after 0017 three times", before, after)
	if n := scalar[int](t, testConn(t, dsn), `SELECT count(*)::int FROM verify.schema_migrations`); n != 0 {
		t.Errorf("0017 under the old loop recorded %d row(s); only the runner records", n)
	}

	// CONTROL: without 0016 the file must stop and say so (so the guard in it can fail).
	files := mustLoad(t, testpg.MigrationsDir())
	_, err := testConn(t, testpg.Empty(t)).Exec(context.Background(), files[16].SQL)
	if sqlState(err) != "55000" || !strings.Contains(fmt.Sprint(err), "0016 must run first") {
		t.Errorf("0017 on an empty database must fail with SQLSTATE 55000 naming 0016, got %v", err)
	}
}

// TestMigrate_FreshEqualsUpgraded compares the catalog fingerprint of a fresh runner install with an upgrade from 0015, from 0016 (old loop), from
// 0017 (old loop, empty tracker) and with the OLD LOOP ALONE (the runner never touched it), and the tracker's (version, checksum) pairs across the four
// runner databases. PAIRED CONTROL: four deliberate changes to one database (a table, a column, a grant, a function body) must each change the
// fingerprint, and the difference must NAME the changed object.
func TestMigrate_FreshEqualsUpgraded(t *testing.T) {
	dir := testpg.MigrationsDir()

	fresh := testpg.Empty(t)
	mustUp(t, fresh, dir)
	d15, _, err := upgradeFrom0015(t, dir, nil)
	if err != nil {
		t.Fatalf("upgrade from 0015: %v", err)
	}
	d16 := testpg.Scratch(t, 16)
	mustUp(t, d16, dir)
	d17 := testpg.Scratch(t, 17)
	mustUp(t, d17, dir)
	loop := testpg.Scratch(t, 0) // the old loop only

	F := fingerprintOf(t, fresh)
	t.Logf("F (fresh runner install): %d objects, %d facts", F.Objects, len(F.Facts))
	if again := fingerprintOf(t, fresh); again.Catalog != F.Catalog || again.Tracker != F.Tracker {
		t.Fatal("the fingerprint of an unchanged database is not stable: it cannot be compared")
	}
	for name, dsn := range map[string]string{"upgrade from 0015": d15, "upgrade from 0016": d16, "upgrade from 0017 (empty tracker)": d17} {
		fp := fingerprintOf(t, dsn)
		requireSameCatalog(t, "fresh", name, F, fp)
		requireSameTracker(t, "fresh", name, F, fp)
	}
	lp := fingerprintOf(t, loop)
	requireSameCatalog(t, "fresh runner install", "the old psql loop alone", F, lp)
	if len(lp.TrackerFacts) != 0 {
		t.Errorf("the old loop alone recorded %d tracker fact(s)", len(lp.TrackerFacts))
	}

	// PAIRED CONTROL: the comparison above can say "different", and it says WHAT.
	conn := testConn(t, fresh)
	for _, step := range []struct{ name, sql, want string }{
		{"a table is added", `CREATE TABLE verify.zz_control_table (a int)`, "rel|verify.zz_control_table"},
		{"a column is added", `ALTER TABLE verify.cache ADD COLUMN zz_control_column integer`, "|zz_control_column|type=integer"},
		{"a privilege is granted", `GRANT DELETE ON verify.verifications TO verify_app`, "acl|verify.verifications|verify_app|DELETE|false"},
		{"a function body changes", `CREATE OR REPLACE FUNCTION verify.ensure_verification_raw_partitions(p_months_ahead integer) RETURNS integer
			LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$ SELECT 0 $f$`, "fn|verify.ensure_verification_raw_partitions(p_months_ahead integer)"},
	} {
		dbExec(t, conn, step.sql)
		fp := fingerprintOf(t, fresh)
		if fp.Catalog == F.Catalog {
			t.Errorf("CONTROL (%s): the fingerprint did not change", step.name)
			continue
		}
		onlyF, onlyNow := diffFacts(F.Facts, fp.Facts)
		if joined := strings.Join(append(onlyF, onlyNow...), "\n"); !strings.Contains(joined, step.want) {
			t.Errorf("CONTROL (%s): the fingerprint changed but the difference does not name %q:\n%s", step.name, step.want, joined)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// refusals: nothing is changed
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrate_ChangedFileIsRefused: after a full run, a copy of the directory with ONE BYTE of an applied file flipped is refused, the message names
// the version and both checksums (recorded, on disk), and the database is exactly as it was (same tracker rows, same catalog). CONTROL: an
// untouched copy of the directory is accepted and is a no-op, so the refusal is about the byte.
func TestMigrate_ChangedFileIsRefused(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)
	mustUp(t, dsn, dir)
	rowsBefore, fpBefore := trackerSnapshot(t, dsn), fingerprintOf(t, dsn)

	tmp := copyMigrations(t)
	target := files[4] // 0005
	b, err := os.ReadFile(filepath.Join(tmp, target.Name))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(filepath.Join(tmp, target.Name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	onDisk := mustLoad(t, tmp)[4].Short()
	if onDisk == target.Short() {
		t.Fatal("flipping a byte did not change the checksum")
	}

	res, err := runUp(t, dsn, tmp)
	if err == nil {
		t.Fatalf("a changed migration was accepted: %+v", res)
	}
	msg := asRefusal(t, err).Error()
	for _, want := range []string{"migration 0005 changed after it was applied", "recorded " + target.Short(), "on disk " + onDisk} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not contain %q", msg, want)
		}
	}
	if res.Applied != 0 || res.Adopted != 0 {
		t.Errorf("a refusal reported work: %+v", res)
	}
	if rowsAfter := trackerSnapshot(t, dsn); !reflect.DeepEqual(rowsBefore, rowsAfter) {
		t.Errorf("the refused run changed the tracker:\n before %+v\n after  %+v", rowsBefore, rowsAfter)
	}
	fpAfter := fingerprintOf(t, dsn)
	requireSameCatalog(t, "before the refused run", "after it", fpBefore, fpAfter)
	requireSameTracker(t, "before the refused run", "after it", fpBefore, fpAfter)

	// CONTROL: an untouched copy is a clean no-op.
	if res := mustUp(t, dsn, copyMigrations(t)); res.Applied != 0 || res.AlreadyApplied != len(files) {
		t.Errorf("CONTROL: an unmodified copy of the directory reported %+v", res)
	}
}

// TestMigrate_OutOfOrderHistoryIsRefused: with the tracker row of 0005 alone deleted (0006..0017 still recorded) the runner refuses, names 0005, and does
// not "helpfully" apply it (that would replay a file out of order). CONTROL: restoring the row makes the same run a no-op. A recorded version with no
// file (0099) is refused as "ahead of this checkout", and removing that row makes it a no-op again.
func TestMigrate_OutOfOrderHistoryIsRefused(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)
	mustUp(t, dsn, dir)
	conn := testConn(t, dsn)

	dbExec(t, conn, `DELETE FROM verify.schema_migrations WHERE version = '0005'`)
	rowsBefore, fpBefore := trackerSnapshot(t, dsn), fingerprintOf(t, dsn)
	res, err := runUp(t, dsn, dir)
	if err == nil {
		t.Fatalf("a history with a hole was accepted: %+v", res)
	}
	if msg := asRefusal(t, err).Error(); !strings.Contains(msg, "migration 0005 is not applied but a later one (0006) is") {
		t.Errorf("refusal %q does not name 0005 and the later version", msg)
	}
	if rowsAfter := trackerSnapshot(t, dsn); !reflect.DeepEqual(rowsBefore, rowsAfter) {
		t.Errorf("the refused run changed the tracker:\n before %+v\n after  %+v", rowsBefore, rowsAfter)
	}
	requireSameCatalog(t, "before the refused run", "after it", fpBefore, fingerprintOf(t, dsn))

	dbExec(t, conn, `INSERT INTO verify.schema_migrations (version, checksum, how) VALUES ('0005', $1, 'applied')`, files[4].Checksum)
	if res := mustUp(t, dsn, dir); res.Applied != 0 || res.AlreadyApplied != len(files) {
		t.Fatalf("CONTROL: with the row restored the run must be a no-op, got %+v", res)
	}

	dbExec(t, conn, `INSERT INTO verify.schema_migrations (version, checksum, how) VALUES ('0099', $1, 'applied')`, files[0].Checksum)
	_, err = runUp(t, dsn, dir)
	if err == nil {
		t.Fatal("a recorded version with no file was accepted")
	}
	for _, want := range []string{"0099", "no file for it", "ahead of this checkout"} {
		if msg := asRefusal(t, err).Error(); !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not contain %q", msg, want)
		}
	}
	dbExec(t, conn, `DELETE FROM verify.schema_migrations WHERE version = '0099'`)
	if res := mustUp(t, dsn, dir); res.Applied != 0 || res.AlreadyApplied != len(files) {
		t.Errorf("CONTROL: with 0099 removed the run must be a no-op, got %+v", res)
	}
}

// TestMigrate_DatabaseAheadOfCheckoutIsRefused is the realistic form of 'a recorded version with no file': a newer checkout (a 0018) migrated the
// database, then an OLDER checkout is run against it (a rollback of the code, a stale host). It is refused and nothing is undone.
func TestMigrate_DatabaseAheadOfCheckoutIsRefused(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	newer := copyMigrations(t)
	ahead := fmt.Sprintf("%04d", len(files)+1)
	if err := os.WriteFile(filepath.Join(newer, ahead+"_a_newer_checkout.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := testpg.Empty(t)
	if res := mustUp(t, dsn, newer); res.Applied != len(files)+1 {
		t.Fatalf("the newer checkout applied %d file(s), want %d", res.Applied, len(files)+1)
	}
	rowsBefore, fpBefore := trackerSnapshot(t, dsn), fingerprintOf(t, dsn)

	_, err := runUp(t, dsn, dir)
	if err == nil {
		t.Fatal("an older checkout was accepted by a database that is ahead of it")
	}
	if msg := asRefusal(t, err).Error(); !strings.Contains(msg, "migration "+ahead+" is recorded") || !strings.Contains(msg, "ahead of this checkout") {
		t.Errorf("refusal %q does not name %s and 'ahead of this checkout'", msg, ahead)
	}
	if rowsAfter := trackerSnapshot(t, dsn); !reflect.DeepEqual(rowsBefore, rowsAfter) {
		t.Errorf("the refused run changed the tracker")
	}
	requireSameCatalog(t, "before", "after", fpBefore, fingerprintOf(t, dsn))
	// CONTROL: the newer checkout itself is fine.
	if res := mustUp(t, dsn, newer); res.Applied != 0 || res.AlreadyApplied != len(files)+1 {
		t.Errorf("CONTROL: the newer checkout reported %+v", res)
	}
}

// TestMigrate_AdoptionRefusesWhenTheReRunHazardIsPresent: if 0001..0015 were re-run on a migrated database, public holds empty module-1 tables next to
// verify's. Adopting such a database would bless that state, so the runner refuses (as 0016 itself does) and changes nothing.
func TestMigrate_AdoptionRefusesWhenTheReRunHazardIsPresent(t *testing.T) {
	for name, through := range map[string]int{"0016 by the old loop (no tracker)": 16, "0017 by the old loop (empty tracker)": 17} {
		t.Run(name, func(t *testing.T) {
			dsn := testpg.Scratch(t, through)
			dbExec(t, testConn(t, dsn), `CREATE TABLE public.verifications (id int)`) // what re-running 0001 after 0016 leaves behind
			fpBefore := fingerprintOf(t, dsn)
			res, err := runUp(t, dsn, testpg.MigrationsDir())
			if err == nil {
				t.Fatalf("adopted a database that has the re-run hazard: %+v", res)
			}
			if msg := asRefusal(t, err).Error(); !strings.Contains(msg, "public.verifications") || !strings.Contains(msg, "re-run") {
				t.Errorf("refusal %q does not name public.verifications and the re-run", msg)
			}
			requireSameCatalog(t, "before", "after", fpBefore, fingerprintOf(t, dsn))
			conn := testConn(t, dsn)
			if through == 16 && scalar[bool](t, conn, `SELECT to_regclass('verify.schema_migrations') IS NOT NULL`) {
				t.Error("the refused run created the tracker")
			}
			if through == 17 {
				if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.schema_migrations`); n != 0 {
					t.Errorf("the refused run recorded %d row(s)", n)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the lock
// ─────────────────────────────────────────────────────────────────────────────

// blockedRunners counts runner backends that are WAITING for the advisory lock: an ungranted advisory lock in pg_locks whose backend shows
// wait_event_type = 'Lock' in pg_stat_activity. It is the server's own statement that the runner is blocked, not a sleep and a guess.
func blockedRunners(t testing.TB, conn *pgx.Conn) int {
	t.Helper()
	return scalar[int](t, conn, `
		SELECT count(*)::int FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid
		 WHERE a.datname = current_database() AND a.application_name = $2 AND a.wait_event_type = 'Lock'
		   AND l.locktype = 'advisory' AND NOT l.granted AND l.objsubid = 1
		   AND ((l.classid::bigint << 32) | l.objid::bigint) = $1`, LockKey, AppName)
}

// TestMigrate_TwoRunnersAtOnce: the test holds the advisory lock itself and starts TWO runners. Both are observed BLOCKED by the server (ungranted
// lock, wait_event_type 'Lock') while the database is still untouched; when the lock is released they run one after the other: exactly one applies
// every file, the other finds them all applied, and there is exactly one set of tracker rows.
func TestMigrate_TwoRunnersAtOnce(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)
	unlock := testpg.LockMigrations(t) // the runners rename roles: hold the cluster-wide lock for the whole test
	defer unlock()

	holder, observer := testConn(t, dsn), testConn(t, dsn)
	dbExec(t, holder, `SELECT pg_advisory_lock($1)`, LockKey)
	if n := blockedRunners(t, observer); n != 0 {
		t.Fatalf("CONTROL: %d runner(s) reported blocked before any was started", n)
	}

	type outcome struct {
		res Result
		err error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := Up(context.Background(), Config{DSN: dsn, Dir: dir, LockWait: 3 * time.Minute})
			results <- outcome{res, err}
		}()
	}

	deadline := time.Now().Add(90 * time.Second)
	for blockedRunners(t, observer) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the two runners were not both observed blocked on the advisory lock within 90s (blocked now: %d)", blockedRunners(t, observer))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !scalar[bool](t, observer, `SELECT to_regnamespace('verify') IS NULL`) {
		t.Error("a runner changed the database while another party held the lock")
	}
	select {
	case r := <-results:
		t.Fatalf("a runner finished while the lock was held: %+v", r)
	default:
	}

	dbExec(t, holder, `SELECT pg_advisory_unlock($1)`, LockKey)
	var got []Result
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("a runner failed after the lock was released: %v", r.err)
			}
			got = append(got, r.res)
		case <-time.After(5 * time.Minute):
			t.Fatal("a runner did not finish within 5 minutes of the lock being released")
		}
	}
	applied, noop := Result{Applied: len(files), Total: len(files)}, Result{AlreadyApplied: len(files), Total: len(files)}
	if !((got[0] == applied && got[1] == noop) || (got[1] == applied && got[0] == noop)) {
		t.Errorf("results %+v: want exactly one runner to apply all %d files and the other to find them already applied", got, len(files))
	}
	rows := trackerSnapshot(t, dsn)
	if !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Errorf("tracker versions = %v, want exactly one set %v", versionsOf(rows), wantVersions(files))
	}
	for _, r := range rows {
		if r.How != "applied" {
			t.Errorf("row %+v: want how=applied", r)
		}
	}
	if n := blockedRunners(t, observer); n != 0 {
		t.Errorf("%d runner(s) still blocked after both finished", n)
	}
}

// TestMigrate_LockWaitIsBounded: a runner that cannot get the lock gives up after its bounded wait with a message that says so, instead of hanging or
// working anyway, and it changes nothing. CONTROL: the very same run succeeds once the lock is free.
func TestMigrate_LockWaitIsBounded(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Empty(t)
	holder := testConn(t, dsn)
	dbExec(t, holder, `SELECT pg_advisory_lock($1)`, LockKey)

	start := time.Now()
	res, err := Up(context.Background(), Config{DSN: dsn, Dir: dir, LockWait: 1500 * time.Millisecond})
	elapsed := time.Since(start)
	var lt *LockTimeoutError
	if !errors.As(err, &lt) {
		t.Fatalf("want a *LockTimeoutError, got %v (result %+v)", err, res)
	}
	if elapsed < time.Second || elapsed > 45*time.Second {
		t.Errorf("gave up after %s, want about 1.5s (bounded, and not instant)", elapsed)
	}
	for _, want := range []string{"another migration runner", "advisory lock", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not contain %q", err.Error(), want)
		}
	}
	if !scalar[bool](t, testConn(t, dsn), `SELECT to_regnamespace('verify') IS NULL`) {
		t.Error("a runner that never got the lock changed the database")
	}

	dbExec(t, holder, `SELECT pg_advisory_unlock($1)`, LockKey)
	if res := mustUp(t, dsn, dir); res.Applied != len(files) {
		t.Errorf("CONTROL: with the lock free the run applied %d file(s), want %d", res.Applied, len(files))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// a failing migration
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrate_FailingMigrationStopsTheRun: a new migration with a syntax error (and a good one behind it) leaves every earlier file recorded, itself not recorded,
// the one after it not run, and a second run reports the SAME error and re-applies nothing.
func TestMigrate_FailingMigrationStopsTheRun(t *testing.T) {
	files := mustLoad(t, testpg.MigrationsDir())
	dir := copyMigrations(t)
	broken, after := fmt.Sprintf("%04d_broken.sql", len(files)+1), fmt.Sprintf("%04d_after.sql", len(files)+2) // the next two numbers, whatever they are
	for name, body := range map[string]string{
		broken: "THIS IS NOT SQL;\n",
		after:  "CREATE TABLE verify.zz_after_the_failure (a int);\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dsn := testpg.Empty(t)

	res, err := runUp(t, dsn, dir)
	if err == nil {
		t.Fatal("a broken migration was accepted")
	}
	if errors.As(err, new(*RefusalError)) {
		t.Fatalf("a failing migration is an error, not a refusal: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{broken, "SQLSTATE 42601", "rolled back"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if res.Applied != len(files) || res.Total != len(files)+2 {
		t.Errorf("result %+v, want %d applied (every file before the broken one) of %d", res, len(files), len(files)+2)
	}
	rows := trackerSnapshot(t, dsn)
	if !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Fatalf("tracker versions = %v, want exactly %v (the broken file must not be recorded)", versionsOf(rows), wantVersions(files))
	}
	conn := testConn(t, dsn)
	if scalar[bool](t, conn, `SELECT to_regclass('verify.zz_after_the_failure') IS NOT NULL`) {
		t.Error("a file after the failing one ran")
	}

	fpBefore := fingerprintOf(t, dsn)
	res2, err2 := runUp(t, dsn, dir)
	if err2 == nil || err2.Error() != msg {
		t.Errorf("the second run must report the same error:\n first  %q\n second %v", msg, err2)
	}
	if want := (Result{AlreadyApplied: len(files), Total: len(files) + 2}); res2 != want {
		t.Errorf("second run result %+v, want %+v (nothing re-applied)", res2, want)
	}
	if rows2 := trackerSnapshot(t, dsn); !reflect.DeepEqual(rows, rows2) {
		t.Error("the second run changed the tracker")
	}
	requireSameCatalog(t, "after the first failure", "after the second", fpBefore, fingerprintOf(t, dsn))
}

// TestMigrate_FailedFileRollsBackItsOwnWork: a file that creates a table and THEN fails leaves no table behind (each file is its own transaction) and
// no tracker row, while everything before it stays committed and recorded. (A pure syntax error cannot show this: Postgres parses the whole file first.)
func TestMigrate_FailedFileRollsBackItsOwnWork(t *testing.T) {
	files := mustLoad(t, testpg.MigrationsDir())
	dir := copyMigrations(t)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d_half_done.sql", len(files)+1)), []byte("CREATE TABLE verify.zz_half_done (a int);\nSELECT 1/0;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := testpg.Empty(t)
	_, err := runUp(t, dsn, dir)
	if err == nil || sqlState(err) != "22012" {
		t.Fatalf("want a division-by-zero failure (22012), got %v", err)
	}
	conn := testConn(t, dsn)
	if scalar[bool](t, conn, `SELECT to_regclass('verify.zz_half_done') IS NOT NULL`) {
		t.Error("the failed file's CREATE TABLE survived: the file did not run in its own transaction")
	}
	if rows := trackerSnapshot(t, dsn); !reflect.DeepEqual(versionsOf(rows), wantVersions(files)) {
		t.Errorf("tracker versions = %v, want %v", versionsOf(rows), wantVersions(files))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────────────────────

func statusOf(t testing.TB, dsn, dir string) ([]StatusLine, error) {
	t.Helper()
	return Status(context.Background(), Config{DSN: dsn, Dir: dir})
}

// TestMigrate_Status: one line per file in the documented format, from pending (empty database) through adopted/applied (old-loop database after `up`)
// to applied everywhere; and a changed file is shown, with the refusal.
func TestMigrate_Status(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)

	empty := testpg.Empty(t)
	lines, err := statusOf(t, empty, dir)
	if err != nil || len(lines) != len(files) {
		t.Fatalf("status of an empty database: %d line(s), err %v", len(lines), err)
	}
	for i, l := range lines {
		if want := fmt.Sprintf("%s pending %s", files[i].Version, files[i].Short()); l.String() != want {
			t.Errorf("line %d = %q, want %q", i, l.String(), want)
		}
	}

	old := testpg.Scratch(t, 16)
	lines, err = statusOf(t, old, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l.State != "pending" {
			t.Errorf("an untracked old-loop database shows %s as %s, want pending (nothing is recorded yet)", l.Version, l.State)
		}
	}
	mustUp(t, old, dir)
	lines, err = statusOf(t, old, dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range lines {
		want := "adopted"
		if files[i].Version >= TrackerVersion {
			want = "applied"
		}
		if l.State != want || l.Short != files[i].Short() || l.Note != "" {
			t.Errorf("after up on an old-loop database: %q, want %s %s with no note", l.String(), want, files[i].Short())
		}
	}

	// A changed file is visible, and status exits non-zero (an error) like `up` would refuse.
	tmp := copyMigrations(t)
	b, err := os.ReadFile(filepath.Join(tmp, files[2].Name))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(filepath.Join(tmp, files[2].Name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err = statusOf(t, old, tmp)
	asRefusal(t, err)
	if len(lines) != len(files) || !strings.Contains(lines[2].String(), "CHANGED") || !strings.HasPrefix(lines[2].String(), "0003 adopted "+files[2].Short()) {
		t.Errorf("status with a changed 0003 printed %q, want the recorded checksum and a CHANGED note", lines[2].String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the tracker table itself
// ─────────────────────────────────────────────────────────────────────────────

// TestTracker_Privileges pins what 0017 grants and refuses. As verify_app (the runtime login) the table is invisible (42501); verify_admin may read it
// and nothing else; verify_owner owns it; PUBLIC has nothing; the CHECKs and the primary key reject a malformed or duplicate row. The role switch is
// shown to bite in both directions (verify_app can read verify.cache; verify_admin can read the tracker), so the denials are not a broken switch.
func TestTracker_Privileges(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	conn := testConn(t, dsn)
	ctx := context.Background()

	if got := scalar[string](t, conn, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'verify.schema_migrations'::regclass`); got != "verify_owner" {
		t.Errorf("verify.schema_migrations is owned by %q, want verify_owner", got)
	}
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid = 'verify.schema_migrations'::regclass AND a.grantee = 0`); n != 0 {
		t.Errorf("PUBLIC holds %d privilege(s) on the tracker", n)
	}
	if got := scalar[string](t, conn, `SELECT COALESCE(string_agg(a.privilege_type, ',' ORDER BY a.privilege_type), '') FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a
	                                    WHERE c.oid = 'verify.schema_migrations'::regclass AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'verify_admin')`); got != "SELECT" {
		t.Errorf("verify_admin holds [%s] on the tracker, want exactly SELECT", got)
	}
	if got := scalar[string](t, conn, `SELECT obj_description('verify.schema_migrations'::regclass, 'pg_class')`); !strings.HasPrefix(got, "Migration tracker") {
		t.Errorf("the tracker has no COMMENT (got %q)", got)
	}

	asRole := func(role, sql string) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatalf("SET LOCAL ROLE %s: %v", role, err)
		}
		_, err = tx.Exec(ctx, sql)
		return err
	}
	const insert = `INSERT INTO verify.schema_migrations (version, checksum, how) VALUES `
	good := `('0001', repeat('a', 64), 'adopted')`

	if err := asRole("verify_app", `SELECT count(*) FROM verify.schema_migrations`); sqlState(err) != "42501" {
		t.Errorf("verify_app must be refused the tracker (42501), got %v", err)
	}
	if err := asRole("verify_app", insert+good); sqlState(err) != "42501" {
		t.Errorf("verify_app must not insert into the tracker (42501), got %v", err)
	}
	if err := asRole("verify_admin", `SELECT count(*) FROM verify.schema_migrations`); err != nil {
		t.Errorf("verify_admin must be able to read the tracker: %v", err)
	}
	for _, sql := range []string{insert + good, `DELETE FROM verify.schema_migrations`, `UPDATE verify.schema_migrations SET how = 'applied'`} {
		if err := asRole("verify_admin", sql); sqlState(err) != "42501" {
			t.Errorf("verify_admin is read-only on the tracker; %q gave %v", sql, err)
		}
	}
	if err := asRole("verify_app", `SELECT count(*) FROM verify.cache`); err != nil {
		t.Errorf("CONTROL: verify_app must still read verify.cache (so the denials above are about the tracker, not a broken role switch): %v", err)
	}
	if err := asRole("verify_owner", insert+good); err != nil {
		t.Errorf("verify_owner must be able to insert a well-formed row: %v", err)
	}
	for name, sql := range map[string]string{
		"a version that is not four digits": insert + `('abc', repeat('a', 64), 'applied')`,
		"a checksum that is not sha256 hex": insert + `('0001', 'XYZ', 'applied')`,
		"an unknown how":                    insert + `('0001', repeat('a', 64), 'maybe')`,
	} {
		if err := asRole("verify_owner", sql); sqlState(err) != "23514" {
			t.Errorf("%s must violate a CHECK (23514), got %v", name, err)
		}
	}
	if err := asRole("verify_owner", insert+`('0001', repeat('a', 64), 'applied'), ('0001', repeat('b', 64), 'adopted')`); sqlState(err) != "23505" {
		t.Errorf("a version recorded twice must violate the primary key (23505), got %v", err)
	}
}
