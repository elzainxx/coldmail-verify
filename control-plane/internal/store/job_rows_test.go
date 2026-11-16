package store

// Tests for the per-job dedup scope and the per-row identity of bulk jobs (migration 0018; owner rulings A1/A2 and the Door-2 / duplicate_of
// follow-ups): what a store does with a job's rows, the LOG row of every malformed row, the duplicate pointers, the ordered listing and its
// cursor, the status counts, the grants, and the chunking of a 100,000-row write.
//
// Every test here that touches a store runs against BOTH — the Mem fake always, and real Postgres in a THROWAWAY database (scratchPG: it needs
// superuser rights to create the database and SKIPS honestly without TM_TEST_DATABASE_URL, like every PG-gated test in this package) — because a
// fake laxer than the real store is how D1 stayed invisible. A scratch database also means the drainer-style DequeueReady below can only ever
// lease THIS test's items, and that a count of LOG rows is exact.
//
// Every property that could pass by accident has a PAIRED CONTROL in the same file that makes the very same check fail:
//   - the cross-job scope scenario is re-run under the OLD global scope (a Mem wrapper that deduplicates across jobs; for Postgres, the old
//     global unique index re-created next to the new ones) and must report the violation;
//   - the "one LOG row per malformed row, served from it" scenario is re-run over a store that behaves as before (malformed rows neither
//     logged nor recorded) and must report it;
//   - the pagination walker is run over a cursor taken from the LOOK-AHEAD row (M2's bug, M2-G5) and over an unsorted listing, and must report a
//     missing ordinal and an out-of-order ordinal;
//   - the status counts are compared with the old per-work-item counter, which stays stuck above zero on the very same job;
//   - the statement-limit checker is fed one giant statement and must object.

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var scopeNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

