package store

// Tests of migration 0019 and what the store does with it (owner rulings OD-6/OD-8 and OD-4; Docs/api-contract.md, Docs/install-and-deploy.md):
//
//   - RecordKeyedJob: an idempotent job. A new (caller, key) writes the job, its rows, the malformed rows' LOG rows and its work items in ONE
//     transaction; a repeat with the same fingerprint writes NOTHING and returns the original job and the numbers of its 202; the same key with
//     another fingerprint is a conflict that writes nothing; another caller's same key is another job; a refused batch leaves nothing behind and
//     does not consume the key;
//   - JobStatusSnapshot: the status of a job from ONE read, whose totals equal the listing row by row;
//   - RegisterNode / SettleProvisioningNode: a node is promoted only from a green audit row recorded after its registration, in one step that
//     records the row's id, with port25_open taken from that row.
//
// Every test runs against BOTH stores (Mem always, Postgres in a throwaway database when TM_TEST_DATABASE_URL is set: they SKIP honestly without
// it, like every PG-gated test here and counted by the test-execution gate in CI, where the database is always present), because a fake laxer than
// the real store is how a defect stays invisible. Every property has a paired control in the same test.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/testpg"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// keyedRecord builds one keyed submission: row 0 an address (item A), row 1 the SAME address (a duplicate of row 0, sharing A), row 2 a malformed
// row (its own LOG row), row 3 another address (item B). So total 4 = enqueued 2 + duplicate 1 + malformed 1.
func keyedRecord(t *testing.T, owner, caller, key string, fp byte) KeyedJobRecord {
	t.Helper()
	jobID := mustV7(t, scopeNow)
	a, b := scopeItem(t, jobID, "addr-a-"+caller+key), scopeItem(t, jobID, "addr-b-"+caller+key)
	rej := rejectRow(t)
	return KeyedJobRecord{
		Job: JobRow{ID: jobID, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: 4, CallerRef: "ref", OwnerUserID: owner},
		Rows: []JobRowEntry{
			{Ordinal: 0, ItemID: "r0", WorkItemID: a.ID},
			{Ordinal: 1, ItemID: "r1", WorkItemID: a.ID, DuplicateOf: intp(0)},
			{Ordinal: 2, ItemID: "r2", Malformed: true, VerificationID: rej.ID},
			{Ordinal: 3, ItemID: "r3", WorkItemID: b.ID},
		},
		Rejects: []VerificationRow{rej},
		Items:   []WorkItemRow{a, b},
		Claim:   IdemClaim{CallerID: caller, Key: key, Fingerprint: bytes.Repeat([]byte{fp}, 32)},
	}
}

// storeCounts reads how many jobs, job rows, work items and LOG rows a store holds (a scratch database or a fresh Mem holds only this test's).
type storeCounts struct{ jobs, jobRows, workItems, log int }

func countsOf(t *testing.T, st Store) storeCounts {
	t.Helper()
	switch s := st.(type) {
	case *MemStore:
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, rows := range s.jobRows {
			n += len(rows)
		}
		return storeCounts{jobs: len(s.jobs), jobRows: n, workItems: len(s.workItems), log: len(s.Log)}
	case *PG:
		var c storeCounts
		if err := s.pool.QueryRow(context.Background(), `
SELECT (SELECT count(*)::int FROM verify.jobs), (SELECT count(*)::int FROM verify.job_rows),
       (SELECT count(*)::int FROM verify.work_items), (SELECT count(*)::int FROM verify.verifications)`).Scan(&c.jobs, &c.jobRows, &c.workItems, &c.log); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		return c
	}
	t.Fatalf("countsOf: unsupported store %T (pass the base store, not a wrapper)", st)
	return storeCounts{}
}

