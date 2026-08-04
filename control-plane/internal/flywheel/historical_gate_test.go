package flywheel

import (
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

func gateParams() HistoricalGateParams {
	return HistoricalGateParams{TTL: 180 * 24 * time.Hour, MinIndependent: 2, PenaltyPoints: 30}
}

// TestHistoricalGate_NeedsMinIndependent is the F-17 anti-forgery test (§4.6/PN-11): a SINGLE forged/
// mislabeled mailbox_not_found must NOT suppress a real address — the gate needs ≥N independent
// outcomes. FAIL-VERIFY: remove the IndependentCount < minN check in EvaluateHistoricalGate → one
// outcome fires the gate → this breaks.
func TestHistoricalGate_NeedsMinIndependent(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	p := gateParams() // MinIndependent 2
	// ONE confirmed bounce — below N. The gate must NOT fire (a single forged outcome can't blacklist).
	one := EvaluateHistoricalGate(HistoricalBounceFacts{IndependentCount: 1, LatestAt: now.Add(-time.Hour), Known: true}, p, now)
	if one.Effect != GateNone {
		t.Fatalf("gate fired on ONE outcome (effect %v) — F-17 requires ≥N independent (§4.6/PN-11)", one.Effect)
	}
	if one.WhyNot != "below_min_independent" {
		t.Fatalf("wrong WhyNot %q, want below_min_independent", one.WhyNot)
	}
	// TWO confirmed bounces — at N, within TTL → HARD invalid.
	two := EvaluateHistoricalGate(HistoricalBounceFacts{IndependentCount: 2, LatestAt: now.Add(-time.Hour), Known: true}, p, now)
	if two.Effect != GateInvalid || two.Reason != vocab.ReasonHistoricalBounce {
		t.Fatalf("gate did not fire at N=2 within TTL (effect %v reason %q) — want GateInvalid/historical_bounce", two.Effect, two.Reason)
	}
}

// TestHistoricalGate_ExpiresToPenalty is the OQ-P7-2 reversibility test (§4.6): past the TTL the hard
// gate DEGRADES to a confidence penalty, so a deleted-then-recreated mailbox can recover. FAIL-VERIFY:
// remove the TTL-window degrade branch → an old bounce stays a hard invalid forever → this breaks
// (PB2: the permanent-blacklist trap the TTL exists to prevent).
func TestHistoricalGate_ExpiresToPenalty(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	p := gateParams() // TTL 180d
	// A confirmed ≥N history, but the latest bounce is OLDER than the TTL → penalty, not a hard gate.
	old := EvaluateHistoricalGate(HistoricalBounceFacts{IndependentCount: 3, LatestAt: now.Add(-365 * 24 * time.Hour), Known: true}, p, now)
	if old.Effect != GatePenalty {
		t.Fatalf("past-TTL history: effect %v, want GatePenalty (recovery is not a one-way door, §4.6)", old.Effect)
	}
	if old.PenaltyPoints != p.PenaltyPoints {
		t.Fatalf("penalty points %d, want %d", old.PenaltyPoints, p.PenaltyPoints)
	}
	// Within the TTL it is still a hard gate.
	fresh := EvaluateHistoricalGate(HistoricalBounceFacts{IndependentCount: 3, LatestAt: now.Add(-24 * time.Hour), Known: true}, p, now)
	if fresh.Effect != GateInvalid {
		t.Fatalf("within-TTL history: effect %v, want GateInvalid", fresh.Effect)
	}
}

// TestHistoricalGate_NoHistoryNeverFires — the gate is inert absent confirmed history (§4.6).
func TestHistoricalGate_NoHistoryNeverFires(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	d := EvaluateHistoricalGate(HistoricalBounceFacts{Known: false}, gateParams(), now)
	if d.Effect != GateNone {
		t.Fatalf("gate fired with no history (effect %v) — must be inert (§4.6)", d.Effect)
	}
}
