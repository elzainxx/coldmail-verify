package queue

// The enqueuer's idempotent create (owner ruling OD-6/OD-8; migration 0019): SubmitBulkRowsKeyed, over BOTH stores like the rest of this package's
// bulk tests (Mem always; Postgres in a throwaway database when TM_TEST_DATABASE_URL is set).
//
// "Nothing was created twice" is measured where it matters, at the seam the drainer uses: a real drainer runs after the submissions and the Computer
// counts how many times each address is checked. A second job (or a second set of work items) would double those checks. Paired controls:
//   - a second keyed request with ANOTHER key makes a second job, and the checks double;
//   - an UNKEYED repeat makes a second job too (today's behaviour is unchanged);
//   - each variant of a request that must be "the same request" has a sibling that must not be.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/store"
)

func keyedRows() []BulkRow {
	return []BulkRow{
		{ItemID: "r0", Address: "Jane@AcmeCorp.com"},
		{ItemID: "r1", Address: "bob@acmecorp.com"},
		{ItemID: "r2", Address: "jane@acmecorp.com"}, // a duplicate of row 0
		{ItemID: "r3", Address: "not-an-address"},    // malformed
	}
}

func sameBulkResult(a, b BulkResult) bool {
	if a.JobID != b.JobID || a.Total != b.Total || a.Enqueued != b.Enqueued || a.Duplicate != b.Duplicate || a.Malformed != b.Malformed ||
		a.MalformedRowsTruncated != b.MalformedRowsTruncated || len(a.MalformedRows) != len(b.MalformedRows) {
		return false
	}
	for i := range a.MalformedRows {
		if a.MalformedRows[i] != b.MalformedRows[i] {
			return false
		}
	}
	return true
}

// A repeat with the same key and the same request is the ORIGINAL job: same id, same numbers, Replayed; no second job and no second set of work items
// (the drainer checks each distinct address exactly once); the listing is the original's. Another key makes another job (the control).
func TestSubmitBulkRowsKeyed_ARepeatIsTheOriginalJob_AndNothingIsCreatedTwice(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		enq, cipher := newBulkEnqueuer(t, st)
		def := rowsDefinition(t)
		idem := IdemInput{CallerID: "caller-1", Key: "retry-me"}

		first, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, idem)
		if err != nil || first.Replayed {
			t.Fatalf("a new key must create the job: %+v err=%v", first, err)
		}
		if first.Total != 4 || first.Enqueued != 2 || first.Duplicate != 1 || first.Malformed != 1 {
			t.Fatalf("the first answer's counts = %+v, want 4/2/1/1", first)
		}
		logsAfterFirst := logRowCount(t, st)

		// The platform's retry after a lost response: the same request, the same key.
		again, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, idem)
		if err != nil || !again.Replayed {
			t.Fatalf("a repeat must be a replay: %+v err=%v", again, err)
		}
		again.Replayed = false // compare everything else
		if !sameBulkResult(first, again) {
			t.Fatalf("a replay must carry the FIRST answer: first %+v, replay %+v", first, again)
		}
		if n := logRowCount(t, st); n != logsAfterFirst {
			t.Fatalf("a replay must not append LOG rows: %d, was %d", n, logsAfterFirst)
		}

		comp := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}
		drainEverything(t, st, cipher, comp)
		if comp.total() != 2 {
			t.Fatalf("with one job of 2 distinct addresses the drainer must run exactly 2 checks, ran %d (%v): a second set of work items exists", comp.total(), comp.calls)
		}
		if got := allResults(t, st, first.JobID, rowsOwnerA); len(got) != 4 {
			t.Fatalf("the original job must list its 4 rows, got %d", len(got))
		}

		// CONTROLS: another key from the same caller is another job (the checks double), and so is an UNKEYED repeat of the same request.
		other, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, IdemInput{CallerID: "caller-1", Key: "another-key"})
		if err != nil || other.Replayed || other.JobID == first.JobID {
			t.Fatalf("control: another key must create another job: %+v err=%v", other, err)
		}
		unkeyed, err := enq.SubmitBulkRows(ctx, keyedRows(), def, "ref", rowsOwnerA)
		if err != nil || unkeyed.Replayed || unkeyed.JobID == first.JobID || unkeyed.JobID == other.JobID {
			t.Fatalf("control: an unkeyed repeat must create another job, as it always did: %+v err=%v", unkeyed, err)
		}
		comp2 := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}
		drainEverything(t, st, cipher, comp2)
		if comp2.total() != 4 { // two more jobs x 2 distinct addresses
			t.Fatalf("control: two more jobs must add 4 checks, added %d (%v)", comp2.total(), comp2.calls)
		}
	})
}