// TestRecordKeyedJob: the idempotent create on both stores.
func TestRecordKeyedJob_NewKeyWritesAllAtOnce_ARepeatReturnsTheOriginalAndWritesNothing_AnotherRequestIsRefused(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		first := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		out, err := st.RecordKeyedJob(ctx, first, 1000)
		if err != nil || out.Replayed || out.Conflict || out.JobID != first.Job.ID {
			t.Fatalf("a new key must create the job: %+v err=%v", out, err)
		}
		written := countsOf(t, st)
		if want := (storeCounts{jobs: 1, jobRows: 4, workItems: 2, log: 1}); written != want {
			t.Fatalf("one keyed submission must write the job, its 4 rows, 2 work items and 1 LOG row: got %+v, want %+v", written, want)
		}

		// A REPEAT: the same caller, key and fingerprint, with a brand-new job id and brand-new work items (what a retried request prepares).
		again := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		rep, err := st.RecordKeyedJob(ctx, again, 1000)
		if err != nil || !rep.Replayed || rep.Conflict || rep.JobID != first.Job.ID {
			t.Fatalf("a repeat must return the ORIGINAL job: %+v err=%v (want job %s)", rep, err, first.Job.ID)
		}
		want := JobSubmission{Total: 4, Enqueued: 2, Duplicate: 1, Malformed: 1, MalformedRows: []MalformedRowRef{{Ordinal: 2, ItemID: "r2"}}}
		if rep.Submission.Total != want.Total || rep.Submission.Enqueued != want.Enqueued || rep.Submission.Duplicate != want.Duplicate ||
			rep.Submission.Malformed != want.Malformed || rep.Submission.MalformedRowsTruncated ||
			len(rep.Submission.MalformedRows) != 1 || rep.Submission.MalformedRows[0] != want.MalformedRows[0] {
			t.Fatalf("a replay must reproduce the numbers of the first answer: got %+v, want %+v", rep.Submission, want)
		}
		if got := countsOf(t, st); got != written {
			t.Fatalf("a replay must create NOTHING (no job, row, work item or LOG row): %+v, was %+v", got, written)
		}
		if _, known, _ := st.JobStatusSnapshot(ctx, again.Job.ID, scopeOwnerA); known {
			t.Fatal("the job id prepared by the repeat must not exist")
		}

		// THE SAME KEY WITH ANOTHER REQUEST: refused, nothing written, the other job's id not disclosed.
		other := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 2)
		cf, err := st.RecordKeyedJob(ctx, other, 1000)
		if err != nil || !cf.Conflict || cf.Replayed || cf.JobID != "" {
			t.Fatalf("the same key with another fingerprint must be a conflict that discloses nothing: %+v err=%v", cf, err)
		}
		if got := countsOf(t, st); got != written {
			t.Fatalf("a conflict must create NOTHING: %+v, was %+v", got, written)
		}

		// CONTROLS: the key is scoped to the caller, and another key is another job.
		diffCaller := keyedRecord(t, scopeOwnerB, "caller-2", "K1", 1)
		if o, err := st.RecordKeyedJob(ctx, diffCaller, 1000); err != nil || o.Replayed || o.Conflict || o.JobID != diffCaller.Job.ID {
			t.Fatalf("control: the same key from ANOTHER caller must create its own job: %+v err=%v", o, err)
		}
		diffKey := keyedRecord(t, scopeOwnerA, "caller-1", "K2", 1)
		if o, err := st.RecordKeyedJob(ctx, diffKey, 1000); err != nil || o.Replayed || o.Conflict || o.JobID != diffKey.Job.ID {
			t.Fatalf("control: another key from the same caller must create its own job: %+v err=%v", o, err)
		}
		if got := countsOf(t, st); got.jobs != 3 || got.workItems != 6 {
			t.Fatalf("three distinct (caller, key) pairs must make three jobs of 2 work items: %+v", got)
		}

		// The list of malformed rows follows the cap the caller asks for, and says so.
		capped, err := st.RecordKeyedJob(ctx, keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1), 0)
		if err != nil || !capped.Replayed || !capped.Submission.MalformedRowsTruncated || len(capped.Submission.MalformedRows) != 0 || capped.Submission.Malformed != 1 {
			t.Fatalf("a malformed list capped at 0 must be empty, truncated and still count 1: %+v err=%v", capped.Submission, err)
		}
	})
}

// A refused batch leaves nothing behind and does not CONSUME the key: the same (caller, key) can still create its job afterwards.
func TestRecordKeyedJob_ARefusedBatchLeavesNothing_AndDoesNotConsumeTheKey(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		before := countsOf(t, st)

		// A row names a work item that is not among the ones written.
		dangling := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		dangling.Rows[3].WorkItemID = mustV7(t, scopeNow)
		// Two work items of one job holding the same idempotency key (the database's uq_work_items_job_idem; the fake mirrors it).
		clash := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		clash.Items[1].IdempotencyKey = clash.Items[0].IdempotencyKey
		// A malformed row whose LOG row is not among the rejects.
		orphan := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		orphan.Rejects = nil
		// A claim with no fingerprint, and a key that is empty.
		noFP := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		noFP.Claim.Fingerprint = nil
		noKey := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		noKey.Claim.Key = ""

		for name, rec := range map[string]KeyedJobRecord{"a dangling work item": dangling, "a repeated work-item key": clash, "an orphan malformed row": orphan, "no fingerprint": noFP, "no key": noKey} {
			if out, err := st.RecordKeyedJob(ctx, rec, 1000); err == nil {
				t.Fatalf("%s must be refused, got %+v", name, out)
			}
			if got := countsOf(t, st); got != before {
				t.Fatalf("%s: a refused batch must leave nothing behind: %+v, was %+v", name, got, before)
			}
		}
		// CONTROL: the key was not consumed by any refusal above.
		good := keyedRecord(t, scopeOwnerA, "caller-1", "K1", 1)
		if out, err := st.RecordKeyedJob(ctx, good, 1000); err != nil || out.Replayed || out.Conflict || out.JobID != good.Job.ID {
			t.Fatalf("control: after the refusals the same (caller, key) must still create its job: %+v err=%v", out, err)
		}
	})
}