const (
	scopeOwnerA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	scopeOwnerB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// forEachStore runs body once against a fresh Mem fake and once against real Postgres in a scratch database.
func forEachStore(t *testing.T, body func(t *testing.T, st Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { body(t, NewMem(func() time.Time { return scopeNow })) })
	t.Run("pg", func(t *testing.T) {
		pg, _ := scratchPG(t, 0)
		body(t, pg)
	})
}

func intp(i int) *int { return &i }

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// newScopeJob creates a running job for owner with `total` input rows and NO recorded rows (the scope tests that only enqueue).
func newScopeJob(t *testing.T, st Store, owner string, total int) string {
	t.Helper()
	id := mustV7(t, scopeNow)
	if _, err := st.CreateJob(context.Background(), JobRow{ID: id, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: total, CallerRef: "ref", OwnerUserID: owner}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return id
}

// recordJob records a job with its rows and the LOG rows of its malformed rows through RecordJob (the one door a bulk job goes in by).
func recordJob(t *testing.T, st Store, jobID, owner string, rows []JobRowEntry, rejects []VerificationRow) {
	t.Helper()
	if err := st.RecordJob(context.Background(), JobRow{ID: jobID, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: len(rows), CallerRef: "ref", OwnerUserID: owner}, rows, rejects); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
}

// rejectRow is the LOG row of one malformed input, as the sync path writes an un-addressable address (orchestrator.computeReject via
// verdict.Assemble): invalid / invalid_syntax / skip, confidence 99, flags {from_cache:false}, signals {syntax_valid:false}, NO email_hash and NO
// hash_scheme (§3.2.7), no domain, no engine provenance. (Built by hand here because package verdict imports package store.)
func rejectRow(t *testing.T) VerificationRow {
	t.Helper()
	return VerificationRow{
		ID: mustV7(t, scopeNow), RequestID: mustV7(t, scopeNow), CheckedAt: scopeNow, CreatedAt: scopeNow,
		Status: "invalid", Reason: "invalid_syntax", Confidence: 99, Recommendation: "skip",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": false},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
	}
}

// logCount is the number of LOG rows in a store that holds nothing else of the kind: Mem's append-only Log, or a scratch Postgres's verifications.
func logCount(t *testing.T, st Store) int {
	t.Helper()
	switch s := st.(type) {
	case *MemStore:
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.Log)
	case *PG:
		var n int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM verify.verifications`).Scan(&n); err != nil {
			t.Fatalf("count verifications: %v", err)
		}
		return n
	}
	t.Fatalf("logCount: unsupported store %T (pass the base store, not a wrapper)", st)
	return 0
}

// scopeItem builds a ready work item holding idempotency key `key` in job `job` ("" = a job-less singular check).
func scopeItem(t *testing.T, job, key string) WorkItemRow {
	t.Helper()
	return WorkItemRow{
		ID: mustV7(t, scopeNow), IdempotencyKey: []byte(key), JobID: job, Domain: "ex.com", EmailHash: []byte("hash-" + key),
		PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg", Status: "ready",
		NotBefore: scopeNow.Add(-time.Minute), CreatedAt: scopeNow, AddrCiphertext: []byte("ct-" + key),
	}
}

// settleAll leases every ready item and settles each one as `settled` with its own verdict row, as a drainer would. It returns work item id ->
// verification id.
func settleAll(t *testing.T, st Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	leased, err := st.DequeueReady(ctx, "w-scope", 1000, scopeNow)
	if err != nil {
		t.Fatalf("DequeueReady: %v", err)
	}
	out := map[string]string{}
	for _, it := range leased {
		v := rtRow(t, scopeNow, it.Domain, it.EmailHash)
		if err := st.SettleWorkItem(ctx, it.ID, "w-scope", "settled", v, nil, WorkItemAttemptRow{}, nil); err != nil {
			t.Fatalf("SettleWorkItem(%s): %v", it.ID, err)
		}
		out[it.ID] = v.ID
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// A2: the dedup scope is the JOB
// ─────────────────────────────────────────────────────────────────────────────

// crossJobScopeViolations is the owner's exit scenario for A2, written as a function that RETURNS what is wrong so the same code can be the check and,
// pointed at the old behaviour, the paired control: tenant A's job is submitted and FULLY SETTLED ("yesterday"), then tenant B submits the same
// address (same policy, same config) in a second job. B must get its own work item and its own result; its job must not be empty.
func crossJobScopeViolations(t *testing.T, st Store) []string {
	t.Helper()
	ctx := context.Background()
	jobA, jobB := mustV7(t, scopeNow), mustV7(t, scopeNow)

	a := scopeItem(t, jobA, "the-same-address")
	recordJob(t, st, jobA, scopeOwnerA, []JobRowEntry{{Ordinal: 0, WorkItemID: a.ID}}, nil)
	if id, existed, err := st.EnqueueWorkItem(ctx, a); err != nil || existed || id != a.ID {
		return []string{fmt.Sprintf("fixture: tenant A's first enqueue returned id=%q existed=%v err=%v", id, existed, err)}
	}
	settleAll(t, st)
	if complete, _, _, err := st.JobCompletion(ctx, jobA, scopeOwnerA); err != nil || !complete {
		return []string{fmt.Sprintf("fixture: tenant A's job must be fully settled first (complete=%v err=%v)", complete, err)}
	}

	var v []string
	b := scopeItem(t, jobB, "the-same-address")
	recordJob(t, st, jobB, scopeOwnerB, []JobRowEntry{{Ordinal: 0, ItemID: "b-row", WorkItemID: b.ID}}, nil)
	id, existed, err := st.EnqueueWorkItem(ctx, b)
	if err != nil || existed || id != b.ID {
		return append(v, fmt.Sprintf("tenant B's enqueue of an address tenant A already verified yielded no work item (id=%q existed=%v err=%v)", id, existed, err))
	}
	settleAll(t, st)
	rows, _, err := st.ListJobResults(ctx, jobB, scopeOwnerB, "", 10)
	switch {
	case err != nil:
		v = append(v, fmt.Sprintf("tenant B's results: %v", err))
	case len(rows) != 1:
		v = append(v, fmt.Sprintf("tenant B's job has %d result(s), want 1: a new job must yield a result for every row", len(rows)))
	case rows[0].Verdict.ID == "" || rows[0].WorkItemID != b.ID || rows[0].ItemID != "b-row":
		v = append(v, fmt.Sprintf("tenant B's result is not B's own: %+v", rows[0]))
	}
	// And tenant A cannot read it (the owner scope is untouched by any of this).
	if rows, _, err := st.ListJobResults(ctx, jobB, scopeOwnerA, "", 10); err != nil || len(rows) != 0 {
		v = append(v, fmt.Sprintf("tenant A read %d row(s) of tenant B's job (err=%v)", len(rows), err))
	}
	return v
}

// TestEnqueueScope_ASettledJobNeverBlocksAnotherTenantsJob: tenant B gets a result for an address tenant A verified yesterday.
func TestEnqueueScope_ASettledJobNeverBlocksAnotherTenantsJob(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		if v := crossJobScopeViolations(t, st); len(v) != 0 {
			t.Fatalf("A2 (no dedup across jobs or tenants) is violated:\n  %s", strings.Join(v, "\n  "))
		}
	})
}

// globalScopeStore re-creates the OLD behaviour in a wrapper: the idempotency key is unique across ALL jobs and owners (0009's global UNIQUE with
// ON CONFLICT DO NOTHING), whatever the underlying store does.
type globalScopeStore struct {
	Store
	holder map[string]string // hex(idempotency_key) -> work item id, across every job
}

func (g *globalScopeStore) EnqueueWorkItem(ctx context.Context, item WorkItemRow) (string, bool, error) {
	k := hex.EncodeToString(item.IdempotencyKey)
	if id, ok := g.holder[k]; ok {
		return id, true, nil
	}
	id, existed, err := g.Store.EnqueueWorkItem(ctx, item)
	if err == nil && !existed {
		g.holder[k] = id
	}
	return id, existed, err
}

// TestEnqueueScope_ControlTheOldGlobalScopeFailsTheSameScenario is the paired control for the test above: the SAME scenario function, run against
// the old global scope, must report the violation, so the scenario can fail. For Mem the old scope is a wrapper; for Postgres it is the old
// global unique index re-created on the migrated table (the migration dropped it), after which tenant B's enqueue is refused by the database.
func TestEnqueueScope_ControlTheOldGlobalScopeFailsTheSameScenario(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		st := &globalScopeStore{Store: NewMem(func() time.Time { return scopeNow }), holder: map[string]string{}}
		v := crossJobScopeViolations(t, st)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "yielded no work item") {
			t.Fatalf("CONTROL: under the old global scope the scenario must report that tenant B got no work item, got %q", v)
		}
	})
	t.Run("pg", func(t *testing.T) {
		pg, _ := scratchPG(t, 0)
		if _, err := pg.pool.Exec(context.Background(), `CREATE UNIQUE INDEX zz_old_global_scope ON verify.work_items (idempotency_key)`); err != nil {
			t.Fatalf("re-create the old global unique index: %v", err)
		}
		v := crossJobScopeViolations(t, pg)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "yielded no work item") {
			t.Fatalf("CONTROL: with the old global unique index back, tenant B's enqueue must fail and the scenario must say so, got %q", v)
		}
	})
}

// TestEnqueueScope_WithinAJobAndForJoblessChecks pins the other half of the scope: inside ONE job the same key is a duplicate that names the
// work item holding it (so a duplicate row can point at it), a job-less singular check still dedups on its key alone (the old semantics), and
// neither blocks the other.
func TestEnqueueScope_WithinAJobAndForJoblessChecks(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		job1 := newScopeJob(t, st, scopeOwnerA, 2)
		job2 := newScopeJob(t, st, scopeOwnerA, 1) // the SAME owner: the scope is the job, not the tenant

		first, second := scopeItem(t, job1, "k"), scopeItem(t, job1, "k")
		if id, existed, err := st.EnqueueWorkItem(ctx, first); err != nil || existed || id != first.ID {
			t.Fatalf("first enqueue in job1: id=%q existed=%v err=%v", id, existed, err)
		}
		if id, existed, err := st.EnqueueWorkItem(ctx, second); err != nil || !existed || id != first.ID {
			t.Fatalf("the same key in the SAME job must be a duplicate naming the holder (%s), got id=%q existed=%v err=%v", first.ID, id, existed, err)
		}
		other := scopeItem(t, job2, "k")
		if id, existed, err := st.EnqueueWorkItem(ctx, other); err != nil || existed || id != other.ID {
			t.Fatalf("the same key in ANOTHER job (same owner) must get its own work item, got id=%q existed=%v err=%v", id, existed, err)
		}

		n1, n2 := scopeItem(t, "", "k"), scopeItem(t, "", "k")
		if id, existed, err := st.EnqueueWorkItem(ctx, n1); err != nil || existed || id != n1.ID {
			t.Fatalf("a job-less check must not be blocked by job-owned items holding the key, got id=%q existed=%v err=%v", id, existed, err)
		}
		if id, existed, err := st.EnqueueWorkItem(ctx, n2); err != nil || !existed || id != n1.ID {
			t.Fatalf("two job-less checks with one key must still dedup (the singular semantics), got id=%q existed=%v err=%v", id, existed, err)
		}
	})
}

// TestPGEnqueueScope_TheOldConstraintIsGoneAndTheNewIndexesAreThere pins the catalog half of 0018 on a database the whole chain built: the global
// constraint named in 0009 is gone, no global unique index on idempotency_key exists, and the two partial per-job indexes do.
func TestPGEnqueueScope_TheOldConstraintIsGoneAndTheNewIndexesAreThere(t *testing.T) {
	pg, _ := scratchPG(t, 0)
	ctx := context.Background()
	var oldConstraint int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM pg_constraint WHERE conrelid = 'verify.work_items'::regclass AND conname = 'work_items_idempotency_key_key'`).Scan(&oldConstraint); err != nil {
		t.Fatal(err)
	}
	if oldConstraint != 0 {
		t.Error("the global UNIQUE constraint work_items_idempotency_key_key still exists")
	}
	const globalIdx = `
		SELECT count(*)::int FROM pg_index i
		WHERE i.indrelid = 'verify.work_items'::regclass AND i.indisunique AND i.indpred IS NULL AND i.indnatts = 1
		  AND i.indkey[0] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'verify.work_items'::regclass AND attname = 'idempotency_key')`
	var global int
	if err := pg.pool.QueryRow(ctx, globalIdx).Scan(&global); err != nil {
		t.Fatal(err)
	}
	if global != 0 {
		t.Errorf("%d global unique index(es) on idempotency_key remain", global)
	}
	for _, name := range []string{"uq_work_items_job_idem", "uq_work_items_unjobbed_idem"} {
		var def string
		if err := pg.pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`, "verify."+name).Scan(&def); err != nil {
			t.Fatalf("index %s is missing: %v", name, err)
		}
		if !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "WHERE") {
			t.Errorf("index %s is not a partial unique index: %s", name, def)
		}
	}
	// CONTROL: the check above reads what is there. Re-creating the global index must make the same query see it.
	if _, err := pg.pool.Exec(ctx, `CREATE UNIQUE INDEX zz_control_global ON verify.work_items (idempotency_key)`); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, globalIdx).Scan(&global); err != nil {
		t.Fatal(err)
	}
	if global != 1 {
		t.Errorf("CONTROL: with a global unique index re-created the query must count 1, got %d", global)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// A1: the rows of a job, and Door 2 for its malformed rows
// ─────────────────────────────────────────────────────────────────────────────

// TestRecordJob_BothStoresRefuseWhatTheConstraintsRefuse: every batch below breaks one rule of verify.job_rows (or of the link between a job's rows
// and the LOG rows it writes), and BOTH the Mem fake and Postgres must refuse it and store NOTHING of it — not the job, not a row, and NOT THE
// LOG ROWS the refused batch carried (all-or-nothing is the Door-2 half: a LOG row nobody points at would be an orphan). To prove nothing
// remained, the corrected batch is then recorded under the SAME job id and the SAME reject id (a leftover would collide on a primary key), and the
// LOG count is exact at every step. On Postgres each database-level refusal also names the right SQLSTATE (the database, not the Go code, is the
// backstop); the rows of a batch that only the Go link check can refuse carry no SQLSTATE. The good batch is the control: both stores accept it.
func TestRecordJob_BothStoresRefuseWhatTheConstraintsRefuse(t *testing.T) {
	w1, w2 := "018f0000-0000-7000-8000-0000000000a1", "018f0000-0000-7000-8000-0000000000a2"
	type refusal struct {
		name    string
		rows    func(r VerificationRow) []JobRowEntry
		rejects func(r VerificationRow) []VerificationRow
		code    string // the SQLSTATE Postgres must answer with; "" = refused by the Go link check before the database is asked
	}
	one := func(r VerificationRow) []VerificationRow { return []VerificationRow{r} }
	none := func(VerificationRow) []VerificationRow { return nil }
	cases := []refusal{
		{"a negative ordinal", func(VerificationRow) []JobRowEntry { return []JobRowEntry{{Ordinal: -1, WorkItemID: w1}} }, none, "23514"},
		{"a malformed row that carries a work item", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, Malformed: true, WorkItemID: w1, VerificationID: r.ID}}
		}, one, "23514"},
		{"an addressable row with no work item", func(VerificationRow) []JobRowEntry { return []JobRowEntry{{Ordinal: 0}} }, none, "23514"},
		{"a malformed row with NO LOG row (the Door-2 breach)", func(VerificationRow) []JobRowEntry { return []JobRowEntry{{Ordinal: 0, Malformed: true}} }, none, "23514"},
		{"an addressable row that also names a LOG row", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, WorkItemID: w1, VerificationID: r.ID}}
		}, one, "23514"},
		{"an item_id over 200 bytes", func(VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, ItemID: strings.Repeat("x", 201), WorkItemID: w1}}
		}, none, "23514"},
		{"the same ordinal twice", func(VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, WorkItemID: w1}, {Ordinal: 0, WorkItemID: w2}}
		}, none, "23505"},
		{"the same item_id twice in one job", func(VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, ItemID: "dup", WorkItemID: w1}, {Ordinal: 1, ItemID: "dup", WorkItemID: w2}}
		}, none, "23505"},
		{"duplicate_of that is not an EARLIER ordinal", func(VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, WorkItemID: w1}, {Ordinal: 1, WorkItemID: w1, DuplicateOf: intp(1)}}
		}, none, "23514"},
		{"duplicate_of on a malformed row", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, WorkItemID: w1}, {Ordinal: 1, Malformed: true, VerificationID: r.ID, DuplicateOf: intp(0)}}
		}, one, "23514"},
		// A good malformed row, then a bad row: the LOG row of the first must roll back with the rest.
		{"a good malformed row followed by a bad row", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: r.ID}, {Ordinal: 1}}
		}, one, "23514"},
		// Refused by the link check (the database holds no foreign key to verifications, by design).
		{"a row naming a LOG row that is not being written", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: r.ID}}
		}, none, ""},
		{"a LOG row that no row names", func(VerificationRow) []JobRowEntry { return []JobRowEntry{{Ordinal: 0, WorkItemID: w1}} }, one, ""},
		{"two rows naming the same LOG row", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: r.ID}, {Ordinal: 1, Malformed: true, VerificationID: r.ID}}
		}, one, ""},
		// The same LOG row handed over twice, with a row that names it: the Mem fake used to store both copies (one id twice in the append-only
		// LOG) while Postgres refused on verifications_pkey. A store must not accept what the database cannot hold.
		{"the same LOG row written twice", func(r VerificationRow) []JobRowEntry {
			return []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: r.ID}}
		}, func(r VerificationRow) []VerificationRow { return []VerificationRow{r, r} }, ""},
	}
	forEachStore(t, func(t *testing.T, st Store) {
		_, isPG := st.(*PG)
		ctx := context.Background()
		wantLog := 0
		for _, c := range cases {
			job := mustV7(t, scopeNow)
			r := rejectRow(t)
			err := st.RecordJob(ctx, JobRow{ID: job, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: 2, OwnerUserID: scopeOwnerA}, c.rows(r), c.rejects(r))
			if err == nil {
				t.Errorf("%s: the batch was accepted", c.name)
				wantLog = logCount(t, st) // keep the running count honest for the next case
				continue
			}
			if isPG && c.code != "" && pgCode(err) != c.code {
				t.Errorf("%s: Postgres answered SQLSTATE %q (%v), want %s", c.name, pgCode(err), err, c.code)
			}
			if _, known, gerr := st.GetJob(ctx, job, scopeOwnerA); gerr != nil || known {
				t.Errorf("%s: the refused batch left its JOB behind (known=%v err=%v)", c.name, known, gerr)
			}
			if got := logCount(t, st); got != wantLog {
				t.Errorf("%s: the LOG holds %d row(s) after the refusal, want %d: a refused batch must not leave a LOG row behind", c.name, got, wantLog)
			}
			// The corrected batch, under the SAME job id and the SAME reject id: a leftover job or LOG row would collide on its primary key.
			fixed := []JobRowEntry{{Ordinal: 0, ItemID: "ok-a", WorkItemID: w1}, {Ordinal: 1, ItemID: "ok-b", Malformed: true, VerificationID: r.ID}}
			if err := st.RecordJob(ctx, JobRow{ID: job, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: 2, OwnerUserID: scopeOwnerA}, fixed, one(r)); err != nil {
				t.Errorf("%s: after the refusal the corrected batch could not be recorded (the refused one left something behind): %v", c.name, err)
				continue
			}
			wantLog++
			if got := logCount(t, st); got != wantLog {
				t.Errorf("%s: the LOG holds %d row(s) after the corrected batch, want %d", c.name, got, wantLog)
			}
		}

		// CONTROL: a batch that breaks no rule is accepted — so the refusals above are about each rule and not a broken store. It is the shape a
		// real job has: a label, duplicates sharing a work item, unlabelled rows, a label of exactly 200 bytes, a malformed row with its LOG row.
		good := []JobRowEntry{
			{Ordinal: 0, ItemID: "a", WorkItemID: w1},
			{Ordinal: 1, WorkItemID: w1, DuplicateOf: intp(0)},
			{Ordinal: 2, WorkItemID: w2},
			{Ordinal: 3, ItemID: "m", Malformed: true, VerificationID: "to-be-set"},
			{Ordinal: 4, ItemID: strings.Repeat("y", 200), WorkItemID: w2, DuplicateOf: intp(2)},
		}
		r := rejectRow(t)
		good[3].VerificationID = r.ID
		job := mustV7(t, scopeNow)
		jr := JobRow{ID: job, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: len(good), OwnerUserID: scopeOwnerA}
		if err := st.RecordJob(ctx, jr, good, one(r)); err != nil {
			t.Fatalf("CONTROL: a batch that breaks no rule was refused: %v", err)
		}
		if got := logCount(t, st); got != wantLog+1 {
			t.Errorf("the good batch left %d LOG row(s), want %d", got, wantLog+1)
		}
		// The same job id again is refused (jobs_pkey) and adds nothing.
		if err := st.RecordJob(ctx, jr, good[:1], nil); err == nil {
			t.Error("recording the same job id twice was accepted")
		}
		// A label is unique per JOB, not globally: the same item_id in another job is fine.
		other := mustV7(t, scopeNow)
		if err := st.RecordJob(ctx, JobRow{ID: other, PolicyKey: "cold-email", PolicyVersion: 1, TotalCount: 1, OwnerUserID: scopeOwnerB}, []JobRowEntry{{Ordinal: 0, ItemID: "a", WorkItemID: w1}}, nil); err != nil {
			t.Errorf("CONTROL: the same item_id in a different job must be accepted: %v", err)
		}
	})
}

// settleRowsFixture builds the job used by the listing/pagination/count tests below: 13 input rows over 6 distinct work items and 2 malformed rows,
//
//	ordinal: 0  1  2 | 3 | 4(bad) | 5  6 | 7 | 8  9  10 | 11(bad) | 12
//	work:    W1 W1 W1  W2          W3 W3  W4  W5 W5 W5            W6
//	dup of:     0  0                  5          8  8
//
// so there are runs of duplicate rows (0-2, 5-6, 8-10) for a page boundary to fall inside, and each duplicate row names the FIRST row of its run.
// Each malformed row has its own LOG row. Every work item is enqueued but NOT yet settled; the caller settles them (settleAll) when it wants the
// results to exist. Returns the job id, the rows as recorded, and the LOG rows written.
func settleRowsFixture(t *testing.T, st Store) (string, []JobRowEntry, []VerificationRow) {
	t.Helper()
	ctx := context.Background()
	job := mustV7(t, scopeNow)
	w := map[string]WorkItemRow{}
	for _, k := range []string{"W1", "W2", "W3", "W4", "W5", "W6"} {
		w[k] = scopeItem(t, job, k)
	}
	r4, r11 := rejectRow(t), rejectRow(t)
	rows := []JobRowEntry{
		{Ordinal: 0, ItemID: "r0", WorkItemID: w["W1"].ID},
		{Ordinal: 1, ItemID: "r1", WorkItemID: w["W1"].ID, DuplicateOf: intp(0)},
		{Ordinal: 2, ItemID: "r2", WorkItemID: w["W1"].ID, DuplicateOf: intp(0)},
		{Ordinal: 3, ItemID: "r3", WorkItemID: w["W2"].ID},
		{Ordinal: 4, ItemID: "r4", Malformed: true, VerificationID: r4.ID},
		{Ordinal: 5, ItemID: "r5", WorkItemID: w["W3"].ID},
		{Ordinal: 6, ItemID: "r6", WorkItemID: w["W3"].ID, DuplicateOf: intp(5)},
		{Ordinal: 7, ItemID: "r7", WorkItemID: w["W4"].ID},
		{Ordinal: 8, ItemID: "r8", WorkItemID: w["W5"].ID},
		{Ordinal: 9, ItemID: "r9", WorkItemID: w["W5"].ID, DuplicateOf: intp(8)},
		{Ordinal: 10, ItemID: "r10", WorkItemID: w["W5"].ID, DuplicateOf: intp(8)},
		{Ordinal: 11, ItemID: "r11", Malformed: true, VerificationID: r11.ID},
		{Ordinal: 12, ItemID: "r12", WorkItemID: w["W6"].ID},
	}
	rejects := []VerificationRow{r4, r11}
	// The enqueuer's order of writes: the job, its rows and the malformed rows' LOG rows in one transaction, then the work items.
	recordJob(t, st, job, scopeOwnerA, rows, rejects)
	for _, k := range []string{"W1", "W2", "W3", "W4", "W5", "W6"} {
		if id, existed, err := st.EnqueueWorkItem(ctx, w[k]); err != nil || existed || id != w[k].ID {
			t.Fatalf("enqueue %s: id=%q existed=%v err=%v", k, id, existed, err)
		}
	}
	return job, rows, rejects
}

// TestListJobResults_OneResultPerRowInOrdinalOrderWithItsIdentity: after every work item settles, the listing holds ONE result per input row — the
// duplicate rows too, each with its own ordinal and item_id, the SAME verdict as the row it shares a work item with, and a duplicate_of naming the
// first row of its run — ordered by ordinal; the malformed rows are in it flagged as such, with no work item and their OWN LOG row as the verdict.
func TestListJobResults_OneResultPerRowInOrdinalOrderWithItsIdentity(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		job, rows, rejects := settleRowsFixture(t, st)
		verdictOf := settleAll(t, st)

		got, next, err := st.ListJobResults(ctx, job, scopeOwnerA, "", 100)
		if err != nil || next != "" {
			t.Fatalf("ListJobResults: next=%q err=%v", next, err)
		}
		if len(got) != len(rows) {
			t.Fatalf("got %d results for %d input rows: a complete job must yield one result per row", len(got), len(rows))
		}
		for i, want := range rows {
			r := got[i]
			if r.Ordinal != want.Ordinal || r.ItemID != want.ItemID || r.Malformed != want.Malformed || r.WorkItemID != want.WorkItemID || !sameIntPtr(r.DuplicateOf, want.DuplicateOf) {
				t.Fatalf("result %d = {ordinal %d item %q malformed %v work %q dup %v}, want the row {%d %q %v %q %v}: results must come back in ordinal order carrying the row's identity",
					i, r.Ordinal, r.ItemID, r.Malformed, r.WorkItemID, r.DuplicateOf, want.Ordinal, want.ItemID, want.Malformed, want.WorkItemID, want.DuplicateOf)
			}
			if want.Malformed {
				// Served FROM the row's own LOG row, like every other result.
				if r.Verdict.ID != want.VerificationID || r.Verdict.Status != "invalid" || r.Verdict.Reason != "invalid_syntax" || r.Verdict.Recommendation != "skip" {
					t.Errorf("malformed row %d carries verdict %+v, want its LOG row %s (invalid/invalid_syntax/skip)", i, r.Verdict, want.VerificationID)
				}
				continue
			}
			if r.Verdict.ID == "" || r.Verdict.ID != verdictOf[want.WorkItemID] {
				t.Errorf("row %d carries verdict %q, want its work item's verdict %q", i, r.Verdict.ID, verdictOf[want.WorkItemID])
			}
		}
		if rejects[0].ID == rejects[1].ID {
			t.Fatal("fixture: the two malformed rows must have two LOG rows")
		}
		// Rows 0, 1 and 2 are one work item: ONE verdict, three results; rows 1 and 2 say they repeat row 0.
		if got[0].Verdict.ID != got[1].Verdict.ID || got[1].Verdict.ID != got[2].Verdict.ID {
			t.Errorf("rows 0-2 share a work item and must share its verdict, got %q %q %q", got[0].Verdict.ID, got[1].Verdict.ID, got[2].Verdict.ID)
		}
		if got[0].DuplicateOf != nil || got[1].DuplicateOf == nil || *got[1].DuplicateOf != 0 || got[2].DuplicateOf == nil || *got[2].DuplicateOf != 0 {
			t.Errorf("duplicate_of of rows 0, 1, 2 = %v, %v, %v, want nil, 0, 0 (the FIRST row of the run, not the previous one)", got[0].DuplicateOf, got[1].DuplicateOf, got[2].DuplicateOf)
		}
		if got[2].Verdict.ID == got[3].Verdict.ID {
			t.Error("CONTROL: rows of DIFFERENT work items must not share a verdict (W1 vs W2)")
		}
	})
}

// TestListJobResults_OnlyRowsWithAnOutcomeAppear: a row appears once its work item is settled, or at once when it is malformed (its LOG row was
// written with the job); the rest wait.
func TestListJobResults_OnlyRowsWithAnOutcomeAppear(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		job, _, rejects := settleRowsFixture(t, st)

		// Nothing has settled: only the two malformed rows (ordinals 4 and 11) have an outcome, each its own LOG row.
		before, _, err := st.ListJobResults(ctx, job, scopeOwnerA, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != 2 || before[0].Ordinal != 4 || before[1].Ordinal != 11 || !before[0].Malformed || !before[1].Malformed ||
			before[0].Verdict.ID != rejects[0].ID || before[1].Verdict.ID != rejects[1].ID {
			t.Fatalf("before any work item settles the listing must hold exactly the malformed rows 4 and 11 with their LOG rows, got %+v", before)
		}
		settleAll(t, st)
		after, _, err := st.ListJobResults(ctx, job, scopeOwnerA, "", 100)
		if err != nil || len(after) != 13 {
			t.Fatalf("after settling: %d results err=%v, want 13", len(after), err)
		}
	})
}

// noLogStore is the OLD behaviour of a bulk submission, as a store: a malformed row was neither logged nor recorded, so it vanished. RecordJob
// drops every malformed row and every LOG row before delegating.
type noLogStore struct{ Store }

func (n noLogStore) RecordJob(ctx context.Context, job JobRow, rows []JobRowEntry, _ []VerificationRow) error {
	var kept []JobRowEntry
	for _, r := range rows {
		if !r.Malformed {
			kept = append(kept, r)
		}
	}
	return n.Store.RecordJob(ctx, job, kept, nil)
}

// malformedLoggingViolations is the Door-2 exit scenario, written as a function that RETURNS what is wrong so it can be the check and, pointed at
// the old behaviour (noLogStore), the paired control. A job of five rows — three of them malformed, two of those the SAME garbage twice — is
// recorded through `write` (the store under test, possibly a wrapper); `base` is the store that actually holds the data and is read back. It
// requires: EXACTLY one new LOG row per malformed row (no more for a repeated address, none for an addressable row); each of them the sync path's
// invalid_syntax row with a NULL hash and from_cache false; and every malformed row served in the listing FROM its own LOG row, in ordinal order.
func malformedLoggingViolations(t *testing.T, base, write Store) []string {
	t.Helper()
	ctx := context.Background()
	job := mustV7(t, scopeNow)
	w := scopeItem(t, job, "good-address")
	r1, r2, r3 := rejectRow(t), rejectRow(t), rejectRow(t)
	rows := []JobRowEntry{
		{Ordinal: 0, ItemID: "garbage-1", Malformed: true, VerificationID: r1.ID},
		{Ordinal: 1, ItemID: "good", WorkItemID: w.ID},
		{Ordinal: 2, ItemID: "garbage-2", Malformed: true, VerificationID: r2.ID},
		{Ordinal: 3, ItemID: "garbage-2-again", Malformed: true, VerificationID: r3.ID}, // the same garbage as row 2: NOT a duplicate, its own row and LOG row
		{Ordinal: 4, ItemID: "good-again", WorkItemID: w.ID, DuplicateOf: intp(1)},
	}
	wantMalformed := []VerificationRow{r1, r2, r3}
	before := logCount(t, base)
	if err := write.RecordJob(ctx, JobRow{ID: job, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: len(rows), OwnerUserID: scopeOwnerA}, rows, wantMalformed); err != nil {
		return []string{fmt.Sprintf("RecordJob: %v", err)}
	}
	var v []string
	if delta := logCount(t, base) - before; delta != len(wantMalformed) {
		v = append(v, fmt.Sprintf("%d LOG row(s) were written for %d malformed rows, want exactly one each (and none for the addressable rows)", delta, len(wantMalformed)))
	}
	listed, _, err := base.ListJobResults(ctx, job, scopeOwnerA, "", 100)
	if err != nil {
		return append(v, fmt.Sprintf("ListJobResults: %v", err))
	}
	// Before any work item settles the listing holds exactly the malformed rows.
	if len(listed) != len(wantMalformed) {
		return append(v, fmt.Sprintf("the listing holds %d result(s), want the %d malformed rows (each has an outcome from the start)", len(listed), len(wantMalformed)))
	}
	for i, want := range wantMalformed {
		got := listed[i]
		if !got.Malformed || got.Verdict.ID != want.ID || got.Verdict.Status != want.Status || got.Verdict.Reason != want.Reason ||
			got.Verdict.Confidence != want.Confidence || got.Verdict.Recommendation != want.Recommendation || got.Verdict.PolicyKey != want.PolicyKey ||
			got.Verdict.PolicyVersion != want.PolicyVersion || got.Verdict.APIVersion != want.APIVersion || got.DuplicateOf != nil || len(got.EmailHash) != 0 || got.Verdict.Domain != "" {
			v = append(v, fmt.Sprintf("result %d = %+v is not what was LOGGED for it (%+v): a result must be served from its LOG row", i, got, want))
		}
	}
	return v
}

// TestRecordJob_AMalformedRowHasExactlyOneLogRowAndIsServedFromIt (Door 2): one LOG row per malformed row — a repeated malformed address is its
// own row with its own LOG row, not a duplicate — each the sync path's invalid_syntax row with a NULL hash, and the result is the logged verdict.
// On Postgres the stored columns are read back too (NULL email_hash and hash_scheme, from_cache false), through a connection that is not the store's.
func TestRecordJob_AMalformedRowHasExactlyOneLogRowAndIsServedFromIt(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		if v := malformedLoggingViolations(t, st, st); len(v) != 0 {
			t.Fatalf("Door 2 (a malformed row's verdict is logged before it is returned, and served from the log) is violated:\n  %s", strings.Join(v, "\n  "))
		}
		if pg, ok := st.(*PG); ok {
			var n int
			if err := pg.pool.QueryRow(context.Background(), `
				SELECT count(*)::int FROM verify.verifications
				WHERE status = 'invalid' AND reason = 'invalid_syntax' AND recommendation = 'skip' AND email_hash IS NULL AND hash_scheme IS NULL
				  AND domain IS NULL AND NOT from_cache AND flags = '{"from_cache": false}'::jsonb AND signals = '{"syntax_valid": false}'::jsonb`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 3 {
				t.Errorf("%d stored LOG row(s) have the sync path's invalid_syntax shape (NULL hash, from_cache false, flags and signals as the sync path writes them), want 3", n)
			}
		}
	})
}

// TestRecordJob_ControlTheOldNoLogBehaviourFailsTheSameScenario is the paired control: the SAME scenario over a store that behaves as before — the
// malformed rows are neither logged nor recorded — must report both that no LOG row was written and that the results are missing.
func TestRecordJob_ControlTheOldNoLogBehaviourFailsTheSameScenario(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		v := malformedLoggingViolations(t, st, noLogStore{st})
		joined := strings.Join(v, "\n")
		if len(v) == 0 || !strings.Contains(joined, "LOG row(s) were written for 3 malformed rows") {
			t.Fatalf("CONTROL: with no log written for malformed rows the scenario must report the missing LOG rows, got %q", v)
		}
	})
}

// settleRowsFixture-based tests continue below.

// pageFunc is one call of a paginated listing: a cursor and a page size in, a page and the next cursor out.
type pageFunc func(cursor string, limit int) ([]WorkItemResult, string, error)

// walkPages pages through a listing with the given page function and returns, per page, the results it held. It fails the test (a fixture
// problem, not a property) if the walk does not terminate.
func walkPages(t *testing.T, list pageFunc, limit int) [][]WorkItemResult {
	t.Helper()
	var pages [][]WorkItemResult
	cursor := ""
	for i := 0; i < 1000; i++ {
		rows, next, err := list(cursor, limit)
		if err != nil {
			t.Fatalf("page %d (cursor %q): %v", i, cursor, err)
		}
		pages = append(pages, rows)
		if next == "" {
			return pages
		}
		cursor = next
	}
	t.Fatalf("the cursor never ran out after 1000 pages at limit %d", limit)
	return nil
}

// exactOnceViolations judges a walk: every wanted ordinal must have been returned EXACTLY ONCE (set equality with multiplicity), nothing else may
// appear, and the ordinals must be strictly increasing across the whole walk. It returns one line per violation; an empty result is a pass.
func exactOnceViolations(pages [][]WorkItemResult, want []int) []string {
	var v []string
	seen := map[int]int{}
	prev := -1
	i := 0
	for p, page := range pages {
		for _, r := range page {
			seen[r.Ordinal]++
			if r.Ordinal <= prev {
				v = append(v, fmt.Sprintf("out of order: ordinal %d came after %d (page %d, position %d)", r.Ordinal, prev, p, i))
			}
			prev = r.Ordinal
			i++
		}
	}
	wanted := map[int]bool{}
	for _, o := range want {
		wanted[o] = true
		if seen[o] != 1 {
			v = append(v, fmt.Sprintf("ordinal %d returned %d times, want exactly once", o, seen[o]))
		}
	}
	for o := range seen {
		if !wanted[o] {
			v = append(v, fmt.Sprintf("ordinal %d was returned but is not a row of the job", o))
		}
	}
	return v
}

// boundaryInsideADuplicateRun reports whether, at some page boundary, the last row of one page and the first row of the next share a work item —
// the case the owner asked the pagination test to include.
func boundaryInsideADuplicateRun(pages [][]WorkItemResult) bool {
	for i := 0; i+1 < len(pages); i++ {
		if len(pages[i]) == 0 || len(pages[i+1]) == 0 {
			continue
		}
		last, first := pages[i][len(pages[i])-1], pages[i+1][0]
		if last.WorkItemID != "" && last.WorkItemID == first.WorkItemID {
			return true
		}
	}
	return false
}

// TestListJobResults_EveryOrdinalExactlyOnceAcrossPages is the A7 exit: several pages (limit < total), including a page boundary INSIDE a run of
// duplicate rows, every ordinal exactly once — no gap, no duplicate, in order — checked with exact set equality, on both stores. Then the PAIRED
// CONTROLS: the same walker over (a) a listing whose next cursor is taken from the look-ahead row (the M2-G5 defect) and (b) an unsorted listing
// must REPORT a violation, so the helper is shown able to fail. A duplicate row keeps its duplicate_of on whichever page it lands.
func TestListJobResults_EveryOrdinalExactlyOnceAcrossPages(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		job, rows, _ := settleRowsFixture(t, st)
		settleAll(t, st)
		want := make([]int, len(rows))
		wantDup := map[int]*int{}
		for i, r := range rows {
			want[i] = r.Ordinal
			wantDup[r.Ordinal] = r.DuplicateOf
		}
		list := func(cursor string, limit int) ([]WorkItemResult, string, error) {
			return st.ListJobResults(ctx, job, scopeOwnerA, cursor, limit)
		}

		sawDuplicateBoundary := false
		for _, limit := range []int{1, 2, 3, 4, 5, 7, 12, 13, 14, 100} {
			pages := walkPages(t, list, limit)
			if v := exactOnceViolations(pages, want); len(v) != 0 {
				t.Errorf("limit %d: %s", limit, strings.Join(v, "; "))
			}
			if wantPages := (len(want) + limit - 1) / limit; len(pages) != wantPages {
				t.Errorf("limit %d: %d page(s), want %d (the last page ends the walk with an empty cursor, and no extra empty page follows)", limit, len(pages), wantPages)
			}
			for i, p := range pages {
				if len(p) > limit {
					t.Errorf("limit %d: page %d holds %d rows", limit, i, len(p))
				}
				for _, r := range p {
					if !sameIntPtr(r.DuplicateOf, wantDup[r.Ordinal]) {
						t.Errorf("limit %d: ordinal %d came back with duplicate_of %v, want %v (the pointer must survive a page boundary)", limit, r.Ordinal, r.DuplicateOf, wantDup[r.Ordinal])
					}
				}
			}
			if limit < len(want) && boundaryInsideADuplicateRun(pages) {
				sawDuplicateBoundary = true
			}
		}
		if !sawDuplicateBoundary {
			t.Fatal("fixture: no page boundary fell inside a run of duplicate rows, so the case the owner asked for was never exercised")
		}

		// CONTROL (a): the cursor comes from the LOOK-AHEAD row. Fetch limit+1, return limit, and hand back the extra row's ordinal as the cursor:
		// the next page starts AFTER the extra row, so that row is never returned. This is M2's bug (M2-G5).
		lookAhead := func(cursor string, limit int) ([]WorkItemResult, string, error) {
			all, _, err := st.ListJobResults(ctx, job, scopeOwnerA, cursor, limit+1)
			if err != nil || len(all) <= limit {
				return all, "", err
			}
			return all[:limit], FormatResultsCursor(all[limit].Ordinal), nil
		}
		v := exactOnceViolations(walkPages(t, lookAhead, 3), want)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "returned 0 times") {
			t.Errorf("CONTROL: a cursor taken from the look-ahead row must be reported as skipping a row, got %q", v)
		}
		// CONTROL (b): an unsorted listing — every page reversed — must be reported as out of order.
		reversed := func(cursor string, limit int) ([]WorkItemResult, string, error) {
			page, next, err := st.ListJobResults(ctx, job, scopeOwnerA, cursor, limit)
			for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
				page[i], page[j] = page[j], page[i]
			}
			return page, next, err
		}
		v = exactOnceViolations(walkPages(t, reversed, 3), want)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "out of order") {
			t.Errorf("CONTROL: an unsorted listing must be reported as out of order, got %q", v)
		}
		// CONTROL (c): the helper also sees a DUPLICATE (a cursor that does not advance past the last row returned repeats it).
		repeating := func(cursor string, limit int) ([]WorkItemResult, string, error) {
			page, next, err := st.ListJobResults(ctx, job, scopeOwnerA, cursor, limit)
			if next != "" && len(page) > 1 {
				next = FormatResultsCursor(page[len(page)-2].Ordinal) // one short: the last row comes back again
			}
			return page, next, err
		}
		v = exactOnceViolations(walkPages(t, repeating, 3), want)
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "returned 2 times") {
			t.Errorf("CONTROL: a cursor that repeats a row must be reported as a duplicate, got %q", v)
		}
	})
}

// TestJobCompletion_PendingReachesZeroWithDuplicates: settled counts ROWS, so total - settled (the status route's `pending`) reaches 0 on a
// completed job even when rows share work items and some rows are malformed. The PAIRED CONTROL reads the old per-work-item counter on the very
// same job: it ends at 6 of 13 (one count per work item), which would leave pending at 7 for ever — the defect this replaces.
func TestJobCompletion_PendingReachesZeroWithDuplicates(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		job, rows, _ := settleRowsFixture(t, st)

		complete, total, settled, err := st.JobCompletion(ctx, job, scopeOwnerA)
		if err != nil || complete || total != 13 || settled != 2 {
			t.Fatalf("before settling: complete=%v total=%d settled=%d err=%v, want not complete, 13 rows, 2 settled (the two malformed rows)", complete, total, settled, err)
		}

		// Settle exactly ONE work item. It answers some number of rows (3 for W1, 1 for W2, ...), and settled must move by that many, not by one.
		leased, err := st.DequeueReady(ctx, "w-one", 1, scopeNow)
		if err != nil || len(leased) != 1 {
			t.Fatalf("lease one item: %d err=%v", len(leased), err)
		}
		rowsAnswered := 0
		for _, r := range rows {
			if r.WorkItemID == leased[0].ID {
				rowsAnswered++
			}
		}
		if rowsAnswered == 0 {
			t.Fatalf("fixture: the leased work item %s answers no row", leased[0].ID)
		}
		v := rtRow(t, scopeNow, leased[0].Domain, leased[0].EmailHash)
		if err := st.SettleWorkItem(ctx, leased[0].ID, "w-one", "settled", v, nil, WorkItemAttemptRow{}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, settledOne, err := st.JobCompletion(ctx, job, scopeOwnerA); err != nil || settledOne != 2+rowsAnswered {
			t.Fatalf("after settling one work item that answers %d row(s): settled=%d err=%v, want %d (rows, not work items)", rowsAnswered, settledOne, err, 2+rowsAnswered)
		}

		settleAll(t, st)
		complete, total, settled, err = st.JobCompletion(ctx, job, scopeOwnerA)
		if err != nil || !complete || total != 13 || settled != 13 || total-settled != 0 {
			t.Fatalf("after settling everything: complete=%v total=%d settled=%d pending=%d err=%v, want complete, 13, 13, 0", complete, total, settled, total-settled, err)
		}

		// CONTROL: the old accounting (one count per WORK ITEM, jobs.settled_count) on this same job. It is 6 of 13 and would leave pending at 7
		// forever; if JobCompletion ever went back to reporting it, the assertion above could not hold.
		j, known, err := st.GetJob(ctx, job, scopeOwnerA)
		if err != nil || !known {
			t.Fatalf("GetJob: known=%v err=%v", known, err)
		}
		if j.TotalCount-j.SettledCount == 0 {
			t.Errorf("CONTROL: the per-work-item counter reaches total (%d of %d) on a job with duplicates and malformed rows, so this fixture cannot tell the two accountings apart", j.SettledCount, j.TotalCount)
		}
	})
}

// TestJobCompletion_CompleteStillComesFromTheWorkItems: the counts moved to rows, but `complete` is still judged by the work items (GAP-38): a job
// whose rows are all malformed is complete from the start (nothing to wait for), and a job with an unsettled work item is not.
func TestJobCompletion_CompleteStillComesFromTheWorkItems(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		allBad := mustV7(t, scopeNow)
		r0, r1 := rejectRow(t), rejectRow(t)
		recordJob(t, st, allBad, scopeOwnerA, []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: r0.ID}, {Ordinal: 1, Malformed: true, VerificationID: r1.ID}}, []VerificationRow{r0, r1})
		if complete, total, settled, err := st.JobCompletion(ctx, allBad, scopeOwnerA); err != nil || !complete || total != 2 || settled != 2 {
			t.Fatalf("a job of only malformed rows: complete=%v total=%d settled=%d err=%v, want complete 2/2", complete, total, settled, err)
		}
		pending, _, _ := settleRowsFixture(t, st)
		if complete, _, _, err := st.JobCompletion(ctx, pending, scopeOwnerA); err != nil || complete {
			t.Fatalf("a job with unsettled work items must not be complete (err=%v)", err)
		}
	})
}

// TestResultsCursor_ParseAndFormat pins the cursor as an opaque, validated position: "" is the start, what Format issues parses back, and
// anything else — including a pre-0018 work-item uuid — is refused with ErrBadCursor by the helper AND by both stores' listing.
func TestResultsCursor_ParseAndFormat(t *testing.T) {
	for _, ordinal := range []int{0, 1, 9, 10, 99999, 2147483647} {
		got, err := ParseResultsCursor(FormatResultsCursor(ordinal))
		if err != nil || got != ordinal {
			t.Errorf("round trip of %d: got %d err=%v", ordinal, got, err)
		}
	}
	if got, err := ParseResultsCursor(""); err != nil || got != -1 {
		t.Errorf(`"" must mean the first page (-1), got %d err=%v`, got, err)
	}
	bad := []string{"-1", "abc", "01", "+1", " 1", "1 ", "1.5", "2147483648", "018f0000-0000-7000-8000-0000000000a1", "0x10"}
	for _, c := range bad {
		if _, err := ParseResultsCursor(c); err != ErrBadCursor {
			t.Errorf("cursor %q must be ErrBadCursor, got %v", c, err)
		}
	}
	forEachStore(t, func(t *testing.T, st Store) {
		job := newScopeJob(t, st, scopeOwnerA, 0)
		for _, c := range bad {
			if _, _, err := st.ListJobResults(context.Background(), job, scopeOwnerA, c, 10); err != ErrBadCursor {
				t.Errorf("ListJobResults with cursor %q must be ErrBadCursor, got %v", c, err)
			}
		}
		if _, _, err := st.ListJobResults(context.Background(), job, scopeOwnerA, "0", 10); err != nil {
			t.Errorf("CONTROL: a well-formed cursor must be accepted: %v", err)
		}
	})
}

// TestPGGrants_JobRowsAreAppendOnlyForTheRuntime: verify_app may SELECT and INSERT into verify.job_rows (the control plane appends rows at
// submit and reads them to serve results) and may NOT UPDATE or DELETE them (a row is a fact about the submission). Both directions are shown in
// the same transaction, so a role switch that silently dropped every privilege cannot make the denials pass for the wrong reason.
func TestPGGrants_JobRowsAreAppendOnlyForTheRuntime(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	if err := asRuntime(t, pg, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO verify.job_rows (job_id, ordinal, item_id, work_item_id, malformed) VALUES (gen_random_uuid(), 0, 'grant-probe', gen_random_uuid(), false)`); err != nil {
			return fmt.Errorf("INSERT as verify_app: %w", err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM verify.job_rows WHERE item_id = 'grant-probe'`).Scan(&n); err != nil {
			return fmt.Errorf("SELECT as verify_app: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("the row verify_app just inserted reads back %d times, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify_app must hold SELECT and INSERT on verify.job_rows (0018): %v", err)
	}
	denied(t, "UPDATE verify.job_rows", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE verify.job_rows SET item_id = 'x' WHERE false`)
		return e
	}))
	denied(t, "DELETE FROM verify.job_rows", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM verify.job_rows WHERE false`)
		return e
	}))
	var owner string
	if err := pg.pool.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'verify.job_rows'::regclass`).Scan(&owner); err != nil || owner != "verify_owner" {
		t.Errorf("verify.job_rows is owned by %q (err %v), want verify_owner", owner, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the 100,000-row worst case stays chunked (no database: the builders are pure)
// ─────────────────────────────────────────────────────────────────────────────

// statementViolations judges a bulk write's statements: none may carry more parameters than Postgres allows (65,535: a single statement for a
// large submission fails outright), and a large input must be spread over the number of statements the chunk size dictates.
func statementViolations(stmts []pgStatement, wantStatements int) []string {
	var v []string
	if len(stmts) != wantStatements {
		v = append(v, fmt.Sprintf("%d statement(s), want %d", len(stmts), wantStatements))
	}
	for i, s := range stmts {
		if len(s.args) > maxStatementParams {
			v = append(v, fmt.Sprintf("statement %d carries %d parameters, over Postgres's limit of %d", i, len(s.args), maxStatementParams))
		}
	}
	return v
}

// TestRecordJobStatements_The100kWorstCaseStaysChunked: a submission of 100,000 rows — and, worse, of 100,000 MALFORMED rows, each with a LOG row —
// is written as a bounded number of multi-row statements, every one under the parameter limit, together covering every row exactly once. The
// builders asserted here are the ones RecordJob executes. The PAIRED CONTROL feeds the same checker one statement holding everything (what an
// unchunked write would be) and it must object.
func TestRecordJobStatements_The100kWorstCaseStaysChunked(t *testing.T) {
	const n = 100_000
	shared := rejectRow(t) // the maps are shared across rows, as the enqueuer shares them: the builders only read them
	rejects := make([]VerificationRow, n)
	rows := make([]JobRowEntry, n)
	for i := range rows {
		id := fmt.Sprintf("018f0000-0000-7000-8000-%012x", i)
		r := shared
		r.ID, r.RequestID = id, id
		rejects[i] = r
		rows[i] = JobRowEntry{Ordinal: i, ItemID: fmt.Sprintf("row-%d", i), Malformed: true, VerificationID: id}
	}
	if err := checkRejectLinks(rows, rejects); err != nil {
		t.Fatalf("the worst-case fixture is not a consistent RecordJob call: %v", err)
	}

	jobStmts := jobRowsStatements("018f0000-0000-7000-8000-00000000ffff", rows)
	if v := statementViolations(jobStmts, 100); len(v) != 0 {
		t.Errorf("job_rows: %s", strings.Join(v, "; "))
	}
	if len(jobStmts) > 0 && len(jobStmts[0].args) != 1+jobRowParams*jobRowsChunk {
		t.Errorf("a full job_rows statement carries %d parameters, want %d", len(jobStmts[0].args), 1+jobRowParams*jobRowsChunk)
	}
	covered := 0
	for _, s := range jobStmts {
		covered += strings.Count(s.sql, "($1,")
	}
	if covered != n {
		t.Errorf("the job_rows statements cover %d rows, want %d", covered, n)
	}

	logStmts, err := verificationStatements(rejects)
	if err != nil {
		t.Fatal(err)
	}
	if v := statementViolations(logStmts, 200); len(v) != 0 {
		t.Errorf("LOG rows: %s", strings.Join(v, "; "))
	}
	if len(logStmts) > 0 && len(logStmts[0].args) != verificationParams*verificationsChunk {
		t.Errorf("a full LOG statement carries %d parameters, want %d", len(logStmts[0].args), verificationParams*verificationsChunk)
	}
	coveredLog := 0
	for _, s := range logStmts {
		coveredLog += strings.Count(s.sql, "($")
	}
	if coveredLog != n {
		t.Errorf("the LOG statements cover %d rows, want %d", coveredLog, n)
	}
	// The last statement ends exactly at the last row: no row is dropped or doubled at the seams.
	last := logStmts[len(logStmts)-1]
	if got := len(last.args); got != verificationParams*(n-verificationsChunk*(len(logStmts)-1)) {
		t.Errorf("the last LOG statement carries %d parameters", got)
	}

	// CONTROL: one statement holding every row (an unchunked write) is over the limit, and the checker says so.
	giant := []pgStatement{{sql: "INSERT ...", args: make([]any, verificationParams*n)}}
	if v := statementViolations(giant, 200); len(v) < 2 || !strings.Contains(strings.Join(v, "\n"), "over Postgres's limit") {
		t.Errorf("CONTROL: an unchunked write must be reported (both the statement count and the parameter limit), got %q", v)
	}
}

// TestVerificationArgs_AreTheColumnsOfTheLogInsert: the bulk LOG insert and the single LOG insert (the settlement CAS, the sync path) share ONE
// conversion; its length must match the column list it feeds, so a column added to one cannot silently be missed by the other.
func TestVerificationArgs_AreTheColumnsOfTheLogInsert(t *testing.T) {
	args, err := verificationArgs(rejectRow(t))
	if err != nil {
		t.Fatal(err)
	}
	cols := 0
	for _, c := range strings.Split(verificationColumns, ",") {
		if strings.TrimSpace(c) != "" {
			cols++
		}
	}
	if len(args) != verificationParams || cols != verificationParams {
		t.Errorf("verificationArgs returns %d values, verificationColumns names %d columns, verificationParams is %d: all three must agree", len(args), cols, verificationParams)
	}
	// A malformed row's NULL mapping: no hash, no scheme, no domain, no engine provenance.
	if args[4] != nil || args[5] != nil || args[6] != nil || args[19] != nil || args[20] != nil {
		t.Errorf("a malformed row's LOG row must write NULL email_hash/hash_scheme/domain/verifier_ip/node_id, got %v %v %v %v %v", args[4], args[5], args[6], args[19], args[20])
	}
	// CONTROL: a row WITH a hash is not mapped to NULL (so the NULLs above come from the row, not from the mapping).
	withHash := rejectRow(t)
	withHash.EmailHash, withHash.HashScheme, withHash.Domain = []byte{1, 2, 3}, "norm/v1+hmac-sha256/v1", "ex.com"
	a2, err := verificationArgs(withHash)
	if err != nil || a2[4] == nil || a2[5] == nil || a2[6] == nil {
		t.Errorf("CONTROL: a row with a hash, scheme and domain must map to non-NULL values, got %v %v %v (err %v)", a2[4], a2[5], a2[6], err)
	}
}

// TestCheckRejectLinks pins the one rule the database cannot see, in isolation (the cases are also run through both stores above).
func TestCheckRejectLinks(t *testing.T) {
	a, b := rejectRow(t), rejectRow(t)
	good := []JobRowEntry{{Ordinal: 0, Malformed: true, VerificationID: a.ID}, {Ordinal: 1, WorkItemID: "w"}, {Ordinal: 2, Malformed: true, VerificationID: b.ID}}
	if err := checkRejectLinks(good, []VerificationRow{a, b}); err != nil {
		t.Fatalf("CONTROL: a consistent call was refused: %v", err)
	}
	noID := a
	noID.ID = ""
	for name, c := range map[string]struct {
		rows    []JobRowEntry
		rejects []VerificationRow
	}{
		"a reject without an id":            {good[:1], []VerificationRow{noID}},
		"a reject written twice":            {good[:1], []VerificationRow{a, a}},
		"a row naming an unwritten LOG row": {good, []VerificationRow{a}},
		"a LOG row no row names":            {good[:1], []VerificationRow{a, b}},
		"two rows naming one LOG row":       {[]JobRowEntry{good[0], {Ordinal: 1, Malformed: true, VerificationID: a.ID}}, []VerificationRow{a}},
		"a LOG row with no rows at all":     {nil, []VerificationRow{a}},
	} {
		if err := checkRejectLinks(c.rows, c.rejects); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
