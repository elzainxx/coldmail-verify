package audit

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// These tests prove the minimal scheduled re-audit + auto-quarantine state machine LOCALLY against
// the simulated audit surface (node.SimAuditSurface) — green promotes, red quarantines, N greens
// recover, and the GAP-24 green-on-wrong-IP does NOT promote. No real box, no HTTP. Only the real
// IP values (real PTR propagation, real egress-25) are PENDING-REAL-BOX; the STATE MACHINE is done.

const (
	schedEgressIP = "203.0.113.10"
	schedPrivIP   = "10.0.0.5"
	schedURL      = "http://box1"
)

func schedSetup(t *testing.T, recoverGreens int) (*Scheduler, *store.MemStore, *node.SimAuditSurface) {
	t.Helper()
	mem := store.NewMem(nil)
	// Register the node as provisioning with egress-25 already proven (§3.4 smoke test done).
	if err := mem.UpsertNodeState(context.Background(), "n1", schedURL, "provisioning", true); err != nil {
		t.Fatalf("register: %v", err)
	}
	sim := node.NewSimAuditSurface()
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	s := NewScheduler(Deps{Client: sim, Store: mem, Now: func() time.Time { return fixedNow }}, recoverGreens)
	return s, mem, sim
}

func nodeRef() NodeRef {
	return NodeRef{ID: "n1", BaseURL: schedURL, Token: "tok", PublicEgressIP: schedEgressIP}
}

func TestScheduler_GreenPromotesProvisioningToActive(t *testing.T) {
	s, mem, sim := schedSetup(t, 3)
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	out := s.AuditNode(context.Background(), nodeRef())
	if out.Err != nil {
		t.Fatalf("audit: %v", out.Err)
	}
	if out.Status != StatusGreen || out.To != LifecycleActive {
		t.Fatalf("green ∧ port25 → active, got status=%s to=%s", out.Status, out.To)
	}
	// The audit history row was appended (append-only), and the summary reflects green.
	if len(mem.NodeAudits) != 1 {
		t.Fatalf("one audit-history row expected, got %d", len(mem.NodeAudits))
	}
	h, _, _ := mem.GetNodeHealth(context.Background(), "n1")
	if h.Status != "active" || h.AuditStatus != "green" || h.LastAuditHostIP != schedEgressIP {
		t.Fatalf("health summary must be active/green on egress IP, got %+v", h)
	}
}

func TestScheduler_RedQuarantinesImmediately(t *testing.T) {
	s, mem, sim := schedSetup(t, 3)
	// First go active.
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	s.AuditNode(context.Background(), nodeRef())
	// Now PTR breaks → red → quarantine on the FIRST red (fail-safe, OQ-P3-7).
	sim.Set(schedURL, node.SimPTRWarning(schedEgressIP, "2026-07-17T12:05:00Z"))
	out := s.AuditNode(context.Background(), nodeRef())
	if out.Status != StatusRed || out.To != LifecycleQuarantined {
		t.Fatalf("first red must quarantine, got status=%s to=%s", out.Status, out.To)
	}
	if len(mem.NodeAudits) != 2 {
		t.Fatalf("two history rows expected (green then red), got %d", len(mem.NodeAudits))
	}
	h, _, _ := mem.GetNodeHealth(context.Background(), "n1")
	if h.Status != "quarantined" {
		t.Fatalf("node must be quarantined after a red, got %s", h.Status)
	}
}

func TestScheduler_RecoverRequiresNGreens(t *testing.T) {
	s, mem, sim := schedSetup(t, 3)
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	s.AuditNode(context.Background(), nodeRef()) // → active
	sim.Set(schedURL, node.SimDNSWarning(schedEgressIP, "2026-07-17T12:05:00Z"))
	s.AuditNode(context.Background(), nodeRef()) // → quarantined (streak reset)

	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:10:00Z"))
	if out := s.AuditNode(context.Background(), nodeRef()); out.To != LifecycleQuarantined {
		t.Fatalf("green #1 must stay quarantined (need 3), got %s", out.To)
	}
	if out := s.AuditNode(context.Background(), nodeRef()); out.To != LifecycleQuarantined {
		t.Fatalf("green #2 must stay quarantined, got %s", out.To)
	}
	out := s.AuditNode(context.Background(), nodeRef())
	if out.To != LifecycleActive {
		t.Fatalf("green #3 must recover to active, got %s", out.To)
	}
	_ = mem
}

func TestScheduler_GreenOnWrongIP_DoesNotPromote_GAP24(t *testing.T) {
	// The GAP-24 trap in the scheduler: the box reports GREEN but on its PRIVATE IP (multi-homed).
	// The audit is recorded (history is truth) but the transition treats it as RED — the node must
	// NOT promote to active on a green whose IP doesn't match the expected public egress IP.
	s, mem, sim := schedSetup(t, 3)
	sim.Set(schedURL, node.SimMultiHomedMismatch(schedPrivIP, "2026-07-17T12:00:00Z"))
	out := s.AuditNode(context.Background(), nodeRef())
	if !out.EgressMismatch {
		t.Fatal("GAP-24: a green on a non-egress IP must be flagged EgressMismatch")
	}
	if out.To == LifecycleActive {
		t.Fatal("GAP-24: a green on the WRONG IP must NOT promote to active (silent-poisoning trap)")
	}
	h, _, _ := mem.GetNodeHealth(context.Background(), "n1")
	if h.Status == "active" {
		t.Fatalf("node must not be active after a wrong-IP green, got %s", h.Status)
	}
	// The audit was still recorded (history is truth — an operator sees the mismatch).
	if len(mem.NodeAudits) != 1 || mem.NodeAudits[0].CurrentHostIP != schedPrivIP {
		t.Fatalf("the mismatched audit must be recorded verbatim, got %+v", mem.NodeAudits)
	}
}

func TestScheduler_FetchFailureRecordsNothingAndLeavesState(t *testing.T) {
	s, mem, sim := schedSetup(t, 3)
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	s.AuditNode(context.Background(), nodeRef()) // → active
	// Box becomes unreachable.
	sim.SetError(schedURL, &node.TransportError{Err: context.DeadlineExceeded})
	out := s.AuditNode(context.Background(), nodeRef())
	if out.Err == nil {
		t.Fatal("an unreachable box must return a fetch error (not a fabricated green)")
	}
	// No new history row; state unchanged (still active — the gate's freshness conjunct excludes it
	// once stale, harden-toward-unknown, but the scheduler doesn't invent a red from a fetch failure).
	if len(mem.NodeAudits) != 1 {
		t.Fatalf("a fetch failure must NOT append a history row, got %d", len(mem.NodeAudits))
	}
	h, _, _ := mem.GetNodeHealth(context.Background(), "n1")
	if h.Status != "active" {
		t.Fatalf("a fetch failure must leave state as-is, got %s", h.Status)
	}
}

func TestScheduler_AuditAllSweep(t *testing.T) {
	s, _, sim := schedSetup(t, 1)
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	outs := s.AuditAll(context.Background(), []NodeRef{nodeRef()})
	if len(outs) != 1 || outs[0].To != LifecycleActive {
		t.Fatalf("sweep should audit each node, got %+v", outs)
	}
}