// TestUnkeyed: a job written WITHOUT a key carries no idempotency record at all, and RecordJob is as it was.
func TestUnkeyedJobsCarryNoIdempotencyRecord(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		rej := rejectRow(t)
		job := mustV7(t, scopeNow)
		a := scopeItem(t, job, "unkeyed-a")
		recordJob(t, st, job, scopeOwnerA, []JobRowEntry{{Ordinal: 0, WorkItemID: a.ID}, {Ordinal: 1, Malformed: true, VerificationID: rej.ID}}, []VerificationRow{rej})
		if _, existed, err := st.EnqueueWorkItem(context.Background(), a); err != nil || existed {
			t.Fatalf("EnqueueWorkItem: existed=%v err=%v", existed, err)
		}
		switch s := st.(type) {
		case *MemStore:
			s.mu.Lock()
			n := len(s.keyedJobs)
			s.mu.Unlock()
			if n != 0 {
				t.Fatalf("an unkeyed job must leave the idempotency index empty, it holds %d", n)
			}
		case *PG:
			var n int
			if err := s.pool.QueryRow(context.Background(),
				`SELECT count(*)::int FROM verify.jobs WHERE idempotency_key IS NOT NULL OR idem_caller_id IS NOT NULL OR request_fingerprint IS NOT NULL`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("an unkeyed job must have NULL idempotency columns, %d job(s) have one", n)
			}
		}
		// CONTROL: a keyed job IS recorded.
		keyed := keyedRecord(t, scopeOwnerA, "c", "K", 1)
		if _, err := st.RecordKeyedJob(context.Background(), keyed, 1000); err != nil {
			t.Fatal(err)
		}
		switch s := st.(type) {
		case *MemStore:
			s.mu.Lock()
			n := len(s.keyedJobs)
			s.mu.Unlock()
			if n != 1 {
				t.Fatalf("control: a keyed job must be in the idempotency index, it holds %d", n)
			}
		case *PG:
			var n int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM verify.jobs WHERE idempotency_key = 'K' AND idem_caller_id = 'c'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("control: the keyed job must carry its key, %d row(s) do", n)
			}
		}
	})
}

// ── JobStatusSnapshot ────────────────────────────────────────────────────────────────────────────────────────────────────────

