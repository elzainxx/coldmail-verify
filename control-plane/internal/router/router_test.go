package router

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/audit"
	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// fakeHealth is a HealthSource backed by a map (the simulated audit surface at the store layer).
type fakeHealth struct {
	byID map[string]store.NodeHealth
}

func (f fakeHealth) GetNodeHealth(_ context.Context, nodeID string) (store.NodeHealth, bool, error) {
	h, ok := f.byID[nodeID]
	return h, ok, nil
}

var t0 = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func fc() SMTPFilterConfig {
	return SMTPFilterConfig{MaxAuditAge: 2 * time.Hour, Now: func() time.Time { return t0 }}
}

// eligibleNode + its matching green health = the routable baseline.
func eligibleSetup() (*Registry, fakeHealth) {
	nodes := []Node{{ID: "n1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"}}
	reg := New(nodes, nil)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"n1": {NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
			LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
	}}
	return reg, h
}

func TestSMTPRoute_GreenNodeRoutes(t *testing.T) {
	reg, h := eligibleSetup()
	res := reg.SMTPRoute(context.Background(), Plan{Domain: "acme.com"}, h, fc())
	if res.Err != nil {
		t.Fatalf("green eligible node must route, got err=%v (perNode=%v)", res.Err, res.PerNode)
	}
	if res.Node.ID != "n1" {
		t.Fatalf("expected n1, got %q", res.Node.ID)
	}
	if res.SkipToken != "" {
		t.Fatalf("a routed node must have no skip token, got %q", res.SkipToken)
	}
}

func TestSMTPRoute_RedNodeHardBlocked(t *testing.T) {
	reg, h := eligibleSetup()
	n := h.byID["n1"]
	n.AuditStatus = "red"
	h.byID["n1"] = n
	res := reg.SMTPRoute(context.Background(), Plan{}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("red node must be excluded → no eligible node, got err=%v", res.Err)
	}
	if res.SkipToken != "audit_red" {
		t.Fatalf("a red-audit exclusion must yield token audit_red, got %q", res.SkipToken)
	}
	if res.PerNode["n1"] != audit.SkipAuditRed {
		t.Fatalf("per-node reason should be SkipAuditRed, got %q", res.PerNode["n1"])
	}
}

func TestSMTPRoute_Port25ClosedExcluded_GAP15(t *testing.T) {
	reg, h := eligibleSetup()
	n := h.byID["n1"]
	n.Port25Open = false // audit-green but egress blocked
	h.byID["n1"] = n
	res := reg.SMTPRoute(context.Background(), Plan{}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("port25-closed node must be excluded (GAP-15), got err=%v", res.Err)
	}
	if res.SkipToken != "audit_red" || res.PerNode["n1"] != audit.SkipPort25Closed {
		t.Fatalf("GAP-15 exclusion: want token=audit_red reason=port25_closed, got token=%q reason=%q", res.SkipToken, res.PerNode["n1"])
	}
}

func TestSMTPRoute_EgressMismatchExcluded_GAP24(t *testing.T) {
	// The GAP-24 trap at the routing layer: the box audited green on its PRIVATE IP, but its
	// expected public egress IP differs → excluded. Proven locally against the simulated surface.
	reg, h := eligibleSetup()
	n := h.byID["n1"]
	n.LastAuditHostIP = "10.0.0.5" // audit self-detected the private interface
	h.byID["n1"] = n               // node's PublicEgressIP is still 203.0.113.10
	res := reg.SMTPRoute(context.Background(), Plan{}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("egress-mismatch node must be excluded (GAP-24), got err=%v", res.Err)
	}
	if res.PerNode["n1"] != audit.SkipEgressMismatch {
		t.Fatalf("GAP-24: per-node reason must be SkipEgressMismatch, got %q", res.PerNode["n1"])
	}
	if res.SkipToken != "audit_red" {
		t.Fatalf("GAP-24 exclusion is a health failure → token audit_red, got %q", res.SkipToken)
	}
}

