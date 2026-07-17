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