// totalsViolations returns everything wrong with a totals object: the identities of the contract, and (given the job's listing, walked page by page)
// the equality with it row by row. It is the check AND, pointed at a doctored total, the paired control.
func totalsViolations(t *testing.T, st Store, jobID, owner string, tot JobTotals) []string {
	t.Helper()
	var v []string
	sum := 0
	for _, n := range tot.ByStatus {
		sum += n
	}
	if tot.Submitted != tot.Settled+tot.Pending {
		v = append(v, fmt.Sprintf("submitted %d != settled %d + pending %d", tot.Submitted, tot.Settled, tot.Pending))
	}
	if sum != tot.Settled {
		v = append(v, fmt.Sprintf("sum(by_status) %d != settled %d", sum, tot.Settled))
	}
	// Walk the listing in pages of 2 (so page boundaries fall inside the data) and tally it.
	listed, dup, mal := 0, 0, 0
	byStatus := map[string]int{}
	cursor := ""
	for {
		rows, next, err := st.ListJobResults(context.Background(), jobID, owner, cursor, 2)
		if err != nil {
			return append(v, fmt.Sprintf("listing: %v", err))
		}
		for _, r := range rows {
			listed++
			byStatus[r.Verdict.Status]++
			if r.DuplicateOf != nil {
				dup++
			}
			if r.Malformed {
				mal++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if listed != tot.Settled {
		v = append(v, fmt.Sprintf("the listing holds %d row(s), totals.settled is %d", listed, tot.Settled))
	}
	for _, s := range []string{"valid", "invalid", "risky", "unknown"} {
		if byStatus[s] != tot.ByStatus[s] {
			v = append(v, fmt.Sprintf("the listing holds %d %s row(s), totals.by_status[%s] is %d", byStatus[s], s, s, tot.ByStatus[s]))
		}
	}
	if tot.Pending == 0 { // the job is fully settled: the listing is every row, so the structural counts must match it too
		if dup != tot.Duplicates {
			v = append(v, fmt.Sprintf("the listing holds %d duplicate row(s), totals.duplicates is %d", dup, tot.Duplicates))
		}
		if mal != tot.Malformed {
			v = append(v, fmt.Sprintf("the listing holds %d malformed row(s), totals.malformed is %d", mal, tot.Malformed))
		}
		if listed != tot.Submitted {
			v = append(v, fmt.Sprintf("the listing holds %d row(s), totals.submitted is %d", listed, tot.Submitted))
		}
	}
	return v
}

// TestJobStatusSnapshot_TotalsEqualTheListingRowByRow_InFlightAndAfterCompletion: a job in flight (pending > 0) and a finished one both satisfy the
// identities and equal the listing; the snapshot of another owner's job is "not known".
func TestJobStatusSnapshot_TotalsEqualTheListingRowByRow_InFlightAndAfterCompletion(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		rec := keyedRecord(t, scopeOwnerA, "caller-1", "snap", 1)
		if _, err := st.RecordKeyedJob(ctx, rec, 1000); err != nil {
			t.Fatal(err)
		}
		snapshot := func() JobSnapshot {
			s, known, err := st.JobStatusSnapshot(ctx, rec.Job.ID, scopeOwnerA)
			if err != nil || !known {
				t.Fatalf("JobStatusSnapshot: known=%v err=%v", known, err)
			}
			return s
		}

		// IN FLIGHT: only the malformed row has an outcome.
		s := snapshot()
		if s.Complete {
			t.Fatal("a job with unsettled work items is not complete")
		}
		if got, want := s.Totals, (JobTotals{Submitted: 4, Settled: 1, Pending: 3, Duplicates: 1, Malformed: 1, ByStatus: map[string]int{"invalid": 1}}); got.Submitted != want.Submitted ||
			got.Settled != want.Settled || got.Pending != want.Pending || got.Duplicates != want.Duplicates || got.Malformed != want.Malformed ||
			got.ByStatus["invalid"] != 1 || got.ByStatus["valid"]+got.ByStatus["risky"]+got.ByStatus["unknown"] != 0 {
			t.Fatalf("in flight: totals %+v, want %+v", got, want)
		}
		if s.Job.TotalCount != 4 || s.Job.CallerRef != "ref" || s.Job.OwnerUserID != scopeOwnerA {
			t.Fatalf("the snapshot's job row is wrong: %+v", s.Job)
		}
		if v := totalsViolations(t, st, rec.Job.ID, scopeOwnerA, s.Totals); len(v) != 0 {
			t.Fatalf("in flight: %v", v)
		}

		// Settle item A (rows 0 and 1 share it): valid. Item B stays leased.
		leased, err := st.DequeueReady(ctx, "w-snap", 10, scopeNow)
		if err != nil || len(leased) != 2 {
			t.Fatalf("DequeueReady: %d item(s) err=%v", len(leased), err)
		}
		verdictFor := map[string]VerificationRow{}
		for _, it := range leased {
			v := rtRow(t, scopeNow, it.Domain, it.EmailHash)
			if it.ID == rec.Items[1].ID { // item B: an unknown caused by the other side
				v.Status, v.Reason, v.Recommendation = "unknown", "smtp_timeout", "review"
			}
			verdictFor[it.ID] = v
		}
		if err := st.SettleWorkItem(ctx, rec.Items[0].ID, "w-snap", "settled", verdictFor[rec.Items[0].ID], nil, WorkItemAttemptRow{}, nil); err != nil {
			t.Fatalf("settle A: %v", err)
		}
		s = snapshot()
		if s.Complete || s.Totals.Settled != 3 || s.Totals.Pending != 1 || s.Totals.ByStatus["valid"] != 2 || s.Totals.ByStatus["invalid"] != 1 {
			t.Fatalf("one item settled (it answers two rows): %+v complete=%v", s.Totals, s.Complete)
		}
		if v := totalsViolations(t, st, rec.Job.ID, scopeOwnerA, s.Totals); len(v) != 0 {
			t.Fatalf("half settled: %v", v)
		}

		// COMPLETE.
		if err := st.SettleWorkItem(ctx, rec.Items[1].ID, "w-snap", "settled", verdictFor[rec.Items[1].ID], nil, WorkItemAttemptRow{}, nil); err != nil {
			t.Fatalf("settle B: %v", err)
		}
		s = snapshot()
		want := JobTotals{Submitted: 4, Settled: 4, Pending: 0, Duplicates: 1, Malformed: 1}
		if !s.Complete || s.Totals.Submitted != want.Submitted || s.Totals.Settled != want.Settled || s.Totals.Pending != 0 || s.Totals.Duplicates != 1 || s.Totals.Malformed != 1 ||
			s.Totals.ByStatus["valid"] != 2 || s.Totals.ByStatus["invalid"] != 1 || s.Totals.ByStatus["unknown"] != 1 || s.Totals.ByStatus["risky"] != 0 {
			t.Fatalf("complete: %+v complete=%v", s.Totals, s.Complete)
		}
		if v := totalsViolations(t, st, rec.Job.ID, scopeOwnerA, s.Totals); len(v) != 0 {
			t.Fatalf("complete: %v", v)
		}

		// PAIRED CONTROL: the checker reports each kind of wrong count, so the green above is not a checker that accepts anything.
		for name, doctor := range map[string]func(t *JobTotals){
			"a settled count one too high":        func(t *JobTotals) { t.Settled++; t.Pending-- },
			"a pending count that breaks the sum": func(t *JobTotals) { t.Pending++ },
			"a by_status count one too high":      func(t *JobTotals) { t.ByStatus["valid"]++ },
			"a by_status count moved":             func(t *JobTotals) { t.ByStatus["valid"]--; t.ByStatus["risky"]++ },
			"a duplicates count of zero":          func(t *JobTotals) { t.Duplicates = 0 },
			"a malformed count of two":            func(t *JobTotals) { t.Malformed = 2 },
		} {
			bad := s.Totals
			bad.ByStatus = map[string]int{}
			for k, n := range s.Totals.ByStatus {
				bad.ByStatus[k] = n
			}
			doctor(&bad)
			if v := totalsViolations(t, st, rec.Job.ID, scopeOwnerA, bad); len(v) == 0 {
				t.Errorf("control: %s was not reported", name)
			}
		}

		// Another owner (or nobody) learns nothing: known=false, exactly like a job that does not exist.
		for _, owner := range []string{scopeOwnerB, ""} {
			if _, known, err := st.JobStatusSnapshot(ctx, rec.Job.ID, owner); known || err != nil {
				t.Errorf("owner %q: a job that is not theirs must read as unknown (known=%v err=%v)", owner, known, err)
			}
		}
		if _, known, _ := st.JobStatusSnapshot(ctx, mustV7(t, scopeNow), scopeOwnerA); known {
			t.Error("control: a job id that does not exist must read as unknown")
		}
	})
}

// ── node self-promotion on both stores ───────────────────────────────────────────────────────────────────────────────────────

func nodeAuditRow(t *testing.T, nodeID, status string) NodeAuditRow {
	t.Helper()
	return NodeAuditRow{ID: mustV7(t, scopeNow), NodeID: nodeID, AuditedAt: scopeNow, AuditStatus: status, Warnings: map[string]any{}, CurrentHostIP: "203.0.113.10"}
}

// pause separates two writes whose order the test relies on: Postgres stamps node_audits.created_at and nodes.registered_at with the database's
// clock, and the test must not depend on two statements landing in different microseconds. (The in-memory store orders by a logical clock.)
func pause() { time.Sleep(15 * time.Millisecond) }

func TestSettleProvisioningNode_PromotesOnlyFromAGreenRowRecordedAfterTheRegistration(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		const node, base = "n-promo", "http://n-promo.test"
		get := func() NodeHealth {
			h, known, err := st.GetNodeHealth(ctx, node)
			if err != nil || !known {
				t.Fatalf("GetNodeHealth: known=%v err=%v", known, err)
			}
			return h
		}

		// A row recorded BEFORE the registration (an earlier boot's, green).
		stale := nodeAuditRow(t, node, "green")
		if err := st.AppendNodeAudit(ctx, stale); err != nil {
			t.Fatal(err)
		}
		pause()
		if err := st.RegisterNode(ctx, node, base); err != nil {
			t.Fatalf("RegisterNode: %v", err)
		}
		if h := get(); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
			t.Fatalf("a registered node is provisioning, port25_open=false, unpromoted: %+v", h)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, stale.ID, true); err != nil || ok {
			t.Fatalf("an audit row recorded BEFORE the registration must not promote: ok=%v err=%v", ok, err)
		}
		if h := get(); h.Status != "provisioning" || h.PromotedByAuditID != "" {
			t.Fatalf("after the refusal: %+v", h)
		}

		// A red row recorded after the registration: refused; the node's port25_open stays what the audit says.
		pause()
		red := nodeAuditRow(t, node, "red")
		if err := st.AppendNodeAudit(ctx, red); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, red.ID, true); err != nil || ok {
			t.Fatalf("a red row must not promote: ok=%v err=%v", ok, err)
		}
		// Another node's green row, and an unknown id: refused.
		pause()
		foreign := nodeAuditRow(t, "someone-else", "green")
		if err := st.AppendNodeAudit(ctx, foreign); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, foreign.ID, true); err != nil || ok {
			t.Fatalf("another node's row must not promote this node: ok=%v err=%v", ok, err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, mustV7(t, scopeNow), true); err != nil || ok {
			t.Fatalf("an unknown row must not promote: ok=%v err=%v", ok, err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, "", true); err != nil || ok {
			t.Fatalf("an empty audit id must not promote: ok=%v err=%v", ok, err)
		}
		if h := get(); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
			t.Fatalf("none of the refusals may change the node: %+v", h)
		}

		// CONTROL: a green row of this node recorded after the registration promotes, in one step, with port25_open from the row and the row's id.
		pause()
		green := nodeAuditRow(t, node, "green")
		if err := st.AppendNodeAudit(ctx, green); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, green.ID, true); err != nil || !ok {
			t.Fatalf("control: a green row recorded after the registration must promote: ok=%v err=%v", ok, err)
		}
		if h := get(); h.Status != "active" || !h.Port25Open || h.PromotedByAuditID != green.ID {
			t.Fatalf("control: promoted node = %+v, want active, port25_open, promoted by %s", h, green.ID)
		}

		// An ACTIVE node is never touched by a later settle (it is not provisioning).
		pause()
		later := nodeAuditRow(t, node, "green")
		if err := st.AppendNodeAudit(ctx, later); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, later.ID, true); err != nil || ok {
			t.Fatalf("a node that is not provisioning must not be promoted again: ok=%v err=%v", ok, err)
		}
		if h := get(); h.PromotedByAuditID != green.ID {
			t.Fatalf("the promoting audit must stay the first one: %+v", h)
		}

		// THE RESTART: the boot registers it again. Provisioning, port25_open false, no promoting audit: a stale active is never trusted.
		pause()
		if err := st.RegisterNode(ctx, node, base); err != nil {
			t.Fatal(err)
		}
		if h := get(); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
			t.Fatalf("a restart must reset the node: %+v", h)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, later.ID, true); err != nil || ok {
			t.Fatalf("the previous boot's green row must not promote the re-registered node: ok=%v err=%v", ok, err)
		}
	})
}