func TestSMTPRoute_EmptyListNoEligibleNode(t *testing.T) {
	reg := New(nil, nil)
	res := reg.SMTPRoute(context.Background(), Plan{}, fakeHealth{byID: map[string]store.NodeHealth{}}, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("empty list must be no eligible node, got %v", res.Err)
	}
	if res.SkipToken != "no_eligible_node" {
		t.Fatalf("empty list is a capacity gap → no_eligible_node, got %q", res.SkipToken)
	}
}

func TestSMTPRoute_ProvisioningIsCapacityGap(t *testing.T) {
	// A provisioning node (never promoted) is a capacity gap → no_eligible_node, NOT audit_red.
	nodes := []Node{{ID: "n1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"}}
	reg := New(nodes, nil)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"n1": {NodeID: "n1", Status: "provisioning", AuditStatus: "green", Port25Open: false,
			LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
	}}
	res := reg.SMTPRoute(context.Background(), Plan{}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("provisioning node not eligible, got %v", res.Err)
	}
	if res.SkipToken != "no_eligible_node" {
		t.Fatalf("provisioning is a capacity gap → no_eligible_node, got %q (perNode=%v)", res.SkipToken, res.PerNode)
	}
}

func TestSMTPRoute_DisabledNodeExcluded(t *testing.T) {
	nodes := []Node{{ID: "n1", BaseURL: "http://box1", Enabled: false, PublicEgressIP: "203.0.113.10"}}
	reg := New(nodes, nil)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"n1": {NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
			LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
	}}
	res := reg.SMTPRoute(context.Background(), Plan{}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("disabled node must be excluded even when healthy, got %v", res.Err)
	}
	if res.PerNode["n1"] != audit.SkipNotActive {
		t.Fatalf("disabled → SkipNotActive, got %q", res.PerNode["n1"])
	}
}

func TestSMTPRoute_NilHealthFailsClosed(t *testing.T) {
	nodes := []Node{{ID: "n1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"}}
	reg := New(nodes, nil)
	res := reg.SMTPRoute(context.Background(), Plan{}, nil, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("nil health must fail closed (never :smtp on unknown health), got %v", res.Err)
	}
}

// A `recovering` node is admitted ONLY as the metered canary (§5.4/GAP-30): with AllowRecovering, it
// becomes a candidate + IsCanary is set (the caller meters it via the low canary cap). Without the
// flag, it is excluded (the P3 behavior).
func TestSMTPRoute_RecoveringIsCanaryOnlyWhenAllowed(t *testing.T) {
	nodes := []Node{{ID: "n1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"}}
	reg := New(nodes, nil)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"n1": {NodeID: "n1", Status: "recovering", AuditStatus: "green", Port25Open: true,
			LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
	}}
	// Without AllowRecovering → excluded (not active).
	res := reg.SMTPRoute(context.Background(), Plan{Domain: "acme.com"}, h, fc())
	if res.Err != ErrNoEligibleNode {
		t.Fatalf("a recovering node must be excluded by default, got %v", res.Err)
	}
	// With AllowRecovering → admitted as the canary.
	f := fc()
	f.AllowRecovering = true
	res = reg.SMTPRoute(context.Background(), Plan{Domain: "acme.com"}, h, f)
	if res.Err != nil || res.Node.ID != "n1" {
		t.Fatalf("a recovering node must be admitted as canary when allowed, got node=%q err=%v", res.Node.ID, res.Err)
	}
	if !res.IsCanary {
		t.Fatal("a recovering node picked as the canary must set IsCanary (so the caller meters it via the low cap)")
	}
}

// An ACTIVE node is preferred over a recovering canary: with both eligible, the active node is
// picked and IsCanary is false (the canary is the fallback only, never steals normal traffic).
func TestSMTPRoute_ActivePreferredOverRecoveringCanary(t *testing.T) {
	nodes := []Node{
		{ID: "active1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"},
		{ID: "recov1", BaseURL: "http://box2", Enabled: true, PublicEgressIP: "203.0.113.11"},
	}
	reg := New(nodes, nil)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"active1": {NodeID: "active1", Status: "active", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
		"recov1":  {NodeID: "recov1", Status: "recovering", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.11"},
	}}
	f := fc()
	f.AllowRecovering = true
	// Try many domains: every pick must land on the ACTIVE node (recovering is fallback-only).
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		res := reg.SMTPRoute(context.Background(), Plan{Domain: d}, h, f)
		if res.Err != nil || res.Node.ID != "active1" {
			t.Fatalf("domain %s must route to the ACTIVE node, not the canary, got node=%q err=%v", d, res.Node.ID, res.Err)
		}
		if res.IsCanary {
			t.Fatalf("an active pick must not be flagged canary (domain %s)", d)
		}
	}
}