// The same key with ANOTHER request is refused and creates nothing; the key stays the original job's. "The same request" is the canonical one: after
// norm/v1 (case, surrounding whitespace) and an omitted-versus-explicit default policy it is the same; an address, a label, the order, the row count,
// the caller_ref or the owner that differs makes it another request. A malformed row is identified by its position and label only.
func TestSubmitBulkRowsKeyed_TheSameKeyWithAnotherRequestIsRefused_AndCanonicalVariantsAreTheSameRequest(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		enq, _ := newBulkEnqueuer(t, st)
		def := rowsDefinition(t)
		idem := IdemInput{CallerID: "caller-1", Key: "the-key"}
		first, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, idem)
		if err != nil {
			t.Fatal(err)
		}

		swap := keyedRows()
		swap[0], swap[1] = swap[1], swap[0]
		relabel := keyedRows()
		relabel[1].ItemID = "other"
		another := keyedRows()
		another[1].Address = "carol@acmecorp.com"
		more := append(keyedRows(), BulkRow{ItemID: "r4", Address: "dave@acmecorp.com"})
		garbage := keyedRows()
		garbage[3].Address = "a different piece of garbage"
		spaced := keyedRows()
		spaced[0].Address = "  jane@ACMECORP.com\t"
		spaced[1].Address = "BOB@acmecorp.com"
		noLabels := []BulkRow{{Address: "Jane@AcmeCorp.com"}, {Address: "bob@acmecorp.com"}, {Address: "jane@acmecorp.com"}, {Address: "not-an-address"}}

		for _, tc := range []struct {
			name      string
			rows      []BulkRow
			callerRef string
			owner     string
			wantSame  bool
		}{
			{"the identical request", keyedRows(), "ref", rowsOwnerA, true},
			{"addresses written with other case and spaces (norm/v1)", spaced, "ref", rowsOwnerA, true},
			{"another piece of garbage in the malformed row", garbage, "ref", rowsOwnerA, true},
			{"an address that differs", another, "ref", rowsOwnerA, false},
			{"two rows swapped", swap, "ref", rowsOwnerA, false},
			{"a label that differs", relabel, "ref", rowsOwnerA, false},
			{"one more row", more, "ref", rowsOwnerA, false},
			{"the addresses[] shape (no labels) for the same addresses", noLabels, "ref", rowsOwnerA, false},
			{"a different caller_ref", keyedRows(), "other-ref", rowsOwnerA, false},
			{"a different owner under the same caller", keyedRows(), "ref", rowsOwnerB, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				res, err := enq.SubmitBulkRowsKeyed(ctx, tc.rows, def, tc.callerRef, tc.owner, idem)
				if tc.wantSame {
					if err != nil || !res.Replayed || res.JobID != first.JobID {
						t.Fatalf("must replay the original job %s: %+v err=%v", first.JobID, res, err)
					}
					return
				}
				if !errors.Is(err, ErrIdempotencyKeyReused) {
					t.Fatalf("must be refused with ErrIdempotencyKeyReused, got %+v err=%v", res, err)
				}
				if res.JobID != "" {
					t.Fatalf("a refusal must not disclose a job id: %q", res.JobID)
				}
			})
		}
		// Nothing above created a job: the original is still the only one (a drain finds only its 2 distinct addresses).
		enq2, cipher := newBulkEnqueuer(t, st)
		_ = enq2
		comp := &bulkCountingComputer{settleE2EComputer: settleE2EComputer{now: rowsNow}}
		drainEverything(t, st, cipher, comp)
		if comp.total() != 2 {
			t.Fatalf("after every refusal and replay there must still be ONE job (2 checks), saw %d (%v)", comp.total(), comp.calls)
		}
	})
}