// "promote=false": a node that is NOT promoted keeps port25_open at what the audit says (false), even if a person set it by hand; and a node that was
// only mirrored by UpsertNodeState (never RegisterNode: no registered_at) cannot be promoted at all.
func TestSettleProvisioningNode_NotPromoted_Port25IsFalse_AndAnUnregisteredNodeCannotBePromoted(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		const node = "n-manual"
		if err := st.UpsertNodeState(ctx, node, "http://n-manual.test", "provisioning", true); err != nil { // the old manual procedure: port25_open set by hand
			t.Fatal(err)
		}
		pause()
		row := nodeAuditRow(t, node, "green")
		if err := st.AppendNodeAudit(ctx, row); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, row.ID, true); err != nil || ok {
			t.Fatalf("a node with no registration of this code (registered_at NULL) must not be promoted: ok=%v err=%v", ok, err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, row.ID, false); err != nil || ok {
			t.Fatalf("promote=false never promotes: ok=%v err=%v", ok, err)
		}
		h, _, _ := st.GetNodeHealth(ctx, node)
		if h.Status != "provisioning" || h.Port25Open {
			t.Fatalf("promote=false must leave the node provisioning with port25_open=false (what the audit says): %+v", h)
		}
		// CONTROL: after RegisterNode the same kind of row promotes.
		pause()
		if err := st.RegisterNode(ctx, node, "http://n-manual.test"); err != nil {
			t.Fatal(err)
		}
		pause()
		fresh := nodeAuditRow(t, node, "green")
		if err := st.AppendNodeAudit(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.SettleProvisioningNode(ctx, node, fresh.ID, true); err != nil || !ok {
			t.Fatalf("control: ok=%v err=%v", ok, err)
		}
	})
}

