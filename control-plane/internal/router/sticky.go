package router

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// This file: the Phase-4 router strategies (§3) — sticky-by-domain (default) + least-inflight
// (fallback), BOTH new implementations behind the UNCHANGED P1 §5.4 Strategy interface. That the
// interface does not change is the Door-5 payoff (P1 §5.4 SEAM: "sticky + least-inflight are P4 —
// new Strategy impls, no interface change"). Adding node #2 stays a config-list edit (GAP-31).
//
// Sticky-by-domain is the load-bearing strategy (§3.1, highest rigor). Without it, a provider's
// checks scatter across all N nodes and you MULTIPLY provider-perceived load by N — defeating any
// per-provider cap and burning every IP in parallel. Sticky routing CONCENTRATES a domain (hence
// largely a provider) on one node, so the per-(node × provider) rate limiter actually bounds what
// the provider sees. "Sticky concentrates; the limiter paces" — one mechanism in two parts (§4.1).
//
// The Strategy sees only the ELIGIBLE candidate set (the SMTPRoute filter already applied audit
// eligibility, §3.3). Rate/warmup headroom is a per-node property the Strategy consults through
// injected oracles (set at construction — NOT interface methods, so Door 5 holds): HasHeadroom
// decides whether the sticky home can take THIS check; Inflight ranks the least-inflight fallback.

// HeadroomFunc reports whether nodeID has rate + warmup headroom for the check described by plan
// (§3.3 conjuncts "not rate-capped ∧ warmup headroom"). It is a PEEK — it consumes nothing (the
// actual reserve happens at call time in the orchestrator). A nil oracle means "always has
// headroom" (the inert/no-limiter path), so the strategy degrades to pure sticky affinity.
type HeadroomFunc func(ctx context.Context, nodeID string, plan Plan) bool

// InflightFunc returns the current per-(node × provider) open-session count for least-inflight
// ranking (§3.4, PM2 — the fallback ranks on the constraint being relieved, this provider's
// concurrency, not node-global). A nil oracle ranks everything 0 → the fallback becomes stable HRW.
type InflightFunc func(nodeID string, plan Plan) int

// StickyStrategy implements sticky-by-domain (HRW/rendezvous, OQ-P4-1) with a least-inflight
// fallback (§3.4). It is stateless + deterministic given (plan, candidates) + the oracles.
type StickyStrategy struct {
	// Headroom decides if a candidate can take this check (rate/warmup). nil => always yes.
	Headroom HeadroomFunc
	// Inflight ranks the least-inflight fallback. nil => all zero (ties broken by HRW).
	Inflight InflightFunc
}

// NewSticky builds a sticky-by-domain strategy. Both oracles are optional (nil => the strategy still
// works as pure sticky affinity, useful for tests / an inert limiter).
func NewSticky(headroom HeadroomFunc, inflight InflightFunc) StickyStrategy {
	return StickyStrategy{Headroom: headroom, Inflight: inflight}
}

// Pick applies the two-stage LOCKED selection (§3.3): (1) the sticky-by-domain home for the
// plan.Domain via HRW over the eligible candidates; (2) if the sticky home has NO headroom (rate-
// capped / warmup-capped), fall through to least-inflight over the remaining candidates WITH
// headroom. Sticky first, least-inflight only as the fallback — exactly the build-plan wording.
//
// If NO eligible candidate has headroom, it returns ErrNoEligibleNode → the orchestrator degrades to
// cheap with smtp_skipped_reason="rate_limited" (§4.6, never invalid/queued). This is the router's
// contribution to harden-toward-unknown at the rate-saturation seam.
func (s StickyStrategy) Pick(ctx context.Context, plan Plan, candidates []Node) (Node, error) {
	if len(candidates) == 0 {
		return Node{}, ErrNoEligibleNode
	}

	// Stage 1: the sticky home = argmax HRW score over the eligible set for this domain. Empty domain
	// (a check with no domain — shouldn't happen for :smtp) still gets a stable home via HRW on "".
	home := hrwPick(plan.Domain, candidates)

	// Stage 1 accept: the sticky home takes the check iff it has headroom (§3.3). No headroom oracle
	// => always accept (pure sticky affinity).
	if s.hasHeadroom(ctx, home, plan) {
		return home, nil
	}

	// Stage 2 (fallback, §3.4): least-inflight over the remaining eligible nodes WITH headroom. The
	// fallback NEVER places a check on an ineligible node (eligibility is absolute, §3.4 boundary) —
	// candidates are already the eligible set; here we further require rate/warmup headroom.
	var withHeadroom []Node
	for _, n := range candidates {
		if n.ID == home.ID {
			continue // the sticky home already lacks headroom
		}
		if s.hasHeadroom(ctx, n, plan) {
			withHeadroom = append(withHeadroom, n)
		}
	}
	if len(withHeadroom) == 0 {
		// Every eligible node is at its per-provider cap (NOT quarantined — just paced). This is
		// rate-SATURATION, not a missing node: return the distinct ErrRateSaturated so the caller
		// emits smtp_skipped_reason="rate_limited" (§4.6), not "no_eligible_node". Never overflow
		// onto a capped/ineligible node. If there was NO headroom oracle wired at all, hasHeadroom is
		// always true and we never reach here — this path is exclusively rate saturation.
		return Node{}, ErrRateSaturated
	}
	return s.leastInflight(withHeadroom, plan), nil
}

