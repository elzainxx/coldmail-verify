package flywheel

import (
	"fmt"
	"time"
)

// This file: FEEDBACK-LOOP SAFETY (§6, the second irreversible surface / HIGHEST RIGOR). Recalibration
// is a CLOSED control loop — new confidence → different verdicts → different sends → different outcomes
// → new confidence. Uncontrolled, a bad batch pushes a bucket the wrong way, which changes sending,
// which produces outcomes that CONFIRM the bad push — a divergent spiral (GAP-50). P7 borrows P4's
// control-loop guards wholesale because the failure mode is identical in shape:
//
//	1. Sample-size gate      — no move below min_bucket_samples (small-N noise, P4 §5.3 / GAP-27)
//	2. Bounded step          — a cycle moves a bucket by at most max_step (a poisoned window can't
//	                           fully capture a bucket before the next cycle + brake catch it)
//	3. Asymmetric hysteresis — DOWN (less trust) faster/easier than UP (more trust). Trust earned
//	                           slowly, doubt applied quickly — the safe error is over-deferring (§6.3)
//	4. Cascade brake         — a SYSTEMIC swing ALERTS a human, it does NOT auto-apply (§6.4, GAP-28)
//
// TWO P4 LESSONS, both defects the brake had (§6, kickoff "do not repeat either"):
//   (a) a single-cycle SNAPSHOT missed a STAGGERED drift → the brake here watches a ROLLING
//       MULTI-CYCLE WINDOW (CascadeBrake over recent run history), not one cycle in isolation.
//   (b) an out-of-range config SILENTLY DISABLED the brake → ValidateGuardParams FAILS AT STARTUP on
//       an out-of-range value (returns an error, never a silent no-op), exactly as config.Validate
//       bounds FleetWidePct to 1..99 for the P4 fleet brake.
//
// This is PURE decision logic over injected facts (the external.Decide / breaker pattern); the store
// supplies bucket counts + run history, the caller applies the result to the versioned calibration
// store (recommend-only by default — an unapproved version simply isn't effective, §5.4).

// GuardParams are the four-guard DIALs (§6.1 — existence LOCKED, values DIAL). All are validated at
// startup by ValidateGuardParams: an out-of-range value is a STARTUP ERROR, never a silent disable
// (the P4 lesson (b)). A nonsensical brake config that can never fire is exactly the auto-heal →
// auto-outage hole the fleet-brake round-2 review closed for P4; P7 refuses to start with one.
type GuardParams struct {
	MinBucketSamples int     // §6.2 — no recalibration below this many qualifying outcomes (start 500)
	MaxStep          int     // §6.3 — max confidence points a bucket may move per cycle (e.g. 10–15)
	UpStepFactor     float64 // §6.3 hysteresis — UP moves are scaled by this (<1 => up is SLOWER than down)

	// Cascade brake (§6.4) — the SYSTEMIC-swing detector, over a ROLLING window (lesson (a)).
	BrakeWindowCycles    int     // how many recent recalibration cycles the brake reasons over (>1 → catches staggered)
	BrakeBucketFraction  float64 // fraction of touched buckets moving > MaxStep across the window → hold (1..99% as a fraction)
	BrakeMinBucketsInWin int     // don't trip the brake on a tiny denominator (the GAP-27 sample-gate, at the systemic layer)
}

