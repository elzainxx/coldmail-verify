package flywheel

import (
	"context"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/idgen"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// This file: the RECALIBRATION pass (§5/§6) — the A6 mechanism that ties the four guards (guards.go) to
// the calibration store, under the exactly-once CAS (§5.5/GAP-53). It runs on a cadence (a batch, not
// per-outcome — OQ-P7-7), reads the window's bucket aggregates, applies the guards, checks the cascade
// brake over the rolling run window, and COMMITS the proposals under the lease CAS.
//
// RECOMMEND-ONLY by default (DR-2 / shadow-mode A6, the sharpest mutator): a committed version is a
// PROPOSAL (effective_from NULL) — computed, stored, alertable, but NOT read by the verdict path until a
// human APPROVES it (the verdict path reads only the effective version; cold-start → P1 baseline). At
// auto (owner-promoted, admin-audited), the pass approves in the same commit. The FRESH-SYSTEM
// INVARIANT (DR-2/pre-takeoff §3.5): on an empty system every bucket is below the sample gate → zero
// proposals become effective → behavior IDENTICAL to end-of-P6.

// RecalibrateDeps wires the recalibration pass.
type RecalibrateDeps struct {
	Store  store.Store
	Params GuardParams
	Mode   Mode      // the A6 mode: recommend_only (default) | auto | shadow
	Now    func() time.Time
	WorkerID string  // the lease principal (the CAS identity, §5.5)
}

// Mode is the shadow-mode dial for A6 (mirrors config.Mode without importing config — layering). A
// recommend-only pass proposes; an auto pass approves; a shadow pass computes but writes nothing.
type Mode string

const (
	ModeShadow        Mode = "shadow"
	ModeRecommendOnly Mode = "recommend_only"
	ModeAuto          Mode = "auto"
)

// RecalibrateResult summarizes one pass (observability §10: recalibration deltas, brake events).
type RecalibrateResult struct {
	BucketsConsidered int
	BucketsMoved      int  // passed the sample gate + would move (a proposal was staged)
	BucketsAtMaxStep  int  // hit the bounded-step ceiling (the cascade-brake input)
	BrakeHeld         bool // §6.4: a systemic swing held this cycle for human confirm (no auto-apply)
	Committed         bool // the CAS committed (false => shadow mode, or ErrRecalLeaseLost)
	Approved          int  // proposals made effective this pass (0 unless mode==auto and not braked)
}

// RunOnce executes one recalibration cycle over [since, now] (§5/§6). Order: lease → read aggregates →
// per-bucket guards (Recompute) → cascade brake over the rolling window (CascadeBrake) → commit under
// the CAS. If the brake holds (§6.4), NO proposals are staged (the cycle is held for a human) but the
// run is still recorded (brake_held=true) so the alert + the held state are durable. The pass is
// exactly-once: a reclaimed lease discards everything (ErrRecalLeaseLost) — settle/spend/recalibrate-once.
func (d *RecalibrateDeps) RunOnce(ctx context.Context, since time.Time) (RecalibrateResult, error) {
	var res RecalibrateResult
	now := d.now()

	// Validate the guard config up-front (§6.4 lesson b — never run with a silently-disabled brake).
	if err := ValidateGuardParams(d.Params); err != nil {
		return res, err
	}

	aggs, err := d.Store.BucketAggregatesSince(ctx, since, now)
	if err != nil {
		return res, err
	}
	res.BucketsConsidered = len(aggs)

	// Per-bucket guards (§6.2/§6.3): sample gate + bounded step + hysteresis. Build the proposal set.
	var proposals []store.CalibrationRow
	for _, a := range aggs {
		// The bucket's CURRENT effective confidence (cold-start → 0 here means "use baseline"; we read
		// the effective row so a move is relative to what the verdict path currently uses).
		cur, known, gerr := d.Store.GetEffectiveCalibration(ctx, a.Reason, a.MXProvider, a.Vendor, a.ResidueClass)
		if gerr != nil {
			return res, gerr
		}
		curConf := 0
		if known {
			curConf = cur.Confidence
		}
		invalidRate := 0.0
		if a.QualifyingN > 0 {
			invalidRate = float64(a.InvalidN) / float64(a.QualifyingN)
		}
		obs := BucketObservation{QualifyingN: a.QualifyingN, InvalidRate: invalidRate, CurrentConf: curConf}
		rc := Recompute(obs, d.Params)
		if !rc.Moved {
			continue // sample gate blocked it — the bucket keeps its confidence (cold-start, §5.6)
		}
		res.BucketsMoved++
		// A bucket "at max step" is one the guards had to CLAMP — the signature of a hard push. Note the
		// deliberate asymmetry: a DOWN move can reach MaxStep, but an UP move is capped at
		// int(MaxStep*UpStepFactor) < MaxStep (down-fast/up-slow, §6.3), so only mass DE-TRUSTING swings
		// contribute to the cascade-brake numerator. That is intended: the dangerous runaway is mass
		// over-trust being APPLIED, which the up-slow cap already throttles; a mass down-swing (e.g. a
		// reputation collapse misread as invalidity) is the systemic event the brake must catch. If a
		// future dial sets UpStepFactor=1, up moves would begin counting here too — a deliberate change.
		if abs(rc.Delta) >= d.Params.MaxStep {
			res.BucketsAtMaxStep++
		}
		proposals = append(proposals, store.CalibrationRow{
			ID:         mustOutcomeID(now), Reason: a.Reason, MXProvider: a.MXProvider,
			Vendor: a.Vendor, ResidueClass: a.ResidueClass,
			Confidence: rc.NewConf, SampleN: a.QualifyingN, Mode: string(d.Mode), CreatedAt: now,
		})
	}

	// Guard 4 — cascade brake over the ROLLING window (§6.4, staggered-drift detector). Include THIS
	// cycle's movement in the window so a single big cycle also trips it.
	prior, err := d.Store.RecentRecalRuns(ctx, d.Params.BrakeWindowCycles-1)
	if err != nil {
		return res, err
	}
	window := make([]CycleMovement, 0, len(prior)+1)
	for _, r := range prior {
		// Both the denominator (BucketsTouched) AND the numerator (BucketsAtMax) come from the DURABLE
		// per-run record (persisted by CommitRecalibration), so the rolling window sums real at-max
		// counts across prior cycles — the fix for the staggered-drift defeat (the P4 lesson-a): without
		// the persisted numerator, prior cycles contributed 0 at-max and the brake was defeated.
		window = append(window, CycleMovement{At: r.LeasedAt, BucketsTouched: r.BucketsTouched, BucketsAtMaxStep: r.BucketsAtMax})
	}
	window = append(window, CycleMovement{At: now, BucketsTouched: res.BucketsMoved, BucketsAtMaxStep: res.BucketsAtMaxStep})
	brake := CascadeBrake(window, d.Params)
	res.BrakeHeld = brake.Held

	// Shadow mode: compute + record, write NOTHING (no lease, no proposal). Pure observation (§ mode).
	if d.Mode == ModeShadow {
		return res, nil
	}

	// Lease + CAS commit (§5.5/GAP-53). If the brake held, commit ZERO proposals (the cycle is held for
	// a human) but still record the run with brake_held=true (the alert + held state are durable).
	runID := mustOutcomeID(now)
	if _, err := d.Store.LeaseRecalibration(ctx, store.RecalRun{
		ID: runID, LeasedBy: d.WorkerID, LeasedAt: now, WindowFrom: since, WindowTo: now,
	}); err != nil {
		return res, err
	}
	toCommit := proposals
	if brake.Held {
		toCommit = nil // §6.4: alert + hold, do NOT auto-apply a systemic swing
	}
	if err := d.Store.CommitRecalibration(ctx, runID, d.WorkerID, toCommit, res.BucketsMoved, res.BucketsAtMaxStep, brake.Held, now); err != nil {
		if err == store.ErrRecalLeaseLost {
			return res, nil // exactly-once: a reclaimed lease discards everything (§5.5)
		}
		return res, err
	}
	res.Committed = true

	// Auto mode (owner-promoted A6, admin-audited): approve the just-committed proposals so they become
	// effective. Recommend-only (the DEFAULT) leaves them as proposals (not read by the verdict path)
	// until a human approves — so this block never runs at default mode (the fresh-system invariant). A
	// braked cycle approves nothing regardless of mode (§6.4). The proposals carry their own ids (both
	// stores insert with row.ID preserved), so we approve each by id directly — no re-read guesswork.
	if d.Mode == ModeAuto && !brake.Held {
		for _, p := range toCommit {
			if err := d.Store.ApproveCalibration(ctx, p.ID, now); err != nil {
				return res, err // a partial approval is surfaced, never silently reported as done
			}
			res.Approved++
		}
	}
	return res, nil
}

func (d *RecalibrateDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func mustOutcomeID(now time.Time) string {
	id, err := idgen.NewV7String(now)
	if err != nil {
		return "00000000-0000-7000-8000-000000000000"
	}
	return id
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
