package router

import (
	"context"
	"fmt"
	"testing"
)

func nodesN(n int) []Node {
	out := make([]Node, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Node{ID: fmt.Sprintf("n%d", i), BaseURL: fmt.Sprintf("http://box%d", i), Enabled: true})
	}
	return out
}

// The anti-multiplication property (§3.1, the phase's spine): a given domain ALWAYS routes to the
// SAME node (concentration), across repeated picks. If it scattered, a per-provider cap would be
// theatre. Proven for many domains over a 3-node pool.
func TestSticky_DomainConcentratesOnOneNode(t *testing.T) {
	s := NewSticky(nil, nil) // pure sticky affinity (no headroom/inflight oracle)
	pool := nodesN(3)
	domains := []string{"acme.com", "globex.com", "initech.com", "umbrella.co", "hooli.com", "piedpiper.com"}
	for _, d := range domains {
		first, err := s.Pick(context.Background(), Plan{Domain: d}, pool)
		if err != nil {
			t.Fatalf("pick %s: %v", d, err)
		}
		// Every subsequent pick for the same domain must land on the same node (stickiness).
		for i := 0; i < 20; i++ {
			got, _ := s.Pick(context.Background(), Plan{Domain: d}, pool)
			if got.ID != first.ID {
				t.Fatalf("domain %s scattered: pick %d went to %s, not the sticky home %s", d, i, got.ID, first.ID)
			}
		}
	}
}

// HRW distributes DIFFERENT domains across the pool (balance) — not everything piled on one node.
// Over many domains and 3 nodes, every node should be some domain's home.
func TestSticky_HRWDistributesAcrossPool(t *testing.T) {
	s := NewSticky(nil, nil)
	pool := nodesN(3)
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		d := fmt.Sprintf("domain-%d.example", i)
		n, _ := s.Pick(context.Background(), Plan{Domain: d}, pool)
		seen[n.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("HRW must spread domains across all 3 nodes, got homes on %d nodes (%v)", len(seen), seen)
	}
	// Rough balance: no node should hoard >70% (HRW is uniform; this is a very loose sanity bound).
	for id, c := range seen {
		if c > 210 {
			t.Fatalf("node %s got %d/300 domains — HRW distribution looks broken", id, c)
		}
	}
}

// HRW minimal reshuffle (§3.2 / §5.6): when a node leaves the eligible set (quarantine), ONLY the
// domains homed on THAT node rehash; every other domain stays put. This is the "re-route on
// quarantine without disturbing the rest" property the phase must prove.
func TestSticky_MinimalReshuffleOnMembershipChange(t *testing.T) {
	s := NewSticky(nil, nil)
	full := nodesN(3)
	// Map every domain to its home in the full pool.
	homeFull := map[string]string{}
	var domains []string
	for i := 0; i < 500; i++ {
		d := fmt.Sprintf("d-%d.example", i)
		domains = append(domains, d)
		n, _ := s.Pick(context.Background(), Plan{Domain: d}, full)
		homeFull[d] = n.ID
	}
	// Quarantine n3 → the eligible set is {n1, n2}. Re-pick every domain.
	reduced := []Node{full[0], full[1]} // n1, n2
	moved, stayed := 0, 0
	for _, d := range domains {
		n, _ := s.Pick(context.Background(), Plan{Domain: d}, reduced)
		if homeFull[d] == "n3" {
			// A domain that was homed on the departed node MUST move (to n1 or n2).
			if n.ID == "n3" {
				t.Fatalf("domain %s still homed on the departed n3", d)
			}
			moved++
		} else {
			// A domain NOT homed on n3 must stay exactly where it was (minimal reshuffle).
			if n.ID != homeFull[d] {
				t.Fatalf("domain %s moved from %s to %s despite its home not leaving — HRW not minimal", d, homeFull[d], n.ID)
			}
			stayed++
		}
	}
	if moved == 0 || stayed == 0 {
		t.Fatalf("expected some moved (n3's domains) and some stayed, got moved=%d stayed=%d", moved, stayed)
	}
	// The only domains that moved are exactly n3's former domains — nothing else shifted.
	t.Logf("membership change n3→gone: %d domains rehashed (n3's), %d stayed put", moved, stayed)
}

