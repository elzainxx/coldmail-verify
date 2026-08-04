package orchestrator

import (
	"context"
	"time"

	"github.com/coldmail-verify/control-plane/internal/flywheel"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the Phase-7 flywheel's TWO touches on the LIVE verdict path (§4.6 + §5.4). Both are
// ADDITIVE and NIL-SAFE (the P6 external-stage pattern): FlywheelDeps==nil => the flywheel is fully OFF
// (the P1–P6 posture, the default) and compute() is byte-identical to P6 (the master invariant §8,
// trivially). Even when WIRED, on a fresh system both touches are inert (no historical bounces on
// record, no effective calibration versions) → behavior IDENTICAL to end-of-P6 (the DR-2/pre-takeoff
// §3.5 fresh-system invariant). Neither touch mutates the LOG (Door 2) or mints vocab (Door 4 — the
// gate uses the P1-RESERVED reason=historical_bounce).
//
//  1. HISTORICAL-BOUNCE GATE (§4.6): BEFORE the expensive layers, if the address has ≥N confirmed
//     invalid-signal (mailbox_not_found) bounces on record within the TTL, short-circuit to
//     invalid/historical_bounce (ground-truth beats any probe). Past the TTL it degrades to a
//     confidence PENALTY (recoverable — a re-created mailbox recovers). Reads the email_hash rollup
//     (the ONE place the rollup is the right key). Auto-safe (A10): TTL + confirmed-only + ≥N.
//
//  2. CALIBRATION READ (§5.4): at assembly, replace the P1 baseline confidence for the (reason ×
//     provider) bucket with the CURRENT EFFECTIVE calibrated confidence, if one exists. Cold-start (no
//     effective version) => the P1 baseline stands (§5.6). Recommend-only means an unapproved proposal
//     is NOT effective, so this read returns nothing until a human approves — zero live mutation at
//     default mode. external_verified verdicts read from the SEPARATE bucket space (GAP-54).

// FlywheelDeps wires the P7 live-path touches. nil => OFF (default). All fields injectable for tests.
type FlywheelDeps struct {
	// HistoricalGate params (§4.6/F-17). Zero TTL => the gate is inert (never fires) — the safe default
	// until an owner sets the window. MinIndependent (≥N) is the F-17 anti-forgery floor.
	GateParams flywheel.HistoricalGateParams

	// GateEnabled gates the §4.6 layer entirely (a policy without the historical-bounce layer in `order`
	// never runs it — the mechanical opt-out; this mirrors that at the wiring level). Default false =>
	// the gate is off even when FlywheelDeps is non-nil (a fresh P7 system reads calibration but does
	// NOT gate until the owner enables the reserved layer).
	GateEnabled bool

	// CalibrationEnabled gates the §5.4 calibrated-confidence read. Default false => the verdict path
	// uses the P1 baseline (cold-start behavior even when wired). When true, an EFFECTIVE calibration
	// version (approved — recommend-only proposals are not effective) overrides the baseline confidence.
	CalibrationEnabled bool

	Now func() time.Time
}

// historicalGate applies §4.6 at the START of compute() (before the layers). It returns a replacement
// verdict + true when the gate FIRES (hard invalid within the TTL); false leaves compute() to proceed
// normally (the penalty case is applied at assembly via calibrationConfidence, not here — a penalty is
// a confidence nudge, not a short-circuit). It reads the address-level email_hash rollup (§3.1). Nil
// deps / gate disabled / no history / below ≥N / read error => does not fire (harden-toward-unknown).
func (o *Orchestrator) historicalGate(ctx context.Context, emailHash []byte, def *policy.Definition) (policy.Verdict, bool) {
	fw := o.flywheel
	if fw == nil || !fw.GateEnabled || len(emailHash) == 0 {
		return policy.Verdict{}, false
	}
	count, latest, err := o.store.HistoricalBounceCount(ctx, emailHash)
	if err != nil {
		return policy.Verdict{}, false // a rollup-read error never fabricates a gate hit (fail-open to normal flow)
	}
	dec := flywheel.EvaluateHistoricalGate(
		flywheel.HistoricalBounceFacts{IndependentCount: count, LatestAt: latest, Known: count > 0},
		fw.GateParams, o.flywheelNow(),
	)
	if dec.Effect != flywheel.GateInvalid {
		return policy.Verdict{}, false // GateNone or GatePenalty — not a hard short-circuit
	}
	// The gate fires: invalid/historical_bounce (RESERVED reason, Door 4 — zero new vocab). Confidence
	// is the policy baseline for historical_bounce if present, else a high default (ground-truth proof).
	conf := 95
	if v, ok := def.Confidence.BaselineByReason[string(vocab.ReasonHistoricalBounce)]; ok {
		conf = v
	}
	v := policy.Verdict{
		Status: vocab.StatusInvalid, Reason: vocab.ReasonHistoricalBounce, Confidence: conf,
		Recommendation: vocab.RecSkip, // invalid ⇒ skip (composability, LOCKED)
		Flags:          map[string]bool{"historical_bounce": true, "from_cache": false},
		Signals:        map[string]any{"historical_bounce": true},
		StoppedAt:      "historical_bounce",
	}
	return v, true
}

// calibrationConfidence applies §5.4 (the calibrated-confidence read) + the §4.6 TTL-expired PENALTY to
// a computed verdict at assembly, returning the possibly-adjusted verdict. It is nil-safe and inert by
// default. The read is per (reason × mx_provider), or the SEPARATE external bucket space for an
// external_verified verdict (GAP-54). Cold-start (no effective version) => the verdict is unchanged
// (P1 baseline stands, §5.6). A recommend-only proposal is NOT effective, so this returns the verdict
// unchanged until a human approves — the fresh-system invariant. When confidence changes, the
// recommendation is RE-DERIVED honoring composability (never up-grades toward send illegally).
func (o *Orchestrator) calibrationConfidence(ctx context.Context, v policy.Verdict, emailHash []byte, def *policy.Definition) policy.Verdict {
	fw := o.flywheel
	if fw == nil {
		return v
	}

	// §4.6 TTL-expired penalty: if the address has confirmed history but it is PAST the TTL, apply a
	// recoverable confidence penalty (not a hard gate — a re-created mailbox recovers). Only when the
	// gate layer is enabled (same opt-in as the hard gate).
	if fw.GateEnabled && len(emailHash) > 0 {
		if count, latest, err := o.store.HistoricalBounceCount(ctx, emailHash); err == nil && count > 0 {
			dec := flywheel.EvaluateHistoricalGate(
				flywheel.HistoricalBounceFacts{IndependentCount: count, LatestAt: latest, Known: true},
				fw.GateParams, o.flywheelNow(),
			)
			if dec.Effect == flywheel.GatePenalty && dec.PenaltyPoints > 0 {
				v.Confidence = clampConf(v.Confidence - dec.PenaltyPoints)
				if v.Signals == nil {
					v.Signals = map[string]any{}
				}
				v.Signals["historical_bounce_penalty"] = dec.PenaltyPoints
				v.Recommendation = policy.Recommend(def, v.Status, v.Reason, v.Confidence)
			}
		}
	}

	// §5.4 calibrated-confidence read (the flywheel's core verdict touch). Disabled => cold-start (P1
	// baseline stands). external_verified reads the separate bucket space (vendor × residue_class).
	if !fw.CalibrationEnabled {
		return v
	}
	vendor, residue := externalBucketKeys(v)
	cal, known, err := o.store.GetEffectiveCalibration(ctx, string(v.Reason), mxProviderOf(v.Signals), vendor, residue)
	if err != nil || !known {
		return v // cold-start / read error → the P1 baseline confidence stands (§5.6)
	}
	if cal.Confidence != v.Confidence {
		v.Confidence = clampConf(cal.Confidence)
		v.Recommendation = policy.Recommend(def, v.Status, v.Reason, v.Confidence)
		if v.Signals == nil {
			v.Signals = map[string]any{}
		}
		v.Signals["calibrated"] = true // provenance: this confidence came from the flywheel, not the baseline
	}
	return v
}

// externalBucketKeys returns the (vendor, residue_class) for the SEPARATE external calibration bucket
// space when the verdict is a rented (external_verified) one (GAP-54); ("","") for a SMTP-earned
// verdict (the base (reason × provider) space). Read off the existing signals.external.* provenance.
func externalBucketKeys(v policy.Verdict) (vendor, residue string) {
	if v.Reason != vocab.ReasonExternalVerified {
		return "", ""
	}
	ext, ok := v.Signals["external"].(map[string]any)
	if !ok {
		return "", ""
	}
	vendor, _ = ext["vendor"].(string)
	residue, _ = ext["residue_class"].(string)
	return vendor, residue
}

func (o *Orchestrator) flywheelNow() time.Time {
	if o.flywheel != nil && o.flywheel.Now != nil {
		return o.flywheel.Now()
	}
	return o.now()
}

func clampConf(c int) int {
	if c < 0 {
		return 0
	}
	if c > 100 {
		return 100
	}
	return c
}
