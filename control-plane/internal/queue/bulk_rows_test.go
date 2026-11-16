package queue

// The enqueuer's side of per-row identity and per-job dedup (owner rulings A1/A2; migration 0018): a REAL Enqueuer feeding a REAL drainer, over BOTH
// stores — the Mem fake always, and real Postgres in a throwaway database (testpg.Scratch; it SKIPS honestly without TM_TEST_DATABASE_URL). A
// scratch database also means the drainer here can only lease THIS test's work items, so the count of how many times an address was checked is exact.
//
// "The check ran once" is measured at the seam the drainer already uses in every other test: the Computer. bulkCountingComputer wraps the existing
// settleE2EComputer and counts how many times each address was computed.
//
// Paired controls (each makes the same assertion fail on the behaviour it replaces):
//   - the cross-tenant scenario is re-run against a store wrapper that dedups across jobs (the OLD global scope) and must report that tenant B got
//     no work item;
//   - the write-order check is run over a synthetic call log with the work items first and must report it.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/testpg"
)

const (
	rowsOwnerA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	rowsOwnerB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var rowsNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// bulkCountingComputer is settleE2EComputer plus a count of how many times each address was checked, and a verdict that DIFFERS per address (so a
// duplicate row carrying its own address's verdict is distinguishable from one carrying somebody else's).
type bulkCountingComputer struct {
	settleE2EComputer
	calls map[string]int
}

func (c *bulkCountingComputer) ComputeForQueue(ctx context.Context, rawEmail string, def *policy.Definition, jobID string) (orchestrator.QueueOutcome, error) {
	out, err := c.settleE2EComputer.ComputeForQueue(ctx, rawEmail, def, jobID)
	if err != nil {
		return out, err
	}
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[rawEmail]++
	h := 0
	for _, b := range []byte(rawEmail) {
		h = (h*31 + int(b)) % 50
	}
	out.Row.Confidence = 40 + h
	out.Row.Domain = rawEmail[strings.LastIndex(rawEmail, "@")+1:]
	return out, nil
}

func (c *bulkCountingComputer) total() int {
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// bulkStores runs body against a fresh Mem fake and against real Postgres in a scratch database.
func bulkStores(t *testing.T, body func(t *testing.T, st store.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { body(t, store.NewMem(func() time.Time { return rowsNow })) })
	t.Run("pg", func(t *testing.T) {
		dsn := testpg.Scratch(t, 0)
		pg, err := store.OpenPG(context.Background(), dsn)
		if err != nil {
			t.Fatalf("OpenPG(scratch): %v", err)
		}
		t.Cleanup(pg.Close)
		body(t, pg)
	})
}

func newBulkEnqueuer(t *testing.T, st store.Store) (*Enqueuer, *AddrCipher) {
	t.Helper()
	h, err := hash.NewHasher([]byte("bulk-rows-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	cipher, err := NewAddrCipher(key32(5))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return &Enqueuer{Store: st, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg", Now: func() time.Time { return rowsNow }}, cipher
}

// drainEverything runs the real drainer until the queue is empty.
func drainEverything(t *testing.T, st store.Store, cipher *AddrCipher, comp Computer) {
	t.Helper()
	dr, err := NewDrainer(Deps{
		Store: st, Compute: comp, Policy: fakePolicy{}, Cipher: cipher,
		Backoff:  Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return rowsNow },
		WorkerID: "bulk-rows-worker", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 1000, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	for i := 0; i < 20; i++ {
		if !dr.TickOnce(context.Background()) {
			return
		}
	}
	t.Fatal("the drainer was still finding work after 20 ticks")
}

// allResults reads a job's whole listing in one page.
func allResults(t *testing.T, st store.Store, job, owner string) []store.WorkItemResult {
	t.Helper()
	rows, next, err := st.ListJobResults(context.Background(), job, owner, "", 1000)
	if err != nil || next != "" {
		t.Fatalf("ListJobResults: next=%q err=%v", next, err)
	}
	return rows
}

// TestSubmitBulkRows_EveryRowKeepsItsOrdinalAndLabel_AndADuplicateIsCheckedOnce is the A1 + A2-within-a-job exit test on both stores. One job holds
// a labelled row per input row: duplicates (by case and by surrounding whitespace), malformed addresses, a row with no label. After the drainer:
// each distinct address was checked EXACTLY ONCE, the job has one result per input row in the caller's own order with its label, duplicate rows carry
// the verdict of the row they share, malformed rows are reported with their ordinal, and the counts add up.
func TestSubmitBulkRows_EveryRowKeepsItsOrdinalAndLabel_AndADuplicateIsCheckedOnce(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		enq, cipher := newBulkEnqueuer(t, st)
		rows := []BulkRow{
			{ItemID: "r0", Address: "Jane@AcmeCorp.com"},    // 0  first of "jane"
			{ItemID: "r1", Address: "bob@acmecorp.com"},     // 1
			{ItemID: "r2", Address: "jane@acmecorp.com"},    // 2  duplicate of 0 (case)
			{ItemID: "r3", Address: "not-an-address"},       // 3  malformed
			{ItemID: "r4", Address: "  jane@acmecorp.com "}, // 4  duplicate of 0 (whitespace)
			{Address: "carol@acmecorp.com"},                 // 5  no label
			{ItemID: "r6", Address: ""},                     // 6  malformed (blank)
		}
		res, err := enq.SubmitBulkRows(ctx, rows, rowsDefinition(t), "ref", rowsOwnerA)
		if err != nil {
			t.Fatalf("SubmitBulkRows: %v", err)
		}
		if res.Total != 7 || res.Enqueued != 3 || res.Duplicate != 2 || res.Malformed != 2 || res.Total != res.Enqueued+res.Duplicate+res.Malformed {
			t.Fatalf("counts = total %d enqueued %d duplicate %d malformed %d, want 7/3/2/2 adding up (total = enqueued + duplicate + malformed)",
				res.Total, res.Enqueued, res.Duplicate, res.Malformed)
		}
		wantBad := []MalformedRow{{Ordinal: 3, ItemID: "r3"}, {Ordinal: 6, ItemID: "r6"}}
		if len(res.MalformedRows) != 2 || res.MalformedRows[0] != wantBad[0] || res.MalformedRows[1] != wantBad[1] || res.MalformedRowsTruncated {
			t.Fatalf("malformed_rows = %+v truncated=%v, want %+v untruncated", res.MalformedRows, res.MalformedRowsTruncated, wantBad)
		}
		// Door 2: the two malformed rows are LOGGED by the time SubmitBulkRows returns (before any 202), one LOG row each and nothing else.
		if n := logRowCount(t, st); n != 2 {
			t.Fatalf("%d LOG row(s) exist right after the submission, want exactly 2 (one per malformed row; the addressable rows are logged when they settle)", n)
		}

		comp := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}
		drainEverything(t, st, cipher, comp)

		// One check per DISTINCT address (the addresses reach the drainer normalised: lowercase, trimmed).
		want := map[string]int{"jane@acmecorp.com": 1, "bob@acmecorp.com": 1, "carol@acmecorp.com": 1}
		if len(comp.calls) != len(want) || comp.total() != 3 {
			t.Fatalf("checks run = %v (total %d), want exactly one check for each of jane, bob and carol", comp.calls, comp.total())
		}
		for addr, n := range want {
			if comp.calls[addr] != n {
				t.Errorf("address %s was checked %d time(s), want %d", addr, comp.calls[addr], n)
			}
		}

		got := allResults(t, st, res.JobID, rowsOwnerA)
		if len(got) != 7 {
			t.Fatalf("%d results for 7 input rows: every input row must have an outcome", len(got))
		}
		for i, r := range got {
			if r.Ordinal != i || r.ItemID != rows[i].ItemID {
				t.Errorf("result %d = ordinal %d item %q, want ordinal %d item %q", i, r.Ordinal, r.ItemID, i, rows[i].ItemID)
			}
			if wantMalformed := i == 3 || i == 6; r.Malformed != wantMalformed {
				t.Errorf("result %d malformed=%v, want %v", i, r.Malformed, wantMalformed)
			}
		}
		// Rows 0, 2 and 4 are one check: ONE verdict (same verification id, same confidence), three results.
		if got[0].Verdict.ID == "" || got[0].Verdict.ID != got[2].Verdict.ID || got[0].Verdict.ID != got[4].Verdict.ID || got[0].Verdict.Confidence != got[4].Verdict.Confidence {
			t.Errorf("rows 0, 2, 4 are the same address and must carry one verdict, got %q %q %q", got[0].Verdict.ID, got[2].Verdict.ID, got[4].Verdict.ID)
		}
		// CONTROL: different addresses carry different verdicts.
		if got[0].Verdict.ID == got[1].Verdict.ID || got[1].Verdict.ID == got[5].Verdict.ID || got[0].Verdict.ID == got[5].Verdict.ID {
			t.Error("CONTROL: jane, bob and carol must each have their own verdict")
		}
		// Rows 2 and 4 repeat row 0's address and say so; no other row carries duplicate_of (row 0 is the first of its address, and a malformed row is never a duplicate).
		for i, r := range got {
			want := (*int)(nil)
			if i == 2 || i == 4 {
				want = intPtr(0)
			}
			if !samePtr(r.DuplicateOf, want) {
				t.Errorf("row %d duplicate_of = %v, want %v", i, r.DuplicateOf, want)
			}
		}
		// The malformed rows are answered by their OWN LOG rows (two distinct rows), served like any other result.
		if got[3].Verdict.ID == "" || got[3].Verdict.ID == got[6].Verdict.ID || got[3].Verdict.Reason != "invalid_syntax" || got[3].Verdict.Status != "invalid" || got[6].Verdict.Reason != "invalid_syntax" {
			t.Errorf("malformed rows 3 and 6 carry %+v / %+v, want two distinct invalid/invalid_syntax LOG rows", got[3].Verdict, got[6].Verdict)
		}
		if n := logRowCount(t, st); n != 2+3 {
			t.Errorf("%d LOG row(s) after the drain, want 5: 2 malformed rows + one per distinct address (jane, bob, carol); duplicates add none", n)
		}

		complete, total, settled, err := st.JobCompletion(ctx, res.JobID, rowsOwnerA)
		if err != nil || !complete || total != 7 || settled != 7 || total-settled != 0 {
			t.Errorf("status counts: complete=%v total=%d settled=%d pending=%d err=%v, want complete 7/7 with pending 0 (duplicates and malformed rows included)", complete, total, settled, total-settled, err)
		}
	})
}

// TestSubmitBulk_TheAddressesShapeKeepsWorking: the original shape (bare addresses) is unchanged except that its rows now have ordinals too.
func TestSubmitBulk_TheAddressesShapeKeepsWorking(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		enq, cipher := newBulkEnqueuer(t, st)
		res, err := enq.SubmitBulk(context.Background(), []string{"a@acmecorp.com", "oops", "b@acmecorp.com", "a@acmecorp.com"}, rowsDefinition(t), "ref", rowsOwnerA)
		if err != nil || res.Total != 4 || res.Enqueued != 2 || res.Duplicate != 1 || res.Malformed != 1 {
			t.Fatalf("SubmitBulk = %+v err=%v, want total 4 enqueued 2 duplicate 1 malformed 1", res, err)
		}
		drainEverything(t, st, cipher, &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}})
		got := allResults(t, st, res.JobID, rowsOwnerA)
		if len(got) != 4 {
			t.Fatalf("%d results, want 4", len(got))
		}
		for i, r := range got {
			if r.Ordinal != i || r.ItemID != "" {
				t.Errorf("result %d = ordinal %d item %q, want ordinal %d and NO item_id (the addresses shape has none)", i, r.Ordinal, r.ItemID, i)
			}
		}
	})
}