// hasHeadroom consults the injected oracle (nil => always true).
func (s StickyStrategy) hasHeadroom(ctx context.Context, n Node, plan Plan) bool {
	if s.Headroom == nil {
		return true
	}
	return s.Headroom(ctx, n.ID, plan)
}

// leastInflight picks the node with the fewest per-(node × provider) in-flight sessions (§3.4). Ties
// break by HRW on the domain (a stable, domain-consistent tie-break — recommended in §3.4) so equal-
// inflight nodes still map a domain consistently. A nil Inflight oracle ranks all 0 → pure HRW.
func (s StickyStrategy) leastInflight(nodes []Node, plan Plan) Node {
	best := nodes[0]
	bestN := s.inflightOf(best, plan)
	bestScore := hrwScore(plan.Domain, best.ID)
	for _, n := range nodes[1:] {
		c := s.inflightOf(n, plan)
		sc := hrwScore(plan.Domain, n.ID)
		if c < bestN || (c == bestN && sc > bestScore) {
			best, bestN, bestScore = n, c, sc
		}
	}
	return best
}

func (s StickyStrategy) inflightOf(n Node, plan Plan) int {
	if s.Inflight == nil {
		return 0
	}
	return s.Inflight(n.ID, plan)
}

// LeastInflightStrategy is the standalone least-inflight strategy (§3.4) — usable directly (e.g. a
// policy that opts out of stickiness) or as the sticky fallback (above). It ranks the eligible set by
// per-(node × provider) in-flight, ties broken by HRW for stability.
type LeastInflightStrategy struct {
	Inflight InflightFunc
}

func (s LeastInflightStrategy) Pick(_ context.Context, plan Plan, candidates []Node) (Node, error) {
	if len(candidates) == 0 {
		return Node{}, ErrNoEligibleNode
	}
	st := StickyStrategy{Inflight: s.Inflight}
	return st.leastInflight(candidates, plan), nil
}

// ── HRW / rendezvous hashing (OQ-P4-1) ──────────────────────────────────────────────────────────
// node = argmax_over_eligible(hash(domain, node_id)). This delivers sticky affinity AND minimal
// reshuffle on membership change (when a node quarantines, ONLY its domains rehash; every other
// domain stays put) from a PURE FUNCTION of (domain, eligible-set) — no stored routing state,
// brain-restart-safe (§3.2 recommendation A). This is exactly the "re-route on quarantine, absorb a
// new node" behavior the phase must prove.

// hrwPick returns the candidate with the maximum HRW score for the domain (the sticky home). Ties
// (astronomically unlikely with a 64-bit hash) break by node ID for determinism.
func hrwPick(domain string, candidates []Node) Node {
	best := candidates[0]
	bestScore := hrwScore(domain, best.ID)
	for _, n := range candidates[1:] {
		sc := hrwScore(domain, n.ID)
		if sc > bestScore || (sc == bestScore && n.ID < best.ID) {
			best, bestScore = n, sc
		}
	}
	return best
}

// hrwScore is the rendezvous weight for (domain, nodeID): the first 8 bytes of SHA-256 over
// "domain\x00nodeID" as a uint64. SHA-256 gives a good uniform distribution (balance) and is stable
// across brain restarts (deterministic — no seed). The hash function is a DIAL (§3.2); SHA-256 is a
// safe, dependency-free default.
func hrwScore(domain, nodeID string) uint64 {
	h := sha256.Sum256([]byte(domain + "\x00" + nodeID))
	return binary.BigEndian.Uint64(h[:8])
}

// eligibleIDs is a small helper for tests/metrics: the sorted candidate IDs.
func eligibleIDs(candidates []Node) []string {
	ids := make([]string, 0, len(candidates))
	for _, n := range candidates {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

// compile-time: both new strategies satisfy the UNCHANGED Strategy interface (Door 5).
var (
	_ Strategy = StickyStrategy{}
	_ Strategy = LeastInflightStrategy{}
)
