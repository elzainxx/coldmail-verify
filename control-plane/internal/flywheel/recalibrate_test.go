package flywheel

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/store"
)

// recalParams: a low sample gate so tests can drive moves with modest volume.
func recalParams(minSamples int) GuardParams {
	return GuardParams{
		MinBucketSamples: minSamples, MaxStep: 10, UpStepFactor: 0.5,
		BrakeWindowCycles: 3, BrakeBucketFraction: 0.9, BrakeMinBucketsInWin: 100,
	}
}

// seed N calibratable outcomes for one bucket: `invalid` of them are mailbox_not_found (invalid-signal),
// the rest delivered on a discriminating server (valid-signal). All join to a valid/accepted×provider
// verdict so the bucket key is (accepted × provider).
func seedBucket(t *testing.T, m *store.MemStore, provider string, total, invalid int, now time.Time) {
	t.Helper()
	ing := &Ingestor{Store: m, Now: func() time.Time { return now }}
	// Per-provider source key: the ingest cursor/watermark is stored PER source key, so a shared key
	// across seedBucket calls would make the second call resume past the first's outcomes (pull nothing).
	src := NewSimulatedSource("src-" + provider)
	for i := 0; i < total; i++ {
		vid := provider + "-v-" + itoaCursor(i)
		seedVerdict(m, vid, "valid", "accepted", provider, false, now.Add(-time.Hour), nil)
		event := "250 delivered"
		if i < invalid {
			event = "550 5.1.1 user unknown"
		}
		src.Add(RawOutcome{VerificationID: vid, MessageID: provider + "-m-" + itoaCursor(i), Event: event, SentAt: now, OccurredAt: now})
	}
	if _, err := ing.IngestOnce(context.Background(), src, total+10); err != nil {
		t.Fatal(err)
	}
}