// crossTenantViolations is the A2 exit scenario through the REAL enqueuer and drainer: tenant A submits and FULLY SETTLES a job; tenant B then
// submits a job holding the same addresses (and one more). B must get a work item, a check and a result for every row — nothing is deduplicated
// across jobs or tenants — and A must not be able to read B's job. It returns what is wrong, so it can be pointed at the old behaviour as a control.
func crossTenantViolations(t *testing.T, st store.Store) []string {
	t.Helper()
	ctx := context.Background()
	enq, cipher := newBulkEnqueuer(t, st)
	comp := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}

	a, err := enq.SubmitBulkRows(ctx, []BulkRow{{ItemID: "a0", Address: "jane@acmecorp.com"}, {ItemID: "a1", Address: "bob@acmecorp.com"}}, rowsDefinition(t), "ref-a", rowsOwnerA)
	if err != nil {
		t.Fatalf("fixture: tenant A's submit: %v", err)
	}
	drainEverything(t, st, cipher, comp)
	if complete, _, _, err := st.JobCompletion(ctx, a.JobID, rowsOwnerA); err != nil || !complete {
		t.Fatalf("fixture: tenant A's job must be fully settled before B submits (complete=%v err=%v)", complete, err)
	}

	var v []string
	b, err := enq.SubmitBulkRows(ctx, []BulkRow{
		{ItemID: "b0", Address: "jane@acmecorp.com"}, {ItemID: "b1", Address: "bob@acmecorp.com"}, {ItemID: "b2", Address: "dave@acmecorp.com"},
	}, rowsDefinition(t), "ref-b", rowsOwnerB)
	if err != nil {
		return append(v, fmt.Sprintf("tenant B's submission of addresses tenant A already verified yielded no work item: %v", err))
	}
	if b.Total != 3 || b.Enqueued != 3 || b.Duplicate != 0 {
		v = append(v, fmt.Sprintf("tenant B's counts = total %d enqueued %d duplicate %d, want 3/3/0: nothing may be a duplicate of another JOB's row", b.Total, b.Enqueued, b.Duplicate))
	}
	drainEverything(t, st, cipher, comp)
	if comp.calls["jane@acmecorp.com"] != 2 || comp.calls["bob@acmecorp.com"] != 2 || comp.calls["dave@acmecorp.com"] != 1 {
		v = append(v, fmt.Sprintf("checks run = %v, want jane and bob checked once PER JOB (2) and dave once", comp.calls))
	}
	rows, _, err := st.ListJobResults(ctx, b.JobID, rowsOwnerB, "", 100)
	if err != nil || len(rows) != 3 {
		return append(v, fmt.Sprintf("tenant B's job has %d result(s) (err=%v), want 3: a new job always yields a result for every row", len(rows), err))
	}
	for i, r := range rows {
		if r.Ordinal != i || r.ItemID != fmt.Sprintf("b%d", i) || r.Verdict.ID == "" {
			v = append(v, fmt.Sprintf("tenant B's result %d = %+v, want its own row b%d with a verdict", i, r, i))
		}
	}
	if other, _, err := st.ListJobResults(ctx, b.JobID, rowsOwnerA, "", 100); err != nil || len(other) != 0 {
		v = append(v, fmt.Sprintf("tenant A read %d row(s) of tenant B's job (err=%v)", len(other), err))
	}
	return v
}

