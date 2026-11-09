package queue

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// =============================================================================
// Kill-switch Rung 4 — accept-and-defer actually stops the drainer (fix-plan item 4 / D2)
// =============================================================================
//
// WHAT WAS WRONG. `FrontDoorDefer = "accept_and_defer"` passed Validate() and had NO consumer: both
// readers of KillSwitch.FrontDoor compared only against FrontDoorReject, so TM_FRONTDOOR_MODE=
// accept_and_defer validated and then behaved EXACTLY like serving. An operator reaching for the
// middle rung during a data-integrity incident got full serving, with no error and no warning.
//
// WHAT THE RUNG MEANS, from its own definition (history/kill-switch-ladder.md Rung 4): "the front door
// accepts submissions and returns a job handle (P5 §7), but the drainer is stopped — work sits
// durably in work_items, nothing is processed. Restart resumes with zero loss." So the rung has
// exactly two testable halves, and this file owns the second:
//
//	(1) the front door still ACCEPTS      → api: TestFrontDoorDefer_* (bulk 202, sync 503)
//	(2) the drainer PROCESSES NOTHING     → here
//
// The four properties proven below are the ones an operator's safety depends on:
//
//	P1 NOTHING RUNS.      A paused tick computes no check and appends no LOG row. Zero verdicts
//	                      produced is the literal content of "verdict production disabled".
//	P2 NOTHING IS TAKEN.  A paused tick leases no item: the row stays 'ready' with an empty
//	                      locked_by. This is why the pause is checked BEFORE DequeueReady — a gate
//	                      after the lease would strand every in-flight item behind a lease that a
//	                      paused drainer will never settle and a paused reclaim will never sweep.
//	P3 NOTHING IS LOST.   Status, attempt and not_before are untouched by the pause — a freeze, not
//	                      a drop, and not a silent attempt burn.
//	P4 RESUME WORKS.      Releasing the rung drains the accumulated work on the very next tick. A
//	                      pause you cannot come back from is an outage, not a safety control.
//
// ANTI-VACUITY. Every pause assertion below is paired with the SAME fixture running unpaused, which
// must settle. Without that pairing a drainer broken in any other way (no items enqueued, a fake
// that never returns, a wrong worker id) would produce "nothing was processed" and read as a pass —
// which is precisely how D2 hid: the absence of an effect looked like the absence of a problem.
//
// FAIL-VERIFY:
//   - Delete the `if paused := dr.d.Paused(); ...` block at the top of tick() → P1/P2/P3 fail
//     (the paused drainer settles the item).
//   - Move that block to AFTER DequeueReady → P2 fails: the item comes back in_flight, leased by a
//     drainer that will not settle it. That is the stranding bug the placement exists to avoid.
//   - Make NewDrainer default Paused to `func() bool { return true }` → the unpaused controls fail.
//   - Change api.go's bulk-submit gate from FrontDoorRejecting to VerdictProductionPaused → the
//     api-side accept half fails (submissions would be refused at the wrong sub-mode).

// countingComputer records every ComputeForQueue call and settles on the first attempt. The COUNT is
// the point: "no LOG row" could also be produced by a compute that ran and failed, and the two are
// very different outcomes for a paused system.
type countingComputer struct {
	calls int
}

func (c *countingComputer) ComputeForQueue(_ context.Context, _ string, _ *policy.Definition, _ string) (orchestrator.QueueOutcome, error) {
	c.calls++
	row := store.VerificationRow{
		ID:        "018f0000-0000-7000-8000-0000000004a0",
		RequestID: "018f0000-0000-7000-8000-000000000fff",
		CheckedAt: time.Unix(1, 0), CreatedAt: time.Unix(1, 0),
		Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
	}
	return orchestrator.QueueOutcome{Class: orchestrator.QSettled, Row: row}, nil
}