// Sticky-first, least-inflight fallback ordering (§3.3/§3.4): when the sticky home has NO headroom
// (rate/warmup capped), the check falls through to the least-inflight eligible peer WITH headroom —
// never to the capped home, never degrade while a peer can serve.
func TestSticky_FallsBackToLeastInflightWhenHomeCapped(t *testing.T) {
	pool := nodesN(3)
	// Find the sticky home for a probe domain, then deny IT headroom and give peers headroom.
	base := NewSticky(nil, nil)
	home, _ := base.Pick(context.Background(), Plan{Domain: "probe.example"}, pool)

	inflight := map[string]int{"n1": 5, "n2": 5, "n3": 5}
	// Make the two non-home nodes differ in inflight so least-inflight has a clear winner.
	var peerLow, peerHigh string
	for _, n := range pool {
		if n.ID == home.ID {
			continue
		}
		if peerLow == "" {
			peerLow = n.ID
		} else {
			peerHigh = n.ID
		}
	}
	inflight[peerLow] = 1  // the least-inflight peer
	inflight[peerHigh] = 9 // the busier peer

	s := NewSticky(
		func(_ context.Context, id string, _ Plan) bool { return id != home.ID }, // home capped, peers ok
		func(id string, _ Plan) int { return inflight[id] },
	)
	got, err := s.Pick(context.Background(), Plan{Domain: "probe.example"}, pool)
	if err != nil {
		t.Fatalf("fallback pick: %v", err)
	}
	if got.ID == home.ID {
		t.Fatalf("capped home must not be picked, got the home %s", got.ID)
	}
	if got.ID != peerLow {
		t.Fatalf("fallback must pick the least-inflight peer %s, got %s", peerLow, got.ID)
	}
}

// When the sticky home HAS headroom, it is picked even if a peer has fewer in-flight (sticky first —
// concentration is the point; least-inflight is only the fallback, §3.3).
func TestSticky_HomeWithHeadroomBeatsLowerInflightPeer(t *testing.T) {
	pool := nodesN(3)
	base := NewSticky(nil, nil)
	home, _ := base.Pick(context.Background(), Plan{Domain: "keep.example"}, pool)
	s := NewSticky(
		func(_ context.Context, _ string, _ Plan) bool { return true }, // everyone has headroom
		func(id string, _ Plan) int {
			if id == home.ID {
				return 100 // home is busiest...
			}
			return 0
		},
	)
	got, _ := s.Pick(context.Background(), Plan{Domain: "keep.example"}, pool)
	if got.ID != home.ID {
		t.Fatalf("sticky-first: a home WITH headroom must win over a lower-inflight peer, got %s want %s", got.ID, home.ID)
	}
}

// Every eligible node capped → degrade with ErrRateSaturated (DISTINCT from ErrNoEligibleNode) so
// the router emits smtp_skipped_reason="rate_limited" (§4.6), not "no_eligible_node" (the round-2
// review finding). Never overflow onto a capped node.
func TestSticky_AllCappedDegradesRateSaturated(t *testing.T) {
	s := NewSticky(func(_ context.Context, _ string, _ Plan) bool { return false }, nil) // nobody has headroom
	_, err := s.Pick(context.Background(), Plan{Domain: "x.example"}, nodesN(3))
	if err != ErrRateSaturated {
		t.Fatalf("all-capped must degrade to ErrRateSaturated (→ rate_limited), got %v", err)
	}
}

// N=1 sticky is a trivial pass-through (the home is the only node) — proves the strategy is
// N-agnostic (the same code serves N=1 and N=2, GAP-31 / node #2 = +1 config line).
func TestSticky_N1PassThrough(t *testing.T) {
	s := NewSticky(nil, nil)
	got, err := s.Pick(context.Background(), Plan{Domain: "solo.example"}, nodesN(1))
	if err != nil || got.ID != "n1" {
		t.Fatalf("N=1 must pass through to the only node, got %s err=%v", got.ID, err)
	}
}

// The standalone least-inflight strategy ranks by in-flight (a policy could select it directly).
func TestLeastInflight_PicksFewestInflight(t *testing.T) {
	inflight := map[string]int{"n1": 3, "n2": 1, "n3": 2}
	s := LeastInflightStrategy{Inflight: func(id string, _ Plan) int { return inflight[id] }}
	got, _ := s.Pick(context.Background(), Plan{Domain: "d.example"}, nodesN(3))
	if got.ID != "n2" {
		t.Fatalf("least-inflight must pick n2 (1 inflight), got %s", got.ID)
	}
}

func TestEligibleIDsHelper(t *testing.T) {
	if got := eligibleIDs(nodesN(2)); len(got) != 2 || got[0] != "n1" || got[1] != "n2" {
		t.Fatalf("eligibleIDs sorted: %v", got)
	}
}
