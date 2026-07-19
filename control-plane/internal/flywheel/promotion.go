package flywheel

// This file: provider auto-promotion (§7), accuracy measurement (§8), and per-provider fallback
// weaning (§9) — the three CONSUMERS of the one accuracy measurement (§8.3: measure the truth, trust a
// provider once the truth is good enough, stop renting once your truth beats the vendor's). All three
// default to RECOMMEND-ONLY (DR-2 / shadow-mode A8/A9): they COMPUTE + ALERT, they never auto-mutate
// routing/spend at default mode. On a fresh system every function here returns "not eligible" because
// the sample gates are unmet (zero buckets promoted / weaned — the §14.2 cold-start truth, in code).
//
// This is PURE decision logic over injected measurements; the store supplies the counts, the caller
// stages a recommendation (or, only when a mechanism is owner-promoted to auto, applies it).

// ── §8 Accuracy measurement — the system's first MEASURED number ──

// Accuracy is the §8.1 metric for one scope (per mx_provider, optionally per reason): of the addresses
// this system labeled valid, what fraction hard-bounced mailbox_not_found. CRUCIALLY it carries its
// COVERAGE (§8.2) — the fraction of valid-labels that got ANY calibratable outcome — so a number is
// NEVER reportable without it (reporting "99% accurate" while 80% of valid-labels never got a
// measurable outcome is the overclaim §14 exists to prevent).
type Accuracy struct {
	Provider          string
	ValidLabeled      int     // valid-labeled verdicts in scope (the population)
	CalibratableValid int     // of those, how many got a calibratable outcome (the §8.1 DENOMINATOR)
	FalseValid        int     // of those, how many hard-bounced invalid-signal (the §8.1 NUMERATOR)
}

// Rate is the measured false-valid fraction (§8.1): FalseValid / CalibratableValid. Ok=false when the
// denominator is zero (no calibratable outcome yet) — there is NO accuracy number to report, not "100%".
// This is the §8.4 unknown-honest posture at the metric layer: absence of measurement is not success.
func (a Accuracy) Rate() (rate float64, ok bool) {
	if a.CalibratableValid <= 0 {
		return 0, false
	}
	return float64(a.FalseValid) / float64(a.CalibratableValid), true
}

// Coverage is the §8.2 companion the number may NEVER be published without: of the valid-labels in
// scope, the fraction that got a calibratable outcome. Low coverage means the accuracy number is
// measured on a small, possibly-unrepresentative slice — the honest caveat that keeps "measured on 5%"
// from masquerading as "accurate." Ok=false when there were no valid-labels at all (nothing to cover).
func (a Accuracy) Coverage() (coverage float64, ok bool) {
	if a.ValidLabeled <= 0 {
		return 0, false
	}
	return float64(a.CalibratableValid) / float64(a.ValidLabeled), true
}

// Reportable enforces the §8.2 LOCKED rule in code: an accuracy rate is only reportable WITH a
// coverage figure. A caller that wants to surface a number must call this — it returns both or neither,
// so there is no code path that yields a bare global rate. (The §14/GAP-52 overclaim is structurally
// prevented: you cannot get the rate without also getting how little of the population it covers.)
func (a Accuracy) Reportable() (rate, coverage float64, ok bool) {
	r, rok := a.Rate()
	c, cok := a.Coverage()
	if !rok || !cok {
		return 0, 0, false
	}
	return r, c, true
}

// ── §7 Provider auto-promotion to :mx-only (the original build-plan "#4") ──

// PromotionParams are the §7.2 thresholds (DIALs). promote requires BOTH a low measured false-valid
// rate AND adequate samples (never one without the other — a provider with 10 clean sends is untested,
// not trusted). demote is HYSTERETIC (higher than promote): quick to demote, slow to promote (§6.3).
type PromotionParams struct {
	PromoteMaxRate  float64 // §7.2 promote when false-valid rate <= this (e.g. 0.01–0.02)
	DemoteMinRate   float64 // §7.2 demote when a promoted provider's rate climbs back >= this (> PromoteMaxRate)
	MinSamples      int     // §7.2 min calibratable valid outcomes before promotion may fire (the §6.2 gate)
}

// PromotionState is the current routing posture of a provider (whether it is promoted to :mx-only).
type PromotionRecommendation struct {
	Provider   string
	Action     string // "promote" | "demote" | "hold" — a RECOMMENDATION (recommend-only, DR-2/A8)
	Rate       float64
	Samples    int
	Reason     string
}