// ── Postgres only ────────────────────────────────────────────────────────────────────────────────────────────────────────────

// The CHECK and the unique index of migration 0019, driven with a superuser connection (so the privileges of the runtime role do not hide a wrong
// constraint): a key outside [A-Za-z0-9._:-] or longer than 128, a key with no caller or no fingerprint, a fingerprint of the wrong length, and a
// fingerprint with no key are all refused by name; a repeated (caller, key) is a unique violation; the same key for another caller, and any number
// of keyless jobs, are fine.
func TestPGKeyedJobs_TheCheckAndTheUniqueIndexRefuseWhatTheAPIRefuses(t *testing.T) {
	_, dsn := scratchPG(t, 0)
	su := superConn(t, dsn)
	ctx := context.Background()
	insert := func(caller, key *string, fp []byte) error {
		_, err := su.Exec(ctx, `INSERT INTO verify.jobs (id, policy_key, policy_version, status, total_count, idem_caller_id, idempotency_key, request_fingerprint)
VALUES ($1,'cold-email',1,'running',0,$2,$3,$4)`, mustV7(t, scopeNow), caller, key, fp)
		return err
	}
	s := func(v string) *string { return &v }
	fp := bytes.Repeat([]byte{7}, 32)

	refused := map[string]error{
		"a space in the key":      insert(s("c"), s("a b"), fp),
		"a slash in the key":      insert(s("c"), s("a/b"), fp),
		"an empty key":            insert(s("c"), s(""), fp),
		"a 129-character key":     insert(s("c"), s(strings.Repeat("k", 129)), fp),
		"a key with no caller":    insert(nil, s("k"), fp),
		"a key with no finger":    insert(s("c"), s("k"), nil),
		"a short fingerprint":     insert(s("c"), s("k"), []byte{1, 2, 3}),
		"a fingerprint, no key":   insert(nil, nil, fp),
		"a caller, no key":        insert(s("c"), nil, nil),
		"a newline in the key":    insert(s("c"), s("a\nb"), fp),
		"a trailing newline only": insert(s("c"), s("ab\n"), fp),
	}
	for name, err := range refused {
		if err == nil || !strings.Contains(err.Error(), "jobs_idempotency_shape") {
			t.Errorf("%s must be refused by jobs_idempotency_shape, got %v", name, err)
		}
	}
	// CONTROLS: the boundary values and the legal alphabet are accepted, and many keyless jobs coexist.
	for name, key := range map[string]string{"128 characters": strings.Repeat("k", 128), "the whole alphabet": "Az09._:-", "one character": "x"} {
		if err := insert(s("caller-"+name), s(key), fp); err != nil {
			t.Errorf("control: %s must be accepted: %v", name, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := insert(nil, nil, nil); err != nil {
			t.Errorf("control: keyless job %d must be accepted: %v", i, err)
		}
	}
	if err := insert(s("c1"), s("dup"), fp); err != nil {
		t.Fatal(err)
	}
	if err := insert(s("c1"), s("dup"), fp); err == nil || !strings.Contains(err.Error(), "uq_jobs_caller_idempotency_key") {
		t.Errorf("a repeated (caller, key) must be a unique violation of uq_jobs_caller_idempotency_key, got %v", err)
	}
	if err := insert(s("c2"), s("dup"), fp); err != nil {
		t.Errorf("control: the same key for ANOTHER caller must be accepted: %v", err)
	}
}

// TestPGKeyedJobs_TwoRequestsWithOneKeyCreateExactlyOneJob_ForcedInterleave forces the interleave instead of hoping for it. A superuser transaction holds
// an ACCESS EXCLUSIVE lock on verify.work_items, so request A (the real RecordKeyedJob) inserts its job row and stops at its work-item INSERT, inside its
// transaction, holding the key. Request B (the same key, the real RecordKeyedJob) is then started: it must WAIT on A's uncommitted job row (a backend
// waiting on a transactionid lock in the jobs INSERT), which proves both backends are in flight at once and that the key is the thing serialising them.
// Releasing the lock lets A commit; B then wakes, finds the key taken and returns the ORIGINAL job. One job exists. The paired control does the same with
// two DIFFERENT keys: B is not waiting on A (it stops later, at the work-item INSERT, on the table lock) and two jobs exist.
func TestPGKeyedJobs_TwoRequestsWithOneKeyCreateExactlyOneJob_ForcedInterleave(t *testing.T) {
	type result struct {
		out KeyedJobOutcome
		err error
	}
	run := func(t *testing.T, keyB string) (a, b KeyedJobRecord, outA, outB KeyedJobOutcome, st storeCounts, bWaitedOnTheJob bool) {
		pg, dsn := scratchPG(t, 0)
		su := superConn(t, dsn)  // holds the table lock inside a transaction
		mon := superConn(t, dsn) // a SEPARATE session reads pg_stat_activity: inside the blocker's transaction the statistics would be a cached snapshot
		ctx := context.Background()
		waiting := func(fragment string) int {
			var n int
			if err := mon.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'active' AND wait_event_type = 'Lock' AND query ILIKE $1`, "%"+fragment+"%").Scan(&n); err != nil {
				t.Fatalf("pg_stat_activity: %v", err)
			}
			return n
		}
		waitUntil := func(what string, cond func() bool) {
			t.Helper()
			deadline := time.Now().Add(20 * time.Second)
			for !cond() {
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s", what)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}

		// The blocker: nobody can INSERT into work_items until this transaction ends.
		blocker, err := su.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.Exec(ctx, `LOCK TABLE verify.work_items IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = blocker.Rollback(ctx)
			}
		}
		t.Cleanup(release)

		a = keyedRecord(t, scopeOwnerA, "caller-1", "the-key", 1)
		b = keyedRecord(t, scopeOwnerA, "caller-1", keyB, 1)
		chA, chB := make(chan result, 1), make(chan result, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); o, e := pg.RecordKeyedJob(ctx, a, 1000); chA <- result{o, e} }()
		// A is in flight: its job row is inserted (holding the key) and it is blocked at its work-item INSERT.
		waitUntil("request A to block at its work-item INSERT", func() bool { return waiting("INSERT INTO verify.work_items") == 1 })
		go func() { defer wg.Done(); o, e := pg.RecordKeyedJob(ctx, b, 1000); chB <- result{o, e} }()

		if keyB == "the-key" {
			// B must be waiting on A's uncommitted job row: a lock wait inside the jobs INSERT, while A is still waiting in the work-item INSERT.
			waitUntil("request B to wait on request A's key", func() bool { return waiting("ON CONFLICT (idem_caller_id") == 1 })
			bWaitedOnTheJob = true
			if n := waiting("INSERT INTO verify.work_items"); n != 1 {
				t.Fatalf("both backends must be in flight at once: %d backend(s) still at the work-item INSERT, want exactly A's", n)
			}
		} else {
			// Another key: B does not wait for A's job; it gets as far as ITS work-item INSERT and waits there on the table lock, beside A.
			waitUntil("request B to reach the work-item INSERT", func() bool { return waiting("INSERT INTO verify.work_items") == 2 })
			if n := waiting("ON CONFLICT (idem_caller_id"); n != 0 {
				t.Fatalf("control: with another key B must not wait on A's job row, but %d backend(s) wait in the jobs INSERT", n)
			}
		}
		select {
		case r := <-chA:
			t.Fatalf("A returned while the table lock was held: %+v", r)
		case r := <-chB:
			t.Fatalf("B returned while it should be waiting: %+v", r)
		default:
		}

		release()
		wg.Wait()
		ra, rb := <-chA, <-chB
		if ra.err != nil || rb.err != nil {
			t.Fatalf("both requests must succeed: A=%v B=%v", ra.err, rb.err)
		}
		return a, b, ra.out, rb.out, countsOf(t, pg), bWaitedOnTheJob
	}

	t.Run("one key: one job", func(t *testing.T) {
		a, _, outA, outB, counts, waited := run(t, "the-key")
		if !waited {
			t.Fatal("the test did not observe B waiting on A's key: it proved nothing")
		}
		if outA.Replayed || outA.JobID != a.Job.ID {
			t.Fatalf("A must create the job: %+v", outA)
		}
		if !outB.Replayed || outB.JobID != a.Job.ID {
			t.Fatalf("B must be a replay of A's job: %+v (A's job is %s)", outB, a.Job.ID)
		}
		if outB.Submission.Total != 4 || outB.Submission.Enqueued != 2 || outB.Submission.Duplicate != 1 || outB.Submission.Malformed != 1 {
			t.Fatalf("B's replay must carry A's numbers: %+v", outB.Submission)
		}
		if want := (storeCounts{jobs: 1, jobRows: 4, workItems: 2, log: 1}); counts != want {
			t.Fatalf("exactly ONE job's worth of rows must exist: %+v, want %+v", counts, want)
		}
	})
	t.Run("control: two keys: two jobs", func(t *testing.T) {
		a, b, outA, outB, counts, _ := run(t, "another-key")
		if outA.Replayed || outB.Replayed || outA.JobID != a.Job.ID || outB.JobID != b.Job.ID || a.Job.ID == b.Job.ID {
			t.Fatalf("two keys must make two jobs: A=%+v B=%+v", outA, outB)
		}
		if want := (storeCounts{jobs: 2, jobRows: 8, workItems: 4, log: 2}); counts != want {
			t.Fatalf("two jobs' worth of rows must exist: %+v, want %+v", counts, want)
		}
	})
}