// TestSubmitBulkRows_AnotherTenantGetsResultsForAddressesVerifiedYesterday: two jobs, two owners, the first fully settled, the second NOT empty.
func TestSubmitBulkRows_AnotherTenantGetsResultsForAddressesVerifiedYesterday(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		if v := crossTenantViolations(t, st); len(v) != 0 {
			t.Fatalf("A2 (no dedup across jobs or tenants) is violated:\n  %s", strings.Join(v, "\n  "))
		}
	})
}

// globalScopeStore is the OLD dedup scope as a wrapper (0009's global UNIQUE with ON CONFLICT DO NOTHING): once ANY job holds an idempotency key,
// every later enqueue of it, in any job, is a "duplicate" that creates nothing.
type globalScopeStore struct {
	store.Store
	holder map[string]string
}

func (g *globalScopeStore) EnqueueWorkItem(ctx context.Context, item store.WorkItemRow) (string, bool, error) {
	k := string(item.IdempotencyKey)
	if id, ok := g.holder[k]; ok {
		return id, true, nil
	}
	id, existed, err := g.Store.EnqueueWorkItem(ctx, item)
	if err == nil && !existed {
		g.holder[k] = id
	}
	return id, existed, err
}

// TestSubmitBulkRows_ControlTheOldGlobalScopeFailsTheSameScenario is the paired control: the scenario above, run over the old global scope, must
// say that tenant B got no work item. (Over Mem, and over Postgres through the same wrapper, which stands in for the dropped global constraint.)
func TestSubmitBulkRows_ControlTheOldGlobalScopeFailsTheSameScenario(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		v := crossTenantViolations(t, &globalScopeStore{Store: st, holder: map[string]string{}})
		if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "yielded no work item") {
			t.Fatalf("CONTROL: under the old global scope the scenario must report that tenant B got no work item, got %q", v)
		}
	})
}