func (c *countingComputer) EscalateExhausted(_ context.Context, _ string, _ *policy.Definition, out orchestrator.QueueOutcome, _, _ string) orchestrator.QueueOutcome {
	return out
}

// pausableDrainer builds a drainer over mem whose pause state is the returned pointer's value, so a
// test can flip the rung mid-run exactly as an operator does.
func pausableDrainer(t *testing.T, mem *store.MemStore, comp Computer, clock time.Time, paused *bool) *Drainer {
	t.Helper()
	dr, err := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w-pause", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
		Paused: func() bool { return *paused },
	})
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	return dr
}

const pauseItemID = "018f0000-0000-7000-8000-000000000401"

// TestDrainer_PausedProcessesNothingThenResumesWithZeroLoss is P1–P4 in one arc, because the arc is
// the property: the same item, the same drainer, paused then released.
func TestDrainer_PausedProcessesNothingThenResumesWithZeroLoss(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	comp := &countingComputer{}
	paused := true
	dr := pausableDrainer(t, mem, comp, clock, &paused)

	enqueue(t, mem, pauseItemID, "p1", "ex.com", "jane@ex.com", "job-paused", clock)

	// Several ticks while paused. More than one, because a pause that only holds for the first tick
	// (say, a latch set in NewDrainer) would pass a single-tick test.
	for i := 0; i < 3; i++ {
		if worked := dr.TickOnce(ctx); worked {
			t.Fatalf("tick %d: a PAUSED drainer must report no work done (Run must idle-sleep, not spin)", i)
		}
	}

	// P1: nothing ran, nothing was logged.
	if comp.calls != 0 {
		t.Fatalf("PAUSED drainer computed %d check(s) — Rung 4 disables verdict production", comp.calls)
	}
	if len(mem.Log) != 0 {
		t.Fatalf("PAUSED drainer appended %d LOG row(s) — a paused system must produce no verdicts", len(mem.Log))
	}

	// P2 + P3: the item was never leased, and is otherwise exactly as enqueued.
	w, ok, err := mem.GetWorkItem(ctx, pauseItemID)
	if err != nil || !ok {
		t.Fatalf("work item must still exist while paused (ok=%v err=%v) — defer stores durably, it does not drop", ok, err)
	}
	if w.Status != "ready" {
		t.Fatalf("paused: item status=%q, want ready — a paused drainer must not move an item", w.Status)
	}
	if w.LockedBy != "" || !w.LockedAt.IsZero() {
		t.Fatalf("paused: item was LEASED (locked_by=%q at=%v) — the pause must be checked BEFORE DequeueReady, or a pause strands every in-flight item behind a lease nothing will settle",
			w.LockedBy, w.LockedAt)
	}
	if w.Attempt != 0 {
		t.Fatalf("paused: attempt=%d, want 0 — a pause must not burn a retry", w.Attempt)
	}
	if !w.NotBefore.Equal(clock) {
		t.Fatalf("paused: not_before moved to %v (was %v) — a pause must not reschedule work", w.NotBefore, clock)
	}
	if len(w.AddrCiphertext) == 0 {
		t.Fatal("paused: the DR-4 ciphertext is gone — the deferred address must still be there to check when the rung is released")
	}

	// P4 (and the anti-vacuity control for everything above): release the rung. The SAME drainer, the
	// SAME item, one tick — and it settles. If this half failed, "nothing was processed" above would
	// prove nothing about the pause.
	paused = false
	if worked := dr.TickOnce(ctx); !worked {
		t.Fatal("RESUMED drainer did no work — the accumulated item must drain once the rung is released")
	}
	if comp.calls != 1 {
		t.Fatalf("after resume: %d compute call(s), want exactly 1", comp.calls)
	}
	if len(mem.Log) != 1 {
		t.Fatalf("after resume: %d LOG row(s), want exactly 1 (zero loss: the deferred check ran)", len(mem.Log))
	}
	w, _, _ = mem.GetWorkItem(ctx, pauseItemID)
	if w.Status != "settled" {
		t.Fatalf("after resume: item status=%q, want settled", w.Status)
	}
}

