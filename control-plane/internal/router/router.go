// Package router implements Door 5's node-LIST discipline: the control-plane talks to a
// LIST of nodes (even length 0/1) through a pluggable Strategy interface — there is
// NO code path that references a single hardcoded endpoint. (PROJECT_RULES.md Door 5;
// P1 §3.5.1/§5.4.)
//
// Phase 0 ships the trivial strategy behind the interface. P4 adds sticky-by-domain +
// least-inflight as NEW Strategy implementations with NO interface change — that is the
// whole point of locking the interface now (P1 §5.4 SEAM). Adding node #2 must be a
// config-list edit, never a code change (GAP-31).
package router

import (
	"context"
	"errors"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/audit"
	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// ErrNoEligibleNode is returned when the candidate list is empty. Callers degrade to a
// cheap verdict — NEVER to invalid (harden-toward-unknown; P3 OQ-P3-6). At Phase 0 the
// list is empty by design, so this is the expected path.
var ErrNoEligibleNode = errors.New("router: no eligible node")

// ErrRateSaturated is returned by a Strategy when candidates DID exist and were eligible, but ALL of
// them lacked per-provider RATE headroom (§4.6). It is distinct from ErrNoEligibleNode so the caller
// emits the correct smtp_skipped_reason="rate_limited" (a capacity gap that clears when the cap frees)
// rather than "no_eligible_node" (a health/routing gap) — the two degrade the same way (cheap,
// provisional, non-cacheable, never invalid) but carry different diagnostics/alerting (§8.2). Found by
// the Phase-4 round-2 review: without this, a pure rate-saturation degrade was mislabeled.
var ErrRateSaturated = errors.New("router: all eligible nodes rate-saturated")

// Node is the routable view of a configured node.
type Node = config.NodeDescriptor

// Plan carries the per-request routing inputs a Strategy may use (domain for stickiness,
// target provider for rate headroom). Fields are added additively as later phases need them.
type Plan struct {
	Domain   string // sticky key (P4); Door 3 note — routing NEVER touches email_hash/local-part
	Provider string // mx_provider bucket (P4 rate limiter)
}

// Strategy is the LOCKED interface (P1 §5.4). Pick chooses one node from the eligible
// candidates, or returns ErrNoEligibleNode. Later phases add implementations, not methods.
type Strategy interface {
	Pick(ctx context.Context, plan Plan, candidates []Node) (Node, error)
}

// SingleStrategy is the Phase-0/N=1 strategy: pick the first eligible candidate. It is a
// real implementation of the interface (not a hardcoded endpoint) — proving the seam works.
type SingleStrategy struct{}

func (SingleStrategy) Pick(_ context.Context, _ Plan, candidates []Node) (Node, error) {
	for _, n := range candidates {
		if n.Enabled {
			return n, nil
		}
	}
	return Node{}, ErrNoEligibleNode
}

// Registry holds the node LIST (from config; config-list is authoritative — P1 OQ-5) and
// the active Strategy. It is the only thing that knows "the nodes" — callers ask it to route.
type Registry struct {
	nodes    []Node
	strategy Strategy
}

// New builds a Registry from the config node list + a strategy. A nil strategy defaults
// to SingleStrategy so the seam is always exercised.
func New(nodes []Node, s Strategy) *Registry {
	if s == nil {
		s = SingleStrategy{}
	}
	return &Registry{nodes: nodes, strategy: s}
}

// Route applies the eligibility filter then the strategy. Phase 0's filter is just
// Enabled; P3 adds (green ∧ port25_open ∧ fresh), P4 adds (active ∧ warmup/rate headroom)
// — all ADDED to this same filter, never a new mechanism (P4 §3.3).
func (r *Registry) Route(ctx context.Context, plan Plan) (Node, error) {
	candidates := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if n.Enabled { // P0 filter; later conjuncts added here.
			candidates = append(candidates, n)
		}
	}
	return r.strategy.Pick(ctx, plan, candidates)
}

// Len reports the configured node count (for readiness/metrics — proves list shape).
func (r *Registry) Len() int { return len(r.nodes) }

