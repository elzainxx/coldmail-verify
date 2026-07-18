package store

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/idgen"
)

// These tests exercise the GAP-37 CAS settlement + GAP-33 log-once + GAP-38 authoritative
// completion against the Mem fake (fast, where the fail-verify neuter/restore runs). The PG
// parity is proven separately in postgres_test.go (TestPGWorkItem*). The Mem CAS/lease semantics
// MUST match PG exactly (the P3 test-double lesson) — same conditions, same ErrLeaseLost.

func mkWorkItem(id, idem, domain string) WorkItemRow {
	return WorkItemRow{
		ID: id, IdempotencyKey: []byte(idem), Domain: domain, EmailHash: []byte("h-" + idem),
		PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg",
		Status: "ready", NotBefore: time.Unix(0, 0), AddrCiphertext: []byte("ciphertext-" + idem),
	}
}

func mkVerdict(t *testing.T, now time.Time) VerificationRow {
	t.Helper()
	id, _ := idgen.NewV7String(now)
	return VerificationRow{
		ID: id, RequestID: id, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte("h"), Domain: "ex.com", Status: "risky", Reason: "greylisted",
		Confidence: 10, Recommendation: "review", PolicyKey: "cold-email", PolicyVersion: 1,
		APIVersion: "v1", ConfigFingerprint: "cfg",
	}
}

// TestSettle_ExactlyOnceUnderSlowDrainerReclaim is the GAP-37 correctness-heart test: two attempts
// end up "live" on the same item (attempt A leased it, then a TTL reclaim re-readied it and attempt
// B leased it). Exactly ONE settle succeeds; the other gets ErrLeaseLost and appends NO LOG row.
//
// FAIL-VERIFY: remove the CAS guard (the `w.LockedBy != workerID || w.Status != "in_flight"` check)
// in MemStore.SettleWorkItem and BOTH settles succeed → the LOG holds TWO rows for one logical
// check → this test fails on the "exactly one LOG row" assertion. That proves the CAS, not the
// lease, is what makes settlement exactly-once.
func TestSettle_ExactlyOnceUnderSlowDrainerReclaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })

	item := mkWorkItem("018f0000-0000-7000-8000-0000000000a1", "idem-1", "ex.com")
	if _, _, err := mem.EnqueueWorkItem(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Attempt A leases it.
	leasedA, _ := mem.DequeueReady(ctx, "worker-A", 1, now)
	if len(leasedA) != 1 {
		t.Fatalf("attempt A: leased %d items, want 1", len(leasedA))
	}

	// The drainer's reclaim (attempt A is SLOW, not dead — but the TTL fired) re-readies it, then
	// attempt B leases it. This is the two-live-attempts scenario GAP-37 defends. Time advances past
	// the lease TTL so A's lease (stamped at `now`) is older than the cutoff.
	later := now.Add(2 * time.Minute)
	reclaimed, _ := mem.ReclaimExpiredLeases(ctx, time.Minute, later) // A's lease is now stale
	if reclaimed != 1 {
		t.Fatalf("reclaim: %d, want 1", reclaimed)
	}
	leasedB, _ := mem.DequeueReady(ctx, "worker-B", 1, later)
	if len(leasedB) != 1 {
		t.Fatalf("attempt B: leased %d items, want 1", len(leasedB))
	}

	// Now BOTH attempts try to settle. Attempt A (the SLOW one) finishes first with a 250-accepted;
	// attempt B finishes with a greylist. Only the CURRENT lease-holder (B) may transition the item.
	vA := mkVerdict(t, now)
	vA.Status, vA.Reason, vA.Recommendation = "valid", "accepted", "send"
	errA := mem.SettleWorkItem(ctx, item.ID, "worker-A", "settled", vA, nil, WorkItemAttemptRow{}, nil)

	vB := mkVerdict(t, now)
	errB := mem.SettleWorkItem(ctx, item.ID, "worker-B", "settled", vB, nil, WorkItemAttemptRow{}, nil)

	// Exactly one lost its lease, exactly one succeeded.
	lostCount := 0
	if errA == ErrLeaseLost {
		lostCount++
	} else if errA != nil {
		t.Fatalf("attempt A unexpected err: %v", errA)
	}
	if errB == ErrLeaseLost {
		lostCount++
	} else if errB != nil {
		t.Fatalf("attempt B unexpected err: %v", errB)
	}
	if lostCount != 1 {
		t.Fatalf("exactly one attempt must lose its lease; got %d lost (errA=%v errB=%v)", lostCount, errA, errB)
	}

	// The A attempt held a STALE lease (B is the current holder) → A must be the one that lost.
	if errA != ErrLeaseLost {
		t.Fatalf("the reclaimed (stale-lease) attempt A must lose the CAS, got errA=%v", errA)
	}

	// GAP-33: the append-only LOG holds EXACTLY ONE row for this logical check (never two verdicts).
	if len(mem.Log) != 1 {
		t.Fatalf("GAP-33 VIOLATED: LOG has %d rows for one logical check, want exactly 1", len(mem.Log))
	}
	// And it is B's verdict (the current lease-holder), not the stale A.
	if mem.Log[0].ID != vB.ID {
		t.Fatalf("the surviving verdict must be the current lease-holder's (B); got a different row")
	}
}