// ValidateGuardParams rejects an out-of-range guard config AT STARTUP (§6.4 / the P4 lesson (b): an
// out-of-range config must not SILENTLY disable a safety valve). It is called from config validation /
// flywheel wiring; a returned error fails the process rather than running with a dead brake.
//
// The load-bearing check is BrakeBucketFraction ∈ (0,1): a fraction ≤ 0 would trip on nothing OR make
// the brake meaningless, and a fraction ≥ 1 would require EVERY touched bucket to swing in one window
// — which staggered poisoning never does, silently disabling "the single most important safety line."
// This is the exact class of bug config.Validate guards for the P4 FleetWidePct (must be 1..99).
func ValidateGuardParams(p GuardParams) error {
	if p.MinBucketSamples <= 0 {
		return fmt.Errorf("flywheel: min_bucket_samples (%d) must be > 0 — a zero sample-gate recalibrates on noise (§6.2/GAP-27)", p.MinBucketSamples)
	}
	if p.MaxStep <= 0 {
		return fmt.Errorf("flywheel: max_step (%d) must be > 0 — a zero bounded-step lets one cycle slam a bucket (§6.3)", p.MaxStep)
	}
	if p.UpStepFactor <= 0 || p.UpStepFactor > 1 {
		return fmt.Errorf("flywheel: up_step_factor (%.3f) must be in (0,1] — >1 would make UP moves FASTER than down, inverting the down-fast/up-slow safety asymmetry (§6.3)", p.UpStepFactor)
	}
	if p.BrakeWindowCycles < 1 {
		return fmt.Errorf("flywheel: brake_window_cycles (%d) must be >= 1 — a zero window disables the cascade brake; >1 is what catches STAGGERED drift the P4 single-snapshot brake missed (§6.4 lesson a)", p.BrakeWindowCycles)
	}
	// The lesson-(b) range guard: a fraction outside (0,1) silently neuters the brake.
	if p.BrakeBucketFraction <= 0 || p.BrakeBucketFraction >= 1 {
		return fmt.Errorf("flywheel: brake_bucket_fraction (%.3f) must be in (0,1) — a value <=0 or >=1 makes the cascade brake mathematically unable to fire on a genuine majority, SILENTLY disabling the systemic-swing safety valve (§6.4; the P4 FleetWidePct lesson b)", p.BrakeBucketFraction)
	}
	if p.BrakeMinBucketsInWin < 1 {
		return fmt.Errorf("flywheel: brake_min_buckets_in_window (%d) must be >= 1 — the brake needs a denominator floor so it doesn't trip on one noisy bucket (§6.4/GAP-27)", p.BrakeMinBucketsInWin)
	}
	return nil
}

// BucketObservation is the measured signal for one (reason × provider[, vendor, residue_class]) bucket
// over the recalibration window (§5.2): the count of qualifying outcomes and the observed
// invalid-signal (mailbox_not_found) rate. weightedValidN is the evidence-weighted valid-signal mass
// (§4.5) — retained for the accuracy/promotion consumers; the recalibration MOVE keys off the observed
// bounce rate vs the current confidence.
type BucketObservation struct {
	QualifyingN     int     // outcomes that passed every exclusion (§5.3) and calibrate (Calibrates())
	InvalidRate     float64 // observed fraction classified invalid-signal (the §8.1 false-valid rate)
	CurrentConf     int     // the bucket's current effective confidence (0..100)
	WeightedValidN  float64 // evidence-weighted valid-signal mass (§4.5) — for §8 accuracy / §7 promotion
}

// RecomputeResult is one bucket's proposed move. Moved=false => the sample gate blocked it (below
// MinBucketSamples) → the bucket KEEPS its current confidence (cold-start behavior, §5.6: the verdict
// path falls back to the P1 baseline for an uncalibrated bucket). NewConf is always within MaxStep of
// CurrentConf (bounded), with the down-fast/up-slow asymmetry applied. Clamped 0..100 (Door-1 range).
type RecomputeResult struct {
	Moved   bool
	NewConf int
	Delta   int // NewConf - CurrentConf (signed; negative = trust reduced)
	Reason  string // "sample_gate" (not moved) | "bounded_down" | "bounded_up" | "no_change"
}

// Recompute applies guards 1–3 (sample-size gate + bounded step + asymmetric hysteresis) to produce
// one bucket's proposed new confidence from its observed signal (§6.2/§6.3). It does NOT apply the
// cascade brake (guard 4) — that reasons over ALL buckets in a cycle (CascadeBrake). It does NOT write
// anything — the caller stages the proposal into the versioned calibration store (recommend-only by
// default: the proposal isn't effective until approved, §5.4).
//
// The target confidence is derived from the observed invalid-signal rate: a higher bounce rate means
// the bucket's addresses are less deliverable than its current confidence claims → target lower. The
// MOVE toward that target is then bounded and made asymmetric: a DOWNWARD move (target < current, less
// trust) is allowed at full MaxStep; an UPWARD move (more trust) is scaled by UpStepFactor (<1) —
// trust earned slowly, doubt applied quickly (§6.3). The safe error is over-deferring to risky/fallback.
func Recompute(obs BucketObservation, p GuardParams) RecomputeResult {
	// Guard 1 — sample-size gate (§6.2/GAP-27): below the gate the bucket is NOT recalibrated; it keeps
	// its current confidence and the verdict path uses the P1 baseline for it (§5.6, cold-start in code).
	if obs.QualifyingN < p.MinBucketSamples {
		return RecomputeResult{Moved: false, NewConf: obs.CurrentConf, Delta: 0, Reason: "sample_gate"}
	}

	// Target confidence from the observed invalid-signal rate. A bucket that bounced X% invalid-signal
	// is at most (100 - X%) trustworthy for a "valid"-style claim; we target that ceiling. (The exact
	// mapping is a modeling DIAL; what is LOCKED is the direction — more observed bounces ⇒ lower target.)
	target := int((1.0 - clampRate(obs.InvalidRate)) * 100.0)

	// Guard 2 + 3 — bounded step with down-fast/up-slow hysteresis (§6.3).
	delta := target - obs.CurrentConf
	if delta == 0 {
		return RecomputeResult{Moved: true, NewConf: obs.CurrentConf, Delta: 0, Reason: "no_change"}
	}
	var step int
	reason := "bounded_down"
	if delta < 0 {
		// DOWN (less trust): full max step. Doubt applied quickly.
		step = -min(-delta, p.MaxStep)
	} else {
		// UP (more trust): scaled by UpStepFactor (<1) → SLOWER. Trust earned slowly. At least 0.
		upCap := int(float64(p.MaxStep) * p.UpStepFactor)
		step = min(delta, upCap)
		reason = "bounded_up"
	}
	newConf := clampInt(obs.CurrentConf+step, 0, 100)
	return RecomputeResult{Moved: true, NewConf: newConf, Delta: newConf - obs.CurrentConf, Reason: reason}
}

