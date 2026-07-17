package audit

import (
	"testing"
	"time"
)

// These tests prove the P3 gate LOCALLY against a simulated audit surface (P3 §11 acceptance): the
// GAP-24 egress-mismatch box FAILS the gate, an audit-green-but-port25-blocked box is NOT eligible
// (GAP-15), a red audit is a HARD block, and the quarantine/recover asymmetry holds. Only the real
// IP *values* are PENDING-REAL-BOX — the invariant logic and its failing cases are proven here.

func TestDeriveStatus(t *testing.T) {
	base := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		r    Result
		want Status
	}{
		{"empty warnings + IP => green", Result{CurrentHostIP: "203.0.113.10", Warnings: map[string]string{}, AuditedAt: base}, StatusGreen},
		{"nil warnings + IP => green", Result{CurrentHostIP: "203.0.113.10", AuditedAt: base}, StatusGreen},
		{"dns warning => red", Result{CurrentHostIP: "203.0.113.10", Warnings: map[string]string{"dns": "A-record mismatch"}}, StatusRed},
		{"ptr warning => red", Result{CurrentHostIP: "203.0.113.10", Warnings: map[string]string{"ptr": "no PTR"}}, StatusRed},
		{"undetectable IP => red (defensive)", Result{CurrentHostIP: "", Warnings: map[string]string{}}, StatusRed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveStatus(tt.r); got != tt.want {
				t.Fatalf("DeriveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// greenEligible is a fully-eligible input; each test perturbs ONE field to prove that conjunct
// is load-bearing (a failing-case test per §4.4 conjunct).
func greenEligible(now time.Time) EligibilityInput {
	return EligibilityInput{
		Status:           LifecycleActive,
		AuditStatus:      StatusGreen,
		Port25Open:       true,
		LastAuditAt:      now.Add(-time.Minute),
		LastAuditHostIP:  "203.0.113.10",
		ExpectedEgressIP: "203.0.113.10",
		Now:              now,
		MaxAuditAge:      2 * time.Hour,
	}
}

func TestEligibility_GreenPath(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if r := Eligibility(greenEligible(now)); r != SkipNone {
		t.Fatalf("fully green node must be eligible, got skip=%q", r)
	}
}

func TestEligibility_RedIsHardBlock(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	in := greenEligible(now)
	in.AuditStatus = StatusRed
	if r := Eligibility(in); r != SkipAuditRed {
		t.Fatalf("red audit must HARD-block :smtp (GAP §4.4), got skip=%q", r)
	}
	if Eligible(in) {
		t.Fatal("red audit node must not be eligible")
	}
}

func TestEligibility_Port25ClosedNotEligible_GAP15(t *testing.T) {
	// GAP-15: audit-green is necessary but NOT sufficient — a green box with egress 25 blocked is
	// NOT eligible. This is the "audit_status==green AND port25_open" rule (§4.4).
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	in := greenEligible(now)
	in.Port25Open = false
	if r := Eligibility(in); r != SkipPort25Closed {
		t.Fatalf("audit-green but port25-closed must be ineligible (GAP-15), got skip=%q", r)
	}
}

func TestEligibility_EgressMismatchFailsGate_GAP24(t *testing.T) {
	// GAP-24 (§6.6.1) — the CRITICAL local proof: a multi-homed box whose audit self-detected a
	// DIFFERENT IP than its expected public egress IP must FAIL the gate (a green on the private IP
	// is meaningless). This is the silent-verdict-poisoning trap; it is enforced and test-proven
	// HERE, not deferred. Only the real IP values are PENDING-REAL-BOX.
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	in := greenEligible(now)
	in.LastAuditHostIP = "10.0.0.5"      // audit ran against the PRIVATE interface
	in.ExpectedEgressIP = "203.0.113.10" // but the real public egress IP is this
	if r := Eligibility(in); r != SkipEgressMismatch {
		t.Fatalf("egress-IP != audited-IP must fail the gate (GAP-24), got skip=%q", r)
	}
	if Eligible(in) {
		t.Fatal("a box whose audit ran on the wrong IP must not be :smtp-eligible (GAP-24)")
	}
}

func TestEligibility_UnknownExpectedIPFailsSafe_GAP24(t *testing.T) {
	// If we don't KNOW the expected egress IP, we cannot assert the GAP-24 invariant → fail-safe
	// (do not run :smtp on a green whose IP we can't confirm).
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	in := greenEligible(now)
	in.ExpectedEgressIP = ""
	if r := Eligibility(in); r != SkipEgressMismatch {
		t.Fatalf("unknown expected egress IP must fail-safe (GAP-24), got skip=%q", r)
	}
}

func TestEligibility_LifecycleAndFreshness(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	quarantined := greenEligible(now)
	quarantined.Status = LifecycleQuarantined
	if r := Eligibility(quarantined); r != SkipNotActive {
		t.Fatalf("quarantined node must be ineligible, got skip=%q", r)
	}

	provisioning := greenEligible(now)
	provisioning.Status = LifecycleProvisioning
	if r := Eligibility(provisioning); r != SkipNotActive {
		t.Fatalf("provisioning node must be ineligible, got skip=%q", r)
	}

	neverAudited := greenEligible(now)
	neverAudited.LastAuditAt = time.Time{}
	if r := Eligibility(neverAudited); r != SkipNeverAudited {
		t.Fatalf("never-audited node must be ineligible, got skip=%q", r)
	}

	stale := greenEligible(now)
	stale.LastAuditAt = now.Add(-3 * time.Hour) // older than MaxAuditAge=2h
	if r := Eligibility(stale); r != SkipAuditStale {
		t.Fatalf("stale green must be ineligible until re-audit (§4.5), got skip=%q", r)
	}
}

func TestSignalToken_OnlyFrozenTokens(t *testing.T) {
	// Every node-health SkipReason maps to the frozen §8 "audit_red" token (finer reasons live in
	// logs, never in the frozen signal). SkipNone maps to "".
	for _, r := range []SkipReason{SkipAuditRed, SkipNotActive, SkipPort25Closed, SkipAuditStale, SkipEgressMismatch, SkipNeverAudited} {
		if got := SignalToken(r); got != "audit_red" {
			t.Fatalf("SignalToken(%q) = %q, want audit_red (frozen §8 enum)", r, got)
		}
	}
	if got := SignalToken(SkipNone); got != "" {
		t.Fatalf("SignalToken(SkipNone) = %q, want empty", got)
	}
}

func TestNextState_QuarantineOnFirstRed(t *testing.T) {
	// OQ-P3-7: a SINGLE red quarantines immediately from active (fail-safe).
	tr := NextState(LifecycleActive, 5, StatusRed, true, 3)
	if tr.NextStatus != LifecycleQuarantined {
		t.Fatalf("first red from active must quarantine, got %q", tr.NextStatus)
	}
	if tr.GreenStreak != 0 {
		t.Fatalf("red must reset the green streak, got %d", tr.GreenStreak)
	}
}

func TestNextState_RecoverRequiresNConsecutiveGreens(t *testing.T) {
	// OQ-P3-7 asymmetry: recovery needs N consecutive greens (debounce flapping). N=3.
	// green #1, #2 stay quarantined; green #3 recovers to active.
	streak := 0
	cur := LifecycleQuarantined
	for i := 1; i <= 2; i++ {
		tr := NextState(cur, streak, StatusGreen, true, 3)
		if tr.NextStatus != LifecycleQuarantined {
			t.Fatalf("green #%d must stay quarantined (need 3), got %q", i, tr.NextStatus)
		}
		cur, streak = tr.NextStatus, tr.GreenStreak
	}
	tr := NextState(cur, streak, StatusGreen, true, 3)
	if tr.NextStatus != LifecycleActive {
		t.Fatalf("3rd consecutive green must recover to active, got %q", tr.NextStatus)
	}
}

func TestNextState_RedMidRecoverResetsStreak(t *testing.T) {
	// A red during recovery resets the streak — flapping cannot sneak through.
	tr := NextState(LifecycleQuarantined, 2, StatusRed, true, 3)
	if tr.NextStatus != LifecycleQuarantined || tr.GreenStreak != 0 {
		t.Fatalf("red mid-recover must stay quarantined with streak reset, got %q streak=%d", tr.NextStatus, tr.GreenStreak)
	}
}

func TestNextState_ProvisioningPromotionNeedsPort25(t *testing.T) {
	// §4.5/GAP-15: provisioning→active needs green ∧ port25_open. A green with port25 closed stays
	// provisioning (audit-green but can't send — not :smtp-ready).
	noEgress := NextState(LifecycleProvisioning, 0, StatusGreen, false, 3)
	if noEgress.NextStatus != LifecycleProvisioning {
		t.Fatalf("green without port25_open must stay provisioning (GAP-15), got %q", noEgress.NextStatus)
	}
	withEgress := NextState(LifecycleProvisioning, 0, StatusGreen, true, 3)
	if withEgress.NextStatus != LifecycleActive {
		t.Fatalf("first green ∧ port25_open must promote to active, got %q", withEgress.NextStatus)
	}
}

func TestNextState_QuarantineRecoverNeedsPort25(t *testing.T) {
	// Recovery also requires egress still proven, even after N greens.
	tr := NextState(LifecycleQuarantined, 5, StatusGreen, false, 3)
	if tr.NextStatus != LifecycleQuarantined {
		t.Fatalf("recovery must require port25_open even after N greens, got %q", tr.NextStatus)
	}
}