// TestSettle_PurgesPlaintextInTransaction proves the DR-4/OQ-P5-9 purge: addr_ciphertext is NULL
// after a successful settle (step 4 of the settlement tx), and remains present on a live item.
// FAIL-VERIFY: remove `w.AddrCiphertext = nil` from SettleWorkItem → the post-settle assertion fails.
func TestSettle_PurgesPlaintextInTransaction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })

	item := mkWorkItem("018f0000-0000-7000-8000-0000000000b2", "idem-2", "ex.com")
	mem.EnqueueWorkItem(ctx, item)

	// While live (ready), the ciphertext is present.
	if w, _, _ := mem.GetWorkItem(ctx, item.ID); len(w.AddrCiphertext) == 0 {
		t.Fatal("ciphertext must be present on a live (not-yet-settled) item")
	}
	leased, _ := mem.DequeueReady(ctx, "w1", 1, now)
	if len(leased) != 1 {
		t.Fatalf("leased %d", len(leased))
	}
	if err := mem.SettleWorkItem(ctx, item.ID, "w1", "settled", mkVerdict(t, now), nil, WorkItemAttemptRow{}, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// After settle: ciphertext purged (§5.3 step 4).
	w, _, _ := mem.GetWorkItem(ctx, item.ID)
	if len(w.AddrCiphertext) != 0 {
		t.Fatal("OQ-P5-9 VIOLATED: addr_ciphertext not purged in the settlement transaction")
	}
	if w.Status != "settled" || w.VerificationID == "" {
		t.Fatalf("settled item must be status=settled with verification_id set; got %s / %q", w.Status, w.VerificationID)
	}
}

// TestSpendOnce_UnderSlowDrainerReclaim is the P6 GAP-42 fail-verify: the paid vendor call's spend
// record commits UNDER THE SAME CAS as the settle, so a slow-drainer reclaim (two live attempts,
// each having called the vendor) records the charge EXACTLY ONCE — the stale attempt's spend record
// rolls back with its discarded settlement. This is the GAP-37 invariant extended from settle-once to
// spend-once (money instead of state).
//
// FAIL-VERIFY: neuter the CAS in SettleWorkItem (drop the locked_by/status guard) → BOTH attempts
// settle → TWO spend records appear → this test's "exactly one" assertion fails (a double-charge).
func TestSpendOnce_UnderSlowDrainerReclaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })

	item := mkWorkItem("018f0000-0000-7000-8000-0000000000d1", "idem-spend", "gmail.com")
	if _, _, err := mem.EnqueueWorkItem(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Attempt A leases; a reclaim (A is slow, not dead) re-readies it; attempt B leases it — the
	// two-live-attempts scenario, now with a PAID vendor call on each attempt.
	if l, _ := mem.DequeueReady(ctx, "worker-A", 1, now); len(l) != 1 {
		t.Fatalf("attempt A leased %d", len(l))
	}
	later := now.Add(2 * time.Minute)
	if n, _ := mem.ReclaimExpiredLeases(ctx, time.Minute, later); n != 1 {
		t.Fatalf("reclaim %d, want 1", n)
	}
	if l, _ := mem.DequeueReady(ctx, "worker-B", 1, later); len(l) != 1 {
		t.Fatalf("attempt B leased %d", len(l))
	}

	// Both attempts made a paid vendor call and try to settle external_verified with a spend record.
	vA := mkVerdict(t, now)
	vA.Status, vA.Reason = "risky", "external_verified"
	callA := &ExternalCallRow{ID: "018f0000-0000-7000-8000-00000000ca01", VerificationID: vA.ID, Vendor: "millionverifier", CostUnits: 1, Outcome: "ok", CalledAt: now}
	errA := mem.SettleWorkItem(ctx, item.ID, "worker-A", "settled", vA, nil, WorkItemAttemptRow{}, callA)

	vB := mkVerdict(t, now)
	vB.Status, vB.Reason = "risky", "external_verified"
	callB := &ExternalCallRow{ID: "018f0000-0000-7000-8000-00000000cb02", VerificationID: vB.ID, Vendor: "millionverifier", CostUnits: 1, Outcome: "ok", CalledAt: now}
	errB := mem.SettleWorkItem(ctx, item.ID, "worker-B", "settled", vB, nil, WorkItemAttemptRow{}, callB)

	// Exactly one attempt lost its lease (the stale A); exactly one settled.
	if errA != ErrLeaseLost {
		t.Fatalf("the stale-lease attempt A must lose the CAS, got errA=%v", errA)
	}
	if errB != nil {
		t.Fatalf("the current lease-holder B must settle, got errB=%v", errB)
	}
	// SPEND-ONCE (GAP-42): exactly ONE spend record — B's. A's charge rolled back with its lost CAS.
	if len(mem.ExternalCalls) != 1 {
		t.Fatalf("GAP-42 VIOLATED (double-charge): %d spend records for one settle, want exactly 1", len(mem.ExternalCalls))
	}
	if mem.ExternalCalls[0].ID != callB.ID {
		t.Fatalf("the surviving spend record must be the current lease-holder's (B), got %s", mem.ExternalCalls[0].ID)
	}
	// And exactly one LOG row (settle-once still holds).
	if len(mem.Log) != 1 {
		t.Fatalf("exactly one LOG row, got %d", len(mem.Log))
	}
}

