package migrate

// Tests of migration 0018 (per-job dedup and per-row identity of bulk jobs; owner rulings A1/A2) as part of the chain. They are in this package
// because they need the chain's own helpers (the runner, the catalog fingerprint, a database built through 0017). The store-level behaviour of the
// new tables and indexes (what a Postgres store does with them) is tested in internal/store/job_rows_test.go; the chain-level properties
// "fresh equals upgraded" and "twice is a no-op" for EVERY file, 0018 included, are TestMigrate_FreshEqualsUpgraded and TestMigrate_TwiceIsANoOp.
// What is here is what only 0018 needs:
//   - it is harmless under the old psql loop (applied again it changes nothing, and adds no row);
//   - it BACKFILLS job_rows for a job submitted before it existed (so that job's results do not vanish), once and only for jobs with no rows yet;
//   - it refuses, naming the missing piece, on a database where 0016 has not run;
//   - what it creates is owned by verify_owner, and verify_app holds exactly SELECT and INSERT on job_rows.
//
// Each test has a paired control that shows the check can fail.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/testpg"
)

const (
	legacyJob      = "01900000-0000-7000-8000-000000000001"
	legacyEmptyJob = "01900000-0000-7000-8000-000000000002"
	legacyOwner    = "0b0f4f2a-6d3c-4c53-9f3b-1b6a2c9d7e11"
)

// legacyItem is the id of the n-th work item of the legacy job; their order by id is their order by n.
func legacyItem(n int) string { return fmt.Sprintf("01900000-0000-7000-8000-0000000000a%d", n) }

// seedLegacyJobs puts into a database at 0017 what the pre-0018 code left behind: a job with three work items (two of which would have collapsed
// further duplicates under the old global rule: total_count 5 > 3 work items), a job with NO work items, and a job-less singular work item.
func seedLegacyJobs(t *testing.T, dsn string) {
	t.Helper()
	conn := testConn(t, dsn)
	dbExec(t, conn, `INSERT INTO verify.jobs (id, policy_key, policy_version, status, total_count, settled_count, owner_user_id)
	                 VALUES ($1, 'cold-email', 1, 'running', 5, 1, $3), ($2, 'cold-email', 1, 'running', 0, 0, $3)`, legacyJob, legacyEmptyJob, legacyOwner)
	for n := 1; n <= 3; n++ {
		dbExec(t, conn, `INSERT INTO verify.work_items (id, idempotency_key, job_id, domain, email_hash, policy_key, policy_version, config_fingerprint, status)
		                 VALUES ($1, decode($2, 'hex'), $3, 'ex.com', decode('00', 'hex'), 'cold-email', 1, 'cfg', $4)`,
			legacyItem(n), fmt.Sprintf("aa0%d", n), legacyJob, map[int]string{1: "settled", 2: "ready", 3: "dead"}[n])
	}
	dbExec(t, conn, `INSERT INTO verify.work_items (id, idempotency_key, job_id, domain, email_hash, policy_key, policy_version, config_fingerprint)
	                 VALUES ('01900000-0000-7000-8000-0000000000b1', decode('bb01', 'hex'), NULL, 'ex.com', decode('00', 'hex'), 'cold-email', 1, 'cfg')`)
}

