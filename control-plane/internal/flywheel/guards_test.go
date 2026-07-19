package flywheel

import (
	"testing"
	"time"
)

func goodParams() GuardParams {
	return GuardParams{
		MinBucketSamples: 500, MaxStep: 10, UpStepFactor: 0.5,
		BrakeWindowCycles: 3, BrakeBucketFraction: 0.5, BrakeMinBucketsInWin: 4,
	}
}

// TestSampleGate_BlocksBelowThreshold (§6.2/GAP-27): a bucket below MinBucketSamples is NOT
// recalibrated — it keeps its confidence (cold-start → P1 baseline, §5.6). FAIL-VERIFY: remove the
// QualifyingN < MinBucketSamples guard in Recompute → this breaks.
func TestSampleGate_BlocksBelowThreshold(t *testing.T) {
	p := goodParams()
	// 8 sends, 1 bounce — noise, not a 12.5%-bounce bucket (the GAP-27 error at the calibration layer).
	r := Recompute(BucketObservation{QualifyingN: 8, InvalidRate: 0.125, CurrentConf: 90}, p)
	if r.Moved {
		t.Fatalf("sample gate did not block a below-threshold bucket (n=8 < 500) — recalibrated on noise (§6.2)")
	}
	if r.NewConf != 90 {
		t.Fatalf("below-gate bucket confidence changed to %d — must keep its value (cold-start, §5.6)", r.NewConf)
	}
	// At/above the gate it moves.
	r2 := Recompute(BucketObservation{QualifyingN: 500, InvalidRate: 0.20, CurrentConf: 90}, p)
	if !r2.Moved {
		t.Fatal("a bucket at the sample gate did not recalibrate")
	}
}

// TestHysteresis_DownFastUpSlow (§6.3): a DOWNward move (less trust) is faster than an UPward move
// (more trust). Trust earned slowly, doubt applied quickly. FAIL-VERIFY: set UpStepFactor to 1.0 (or
// remove the asymmetry) → the up move equals the down move → this breaks.
func TestHysteresis_DownFastUpSlow(t *testing.T) {
	p := goodParams() // MaxStep 10, UpStepFactor 0.5 → up cap 5
	// Big desired DOWN move: target ~20 from 90 → bounded to a full -10 step.
	down := Recompute(BucketObservation{QualifyingN: 500, InvalidRate: 0.80, CurrentConf: 90}, p)
	if down.Delta != -10 {
		t.Fatalf("down move = %d, want -10 (full max step — doubt applied quickly)", down.Delta)
	}
	// Big desired UP move: target ~100 from 50 → bounded to +5 (half step — trust earned slowly).
	up := Recompute(BucketObservation{QualifyingN: 500, InvalidRate: 0.0, CurrentConf: 50}, p)
	if up.Delta != 5 {
		t.Fatalf("up move = %d, want 5 (half max step — trust earned slowly, §6.3)", up.Delta)
	}
	if !(abs(down.Delta) > up.Delta) {
		t.Fatal("hysteresis asymmetry violated: down must move faster than up (§6.3)")
	}
}

// TestBoundedStep_CapsPerCycle (§6.3): one cycle can't slam a bucket 90→20; it moves at most MaxStep.
func TestBoundedStep_CapsPerCycle(t *testing.T) {
	p := goodParams()
	r := Recompute(BucketObservation{QualifyingN: 500, InvalidRate: 0.90, CurrentConf: 90}, p)
	if r.NewConf < 80 {
		t.Fatalf("bounded step exceeded: 90 → %d in one cycle (max step %d) — §6.3 violated", r.NewConf, p.MaxStep)
	}
}

// TestCascadeBrake_CatchesStaggeredDrift is the P4-LESSON-(a) test-of-record (§6.4): a STAGGERED drift
// (a few buckets moving hard each cycle, spread across cycles) that a SINGLE-CYCLE snapshot would miss,
// but the ROLLING window catches. FAIL-VERIFY: make CascadeBrake consider only the last cycle (window
// = cycles[len-1:]) → the staggered case stops tripping → this breaks.
func TestCascadeBrake_CatchesStaggeredDrift(t *testing.T) {
	p := goodParams() // window 3, fraction 0.5, min buckets 4
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	// Each cycle: 2 buckets touched, both at max step. No SINGLE cycle looks systemic by count (only 2),
	// but across the 3-cycle window: 6 touched, 6 at-max → fraction 1.0 >= 0.5 → HELD.
	staggered := []CycleMovement{
		{At: base, BucketsTouched: 2, BucketsAtMaxStep: 2},
		{At: base.Add(time.Hour), BucketsTouched: 2, BucketsAtMaxStep: 2},
		{At: base.Add(2 * time.Hour), BucketsTouched: 2, BucketsAtMaxStep: 2},
	}
	br := CascadeBrake(staggered, p)
	if !br.Held {
		t.Fatalf("cascade brake did NOT hold on staggered drift (window touched=%d at_max=%d frac=%.2f) — the P4 lesson-(a) regression (§6.4)", br.WindowTouched, br.WindowAtMax, br.Fraction)
	}
	// A single healthy cycle in isolation (mostly small moves) does NOT hold.
	healthy := []CycleMovement{{At: base, BucketsTouched: 10, BucketsAtMaxStep: 1}}
	if CascadeBrake(healthy, p).Held {
		t.Fatal("cascade brake held on a healthy single cycle (10 touched, 1 at-max) — false positive")
	}
}

// TestValidateGuardParams_RejectsSilentDisable is the P4-LESSON-(b) test (§6.4): an out-of-range config
// must FAIL AT STARTUP, never silently disable the brake. FAIL-VERIFY: remove the BrakeBucketFraction
// range check in ValidateGuardParams → a fraction of 0 or 1.5 passes → this breaks.
func TestValidateGuardParams_RejectsSilentDisable(t *testing.T) {
	if err := ValidateGuardParams(goodParams()); err != nil {
		t.Fatalf("good params rejected: %v", err)
	}
	bad := []GuardParams{
		func() GuardParams { p := goodParams(); p.BrakeBucketFraction = 0; return p }(),   // ≤0 → never fires
		func() GuardParams { p := goodParams(); p.BrakeBucketFraction = 1; return p }(),   // ≥1 → needs EVERY bucket (staggered never)
		func() GuardParams { p := goodParams(); p.BrakeBucketFraction = 1.5; return p }(), // >1 → impossible
		func() GuardParams { p := goodParams(); p.UpStepFactor = 2; return p }(),          // >1 → inverts the asymmetry
		func() GuardParams { p := goodParams(); p.BrakeWindowCycles = 0; return p }(),     // 0 → disables the brake
		func() GuardParams { p := goodParams(); p.MaxStep = 0; return p }(),               // 0 → one cycle slams a bucket
		func() GuardParams { p := goodParams(); p.MinBucketSamples = 0; return p }(),      // 0 → recalibrates on noise
	}
	for i, p := range bad {
		if err := ValidateGuardParams(p); err == nil {
			t.Fatalf("case %d: out-of-range guard config passed validation — it would SILENTLY disable a safety valve (the P4 lesson-b regression, §6.4)", i)
		}
	}
}
