package flywheel

import (
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// This file: the HISTORICAL-BOUNCE GATE (§4.6) — the ONE new gate P7 adds, using the P1-RESERVED
// reason=historical_bounce (Door 4: ZERO new vocabulary). Once an address has a CONFIRMED
// invalid-signal (mailbox_not_found) bounce on record, re-verifying it short-circuits to
// invalid/historical_bounce — you have GROUND-TRUTH proof it doesn't exist, stronger than any SMTP
// probe. This is the only place P7 emits a verdict-affecting value, and it was reserved a phase ago.
//
// THREE safety properties make this gate safe rather than a permanent-blacklist trap:
//
//  1. TTL-windowed, then DEGRADES to a confidence penalty (OQ-P7-2 DECIDED, §4.6). A mailbox can be
//     deleted then re-created; a username reclaimed. Within the TTL (6–12mo DIAL) the gate is a HARD
//     invalid; after it, it becomes a strong confidence PENALTY (not a hard gate) so a re-created
//     mailbox can recover ("recovery is not a one-way door" — the P4 canary instinct, GAP-30).
//
//  2. Fires ONLY on invalid-signal (§4.6 anti-poisoning). A zero-information bounce (blocked,
//     greylisted, policy) NEVER creates a historical-bounce record — otherwise a sender-reputation
//     problem would permanently mislabel real addresses as invalid. The classifier's exclusion
//     discipline (§4.3) protects the gate.
//
//  3. Requires ≥N INDEPENDENT invalid-signal outcomes before firing (F-17 / PN-11, defense-in-depth
//     BEHIND the ADR-0001/ADR-0013 authentication door). A SINGLE forged/mislabeled mailbox_not_found
//     could otherwise suppress a real address for the whole TTL. With authentication now mandatory
//     (ADR-0013), ≥N is LAYERED protection, not the sole guard. N is a DIAL (owner/P7-time).
//
// It reads the ADDRESS-LEVEL email_hash rollup — the ONE place the rollup (not verification_id) is the
// right key (§3.1): "has this address EVER hard-bounced". This is A10 in the shadow-mode audit; it is
// auto-safe (TTL + confirmed-only + ≥N) and stays auto (shadow-mode §3), unlike the recommend-only
// A6/A8/A9 mutators. Pure logic over injected facts; the store supplies the rollup.

// HistoricalGateParams are the §4.6 / F-17 DIALs (existence LOCKED; values owner/P7-time).
type HistoricalGateParams struct {
	TTL          time.Duration // §4.6 hard-gate window on a confirmed mailbox_not_found; after it → penalty
	MinIndependent int         // F-17/PN-11: ≥N INDEPENDENT invalid-signal outcomes before the gate may fire
	PenaltyPoints  int         // §4.6 the post-TTL confidence penalty (a strong nudge, not a hard invalid)
}

// HistoricalBounceFacts is the address-level rollup the gate reads (§3.1, email_hash rollup). It counts
// only CONFIRMED invalid-signal (mailbox_not_found) outcomes for the address — a zero-information bounce
// is NEVER counted here (§4.6 anti-poisoning; enforced upstream by the classifier + the ingestion
// writer, which only records an invalid-signal into this rollup). IndependentCount is the number of
// DISTINCT invalid-signal outcomes (distinct message_id / send events — F-17's "independent"), so a
// single replayed/forged outcome counts once, not N times.
type HistoricalBounceFacts struct {
	IndependentCount int       // distinct confirmed invalid-signal outcomes for this address (F-17 numerator)
	LatestAt         time.Time // when the most recent confirmed invalid-signal occurred (TTL anchor)
	Known            bool      // false => no confirmed invalid-signal on record (the gate never fires)
}

// GateEffect is what the historical-bounce gate does to a verdict at scoring time.
type GateEffect int

const (
	GateNone    GateEffect = iota // no confirmed history / below ≥N → the gate does not fire
	GateInvalid                   // within TTL + ≥N met → HARD short-circuit to invalid/historical_bounce
	GatePenalty                   // past TTL (but ≥N met) → a strong confidence PENALTY, recoverable (§4.6)
)

// GateDecision is the resolved gate effect + (for the hard case) the verdict values to apply. When
// Effect==GateInvalid the caller sets status=invalid, reason=historical_bounce (RESERVED, Door 4), and
// flags.historical_bounce (an additive Door-1 flag). When Effect==GatePenalty the caller subtracts
// PenaltyPoints from the computed confidence (never below 0) but does NOT change status/reason — a
// re-created mailbox can still earn its way back (§4.6, recovery is not a one-way door).
type GateDecision struct {
	Effect        GateEffect
	Reason        vocab.Reason // vocab.ReasonHistoricalBounce when Effect==GateInvalid; "" otherwise
	PenaltyPoints int          // when Effect==GatePenalty
	WhyNot        string       // "no_history" | "below_min_independent" — when Effect==GateNone (observability)
}

// EvaluateHistoricalGate resolves the gate for an address at scoring time (§4.6). It fires ONLY on
// confirmed invalid-signal history (facts.Known), requires ≥N INDEPENDENT outcomes (F-17), and is
// TTL-windowed with a post-window degrade to a penalty (OQ-P7-2). now is injected (deterministic).
//
// The ≥N guard is the load-bearing anti-forgery line BEHIND authentication: a single forged
// mailbox_not_found yields IndependentCount==1, so with MinIndependent≥2 the gate does NOT fire — the
// fail-verify test asserts one outcome never blacklists an address, and that the gate DOES fire at N,
// and that it EXPIRES to a penalty past the TTL. (Neuter the ≥N check → the one-forged-outcome test
// breaks; neuter the TTL degrade → the re-created-mailbox-recovers test breaks.)
func EvaluateHistoricalGate(facts HistoricalBounceFacts, p HistoricalGateParams, now time.Time) GateDecision {
	// No confirmed invalid-signal history → the gate never fires (a zero-information bounce never
	// reached this rollup — §4.6 anti-poisoning, enforced at the ingestion writer).
	if !facts.Known || facts.IndependentCount <= 0 {
		return GateDecision{Effect: GateNone, WhyNot: "no_history"}
	}
	// F-17 / PN-11: require ≥N INDEPENDENT invalid-signal outcomes. Below N, the gate does not fire —
	// a single forged/mislabeled outcome cannot suppress a real address for the TTL (defense-in-depth
	// behind the ADR-0001/ADR-0013 authenticated feed). MinIndependent<=0 is treated as 1 (a confirmed
	// bounce), but the DIAL should be >=2 to get the F-17 protection; startup config should set it.
	minN := p.MinIndependent
	if minN < 1 {
		minN = 1
	}
	if facts.IndependentCount < minN {
		return GateDecision{Effect: GateNone, WhyNot: "below_min_independent"}
	}
	// TTL window (OQ-P7-2): within the window → HARD invalid; past it → degrade to a recoverable
	// confidence penalty (a re-created mailbox can recover — recovery is not a one-way door, §4.6).
	if p.TTL > 0 && !facts.LatestAt.IsZero() && now.Sub(facts.LatestAt) > p.TTL {
		return GateDecision{Effect: GatePenalty, PenaltyPoints: p.PenaltyPoints}
	}
	return GateDecision{Effect: GateInvalid, Reason: vocab.ReasonHistoricalBounce}
}
