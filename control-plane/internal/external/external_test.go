package external

import (
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// The anti-laundering headline (§3.4, acceptance criterion 2, the phase's correctness heart): a
// vendor "valid" can NEVER become status:valid. This is the demo the fail-verify test breaks by
// removing the cap; here we pin the correct behavior across all four residue classes.
func TestResolve_VendorValid_IsNeverValid(t *testing.T) {
	for _, class := range []ResidueClass{ResidueCatchAll, ResidueProviderNoSMTP, ResidueRetryExhausted, ResidueGaveUpRateLimited} {
		got := Resolve(VendorResult{Result: vocab.StatusValid, ConfidenceRaw: 99}, class, 60)
		if got.Status == vocab.StatusValid {
			t.Fatalf("class %s: a vendor 'valid' was laundered into status:valid — the anti-laundering rule (§3.4) is BROKEN", class)
		}
		if got.Status != vocab.StatusRisky {
			t.Errorf("class %s: vendor valid must map to risky, got %s", class, got.Status)
		}
		if got.Reason != vocab.ReasonExternalVerified {
			t.Errorf("class %s: reason must be external_verified, got %s", class, got.Reason)
		}
		if got.Confidence > 60 {
			t.Errorf("class %s: confidence %d exceeds the cap 60 — a rented verdict is never SMTP-grade", class, got.Confidence)
		}
	}
}

// The confidence cap is a hard ceiling: a vendor reporting 99 is capped to 60; a vendor reporting a
// LOWER own confidence lowers it further (never raises above the cap).
func TestResolve_ConfidenceCapIsCeiling(t *testing.T) {
	hi := Resolve(VendorResult{Result: vocab.StatusValid, ConfidenceRaw: 99}, ResidueCatchAll, 60)
	if hi.Confidence != 60 {
		t.Errorf("a high vendor confidence must be capped to 60, got %d", hi.Confidence)
	}
	lo := Resolve(VendorResult{Result: vocab.StatusValid, ConfidenceRaw: 40}, ResidueCatchAll, 60)
	if lo.Confidence != 40 {
		t.Errorf("a lower vendor confidence must lower the result below the cap, got %d", lo.Confidence)
	}
}

// OQ-P6-3 (DECIDED, asymmetric): a vendor "invalid" is accepted as invalid ONLY where independent
// non-existence is plausible (retry-exhausted, gave-up-capacity), and DOWNGRADED to risky on
// catch-all/:mx (where a vendor "invalid" is itself a guess and would irreversibly discard a real
// address). Trust their "no" only where "no" is structurally possible.
func TestResolve_VendorInvalid_Asymmetry_OQP63(t *testing.T) {
	// Accepted as invalid on the "non-existence plausible" classes.
	for _, class := range []ResidueClass{ResidueRetryExhausted, ResidueGaveUpRateLimited} {
		got := Resolve(VendorResult{Result: vocab.StatusInvalid, ConfidenceRaw: 95}, class, 60)
		if got.Status != vocab.StatusInvalid {
			t.Errorf("class %s: a vendor invalid must be accepted as invalid (independent non-existence plausible), got %s", class, got.Status)
		}
		if got.Reason != vocab.ReasonExternalVerified {
			t.Errorf("class %s: reason must be external_verified, got %s", class, got.Reason)
		}
	}
	// Downgraded to risky on the accept-all/:mx classes — a false invalid there discards a real
	// address IRREVERSIBLY; a vendor "invalid" on a catch-all domain is a guess.
	for _, class := range []ResidueClass{ResidueCatchAll, ResidueProviderNoSMTP} {
		got := Resolve(VendorResult{Result: vocab.StatusInvalid, ConfidenceRaw: 95}, class, 60)
		if got.Status != vocab.StatusRisky {
			t.Errorf("class %s: a vendor invalid must DOWNGRADE to risky (OQ-P6-3), got %s — a guess would irreversibly discard a real address", class, got.Status)
		}
	}
}

// unknown stays first-class (principle 2, §3.2 on_vendor_unknown=keep_residue): a vendor unknown (or
// any unrecognized status normalized to unknown) leaves the residue verdict unchanged — never a
// fabricated invalid, never a degrade.
func TestResolve_VendorUnknown_KeepsResidue(t *testing.T) {
	got := Resolve(VendorResult{Result: vocab.StatusUnknown}, ResidueCatchAll, 60)
	if !got.KeepResidue {
		t.Fatal("a vendor unknown must keep_residue (principle 2 — the fallback can only improve, never degrade)")
	}
}

func TestResolve_VendorRisky_IsExternalRisky(t *testing.T) {
	got := Resolve(VendorResult{Result: vocab.StatusRisky, ConfidenceRaw: 50}, ResidueCatchAll, 60)
	if got.KeepResidue || got.Status != vocab.StatusRisky || got.Reason != vocab.ReasonExternalVerified {
		t.Errorf("vendor risky → risky/external_verified, got %+v", got)
	}
}

// Classify is the CLOSED-SET cost firewall (§4.1/§4.2): exactly the four residue classes are
// escalatable, and greylist/gave-up only AT EXHAUSTION (preserving the P5 retry-first invariant).
// Everything else (a hard invalid, an SMTP-earned valid, a disposable-risky) is NOT escalatable.
func TestClassify_ClosedSet(t *testing.T) {
	rl := map[string]any{"smtp_skipped_reason": "rate_limited"}
	cases := []struct {
		name      string
		status    vocab.Status
		reason    vocab.Reason
		signals   map[string]any
		exhausted bool
		wantClass ResidueClass
		wantOK    bool
	}{
		{"catch_all inline", vocab.StatusRisky, vocab.ReasonCatchAll, nil, false, ResidueCatchAll, true},
		{"accept_all inline", vocab.StatusRisky, vocab.ReasonAcceptAll, nil, false, ResidueCatchAll, true},
		{"provider_no_smtp inline", vocab.StatusRisky, vocab.ReasonProviderNoSMTP, nil, false, ResidueProviderNoSMTP, true},
		{"greylisted NOT escalatable before exhaustion", vocab.StatusUnknown, vocab.ReasonGreylisted, nil, false, "", false},
		{"greylisted escalatable at exhaustion", vocab.StatusUnknown, vocab.ReasonGreylisted, nil, true, ResidueRetryExhausted, true},
		{"gave-up rate_limited only at exhaustion", vocab.StatusRisky, vocab.ReasonPlausible, rl, true, ResidueGaveUpRateLimited, true},
		{"rate_limited NOT escalatable before exhaustion", vocab.StatusRisky, vocab.ReasonPlausible, rl, false, "", false},
		// The never-escalated set (the cost firewall §4.2): none of these is a residue class.
		{"hard invalid never", vocab.StatusInvalid, vocab.ReasonInvalidSyntax, nil, true, "", false},
		{"no_mx invalid never", vocab.StatusInvalid, vocab.ReasonNoMX, nil, true, "", false},
		{"SMTP-earned valid never", vocab.StatusValid, vocab.ReasonAccepted, nil, true, "", false},
		{"disposable-risky never", vocab.StatusRisky, vocab.ReasonDisposable, nil, true, "", false},
		{"plausible without rate_limited never", vocab.StatusRisky, vocab.ReasonPlausible, nil, true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class, ok := Classify(c.status, c.reason, c.signals, c.exhausted)
			if ok != c.wantOK || class != c.wantClass {
				t.Errorf("Classify(%s/%s exhausted=%v) = (%q,%v), want (%q,%v)",
					c.status, c.reason, c.exhausted, class, ok, c.wantClass, c.wantOK)
			}
		})
	}
}