// orderLog records the Store calls the enqueuer makes, in order.
type orderLog struct {
	store.Store
	calls []string
}

func (o *orderLog) RecordJob(ctx context.Context, j store.JobRow, rows []store.JobRowEntry, rejects []store.VerificationRow) error {
	o.calls = append(o.calls, "RecordJob")
	return o.Store.RecordJob(ctx, j, rows, rejects)
}
func (o *orderLog) AppendVerification(ctx context.Context, row store.VerificationRow, raw *store.RawRow) error {
	o.calls = append(o.calls, "AppendVerification")
	return o.Store.AppendVerification(ctx, row, raw)
}
func (o *orderLog) EnqueueWorkItem(ctx context.Context, it store.WorkItemRow) (string, bool, error) {
	o.calls = append(o.calls, "EnqueueWorkItem")
	return o.Store.EnqueueWorkItem(ctx, it)
}

// writeOrderViolation judges a call log: ONE RecordJob (the job, ALL its rows and the malformed rows' LOG rows, in one transaction) first, then
// the work items — so a work item can never settle before the rows that point at it exist, the status counts are exact at every instant, and no
// malformed row's verdict is logged apart from (or after) its job.
func writeOrderViolation(calls []string) string {
	recordAt, firstItemAt, records := -1, -1, 0
	for i, c := range calls {
		if c == "RecordJob" {
			records++
			if recordAt < 0 {
				recordAt = i
			}
		}
		if c == "EnqueueWorkItem" && firstItemAt < 0 {
			firstItemAt = i
		}
	}
	switch {
	case records != 1:
		return fmt.Sprintf("the job, its rows and its LOG rows must be recorded by exactly ONE RecordJob call, calls = %v", calls)
	case firstItemAt >= 0 && firstItemAt < recordAt:
		return fmt.Sprintf("a work item was enqueued before the job's rows were recorded, calls = %v", calls)
	}
	for _, c := range calls {
		if c == "AppendVerification" {
			return fmt.Sprintf("a LOG row was appended outside the RecordJob transaction, calls = %v", calls)
		}
	}
	return ""
}