// The key is scoped to the AUTHENTICATED caller: the same key from another caller is another job, and one caller's key is invisible to the other
// (a repeat by the other caller is a replay of ITS job, never of the first caller's).
func TestSubmitBulkRowsKeyed_TheKeyOfOneCallerIsInvisibleToAnother(t *testing.T) {
	bulkStores(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		enq, _ := newBulkEnqueuer(t, st)
		def := rowsDefinition(t)
		a, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, IdemInput{CallerID: "caller-a", Key: "shared-key"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerB, IdemInput{CallerID: "caller-b", Key: "shared-key"})
		if err != nil || b.Replayed || b.JobID == a.JobID {
			t.Fatalf("the same key from another caller must create its own job: %+v err=%v", b, err)
		}
		// Each caller's own repeat returns its OWN job.
		a2, err := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerA, IdemInput{CallerID: "caller-a", Key: "shared-key"})
		b2, err2 := enq.SubmitBulkRowsKeyed(ctx, keyedRows(), def, "ref", rowsOwnerB, IdemInput{CallerID: "caller-b", Key: "shared-key"})
		if err != nil || err2 != nil || !a2.Replayed || !b2.Replayed || a2.JobID != a.JobID || b2.JobID != b.JobID {
			t.Fatalf("each caller's repeat must be a replay of its own job: a2=%+v (%v) b2=%+v (%v)", a2, err, b2, err2)
		}
		// And the first caller can read only its own job.
		if _, known, _ := st.JobStatusSnapshot(ctx, a.JobID, rowsOwnerB); known {
			t.Error("caller B must not be able to read caller A's job")
		}
	})
}

// Concurrency on the in-memory store: N requests with one key at once create ONE job (the others are replays of it); N requests with N keys create N
// jobs (the control). The Postgres interleave, with both database backends proven in flight, is TestPGKeyedJobs_TwoRequestsWithOneKeyCreateExactlyOneJob_ForcedInterleave
// in internal/store.
func TestSubmitBulkRowsKeyed_ConcurrentRequestsWithOneKeyCreateOneJob(t *testing.T) {
	const n = 24
	run := func(keyOf func(i int) string) (jobs map[string]int, replays int) {
		st := store.NewMem(nil)
		enq, _ := newBulkEnqueuer(t, st)
		def := rowsDefinition(t)
		var mu sync.Mutex
		jobs = map[string]int{}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // all goroutines released together
				res, err := enq.SubmitBulkRowsKeyed(context.Background(), keyedRows(), def, "ref", rowsOwnerA, IdemInput{CallerID: "caller-1", Key: keyOf(i)})
				if err != nil {
					t.Errorf("request %d: %v", i, err)
					return
				}
				mu.Lock()
				jobs[res.JobID]++
				if res.Replayed {
					replays++
				}
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()
		return jobs, replays
	}
	jobs, replays := run(func(int) string { return "one-key" })
	if len(jobs) != 1 || replays != n-1 {
		t.Fatalf("%d concurrent requests with ONE key must create exactly one job and replay it %d times, got %d job(s) and %d replay(s): %v", n, n-1, len(jobs), replays, jobs)
	}
	jobs, replays = run(func(i int) string { return "key-" + string(rune('a'+i)) })
	if len(jobs) != n || replays != 0 {
		t.Fatalf("control: %d requests with %d different keys must create %d jobs, got %d job(s) and %d replay(s)", n, n, n, len(jobs), replays)
	}
}

// The prepared-but-discarded rows of a replay hold sealed plaintext only in memory: after a replay nothing of them is in the store (checked through
// the work items the drainer can lease: only the original's).
func TestSubmitBulkRowsKeyed_AReplayLeavesNoWorkItemBehind(t *testing.T) {
	st := store.NewMem(nil)
	enq, _ := newBulkEnqueuer(t, st)
	def := rowsDefinition(t)
	idem := IdemInput{CallerID: "caller-1", Key: "k"}
	first, err := enq.SubmitBulkRowsKeyed(context.Background(), keyedRows(), def, "ref", rowsOwnerA, idem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enq.SubmitBulkRowsKeyed(context.Background(), keyedRows(), def, "ref", rowsOwnerA, idem); err != nil {
		t.Fatal(err)
	}
	if n := len(st.WorkItemsForJob(first.JobID)); n != 2 {
		t.Fatalf("the original job must hold its 2 work items, holds %d", n)
	}
	total := 0
	for _, w := range st.AllWorkItems() {
		total++
		if w.JobID != first.JobID {
			t.Errorf("work item %s belongs to job %s, which is not the original", w.ID, w.JobID)
		}
	}
	if total != 2 {
		t.Fatalf("the store must hold exactly the original's 2 work items after a replay, holds %d", total)
	}
}
