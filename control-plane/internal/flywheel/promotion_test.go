package flywheel

import "testing"

// TestAccuracy_NeverBareNumber is the §8.2 test: an accuracy rate is NEVER reportable without its
// coverage. FAIL-VERIFY: make Reportable return the rate without requiring coverage → this breaks
// (the GAP-52 overclaim §14 exists to prevent).
func TestAccuracy_NeverBareNumber(t *testing.T) {
	// A high apparent accuracy on tiny coverage: 1000 valid-labels, only 10 got a measurable outcome,
	// 0 false. "0% false-valid!" would be a lie — coverage is 1%.
	a := Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 10, FalseValid: 0}
	rate, cov, ok := a.Reportable()
	if !ok {
		t.Fatal("Reportable returned not-ok on measured data")
	}
	if cov >= 0.02 {
		t.Fatalf("coverage %.3f — expected ~1%% (the honesty signal §8.2 forces alongside the rate)", cov)
	}
	_ = rate
	// With ZERO calibratable outcomes there is NO number to report (not "100% accurate", §8.4).
	empty := Accuracy{Provider: "yahoo", ValidLabeled: 500, CalibratableValid: 0, FalseValid: 0}
	if _, _, ok := empty.Reportable(); ok {
		t.Fatal("Reportable returned a number with zero coverage — absence of measurement is not 100% (§8.4)")
	}
	if _, ok := empty.Rate(); ok {
		t.Fatal("Rate returned ok with a zero denominator — must be not-ok (§8.1)")
	}
}

// TestPromotion_SampleGatedAndReversible (§7.2): promote only with a low rate AND adequate samples;
// demote hysteretically (higher threshold). On a fresh (thin-sample) provider: HOLD (the §14.2 cold-
// start truth — zero promotions on day one). FAIL-VERIFY: drop the MinSamples gate → a 10-send provider
// promotes → this breaks.
func TestPromotion_SampleGatedAndReversible(t *testing.T) {
	p := PromotionParams{PromoteMaxRate: 0.02, DemoteMinRate: 0.05, MinSamples: 500}
	// Fresh provider: 10 clean sends. Untested, not trusted → HOLD (cold-start, §7.4).
	fresh := EvaluatePromotion(Accuracy{Provider: "google", ValidLabeled: 10, CalibratableValid: 10, FalseValid: 0}, false, p)
	if fresh.Action != "hold" {
		t.Fatalf("fresh provider (10 samples): action %q, want hold (sample gate, §7.2/§7.4)", fresh.Action)
	}
	// Well-sampled, low rate → PROMOTE.
	good := EvaluatePromotion(Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 5}, false, p)
	if good.Action != "promote" {
		t.Fatalf("well-sampled low-rate provider: action %q, want promote", good.Action)
	}
	// A promoted provider whose rate regressed past the (higher) demote threshold → DEMOTE (reversible).
	regressed := EvaluatePromotion(Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 60}, true, p)
	if regressed.Action != "demote" {
		t.Fatalf("regressed promoted provider (6%% > 5%% demote): action %q, want demote (hysteresis, §7.2)", regressed.Action)
	}
}

// TestWeaning_RequiresMeasuredMarginOnBoth (§9.2): wean only when OWN measured accuracy beats the
// VENDOR's by a safety margin, with samples on BOTH. FAIL-VERIFY: drop the MinSamples-on-both gate, or
// the safety margin → a thin/parity comparison weans → this breaks (§9.2/OQ-P7-6).
func TestWeaning_RequiresMeasuredMarginOnBoth(t *testing.T) {
	p := WeaningParams{SafetyMargin: 0.01, MinSamples: 500}
	own := Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 10}     // 1.0% false
	vendor := Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 40}  // 4.0% false
	// Own beats vendor by 3% > 1% margin, both well-sampled → WEAN.
	if r := EvaluateWeaning(own, vendor, false, p); r.Action != "wean" {
		t.Fatalf("own 1%% vs vendor 4%% (both n=1000): action %q, want wean (§9.2)", r.Action)
	}
	// Thin OWN samples → HOLD (can't stop renting on unearned evidence, §9.2 LOCKED).
	thinOwn := Accuracy{Provider: "google", ValidLabeled: 50, CalibratableValid: 50, FalseValid: 0}
	if r := EvaluateWeaning(thinOwn, vendor, false, p); r.Action != "hold" {
		t.Fatalf("thin own samples: action %q, want hold (measured-evidence gate, §9.2)", r.Action)
	}
	// Parity (no margin) → HOLD (don't wean on noise, §9.2/OQ-P7-6).
	parity := Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 40}
	if r := EvaluateWeaning(parity, vendor, false, p); r.Action != "hold" {
		t.Fatalf("parity own==vendor: action %q, want hold (safety margin, not parity)", r.Action)
	}
	// A weaned bucket whose own accuracy regressed past the vendor → RE_ENABLE (reversible, §9.2).
	regressed := Accuracy{Provider: "google", ValidLabeled: 1000, CalibratableValid: 1000, FalseValid: 50} // 5% > vendor 4%
	if r := EvaluateWeaning(regressed, vendor, true, p); r.Action != "re_enable" {
		t.Fatalf("regressed weaned bucket: action %q, want re_enable (reversible, §9.2)", r.Action)
	}
}