// The real runtime login: every new statement works as verify_app (no superuser, default search_path), which is what the control plane runs as. A grant
// that 0019 forgot (the new columns ride the table-level grants, which this proves) would fail here with 42501, not in production.
func TestRuntimeAsVerifyApp_IdempotentJobsSnapshotsAndSelfPromotion(t *testing.T) {
	ctx := context.Background()
	suDSN := testpg.Scratch(t, 0)
	su := superConn(t, suDSN)
	pg := openAsVerifyApp(t, suDSN)

	rec := keyedRecord(t, scopeOwnerA, "caller-rt", "rt-key", 1)
	if out, err := pg.RecordKeyedJob(ctx, rec, 1000); err != nil || out.Replayed || out.JobID != rec.Job.ID {
		t.Fatalf("RecordKeyedJob as verify_app: %+v err=%v", out, err)
	}
	if out, err := pg.RecordKeyedJob(ctx, keyedRecord(t, scopeOwnerA, "caller-rt", "rt-key", 1), 1000); err != nil || !out.Replayed || out.JobID != rec.Job.ID {
		t.Fatalf("the replay as verify_app: %+v err=%v", out, err)
	}
	if got := suOne[int](t, su, `SELECT count(*)::int FROM verify.jobs WHERE idempotency_key = 'rt-key'`); got != 1 {
		t.Fatalf("the superuser read-back must find exactly one job with the key, found %d", got)
	}
	snap, known, err := pg.JobStatusSnapshot(ctx, rec.Job.ID, scopeOwnerA)
	if err != nil || !known || snap.Totals.Submitted != 4 {
		t.Fatalf("JobStatusSnapshot as verify_app: known=%v err=%v %+v", known, err, snap.Totals)
	}

	const node = "n-rt-promo"
	if err := pg.RegisterNode(ctx, node, "http://n-rt-promo.test"); err != nil {
		t.Fatalf("RegisterNode as verify_app: %v", err)
	}
	pause()
	row := nodeAuditRow(t, node, "green")
	if err := pg.AppendNodeAudit(ctx, row); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.SettleProvisioningNode(ctx, node, row.ID, true); err != nil || !ok {
		t.Fatalf("SettleProvisioningNode as verify_app: ok=%v err=%v", ok, err)
	}
	if got := suOne[string](t, su, `SELECT status || '/' || port25_open::text || '/' || promoted_by_audit_id::text FROM verify.nodes WHERE id=$1`, node); got != "active/true/"+row.ID {
		t.Fatalf("the superuser read-back of the promoted node = %q, want active/true/%s", got, row.ID)
	}
}