// Rate saturation via the sticky headroom oracle must emit smtp_skipped_reason="rate_limited", NOT
// "no_eligible_node" (the round-2 review finding). Two healthy active nodes, both denied headroom.
func TestSMTPRoute_AllRateCapped_EmitsRateLimited(t *testing.T) {
	nodes := []Node{
		{ID: "n1", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"},
		{ID: "n2", BaseURL: "http://box2", Enabled: true, PublicEgressIP: "203.0.113.11"},
	}
	// A sticky strategy whose headroom oracle denies EVERY node → all-capped → ErrRateSaturated.
	sticky := NewSticky(func(_ context.Context, _ string, _ Plan) bool { return false }, nil)
	reg := New(nodes, sticky)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"n1": {NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
		"n2": {NodeID: "n2", Status: "active", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.11"},
	}}
	res := reg.SMTPRoute(context.Background(), Plan{Domain: "acme.com", Provider: "other"}, h, fc())
	if res.Err != ErrRateSaturated {
		t.Fatalf("all-rate-capped must return ErrRateSaturated, got %v", res.Err)
	}
	if res.SkipToken != "rate_limited" {
		t.Fatalf("rate saturation must emit smtp_skipped_reason=rate_limited, got %q", res.SkipToken)
	}
}

// Even when a PEER is excluded for a health reason (stale) while the remaining active node is
// rate-capped, the token is rate_limited (the pick error wins over the per-node exclusion mapping) —
// the second variant of the round-2 finding.
func TestSMTPRoute_RateCappedWithStalePeer_StillRateLimited(t *testing.T) {
	nodes := []Node{
		{ID: "healthy", BaseURL: "http://box1", Enabled: true, PublicEgressIP: "203.0.113.10"},
		{ID: "stale", BaseURL: "http://box2", Enabled: true, PublicEgressIP: "203.0.113.11"},
	}
	sticky := NewSticky(func(_ context.Context, _ string, _ Plan) bool { return false }, nil) // healthy node is rate-capped
	reg := New(nodes, sticky)
	h := fakeHealth{byID: map[string]store.NodeHealth{
		"healthy": {NodeID: "healthy", Status: "active", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-time.Minute), LastAuditHostIP: "203.0.113.10"},
		// stale: audited long ago → SkipAuditStale in perNode (would map skipTokenFor → audit_red).
		"stale": {NodeID: "stale", Status: "active", AuditStatus: "green", Port25Open: true, LastAuditAt: t0.Add(-10 * time.Hour), LastAuditHostIP: "203.0.113.11"},
	}}
	res := reg.SMTPRoute(context.Background(), Plan{Domain: "acme.com", Provider: "other"}, h, fc())
	if res.SkipToken != "rate_limited" {
		t.Fatalf("a rate-capped active node (with a stale peer) must still emit rate_limited, not audit_red, got %q", res.SkipToken)
	}
}

// Route (the P0/bootstrap enabled-only path) must remain unchanged — Door 5 seam intact.
func TestRoute_UnchangedEnabledOnly(t *testing.T) {
	reg := New([]Node{{ID: "n1", BaseURL: "http://box1", Enabled: true}}, nil)
	n, err := reg.Route(context.Background(), Plan{})
	if err != nil || n.ID != "n1" {
		t.Fatalf("Route should still pick the enabled node, got n=%q err=%v", n.ID, err)
	}
}

// Guard: the config alias is what the router routes over (Door 5 list shape).
var _ = config.NodeDescriptor{}