func TestRouteWhenAllows(t *testing.T) {
	rw := []string{"catch_all", "retry_exhausted_greylisted"}
	if !RouteWhenAllows(rw, ResidueCatchAll) {
		t.Error("catch_all should be allowed by route_when")
	}
	if RouteWhenAllows(rw, ResidueProviderNoSMTP) {
		t.Error("provider_no_smtp is NOT in route_when — must not escalate")
	}
	if RouteWhenAllows(nil, ResidueCatchAll) {
		t.Error("an empty route_when must escalate NOTHING (safe default)")
	}
}

// Breaker condition 1 — budget exhaustion (§5.3.1): any reached window cap opens the breaker; a
// per-job cap trips first so a pathological list is bounded before it can push the global over.
func TestBreaker_BudgetExhaustion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := BreakerParams{}
	// per_job cap reached (a 90%-catch-all job) → open on per_job before day/month.
	d := Decide(Budget{PerJobCap: 10, DailyCap: 1000}, p, SpendFacts{SpentJob: 10, SpentDay: 20}, now)
	if d.Allow || d.Reason != TripBudgetJob {
		t.Errorf("per_job cap reached must trip on per_job, got %+v", d)
	}
	// daily cap reached.
	d = Decide(Budget{DailyCap: 100}, p, SpendFacts{SpentDay: 100}, now)
	if d.Allow || d.Reason != TripBudgetDay {
		t.Errorf("daily cap reached must open, got %+v", d)
	}
	// under all caps → allowed.
	d = Decide(Budget{DailyCap: 100, MonthlyCap: 1000, PerJobCap: 10}, p, SpendFacts{SpentDay: 50, SpentMonth: 500, SpentJob: 5}, now)
	if !d.Allow || d.State != Closed {
		t.Errorf("under all caps must allow (closed), got %+v", d)
	}
	// unset caps (0) are not enforced.
	d = Decide(Budget{}, p, SpendFacts{SpentDay: 1_000_000}, now)
	if !d.Allow {
		t.Errorf("unset caps must not enforce a ceiling, got %+v", d)
	}
}

