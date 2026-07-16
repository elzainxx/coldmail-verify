// Package router implements Door 5's node-LIST discipline: the brain talks to a
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

	"github.com/mnfi/codmailling/brain/internal/config"
)

// ErrNoEligibleNode is returned when the candidate list is empty. Callers degrade to a
// cheap verdict — NEVER to invalid (harden-toward-unknown; P3 OQ-P3-6). At Phase 0 the
// list is empty by design, so this is the expected path.
var ErrNoEligibleNode = errors.New("router: no eligible node")

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