// ── Phase 3: the :smtp candidacy filter (§4.4, the phase's spine) ───────────────────────────
// This is the ONE meaningful brain behavior Phase 3 adds (P3 §2): a candidacy filter, NOT new
// routing logic. The Strategy interface is UNCHANGED (Door 5 payoff — P4 adds sticky/least-inflight
// as new Strategy impls, no interface churn). SMTPRoute layers the LOCKED §4.4 eligibility rule
// (active ∧ green ∧ port25_open ∧ fresh ∧ GAP-24 egress-IP-match) on top of the enabled filter,
// consulting per-node health from the store, then defers node SELECTION to the same Strategy.

// HealthSource is the narrow read the SMTP filter needs (satisfied by store.Store). Kept minimal so
// the router depends only on the health read, and tests can inject a fake without a DB.
type HealthSource interface {
	GetNodeHealth(ctx context.Context, nodeID string) (store.NodeHealth, bool, error)
}

// SMTPFilterConfig carries the §4.5 dials the eligibility rule needs: the staleness window and the
// consecutive-green recover count are set once at wire time (config), not per request.
type SMTPFilterConfig struct {
	MaxAuditAge time.Duration // §4.5 staleness window; <=0 disables the freshness conjunct
	Now         func() time.Time
	// AllowRecovering admits `recovering` (canary) nodes to the candidate set (P4 §5.4/GAP-30). They
	// must still pass every other conjunct (green ∧ port25 ∧ egress-match ∧ fresh); the CALLER meters
	// them via the low canary warmup cap so a recovering node gets only "a few checks/window" — the
	// metered canary that lets it accrue REAL block-rate samples to prove recovery (without this, a
	// recovering node gets zero traffic → zero samples → can never recover; the Phase-4 review finding).
	AllowRecovering bool
}

// SMTPRouteResult reports the routing outcome for the SMTP stage. When Node is valid, SMTP runs on
// it. When it is not (Err==ErrNoEligibleNode), the caller degrades to the cheap verdict and sets
// signals.smtp_skipped_reason — SkipToken carries the frozen §8 token (audit_red vs no_eligible_node),
// and PerNode records why each configured node was excluded (diagnosis/logging, never a fabricated
// verdict). NEVER invalid (OQ-P3-6).
type SMTPRouteResult struct {
	Node      Node
	Err       error
	SkipToken string                      // frozen §8 signals.smtp_skipped_reason ("" when a node was picked)
	PerNode   map[string]audit.SkipReason // node_id -> finer internal reason (diagnosis)
	IsCanary  bool                        // the picked node is `recovering` → the caller meters it via the LOW canary cap (§5.4)
}