// TestVocabMirror_JobStatuses: the database CHECK on verify.jobs.status (migration 0009, valid_job_status) must name exactly the job statuses the code
// declares (vocab.JobStatuses, which GET /v1/describe publishes). A status the code can write but the database refuses fails at the worst moment: an
// INSERT of a job. The control is a doctored code list, which must differ.
func TestVocabMirror_JobStatuses(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	su := superConn(t, dsn)
	var def string
	if err := su.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'valid_job_status' AND conrelid = 'verify.jobs'::regclass`).Scan(&def); err != nil {
		t.Fatalf("read valid_job_status: %v", err)
	}
	var inDB []string
	for _, m := range quotedText.FindAllStringSubmatch(def, -1) {
		inDB = append(inDB, m[1])
	}
	var inCode []string
	for _, s := range vocab.JobStatuses() {
		inCode = append(inCode, string(s))
	}
	if onlyCode, onlyDB := diff(inCode, inDB); len(onlyCode) != 0 || len(onlyDB) != 0 {
		t.Fatalf("verify.jobs.valid_job_status drifted from vocab.JobStatuses(): in the code but REJECTED by the database: %v; in the database but not in the code: %v", onlyCode, onlyDB)
	}
	// CONTROL: the comparison reports a status the code gained that the database does not have.
	if onlyCode, _ := diff(append(append([]string(nil), inCode...), "paused"), inDB); len(onlyCode) != 1 || onlyCode[0] != "paused" {
		t.Fatalf("control: a status missing from the CHECK must be reported, got %v", onlyCode)
	}
}