// TestSubmitBulkRows_WritesTheRowsBeforeAnyWorkItem pins the order of writes the status counts rely on, with its paired control over a log in the
// wrong order.
func TestSubmitBulkRows_WritesTheRowsBeforeAnyWorkItem(t *testing.T) {
	rec := &orderLog{Store: store.NewMem(func() time.Time { return rowsNow })}
	enq, _ := newBulkEnqueuer(t, rec)
	if _, err := enq.SubmitBulkRows(context.Background(), []BulkRow{{Address: "a@acmecorp.com"}, {Address: "b@acmecorp.com"}, {Address: "x"}}, rowsDefinition(t), "", rowsOwnerA); err != nil {
		t.Fatal(err)
	}
	if v := writeOrderViolation(rec.calls); v != "" {
		t.Fatalf("%s", v)
	}
	if n := len(rec.calls); n != 3 { // ONE RecordJob (job + rows + the malformed row's LOG row), and one EnqueueWorkItem per DISTINCT normalisable address
		t.Errorf("calls = %v, want RecordJob and two EnqueueWorkItem", rec.calls)
	}
	// CONTROL: the same judgement over logs in the wrong shape must object.
	if v := writeOrderViolation([]string{"EnqueueWorkItem", "RecordJob", "EnqueueWorkItem"}); !strings.Contains(v, "before the job's rows were recorded") {
		t.Errorf("CONTROL: a work item enqueued before the rows must be reported, got %q", v)
	}
	if v := writeOrderViolation([]string{"EnqueueWorkItem"}); !strings.Contains(v, "exactly ONE RecordJob") {
		t.Errorf("CONTROL: a job never recorded must be reported, got %q", v)
	}
	if v := writeOrderViolation([]string{"RecordJob", "AppendVerification", "EnqueueWorkItem"}); !strings.Contains(v, "outside the RecordJob transaction") {
		t.Errorf("CONTROL: a LOG row written apart from the job must be reported, got %q", v)
	}
}