// SMTPRoute applies the §4.4 eligibility filter then the Strategy. For each ENABLED node it reads
// health from the store and evaluates audit.Eligibility (active ∧ green ∧ port25_open ∧ fresh ∧
// GAP-24 egress-match). Eligible nodes go to the Strategy for selection; if NONE is eligible it
// returns ErrNoEligibleNode with the frozen skip token. A node with no health row yet (never
// mirrored/audited) is treated as never-audited → not eligible (fail-safe).
//
// The skip token is: "audit_red" when at least one node was excluded for a health reason (the §8
// node-health token; finer reasons live in PerNode), else "no_eligible_node" (the capacity-gap
// token — the list is empty or all nodes are provisioning/disabled). PB1: the caller uses the token
// to decide cacheability (no_eligible_node/audit_red/warmup_cap are provisional → non-cacheable).
func (r *Registry) SMTPRoute(ctx context.Context, plan Plan, health HealthSource, fc SMTPFilterConfig) SMTPRouteResult {
	now := time.Now
	if fc.Now != nil {
		now = fc.Now
	}
	perNode := map[string]audit.SkipReason{}
	// Two candidate classes (§5.4): ACTIVE nodes serve normal traffic; RECOVERING nodes serve ONLY
	// the metered canary. We prefer active — a recovering node must not steal traffic from a healthy
	// peer — and fall to recovering only when NO active node can take the check. This keeps the canary
	// a genuine trickle (it gets a check only when it's the last resort for that domain), while still
	// letting it accrue REAL block-rate samples to prove recovery (the Phase-4 review finding).
	active := make([]Node, 0, len(r.nodes))
	recovering := make([]Node, 0, len(r.nodes))
	recoveringStatus := map[string]bool{}

	for _, n := range r.nodes {
		if !n.Enabled {
			perNode[n.ID] = audit.SkipNotActive // operator switch off (disabled) — a capacity gap, not a health failure
			continue
		}
		if health == nil {
			perNode[n.ID] = audit.SkipNeverAudited
			continue
		}
		h, known, err := health.GetNodeHealth(ctx, n.ID)
		if err != nil || !known {
			// A health-read error or an absent health row must NOT open the gate — fail closed
			// (never :smtp on unknown health). Treated as never-audited (a capacity gap): the node
			// simply isn't in a confirmable state to serve.
			perNode[n.ID] = audit.SkipNeverAudited
			continue
		}
		reason := audit.Eligibility(audit.EligibilityInput{
			Status:           audit.Lifecycle(h.Status),
			AuditStatus:      audit.Status(h.AuditStatus),
			Port25Open:       h.Port25Open,
			LastAuditAt:      h.LastAuditAt,
			LastAuditHostIP:  h.LastAuditHostIP,
			ExpectedEgressIP: n.PublicEgressIP,
			Now:              now(),
			MaxAuditAge:      fc.MaxAuditAge,
			AllowRecovering:  fc.AllowRecovering,
		})
		if reason != audit.SkipNone {
			perNode[n.ID] = reason
			continue
		}
		if h.Status == string(audit.LifecycleRecovering) {
			recovering = append(recovering, n)
			recoveringStatus[n.ID] = true
		} else {
			active = append(active, n)
		}
	}

	// Prefer active; the canary (recovering) is the fallback ONLY when no active node can serve.
	if len(active) > 0 {
		picked, err := r.strategy.Pick(ctx, plan, active)
		if err != nil {
			return SMTPRouteResult{Err: err, SkipToken: skipTokenForErr(err, perNode), PerNode: perNode}
		}
		return SMTPRouteResult{Node: picked, PerNode: perNode}
	}
	if len(recovering) > 0 {
		picked, err := r.strategy.Pick(ctx, plan, recovering)
		if err != nil {
			return SMTPRouteResult{Err: err, SkipToken: skipTokenForErr(err, perNode), PerNode: perNode}
		}
		// The picked node is the metered canary — the caller reserves against the LOW canary cap so
		// the recovering node gets only a trickle (§5.4), not the full ramp.
		return SMTPRouteResult{Node: picked, PerNode: perNode, IsCanary: recoveringStatus[picked.ID]}
	}
	return SMTPRouteResult{Err: ErrNoEligibleNode, SkipToken: skipTokenFor(perNode), PerNode: perNode}
}

// skipTokenForErr maps a Strategy Pick error to the frozen §8 token. ErrRateSaturated (candidates
// existed but ALL were rate-capped) → "rate_limited" (§4.6) — NOT the per-node-exclusion token, which
// would mislabel a pure rate-saturation degrade as no_eligible_node/audit_red (the round-2 review
// finding). Any other error falls back to the per-node exclusion mapping.
func skipTokenForErr(err error, perNode map[string]audit.SkipReason) string {
	if errors.Is(err, ErrRateSaturated) {
		return "rate_limited"
	}
	return skipTokenFor(perNode)
}

// skipTokenFor maps the per-node exclusion reasons to the frozen §8 signals.smtp_skipped_reason
// enum. If ANY node was excluded for a genuine AUDIT/HEALTH failure — red audit, port25-closed
// (GAP-15), stale audit, or egress-IP mismatch (GAP-24) — the token is "audit_red" (a node exists
// but its health failed the gate). Otherwise the exclusions are all capacity gaps (disabled,
// provisioning, never-audited, or an empty list) → "no_eligible_node". Both tokens are PROVISIONAL
// (PB1: non-cacheable — the answer improves once the node recovers). Deterministic (no map-order
// dependence): it's a set-membership test, not a first-wins scan.
func skipTokenFor(perNode map[string]audit.SkipReason) string {
	for _, r := range perNode {
		switch r {
		case audit.SkipAuditRed, audit.SkipPort25Closed, audit.SkipAuditStale, audit.SkipEgressMismatch:
			return "audit_red"
		}
	}
	return "no_eligible_node"
}