// EvaluatePromotion produces a promotion RECOMMENDATION for a provider from its measured accuracy and
// current state (§7.2). It NEVER flips routing itself — at the default recommend-only mode (DR-2/A8) the
// caller surfaces this for owner sign-off; only when A8 is owner-promoted to auto does the caller apply
// it. Reversible + hysteretic: a promoted provider whose rate later climbs back above DemoteMinRate is
// recommended for demotion (§7.2 quick-to-demote). The honest limit (§7.4): a provider whose volume
// never reaches MinSamples never accumulates the evidence and stays on the cautious path — "hold" —
// forever, correctly. On a fresh system EVERY provider is "hold" (samples unmet) → zero promotions.
func EvaluatePromotion(acc Accuracy, currentlyPromoted bool, p PromotionParams) PromotionRecommendation {
	rate, ok := acc.Rate()
	rec := PromotionRecommendation{Provider: acc.Provider, Rate: rate, Samples: acc.CalibratableValid}

	// Sample gate first (§7.2/§6.2): no promotion OR demotion decision on thin evidence. A promoted
	// provider that has since gone quiet keeps its state (we don't demote on absence of data either —
	// demotion needs measured evidence of degradation, symmetric with promotion needing measured trust).
	if !ok || acc.CalibratableValid < p.MinSamples {
		rec.Action = "hold"
		rec.Reason = "insufficient_samples"
		return rec
	}
	if currentlyPromoted {
		// Hysteretic demote: only when the rate climbs back to the HIGHER demote threshold (§7.2).
		if rate >= p.DemoteMinRate {
			rec.Action = "demote"
			rec.Reason = "rate_regressed"
			return rec
		}
		rec.Action = "hold"
		rec.Reason = "still_trusted"
		return rec
	}
	// Not yet promoted: promote only when the rate is at/under the LOWER promote threshold (§7.2).
	if rate <= p.PromoteMaxRate {
		rec.Action = "promote"
		rec.Reason = "measured_trust"
		return rec
	}
	rec.Action = "hold"
	rec.Reason = "rate_above_threshold"
	return rec
}

// ── §9 Per-provider fallback weaning (spending the SEAM P6 left) ──

// WeaningParams are the §9.2 thresholds (DIALs). Weaning drops the P6 fallback for a
// (residue_class × provider) bucket when the system's OWN measured false-valid rate beats the vendor's
// by a SAFETY MARGIN (not mere parity — don't wean on noise, §9.2/OQ-P7-6), with adequate samples on
// BOTH populations (the §6.2 gate on each — the own bucket AND the rented bucket).
type WeaningParams struct {
	SafetyMargin float64 // §9.2 own-rate must be <= vendor-rate - this margin before dropping (e.g. 0.005)
	MinSamples   int     // §9.2 min calibratable outcomes required on BOTH the own and vendor buckets
}

// WeaningRecommendation is one (residue_class × provider) bucket's recommendation (recommend-only,
// DR-2/A9). Action "wean" => drop the paid fallback for this bucket; "re_enable" => a previously-weaned
// bucket whose own accuracy degraded gets the fallback back (reversible, §9.2); "hold" => keep renting.
type WeaningRecommendation struct {
	Provider     string
	ResidueClass string
	Action       string // "wean" | "re_enable" | "hold"
	OwnRate      float64
	VendorRate   float64
	Reason       string
}

// EvaluateWeaning compares the system's OWN measured accuracy for a bucket against the VENDOR's
// measured accuracy for the SAME bucket (§9.2) and recommends whether to drop the paid fallback. Both
// must be MEASURED (not baseline-guessed) with adequate samples — you can only stop renting once you've
// EARNED the evidence you're better, not merely hoped it (§9.2 LOCKED). The two rates come from the two
// SEPARATE bucket spaces P6 kept isolable (own vs vendor × residue_class × provider, GAP-54) — the
// precondition weaning consumes. Reversible: a weaned bucket whose own rate later regresses past the
// vendor's is recommended for re_enable (§9.2). On a fresh system BOTH samples are unmet → "hold" →
// zero buckets weaned, the fallback carries full residue coverage exactly as P6 shipped it (§9.4).
func EvaluateWeaning(own, vendor Accuracy, currentlyWeaned bool, p WeaningParams) WeaningRecommendation {
	rec := WeaningRecommendation{Provider: own.Provider, ResidueClass: "" /* set by caller from bucket */}
	ownRate, ownOk := own.Rate()
	vendorRate, vendorOk := vendor.Rate()
	rec.OwnRate, rec.VendorRate = ownRate, vendorRate

	// §9.2: adequate samples on BOTH populations, both measured. Absent either, we cannot compare —
	// hold (keep renting; the vendor exists exactly for the residue we can't yet measure ourselves).
	if !ownOk || !vendorOk || own.CalibratableValid < p.MinSamples || vendor.CalibratableValid < p.MinSamples {
		rec.Action = "hold"
		rec.Reason = "insufficient_samples_on_both"
		return rec
	}
	if currentlyWeaned {
		// Reversible: if our own rate has regressed to NO LONGER beat the vendor (even without margin),
		// re-enable the fallback for this bucket (§9.2 hysteresis — quick to re-protect).
		if ownRate > vendorRate {
			rec.Action = "re_enable"
			rec.Reason = "own_accuracy_regressed"
			return rec
		}
		rec.Action = "hold"
		rec.Reason = "still_beating_vendor"
		return rec
	}
	// Not yet weaned: wean only when own beats vendor by the SAFETY MARGIN (§9.2 — not parity).
	if ownRate <= vendorRate-p.SafetyMargin {
		rec.Action = "wean"
		rec.Reason = "own_beats_vendor_by_margin"
		return rec
	}
	rec.Action = "hold"
	rec.Reason = "own_not_yet_beating_vendor"
	return rec
}