// CycleMovement records how much one recalibration cycle moved buckets — the input the cascade brake
// reasons over ACROSS a rolling window (§6.4). BucketsTouched is how many buckets qualified (passed the
// sample gate) this cycle; BucketsAtMaxStep is how many hit the MaxStep bound (the signature of a
// bucket being pushed hard). A run history of these is what makes STAGGERED drift visible (lesson a).
type CycleMovement struct {
	At               time.Time
	BucketsTouched   int
	BucketsAtMaxStep int
}

// BrakeResult is the cascade brake's decision over the rolling window. Held=true => a SYSTEMIC swing was
// detected → the cycle does NOT auto-apply; it raises a critical alert, HOLDS the current calibration,
// and requires human confirmation (§6.4 LOCKED: alert, never auto-apply — a bad global recalibration is
// far more dangerous than a delayed good one, the P4 fleet-brake posture [[truemail-fleet-brake-no-cascade]]).
type BrakeResult struct {
	Held           bool
	Reason         string
	WindowTouched  int     // total buckets touched across the window (the denominator)
	WindowAtMax    int     // total buckets at MaxStep across the window (the numerator)
	Fraction       float64 // WindowAtMax / WindowTouched
}

// CascadeBrake is guard 4 (§6.4) — the SYSTEMIC-swing detector over a ROLLING multi-cycle window (the
// P4 lesson (a) fix: NOT a single-cycle snapshot). Given the recent cycle movements (most-recent-first
// or any order; only the last BrakeWindowCycles are considered) INCLUDING the current cycle, it holds
// if a large FRACTION of touched buckets hit the MaxStep bound across the window — the signature of a
// systemic cause (sender-reputation collapse misread as address-invalidity, a classifier bug, a
// poisoned feed) rather than honest per-bucket drift.
//
// Why the window matters (the staggered-drift test-of-record): an attacker (or a bug) that moves a FEW
// buckets hard each cycle, spread across cycles, never trips a single-cycle brake — but the rolling sum
// crosses the fraction. This function's window is exactly what catches that. BrakeMinBucketsInWin is the
// denominator floor (don't trip on one noisy bucket — the systemic-layer GAP-27 gate).
func CascadeBrake(window []CycleMovement, p GuardParams) BrakeResult {
	// Consider only the most recent BrakeWindowCycles cycles. The caller passes recent history; we sum
	// over the tail so a long history doesn't dilute the signal. (Order-agnostic: we sum the last N by
	// slice position — the caller appends the current cycle last.)
	cycles := window
	if len(cycles) > p.BrakeWindowCycles {
		cycles = cycles[len(cycles)-p.BrakeWindowCycles:]
	}
	touched, atMax := 0, 0
	for _, c := range cycles {
		touched += c.BucketsTouched
		atMax += c.BucketsAtMaxStep
	}
	res := BrakeResult{WindowTouched: touched, WindowAtMax: atMax}
	if touched < p.BrakeMinBucketsInWin {
		// Too few buckets in the window to call anything systemic — don't hold (§6.4 denominator floor).
		res.Reason = "below_min_buckets"
		return res
	}
	res.Fraction = float64(atMax) / float64(touched)
	if res.Fraction >= p.BrakeBucketFraction {
		res.Held = true
		res.Reason = "systemic_swing"
	}
	return res
}

// ── small helpers (local, no external deps) ──

func clampRate(r float64) float64 {
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