// TestExternalSpendSince_WindowsAndOutcomes proves the budget read: only 'ok' calls count toward
// spend; all calls count toward the error-rate window; job/day/month windows scope correctly.
func TestExternalSpendSince_WindowsAndOutcomes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })
	mk := func(id string, cost int, outcome, job string, at time.Time) ExternalCallRow {
		return ExternalCallRow{ID: id, Vendor: "v1", JobID: job, CostUnits: cost, Outcome: outcome, CalledAt: at}
	}
	_ = mem.AppendExternalCall(ctx, mk("018f0000-0000-7000-8000-00000000e001", 3, "ok", "job1", now.Add(-time.Hour)))
	_ = mem.AppendExternalCall(ctx, mk("018f0000-0000-7000-8000-00000000e002", 5, "ok", "", now.Add(-2*time.Hour)))
	_ = mem.AppendExternalCall(ctx, mk("018f0000-0000-7000-8000-00000000e003", 0, "error", "job1", now.Add(-30*time.Minute)))
	// An OLD ok call outside the day window (25h ago) — counts toward month, not day.
	_ = mem.AppendExternalCall(ctx, mk("018f0000-0000-7000-8000-00000000e004", 7, "ok", "", now.Add(-25*time.Hour)))

	// A very recent call (10s ago) to exercise the last-minute rate_ceiling window.
	_ = mem.AppendExternalCall(ctx, mk("018f0000-0000-7000-8000-00000000e005", 1, "ok", "", now.Add(-10*time.Second)))

	s, err := mem.ExternalSpendSince(ctx, "v1", "job1", now.Add(-24*time.Hour), now.Add(-30*24*time.Hour), now.Add(-time.Hour), now.Add(-time.Minute), now)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if s.SpentDay != 9 { // 3 + 5 + the 1 recent (the 25h-old 7 is outside the day)
		t.Errorf("SpentDay = %d, want 9 (ok calls within the day only)", s.SpentDay)
	}
	if s.SpentMonth != 16 { // 3 + 5 + 7 + 1
		t.Errorf("SpentMonth = %d, want 16", s.SpentMonth)
	}
	if s.SpentJob != 3 { // only job1's ok call (the error cost 0)
		t.Errorf("SpentJob = %d, want 3", s.SpentJob)
	}
	// Error-rate window = last hour: ok@-1h (boundary) + error@-30m + ok@-10s = 3 calls, 1 error.
	if s.Calls != 3 || s.Errors != 1 {
		t.Errorf("error window: calls=%d errors=%d, want 3/1", s.Calls, s.Errors)
	}
	// Rate-ceiling window = last minute: only the ok@-10s call.
	if s.CallsLastMinute != 1 {
		t.Errorf("last-minute window: CallsLastMinute=%d, want 1", s.CallsLastMinute)
	}
}