// TestDrainer_PauseDoesNotReclaimLeases proves the pause also freezes the §5.2 reclaim sweep, and
// that this is SAFE rather than a leak: because a paused drainer takes no new lease (the test above),
// the only in_flight items during a pause are ones a pre-pause attempt owns. Sweeping them mid-pause
// would re-ready work that nothing is allowed to process; leaving them is what "restart resumes with
// zero loss" requires, and the sweep catches them on the first tick after release.
func TestDrainer_PauseDoesNotReclaimLeases(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	comp := &countingComputer{}
	paused := false
	dr := pausableDrainer(t, mem, comp, clock, &paused)

	// Hand-place an item that is in_flight under a DEAD holder with an expired lease — exactly what
	// the reclaim sweep exists for.
	// Born two hours ago so it was already due when the dead worker leased it an hour ago.
	born := clock.Add(-2 * time.Hour)
	stale := store.WorkItemRow{
		ID: "018f0000-0000-7000-8000-000000000402", IdempotencyKey: []byte("p2"),
		Domain: "ex.com", EmailHash: []byte("h-p2"), PolicyKey: "cold-email", PolicyVersion: 1,
		ConfigFingerprint: "cfg", Status: "ready", JobID: "job-paused",
		NotBefore: born, CreatedAt: born, AddrCiphertext: []byte("bob@ex.com"),
	}
	if _, _, err := mem.EnqueueWorkItem(ctx, stale); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Stage the stale lease through the REAL store API (a dequeue an hour ago by a worker that then
	// died), not by writing the row by hand — so the fixture is a state the system can actually
	// reach, and LockedAt is set the way DequeueReady sets it.
	leased, err := mem.DequeueReady(ctx, "dead-worker", 5, clock.Add(-time.Hour))
	if err != nil || len(leased) != 1 {
		t.Fatalf("stage a stale lease: leased=%d err=%v", len(leased), err)
	}

	// Paused: the sweep must not run.
	paused = true
	dr.TickOnce(ctx)
	w, _, _ := mem.GetWorkItem(ctx, stale.ID)
	if w.Status != "in_flight" || w.LockedBy != "dead-worker" {
		t.Fatalf("paused: the stale lease was reclaimed (status=%q locked_by=%q) — a paused drainer must not re-ready work it is not allowed to process",
			w.Status, w.LockedBy)
	}

	// Released: the sweep runs and the item is recovered — the anti-vacuity control proving the
	// assertion above is about the pause and not about a reclaim that never works.
	paused = false
	dr.TickOnce(ctx)
	w, _, _ = mem.GetWorkItem(ctx, stale.ID)
	if w.LockedBy == "dead-worker" {
		t.Fatalf("after release: the stale lease was NOT reclaimed (status=%q locked_by=%q) — the pause assertion above would be vacuous",
			w.Status, w.LockedBy)
	}
}

// TestDrainer_NilPausedNeverPauses pins the default: Rungs 0–3 are the overwhelmingly common case,
// and every existing caller (including every other test in this package) passes no Paused at all. A
// nil that defaulted the wrong way, or that panicked, would silently stop the queue.
func TestDrainer_NilPausedNeverPauses(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	comp := &countingComputer{}
	dr, err := NewDrainer(Deps{
		Store: mem, Compute: comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w-nil", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
		// Paused deliberately unset.
	})
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	enqueue(t, mem, "018f0000-0000-7000-8000-000000000403", "p3", "ex.com", "carol@ex.com", "job-nil", clock)
	if worked := dr.TickOnce(ctx); !worked {
		t.Fatal("a drainer with no Paused func must drain normally — nil must mean 'never paused', not 'always paused' and not a panic")
	}
	if comp.calls != 1 {
		t.Fatalf("nil Paused: %d compute call(s), want 1", comp.calls)
	}
}