// Breaker rate-ceiling (§5.2): a burst brake on vendor CALLS per minute, distinct from the
// cumulative cost caps. Reached calls/min → open (keep_residue); self-clears as the window rolls.
func TestBreaker_RateCeiling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := BreakerParams{}
	// Under the ceiling → allowed.
	d := Decide(Budget{RateCeiling: 10}, p, SpendFacts{CallsLastMinute: 9}, now)
	if !d.Allow {
		t.Errorf("under the rate ceiling must allow, got %+v", d)
	}
	// At the ceiling → open (burst brake).
	d = Decide(Budget{RateCeiling: 10}, p, SpendFacts{CallsLastMinute: 10}, now)
	if d.Allow || d.Reason != TripRateCeiling {
		t.Errorf("at the rate ceiling must open with TripRateCeiling, got %+v", d)
	}
	// Unset (0) → not enforced.
	d = Decide(Budget{}, p, SpendFacts{CallsLastMinute: 1_000_000}, now)
	if !d.Allow {
		t.Errorf("an unset rate ceiling must not enforce, got %+v", d)
	}
}

// Breaker conditions 2/3 — error-rate + cooldown/half-open (§5.3.2/§5.3.3), gated by MinSamples.
func TestBreaker_ErrorRateAndHalfOpen(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := BreakerParams{ErrorRateThreshold: 0.5, MinSamples: 4, Cooldown: 5 * time.Minute}

	// Below the sample gate: high error fraction but too few calls → still allowed (GAP-27).
	d := Decide(Budget{}, p, SpendFacts{Calls: 2, Errors: 2}, now)
	if !d.Allow {
		t.Errorf("error-rate must not trip below MinSamples (GAP-27), got %+v", d)
	}
	// Above the gate and over threshold → open.
	d = Decide(Budget{}, p, SpendFacts{Calls: 10, Errors: 6}, now)
	if d.Allow || d.Reason != TripErrorRate {
		t.Errorf("error-rate over threshold must open, got %+v", d)
	}
	// Already open, within cooldown → stays open.
	opened := now
	d = Decide(Budget{}, p, SpendFacts{OpenedAt: opened}, opened.Add(1*time.Minute))
	if d.Allow || d.State != Open {
		t.Errorf("within cooldown must stay open, got %+v", d)
	}
	// Cooldown elapsed → half-open (one probe allowed).
	d = Decide(Budget{}, p, SpendFacts{OpenedAt: opened}, opened.Add(6*time.Minute))
	if !d.Allow || d.State != HalfOpen {
		t.Errorf("after cooldown must be half-open (probe allowed), got %+v", d)
	}
}

// The mock is controllable per the kickoff: valid/invalid/unknown/error on command, counts calls,
// records idempotency keys (the spend-once assertion substrate) and addresses (privacy assertion).
func TestMockVendor_Controllable(t *testing.T) {
	m := NewMockVendor("millionverifier", MockOutcome{Result: vocab.StatusValid, StatusRaw: "ok"})
	m.Set("bad@x.com", MockOutcome{Result: vocab.StatusInvalid, StatusRaw: "invalid"})
	m.Set("down@x.com", MockOutcome{Err: ErrVendorDown})

	if r, err := m.Verify(t.Context(), "good@x.com", "k1"); err != nil || r.Result != vocab.StatusValid || r.CostUnits != 1 {
		t.Errorf("default outcome: %+v err=%v", r, err)
	}
	if r, _ := m.Verify(t.Context(), "bad@x.com", "k2"); r.Result != vocab.StatusInvalid {
		t.Errorf("per-address override not applied: %+v", r)
	}
	if _, err := m.Verify(t.Context(), "down@x.com", "k3"); err != ErrVendorDown {
		t.Errorf("programmed error not returned: %v", err)
	}
	if m.Calls() != 3 {
		t.Errorf("call count = %d, want 3", m.Calls())
	}
	if m.KeysSeen["k1"] != 1 {
		t.Errorf("idempotency key k1 seen %d times, want 1", m.KeysSeen["k1"])
	}
}