// TestJobCompletion_AuthoritativeNotCounter is the GAP-38 test: completion is judged by the
// presence of non-terminal items, NOT by settled_count == total_count. We deliberately DRIFT the
// counter (a settled item whose counter increment we skip) and prove completion still reports true.
// FAIL-VERIFY: change MemStore.JobCompletion to `return j.SettledCount == j.TotalCount` → this test
// fails (drifted counter makes a finished job report "not complete" forever).
func TestJobCompletion_AuthoritativeNotCounter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })

	jobID := "018f0000-0000-7000-8000-0000000000c3"
	mem.CreateJob(ctx, JobRow{ID: jobID, PolicyKey: "cold-email", PolicyVersion: 1, TotalCount: 2})

	// Two items in the job.
	i1 := mkWorkItem("018f0000-0000-7000-8000-0000000000c1", "idem-c1", "ex.com")
	i1.JobID = jobID
	i2 := mkWorkItem("018f0000-0000-7000-8000-0000000000c2", "idem-c2", "ex.com")
	i2.JobID = jobID
	mem.EnqueueWorkItem(ctx, i1)
	mem.EnqueueWorkItem(ctx, i2)

	// Settle both (each bumps the counter to a nominal 2).
	for _, it := range []WorkItemRow{i1, i2} {
		leased, _ := mem.DequeueReady(ctx, "w1", 1, now)
		_ = leased
		if err := mem.SettleWorkItem(ctx, it.ID, "w1", "settled", mkVerdict(t, now), nil, WorkItemAttemptRow{}, nil); err != nil {
			t.Fatalf("settle %s: %v", it.ID, err)
		}
	}

	// Simulate counter drift (the exact hazard GAP-38 defends): forcibly set settled_count BELOW
	// total, as a missed increment under a reclaim/no-op would. Completion must STILL be true.
	j, _, _ := mem.GetJob(ctx, jobID)
	j.SettledCount = 0 // drifted low
	mem.CreateJob(ctx, j)

	complete, total, settled, err := mem.JobCompletion(ctx, jobID)
	if err != nil {
		t.Fatalf("JobCompletion: %v", err)
	}
	if !complete {
		t.Fatal("GAP-38 VIOLATED: job with all items settled reports not-complete because the counter drifted low")
	}
	if total != 2 {
		t.Fatalf("total=%d, want 2", total)
	}
	_ = settled // settled is cheap-display only; completion did not depend on it
}

// TestRequeue_CapacityWaitDoesNotBumpAttempt proves the §4.6/OQ-P5-7 rule: a rate_limited/no-eligible
// requeue (bumpAttempt=false) does NOT increment attempt (a capacity wait must not burn the greylist
// retry budget), while a provider-transient requeue (bumpAttempt=true) does.
func TestRequeue_CapacityWaitDoesNotBumpAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := NewMem(func() time.Time { return now })

	item := mkWorkItem("018f0000-0000-7000-8000-0000000000d4", "idem-d", "ex.com")
	mem.EnqueueWorkItem(ctx, item)

	// Capacity wait: no bump. (not_before is set into the future, so the next dequeue happens later.)
	mem.DequeueReady(ctx, "w1", 1, now)
	t1 := now.Add(time.Minute)
	if err := mem.RequeueWorkItem(ctx, item.ID, "w1", t1, "rate_limited", false, WorkItemAttemptRow{}); err != nil {
		t.Fatalf("requeue (capacity): %v", err)
	}
	if w, _, _ := mem.GetWorkItem(ctx, item.ID); w.Attempt != 0 {
		t.Fatalf("capacity-wait requeue must NOT bump attempt; got attempt=%d", w.Attempt)
	}

	// Provider-transient: bump. Dequeue at a time past the requeue's not_before so it's ready again.
	t2 := t1.Add(time.Second)
	if leased, _ := mem.DequeueReady(ctx, "w1", 1, t2); len(leased) != 1 {
		t.Fatalf("item should be ready again at t2; leased %d", len(leased))
	}
	if err := mem.RequeueWorkItem(ctx, item.ID, "w1", t2.Add(time.Minute), "greylisted", true, WorkItemAttemptRow{}); err != nil {
		t.Fatalf("requeue (greylist): %v", err)
	}
	if w, _, _ := mem.GetWorkItem(ctx, item.ID); w.Attempt != 1 {
		t.Fatalf("greylist requeue must bump attempt to 1; got attempt=%d", w.Attempt)
	}
}

// TestEnqueue_DedupsOnIdempotencyKey proves §5.5: a duplicate idempotency_key is a no-op.
func TestEnqueue_DedupsOnIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	mem := NewMem(nil)
	a := mkWorkItem("018f0000-0000-7000-8000-0000000000e1", "same-key", "ex.com")
	b := mkWorkItem("018f0000-0000-7000-8000-0000000000e2", "same-key", "ex.com") // same idem key, different id
	id1, existed1, _ := mem.EnqueueWorkItem(ctx, a)
	id2, existed2, _ := mem.EnqueueWorkItem(ctx, b)
	if existed1 {
		t.Fatal("first enqueue must not report alreadyExists")
	}
	if !existed2 {
		t.Fatal("second enqueue with the same idempotency_key must dedup (alreadyExists=true)")
	}
	if id2 != id1 {
		t.Fatalf("dedup must return the existing id %q, got %q", id1, id2)
	}
}