// TestSubmitBulkRows_TheMalformedListIsCappedButTheCountIsExact: with more than MalformedRowsCap malformed rows, the LIST stops at the cap and says
// so; the COUNT does not. Exactly at the cap it is not truncated (the boundary), and every malformed row is still a row of the job.
func TestSubmitBulkRows_TheMalformedListIsCappedButTheCountIsExact(t *testing.T) {
	build := func(nBad int) []BulkRow {
		rows := make([]BulkRow, 0, nBad+2)
		rows = append(rows, BulkRow{ItemID: "good-first", Address: "a@acmecorp.com"})
		for i := 0; i < nBad; i++ {
			rows = append(rows, BulkRow{ItemID: fmt.Sprintf("bad-%d", i), Address: "no-at-sign"})
		}
		return append(rows, BulkRow{ItemID: "good-last", Address: "b@acmecorp.com"})
	}
	for _, c := range []struct {
		bad       int
		truncated bool
	}{{MalformedRowsCap - 1, false}, {MalformedRowsCap, false}, {MalformedRowsCap + 1, true}, {MalformedRowsCap + 500, true}} {
		st := store.NewMem(func() time.Time { return rowsNow })
		enq, _ := newBulkEnqueuer(t, st)
		res, err := enq.SubmitBulkRows(context.Background(), build(c.bad), rowsDefinition(t), "", rowsOwnerA)
		if err != nil {
			t.Fatal(err)
		}
		if res.Malformed != c.bad || res.Total != c.bad+2 {
			t.Errorf("%d malformed: counts total=%d malformed=%d, want exact %d / %d", c.bad, res.Total, res.Malformed, c.bad+2, c.bad)
		}
		wantListed := c.bad
		if wantListed > MalformedRowsCap {
			wantListed = MalformedRowsCap
		}
		if len(res.MalformedRows) != wantListed || res.MalformedRowsTruncated != c.truncated {
			t.Errorf("%d malformed: listed %d truncated=%v, want %d listed truncated=%v", c.bad, len(res.MalformedRows), res.MalformedRowsTruncated, wantListed, c.truncated)
		}
		// The listed rows are the FIRST ones, in order, with the caller's own ordinal (row 0 is the good first row, so bad row i is ordinal i+1).
		for i, m := range res.MalformedRows {
			if m.Ordinal != i+1 || m.ItemID != fmt.Sprintf("bad-%d", i) {
				t.Fatalf("%d malformed: listed entry %d = %+v, want ordinal %d item bad-%d", c.bad, i, m, i+1, i)
			}
		}
		// Every malformed row is a result of the job (they have an outcome from the start).
		_, total, settled, err := st.JobCompletion(context.Background(), res.JobID, rowsOwnerA)
		if err != nil || total != c.bad+2 || settled != c.bad {
			t.Errorf("%d malformed: job total=%d settled=%d err=%v, want %d rows of which the %d malformed have an outcome", c.bad, total, settled, err, c.bad+2, c.bad)
		}
	}
}
