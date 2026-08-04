package queue

import (
	"context"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/router"
)

// TestRetryAffinity_SameDomainSameNode_GAP32 proves the P5 §14/GAP-32 acceptance criterion:
// greylisting is keyed on the sending IP and expects the SAME IP to retry, and P4 sticky routing
// DELIVERS that for free — a retry for the same domain, over an UNCHANGED eligible set, deterministic-
// ally routes to the SAME node/IP as the first attempt. This is a pure property of the sticky
// Strategy (HRW over (domain, eligible-set)); the drainer stores NO last-node routing input (which
// would require changing Strategy.Pick — a Door-5 violation, per the seam analysis). We assert the
// Strategy the drainer uses is stable across attempts, and re-routes only when the home leaves.
func TestRetryAffinity_SameDomainSameNode_GAP32(t *testing.T) {
	ctx := context.Background()
	nodes := []router.Node{
		{ID: "node-1", BaseURL: "http://box1", Enabled: true},
		{ID: "node-2", BaseURL: "http://box2", Enabled: true},
		{ID: "node-3", BaseURL: "http://box3", Enabled: true},
	}
	s := router.NewSticky(nil, nil) // pure sticky affinity (the retry path uses the same Strategy)

	// First attempt + several "retries" for the same domain over the SAME eligible set → same node.
	plan := router.Plan{Domain: "acme.com"}
	first, err := s.Pick(ctx, plan, nodes)
	if err != nil {
		t.Fatalf("first pick: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.Pick(ctx, plan, nodes)
		if err != nil {
			t.Fatalf("retry pick %d: %v", i, err)
		}
		if again.ID != first.ID {
			t.Fatalf("GAP-32 VIOLATED: retry routed to %s, first attempt was %s (same-IP affinity lost)", again.ID, first.ID)
		}
	}

	// When the sticky home is quarantined (leaves the eligible set), the retry re-routes to a
	// DIFFERENT surviving node — the desired behavior (the old IP is gone; accept an honest
	// re-greylist that eventually settles unknown). Only the departed node's domains rehash.
	survivors := make([]router.Node, 0, 2)
	for _, n := range nodes {
		if n.ID != first.ID {
			survivors = append(survivors, n)
		}
	}
	afterQuarantine, err := s.Pick(ctx, plan, survivors)
	if err != nil {
		t.Fatalf("post-quarantine pick: %v", err)
	}
	if afterQuarantine.ID == first.ID {
		t.Fatal("the quarantined home must not be picked from the reduced set")
	}
	// And it is STABLE again on the reduced set (the new home holds across further retries).
	for i := 0; i < 3; i++ {
		again, _ := s.Pick(ctx, plan, survivors)
		if again.ID != afterQuarantine.ID {
			t.Fatalf("post-quarantine home not stable: %s vs %s", again.ID, afterQuarantine.ID)
		}
	}
}

// TestStrategyInterfaceUnchanged_Door5 is a compile-time-ish guard: the drainer routes through the
// UNCHANGED P4 Strategy interface (Pick(ctx, plan, candidates) (Node, error)) — P5 adds NO method.
// If someone "improves" retry affinity by adding a last-node parameter to Pick, this stops
// compiling, flagging the Door-5 violation the seam analysis warned about.
func TestStrategyInterfaceUnchanged_Door5(t *testing.T) {
	var _ router.Strategy = router.StickyStrategy{}
	var _ router.Strategy = router.SingleStrategy{}
	_ = config.NodeDescriptor{} // the node LIST shape is config-driven (GAP-31), unchanged
}