// TestRecalibrate_RecommendOnly_NoEffectiveVersion is the FRESH-SYSTEM / recommend-only invariant
// (DR-2 / shadow-mode §2.1): a recalibration pass at recommend-only writes a PROPOSAL but NO effective
// version — the verdict path (GetEffectiveCalibration) still sees nothing → cold-start → P1 baseline.
// This is the "zero live mutation at default mode" acceptance criterion. FAIL-VERIFY: make the
// recommend-only pass stamp effective_from → GetEffectiveCalibration returns a version → this breaks.
func TestRecalibrate_RecommendOnly_NoEffectiveVersion(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	seedBucket(t, m, "google", 600, 120, now) // 600 outcomes, 20% invalid-signal — above a 500 gate

	d := &RecalibrateDeps{Store: m, Params: recalParams(500), Mode: ModeRecommendOnly, Now: func() time.Time { return now }, WorkerID: "w1"}
	res, err := d.RunOnce(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || res.BucketsMoved != 1 {
		t.Fatalf("recommend-only pass: committed=%v moved=%d, want committed + 1 proposal", res.Committed, res.BucketsMoved)
	}
	// The proposal exists in the store...
	if len(m.Calibration) != 1 {
		t.Fatalf("calibration versions = %d, want 1 proposal", len(m.Calibration))
	}
	// ...but is NOT effective → the verdict path sees cold-start (the fresh-system invariant).
	_, known, err := m.GetEffectiveCalibration(context.Background(), "accepted", "google", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if known {
		t.Fatal("a recommend-only proposal became EFFECTIVE — zero-live-mutation-at-default-mode violated (DR-2/§5.4)")
	}
	// After a deliberate approval, it becomes effective (the owner action).
	if err := m.ApproveCalibration(context.Background(), m.Calibration[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if _, known, _ := m.GetEffectiveCalibration(context.Background(), "accepted", "google", "", ""); !known {
		t.Fatal("an APPROVED calibration is not effective — approval must promote a proposal (§5.4)")
	}
}

// TestRecalibrate_SampleGateBlocks (§6.2 end-to-end): a bucket below the gate produces NO proposal.
func TestRecalibrate_SampleGateBlocks(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	seedBucket(t, m, "yahoo", 50, 10, now) // only 50 outcomes — below a 500 gate
	d := &RecalibrateDeps{Store: m, Params: recalParams(500), Mode: ModeRecommendOnly, Now: func() time.Time { return now }, WorkerID: "w1"}
	res, err := d.RunOnce(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.BucketsMoved != 0 || len(m.Calibration) != 0 {
		t.Fatalf("below-gate bucket produced %d proposals — the sample gate must block it (§6.2)", res.BucketsMoved)
	}
}

// TestRecalibrate_CASExactlyOnce is the §5.5/GAP-53 exactly-once test: a recalibration cycle whose lease
// was RECLAIMED (a slow, not-dead cycle) matches zero rows on commit and DISCARDS its bucket writes —
// the settle-once/spend-once invariant transferring a third time. FAIL-VERIFY: drop the status='leased'
// guard in CommitRecalibration → the reclaimed cycle double-writes → this breaks.
func TestRecalibrate_CASExactlyOnce(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	// Simulate: worker A leases, then its lease is reclaimed (a competing worker B takes over and
	// commits), and A tries to commit its stale cycle — it must be DISCARDED.
	runID := "run-1"
	if _, err := m.LeaseRecalibration(context.Background(), store.RecalRun{ID: runID, LeasedBy: "A", LeasedAt: now, WindowFrom: now.Add(-time.Hour), WindowTo: now}); err != nil {
		t.Fatal(err)
	}
	// Reclaim: B takes the lease (model the reclaim by re-leasing the same id under B). In the real
	// store this is ReclaimExpiredLeases; here we mutate via a fresh lease row is not possible (PK), so
	// we simulate the reclaim by committing as B first — B is NOT the holder 'A', so we must first make
	// B the holder. The mem/PG CAS keys on leased_by: A's commit must fail because status!='leased' for A
	// after B commits. Model B's takeover by committing under A then asserting A's SECOND commit fails.
	prop := []store.CalibrationRow{{Reason: "accepted", MXProvider: "google", Confidence: 80, SampleN: 600, Mode: "recommend_only", CreatedAt: now}}
	if err := m.CommitRecalibration(context.Background(), runID, "A", prop, 1, 0, false, now); err != nil {
		t.Fatalf("first commit by the lease holder failed: %v", err)
	}
	// A SECOND commit on the same run (now status='committed', not 'leased') must be discarded.
	err := m.CommitRecalibration(context.Background(), runID, "A", prop, 1, 0, false, now)
	if err != store.ErrRecalLeaseLost {
		t.Fatalf("second commit err = %v, want ErrRecalLeaseLost (exactly-once, §5.5/GAP-53)", err)
	}
	// Exactly ONE proposal was written despite two commit attempts.
	if len(m.Calibration) != 1 {
		t.Fatalf("calibration versions = %d after a double-commit — want exactly 1 (recalibrate-once, §5.5)", len(m.Calibration))
	}
}

// TestRecalibrate_StaggeredDriftHeldViaStorePath is the HIGH-fix regression (§6.4, the P4 lesson-a):
// the cascade brake must catch STAGGERED drift THROUGH THE REAL STORE PATH — i.e. the per-run
// buckets_at_max must be PERSISTED and summed across prior committed cycles by RunOnce, not just in a
// hand-built CascadeBrake unit test. FAIL-VERIFY: stop persisting BucketsAtMax (revert the
// CommitRecalibration numerator) → prior cycles contribute 0 at-max → the window never crosses → the
// brake stops holding → this breaks.
//
// Setup: a brake that trips at 60% of touched buckets at-max over a 3-cycle window, min 3 buckets. Each
// cycle moves 2 buckets, BOTH at max step (a hard down-swing). No single cycle is "systemic" by count
// (only 2 buckets), but across 3 cycles: 6 touched, 6 at-max → 100% ≥ 60% → HELD on the 3rd cycle.
func TestRecalibrate_StaggeredDriftHeldViaStorePath(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	params := GuardParams{
		MinBucketSamples: 100, MaxStep: 10, UpStepFactor: 0.5,
		BrakeWindowCycles: 3, BrakeBucketFraction: 0.6, BrakeMinBucketsInWin: 3,
	}
	// Pre-load two prior committed cycles, each 2 buckets touched + 2 at max-step (the durable record
	// the rolling window sums). These model the earlier stages of a staggered drift.
	for c := 0; c < 2; c++ {
		runID := "prior-" + itoaCursor(c)
		if _, err := m.LeaseRecalibration(context.Background(), store.RecalRun{ID: runID, LeasedBy: "w1", LeasedAt: now.Add(time.Duration(c) * time.Minute), WindowFrom: now, WindowTo: now}); err != nil {
			t.Fatal(err)
		}
		if err := m.CommitRecalibration(context.Background(), runID, "w1", nil, 2 /*touched*/, 2 /*atMax*/, false, now); err != nil {
			t.Fatal(err)
		}
	}
	// The current cycle: two buckets each previously TRUSTED (an approved effective calibration at 90)
	// now seeing 100% invalid-signal → target ~0 → a move of ~-90 clamped to -MaxStep → BOTH at-max.
	for _, prov := range []string{"provA", "provB"} {
		id := "eff-" + prov
		if _, err := m.AppendCalibrationVersion(context.Background(), store.CalibrationRow{ID: id, Reason: "accepted", MXProvider: prov, Confidence: 90, SampleN: 600, Mode: "auto", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := m.ApproveCalibration(context.Background(), id, now); err != nil {
			t.Fatal(err)
		}
	}
	seedBucket(t, m, "provA", 200, 200, now) // 100% invalid → hard down move from 90, at max step
	seedBucket(t, m, "provB", 200, 200, now)
	d := &RecalibrateDeps{Store: m, Params: params, Mode: ModeRecommendOnly, Now: func() time.Time { return now }, WorkerID: "w1"}
	res, err := d.RunOnce(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.BucketsAtMaxStep < 2 {
		t.Fatalf("current cycle at-max = %d, want >=2 (two 100%%-invalid buckets should each hit the down max step)", res.BucketsAtMaxStep)
	}
	if !res.BrakeHeld {
		t.Fatalf("cascade brake did NOT hold on staggered drift via the store path (at_max=%d) — the persisted-numerator fix (P4 lesson-a) regressed", res.BucketsAtMaxStep)
	}
	// A held cycle commits ZERO NEW proposals (alert + hold, never auto-apply — §6.4). Only the two
	// pre-seeded effective baselines (eff-provA/eff-provB) exist; this cycle added none.
	if len(m.Calibration) != 2 {
		t.Fatalf("a braked cycle changed the calibration set to %d rows — must stage zero new proposals (only the 2 pre-seeded remain, §6.4 alert-not-apply)", len(m.Calibration))
	}
}

// TestRecalibrate_ShadowWritesNothing — shadow mode computes but writes no lease/proposal (pure observe).
func TestRecalibrate_ShadowWritesNothing(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	seedBucket(t, m, "google", 600, 120, now)
	d := &RecalibrateDeps{Store: m, Params: recalParams(500), Mode: ModeShadow, Now: func() time.Time { return now }, WorkerID: "w1"}
	res, err := d.RunOnce(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed || len(m.Calibration) != 0 || len(m.RecalRuns) != 0 {
		t.Fatalf("shadow mode wrote state (committed=%v cal=%d runs=%d) — must be pure observation", res.Committed, len(m.Calibration), len(m.RecalRuns))
	}
	if res.BucketsMoved != 1 {
		t.Fatalf("shadow mode should still COMPUTE the move (got %d) — it just doesn't write it", res.BucketsMoved)
	}
}

// TestRecalibrate_BadGuardConfigFailsAtStart — the pass refuses to run with an out-of-range brake config
// (the P4 lesson-b, enforced at the pass entry, not just at wiring).
func TestRecalibrate_BadGuardConfigFailsAtStart(t *testing.T) {
	now := fixedNow()
	m := store.NewMem(func() time.Time { return now })
	bad := recalParams(500)
	bad.BrakeBucketFraction = 1.5 // impossible → would silently disable the brake
	d := &RecalibrateDeps{Store: m, Params: bad, Mode: ModeRecommendOnly, Now: func() time.Time { return now }, WorkerID: "w1"}
	if _, err := d.RunOnce(context.Background(), now.Add(-24*time.Hour)); err == nil {
		t.Fatal("recalibration ran with an out-of-range brake config — must fail at start (§6.4 lesson b)")
	}
}