// TestMigrate_0018BackfillsLegacyJobsOnceAndOnlyThem: a database at 0017 holding a legacy job is upgraded by the runner (which adopts 0001..0017
// and applies 0018): the legacy job gets one job_rows row per work item with ordinals 0..2 in work-item id order, no item_id, not malformed; the
// job with no work items and the job-less item get none; and applying 0018 again — which the old psql loop may do — adds nothing, not even for a
// job that gained rows of its own after the first run.
func TestMigrate_0018BackfillsLegacyJobsOnceAndOnlyThem(t *testing.T) {
	dir := testpg.MigrationsDir()
	files := mustLoad(t, dir)
	dsn := testpg.Scratch(t, 17)
	conn := testConn(t, dsn)
	if scalar[bool](t, conn, `SELECT to_regclass('verify.job_rows') IS NOT NULL`) {
		t.Fatal("fixture: the database already has verify.job_rows, so it does not predate 0018")
	}
	seedLegacyJobs(t, dsn)

	res := mustUp(t, dsn, dir)
	if want := (Result{Applied: len(files) - 17, Adopted: 17, Total: len(files)}); res != want {
		t.Fatalf("runner reported %+v, want %+v", res, want)
	}

	type row struct {
		Ordinal   int
		ItemID    *string
		Work      string
		Malformed bool
	}
	read := func(job string) []row {
		rows, err := conn.Query(context.Background(),
			`SELECT ordinal, item_id, COALESCE(work_item_id::text, ''), malformed FROM verify.job_rows WHERE job_id = $1 ORDER BY ordinal`, job)
		if err != nil {
			t.Fatalf("read job_rows: %v", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.Ordinal, &r.ItemID, &r.Work, &r.Malformed); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	got := read(legacyJob)
	if len(got) != 3 {
		t.Fatalf("the legacy job has %d job_rows, want one per work item (3): %+v", len(got), got)
	}
	for i, r := range got {
		if r.Ordinal != i || r.Work != legacyItem(i+1) || r.ItemID != nil || r.Malformed {
			t.Errorf("legacy row %d = %+v, want ordinal %d, work item %s, no item_id, not malformed", i, r, i, legacyItem(i+1))
		}
	}
	// A backfilled row is an ordinary addressable row: no LOG row of its own (its verdict is its work item's) and no duplicate pointer (the old code
	// never kept which rows collapsed, so none can be invented).
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.job_rows WHERE job_id = $1 AND verification_id IS NULL AND duplicate_of_ordinal IS NULL AND NOT malformed`, legacyJob); n != 3 {
		t.Errorf("%d of the 3 backfilled rows have NULL verification_id and duplicate_of_ordinal and malformed=false, want all 3", n)
	}
	if n := len(read(legacyEmptyJob)); n != 0 {
		t.Errorf("a job with no work items got %d row(s)", n)
	}
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.job_rows`); n != 3 {
		t.Errorf("job_rows holds %d row(s) in all, want 3 (the job-less work item must get none)", n)
	}

	// The legacy job is now readable the way the new code reads it: 3 rows, of which 2 have a terminal work item (settled, dead).
	if settled := scalar[int](t, conn, `SELECT count(*)::int FROM verify.job_rows r JOIN verify.work_items w ON w.id = r.work_item_id
	                                     WHERE r.job_id = $1 AND w.status IN ('settled','dead')`, legacyJob); settled != 2 {
		t.Errorf("legacy job: %d row(s) with a terminal work item, want 2", settled)
	}

	// A job that has rows of its own (what the new code writes) must not be touched when 0018 is applied again.
	const newJob = "01900000-0000-7000-8000-000000000003"
	dbExec(t, conn, `INSERT INTO verify.jobs (id, policy_key, policy_version, status, total_count, owner_user_id) VALUES ($1, 'cold-email', 1, 'running', 4, $2)`, newJob, legacyOwner)
	dbExec(t, conn, `INSERT INTO verify.work_items (id, idempotency_key, job_id, domain, email_hash, policy_key, policy_version, config_fingerprint)
	                 VALUES ('01900000-0000-7000-8000-0000000000c1', decode('cc01', 'hex'), $1, 'ex.com', decode('00', 'hex'), 'cold-email', 1, 'cfg')`, newJob)
	dbExec(t, conn, `INSERT INTO verify.job_rows (job_id, ordinal, item_id, work_item_id, malformed, verification_id, duplicate_of_ordinal) VALUES
	                 ($1, 0, 'mine-0', '01900000-0000-7000-8000-0000000000c1', false, NULL, NULL),
	                 ($1, 1, 'mine-1', '01900000-0000-7000-8000-0000000000c1', false, NULL, 0),
	                 ($1, 2, 'mine-2', NULL, true, '01900000-0000-7000-8000-0000000000d1', NULL)`, newJob)
	before := fingerprintOf(t, dsn)
	testpg.Apply(t, dsn, 18)
	testpg.Apply(t, dsn, 18)
	requireSameCatalog(t, "after 0018 once", "after 0018 three times", before, fingerprintOf(t, dsn))
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM verify.job_rows`); n != 6 {
		t.Errorf("after re-applying 0018 twice job_rows holds %d row(s), want 6 (3 legacy + 3 of the new job; nothing added)", n)
	}
	if rows := read(newJob); len(rows) != 3 || rows[0].ItemID == nil || *rows[0].ItemID != "mine-0" || !rows[2].Malformed {
		t.Errorf("the new job's own rows were altered: %+v", rows)
	}

	// CONTROL: the backfill really is conditional on "no rows yet". Remove the legacy job's rows and apply 0018 again: they come back. If the
	// guard were absent (or the backfill never ran) one of the two checks above or this one would be wrong.
	dbExec(t, conn, `DELETE FROM verify.job_rows WHERE job_id = $1`, legacyJob)
	testpg.Apply(t, dsn, 18)
	if n := len(read(legacyJob)); n != 3 {
		t.Errorf("CONTROL: with the legacy job's rows removed, applying 0018 must backfill them again, got %d", n)
	}
	if n := len(read(newJob)); n != 3 {
		t.Errorf("CONTROL: the new job (which has rows) must still be left alone, got %d row(s)", n)
	}
}

// TestMigrate_0018NeedsTheEarlierMigrationsAndSaysSo: on an empty database the file stops with SQLSTATE 55000 naming 0009 and 0016 (it must not
// half-run), and on a database at 0017 it applies. The second half is the control for the first.
func TestMigrate_0018NeedsTheEarlierMigrationsAndSaysSo(t *testing.T) {
	files := mustLoad(t, testpg.MigrationsDir())
	var sql string
	for _, f := range files {
		if f.Version == "0018" {
			sql = f.SQL
		}
	}
	if sql == "" {
		t.Fatal("there is no migration 0018 in the directory")
	}
	_, err := testConn(t, testpg.Empty(t)).Exec(context.Background(), sql)
	if sqlState(err) != "55000" || !strings.Contains(fmt.Sprint(err), "must run first") {
		t.Errorf("0018 on an empty database must fail with SQLSTATE 55000 saying what must run first, got %v", err)
	}
	if _, err := testConn(t, testpg.Scratch(t, 17)).Exec(context.Background(), sql); err != nil {
		t.Errorf("CONTROL: 0018 on a database at 0017 must apply: %v", err)
	}
}

// TestMigrate_0018OwnershipAndPrivileges: verify.job_rows and its indexes are owned by verify_owner (a table created by the migrating role and not
// handed over is the defect 0016's self-check exists for), PUBLIC holds nothing on it, verify_app holds exactly SELECT and INSERT, verify_admin holds
// everything. The control reads the same facts for a table that does not follow the rule (the tracker: verify_app has nothing on it).
func TestMigrate_0018OwnershipAndPrivileges(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	conn := testConn(t, dsn)

	if got := scalar[string](t, conn, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'verify.job_rows'::regclass`); got != "verify_owner" {
		t.Errorf("verify.job_rows is owned by %q, want verify_owner", got)
	}
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM pg_class c WHERE c.relnamespace = 'verify'::regnamespace AND c.relkind IN ('i','I')
	                               AND c.oid IN (SELECT indexrelid FROM pg_index WHERE indrelid = 'verify.job_rows'::regclass)
	                               AND pg_get_userbyid(c.relowner) <> 'verify_owner'`); n != 0 {
		t.Errorf("%d index(es) of verify.job_rows are not owned by verify_owner", n)
	}
	if n := scalar[int](t, conn, `SELECT count(*)::int FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid = 'verify.job_rows'::regclass AND a.grantee = 0`); n != 0 {
		t.Errorf("PUBLIC holds %d privilege(s) on verify.job_rows", n)
	}
	privs := func(role, table string) string { // table is a constant of this test, never input
		return scalar[string](t, conn, fmt.Sprintf(`SELECT COALESCE(string_agg(a.privilege_type, ',' ORDER BY a.privilege_type), '') FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a
		                                 WHERE c.oid = '%s'::regclass AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)`, table), role)
	}
	if got := privs("verify_app", "verify.job_rows"); got != "INSERT,SELECT" {
		t.Errorf("verify_app holds [%s] on verify.job_rows, want exactly INSERT,SELECT", got)
	}
	if got := privs("verify_admin", "verify.job_rows"); !strings.Contains(got, "DELETE") || !strings.Contains(got, "SELECT") || !strings.Contains(got, "UPDATE") {
		t.Errorf("verify_admin holds [%s] on verify.job_rows, want every table privilege", got)
	}
	// CONTROL: the helper can say "nothing": verify_app has no privilege on the tracker.
	if got := privs("verify_app", "verify.schema_migrations"); got != "" {
		t.Errorf("CONTROL: verify_app holds [%s] on the tracker, want none (so an empty answer is possible)", got)
	}
}
