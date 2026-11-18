package health

// Node SELF-PROMOTION (owner ruling OD-4; migration 0019; Docs/install-and-deploy.md "d.4"). Before it, nothing in the running code moved a node from
// `provisioning` to `active` or set `port25_open`, and every control-plane boot re-registered the node as provisioning with port25_open=false: a human
// had to run SQL after every restart. Now:
//
//   - a node becomes `active` ONLY from a green audit row recorded for it AFTER this boot's registration, on its configured egress IP, with
//     port25_open TAKEN FROM that audit, in one atomic store step that records the audit row's id;
//   - a red audit (or a green on the wrong IP, or a node that does not answer) leaves it `provisioning`, port25_open false: it is NOT quarantined;
//   - a restart re-registers it (provisioning, port25 false) and the next green audit promotes it again: a stale `active` is never trusted.
//
// Every property has its paired control in this file: the green case is the control of each refusal, and each refusal is the control of the green one.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/audit"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/store"
)

const (
	promoNode = "n1"
	promoBase = "http://box-n1"
	promoIP   = "203.0.113.10" // the node's configured egress IP (a documentation address)
)

// promoFixture registers ONE fresh node the way main does (RegisterNode) and wires a watcher over a simulated audit surface and liveness surface.
func promoFixture(t *testing.T) (*Watcher, *store.MemStore, []WatchedNode, *node.SimAuditSurface, *node.SimLivenessSurface) {
	t.Helper()
	mem := store.NewMem(func() time.Time { return wnow })
	if err := mem.RegisterNode(context.Background(), promoNode, promoBase); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	simAudit := node.NewSimAuditSurface()
	simLive := node.NewSimLivenessSurface()
	pool := []WatchedNode{{NodeRef: audit.NodeRef{ID: promoNode, BaseURL: promoBase, Token: "t", PublicEgressIP: promoIP}, VerifierIP: promoIP}}
	sched := audit.NewScheduler(audit.Deps{Client: simAudit, Store: mem, Now: func() time.Time { return wnow }}, 2)
	w := NewWatcher(Deps{
		Audit: sched, Liveness: simLive, BlockRate: mem, Status: mem,
		Now: func() time.Time { return wnow }, Log: func(string, ...any) {},
	}, params(), 30*time.Minute, 5)
	return w, mem, pool, simAudit, simLive
}

func health(t *testing.T, mem *store.MemStore) store.NodeHealth {
	t.Helper()
	h, known, err := mem.GetNodeHealth(context.Background(), promoNode)
	if err != nil || !known {
		t.Fatalf("GetNodeHealth: known=%v err=%v", known, err)
	}
	return h
}

// lastAudit is the newest node_audits row the fixture's watcher recorded.
func lastAudit(t *testing.T, mem *store.MemStore) store.NodeAuditRow {
	t.Helper()
	if len(mem.NodeAudits) == 0 {
		t.Fatal("no audit row was recorded")
	}
	return mem.NodeAudits[len(mem.NodeAudits)-1]
}

// A fresh node and a green audit on its egress IP: ACTIVE, port25_open true, promoted by THAT audit row (its id recorded beside the status).
// This is the control of every refusal below.
func TestPromotion_FreshNodeGreenAudit_BecomesActive_Port25FromTheAudit_RecordedWithTheAuditID(t *testing.T) {
	w, mem, pool, simAudit, _ := promoFixture(t)
	if h := health(t, mem); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
		t.Fatalf("fixture: a registered node must start provisioning, port25_open=false, unpromoted: %+v", h)
	}
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))

	w.Tick(context.Background(), pool)

	h := health(t, mem)
	row := lastAudit(t, mem)
	if h.Status != "active" {
		t.Fatalf("a green audit on the egress IP must promote a fresh node; status = %q", h.Status)
	}
	if !h.Port25Open {
		t.Error("port25_open must be taken from the (green) audit: true")
	}
	if row.AuditStatus != "green" || h.PromotedByAuditID == "" || h.PromotedByAuditID != row.ID {
		t.Errorf("the promotion must name the audit row that caused it: promoted_by_audit_id = %q, newest audit row = %q (%s)", h.PromotedByAuditID, row.ID, row.AuditStatus)
	}
}

// A red audit: the node stays PROVISIONING (not quarantined: it was never serving), port25_open is false, and nothing records a promotion.
func TestPromotion_RedAudit_StaysProvisioning_Port25False_NotQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    node.AuditResult
	}{
		{"ptr warning", node.SimPTRWarning(promoIP, wt)},
		{"dns warning", node.SimDNSWarning(promoIP, wt)},
		{"resolver warning", node.SimResolverWarning(promoIP, wt)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mem, pool, simAudit, _ := promoFixture(t)
			// An operator once set port25_open by hand (the old procedure): a red audit says the node cannot be trusted to send, so it is cleared.
			if err := mem.UpsertNodeState(context.Background(), promoNode, promoBase, "provisioning", true); err != nil {
				t.Fatal(err)
			}
			simAudit.Set(promoBase, tc.r)

			for i := 0; i < 3; i++ { // a red audit repeated is still red: it never quarantines a provisioning node either
				w.Tick(context.Background(), pool)
			}

			h := health(t, mem)
			if h.Status != "provisioning" {
				t.Fatalf("a red audit must leave a fresh node provisioning (never active, never quarantined); status = %q", h.Status)
			}
			if h.Port25Open {
				t.Error("port25_open must be what the red audit says: false")
			}
			if h.PromotedByAuditID != "" {
				t.Errorf("a red audit promoted nothing but promoted_by_audit_id = %q", h.PromotedByAuditID)
			}
			if row := lastAudit(t, mem); row.AuditStatus != "red" {
				t.Errorf("fixture: the audit row must be red, got %q", row.AuditStatus)
			}
		})
	}
}

// A green audit on ANOTHER ip than the node's configured egress IP (GAP-24, ADR-0009: a multi-homed box audited its private interface) does not
// promote. A node that does not answer its liveness probe does not either. Each is the control of the other and of the green case above.
func TestPromotion_GreenOnTheWrongIP_OrANodeThatDoesNotAnswer_DoesNotPromote(t *testing.T) {
	t.Run("green on the wrong ip", func(t *testing.T) {
		w, mem, pool, simAudit, _ := promoFixture(t)
		simAudit.Set(promoBase, node.SimMultiHomedMismatch("10.0.0.5", wt)) // green warnings, wrong IP
		w.Tick(context.Background(), pool)
		if h := health(t, mem); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
			t.Fatalf("a green on the wrong IP must not promote: %+v", h)
		}
	})
	t.Run("a node that does not answer its liveness probe", func(t *testing.T) {
		w, mem, pool, simAudit, simLive := promoFixture(t)
		simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
		simLive.SetDown(promoBase, true)
		w.Tick(context.Background(), pool)
		if h := health(t, mem); h.Status != "provisioning" || h.PromotedByAuditID != "" {
			t.Fatalf("a node that is down must not be promoted on a green audit: %+v", h)
		}
		simLive.SetDown(promoBase, false) // CONTROL: the same audit and a node that answers promotes it
		w.Tick(context.Background(), pool)
		if h := health(t, mem); h.Status != "active" {
			t.Fatalf("control: once the node answers, the green audit promotes it; status = %q", h.Status)
		}
	})
	t.Run("a node configured with no egress IP", func(t *testing.T) {
		w, mem, _, simAudit, _ := promoFixture(t)
		simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
		noIP := []WatchedNode{{NodeRef: audit.NodeRef{ID: promoNode, BaseURL: promoBase, Token: "t"}}}
		w.Tick(context.Background(), noIP)
		if h := health(t, mem); h.Status != "provisioning" {
			t.Fatalf("with no configured egress IP the audit cannot be tied to the node's address: it must not promote; status = %q", h.Status)
		}
	})
}

// A node whose audit cannot be FETCHED records nothing and is not promoted (an unreachable box is not green); the next cycle that does fetch a
// green audit promotes it. That is also the retry: a node that is not up at boot is promoted by a later tick, not never.
func TestPromotion_AnUnreachableNodeIsNotPromoted_AndTheNextGreenTickPromotesIt(t *testing.T) {
	w, mem, pool, simAudit, _ := promoFixture(t)
	simAudit.SetError(promoBase, &node.TransportError{Err: errors.New("connection refused")})
	w.Tick(context.Background(), pool)
	if h := health(t, mem); h.Status != "provisioning" || len(mem.NodeAudits) != 0 {
		t.Fatalf("an audit that could not be fetched records nothing and promotes nothing: status %q, %d audit rows", h.Status, len(mem.NodeAudits))
	}
	simAudit.SetError(promoBase, nil) // the box comes back
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
	w.Tick(context.Background(), pool)
	if h := health(t, mem); h.Status != "active" {
		t.Fatalf("the next tick with a green audit must promote the node; status = %q", h.Status)
	}
}

// THE RESTART. A node that was active before a control-plane restart is provisioning again the moment the boot registers it (stale active never
// trusted), stays so on a red audit, and is promoted again by the next green one with a NEW audit row's id.
func TestPromotion_ARestartReRegistersAndReAudits_AStaleActiveIsNeverTrusted(t *testing.T) {
	w, mem, pool, simAudit, _ := promoFixture(t)
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
	w.Tick(context.Background(), pool)
	before := health(t, mem)
	if before.Status != "active" || !before.Port25Open || before.PromotedByAuditID == "" {
		t.Fatalf("fixture: the node must be promoted before the 'restart': %+v", before)
	}

	// The restart: the new process registers the node again, before any audit of its own.
	if err := mem.RegisterNode(context.Background(), promoNode, promoBase); err != nil {
		t.Fatal(err)
	}
	if h := health(t, mem); h.Status != "provisioning" || h.Port25Open || h.PromotedByAuditID != "" {
		t.Fatalf("a restart must NOT trust the stale active: re-registered node = %+v", h)
	}
	// The router's own view: not eligible (status is not active), whatever the old audit summary says.
	h := health(t, mem)
	if audit.Eligible(audit.EligibilityInput{
		Status: audit.Lifecycle(h.Status), AuditStatus: audit.Status(h.AuditStatus), Port25Open: h.Port25Open, LastAuditAt: h.LastAuditAt,
		LastAuditHostIP: h.LastAuditHostIP, ExpectedEgressIP: promoIP, Now: wnow, MaxAuditAge: time.Hour,
	}) {
		t.Fatal("a re-registered node must not be :smtp-eligible before its first audit of this boot, even though the previous boot's audit summary is green and fresh")
	}

	// The first audits of the new boot: a red one changes nothing, a green one promotes with a new audit id.
	simAudit.Set(promoBase, node.SimPTRWarning(promoIP, wt))
	w.Tick(context.Background(), pool)
	if h := health(t, mem); h.Status != "provisioning" {
		t.Fatalf("a red audit after the restart must leave the node provisioning; status = %q", h.Status)
	}
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
	w.Tick(context.Background(), pool)
	after := health(t, mem)
	if after.Status != "active" || !after.Port25Open {
		t.Fatalf("the next green audit after the restart must promote again: %+v", after)
	}
	if after.PromotedByAuditID == before.PromotedByAuditID {
		t.Errorf("the second promotion must record its OWN audit row, not the previous boot's (%q both times)", after.PromotedByAuditID)
	}
}

// "The first audit runs promptly after boot": Run ticks ONCE at once, before any cadence wait, and with cadence 0 it ticks only that once, so one
// call promotes a fresh node whose audit is green. CONTROL: with a red audit the same call leaves it provisioning.
func TestPromotion_TheFirstAuditRunsAtOnce_NotAfterTheCadence(t *testing.T) {
	w, mem, pool, simAudit, _ := promoFixture(t)
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
	done := make(chan struct{})
	go func() { w.Run(context.Background(), pool, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with cadence 0 must return after its immediate first tick")
	}
	if h := health(t, mem); h.Status != "active" {
		t.Fatalf("the immediate first tick must promote the node; status = %q", h.Status)
	}

	w2, mem2, pool2, simAudit2, _ := promoFixture(t)
	simAudit2.Set(promoBase, node.SimPTRWarning(promoIP, wt))
	w2.Run(context.Background(), pool2, 0)
	if h := health(t, mem2); h.Status != "provisioning" {
		t.Fatalf("control: the same call with a red audit must leave the node provisioning; status = %q", h.Status)
	}
}

// THE STORE'S OWN WALL. Even if the decision above were wrong, the store promotes only from a GREEN row recorded AFTER the registration, and only a
// node that is still provisioning. These run on the in-memory store here and on Postgres in the store package (TestSettleProvisioningNode_*).
func TestPromotion_AnAuditRowOlderThanTheRegistrationDoesNotPromote(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem(func() time.Time { return wnow })
	// An audit row recorded BEFORE the registration (an earlier boot's), green and on the right IP.
	if err := mem.RegisterNode(ctx, promoNode, promoBase); err != nil {
		t.Fatal(err)
	}
	old := store.NodeAuditRow{ID: "old-green", NodeID: promoNode, AuditedAt: wnow, AuditStatus: "green", Warnings: map[string]any{}, CurrentHostIP: promoIP}
	if err := mem.AppendNodeAudit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := mem.RegisterNode(ctx, promoNode, promoBase); err != nil { // the restart: the row above is now older than the registration
		t.Fatal(err)
	}
	if ok, err := mem.SettleProvisioningNode(ctx, promoNode, old.ID, true); err != nil || ok {
		t.Fatalf("an audit row older than the registration must not promote: ok=%v err=%v", ok, err)
	}
	if h := health(t, mem); h.Status != "provisioning" || h.PromotedByAuditID != "" {
		t.Fatalf("the node must still be provisioning: %+v", h)
	}
	// CONTROL: a row recorded after the registration promotes.
	fresh := store.NodeAuditRow{ID: "fresh-green", NodeID: promoNode, AuditedAt: wnow, AuditStatus: "green", Warnings: map[string]any{}, CurrentHostIP: promoIP}
	if err := mem.AppendNodeAudit(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if ok, err := mem.SettleProvisioningNode(ctx, promoNode, fresh.ID, true); err != nil || !ok {
		t.Fatalf("control: a green row recorded after the registration must promote: ok=%v err=%v", ok, err)
	}
	if h := health(t, mem); h.Status != "active" || h.PromotedByAuditID != fresh.ID || !h.Port25Open {
		t.Fatalf("control: %+v", h)
	}
}

// The store refuses a red row, another node's row, an unknown row, an empty id, and a node that is not provisioning; each refusal changes nothing.
func TestPromotion_TheStoreRefusesWhatIsNotAGreenRowOfThisNodeAfterItsRegistration(t *testing.T) {
	ctx := context.Background()
	newMem := func() *store.MemStore {
		m := store.NewMem(func() time.Time { return wnow })
		if err := m.RegisterNode(ctx, promoNode, promoBase); err != nil {
			t.Fatal(err)
		}
		return m
	}
	row := func(id, nodeID, status string) store.NodeAuditRow {
		return store.NodeAuditRow{ID: id, NodeID: nodeID, AuditedAt: wnow, AuditStatus: status, Warnings: map[string]any{}, CurrentHostIP: promoIP}
	}
	for _, tc := range []struct {
		name  string
		setup func(m *store.MemStore) string // returns the audit id to settle with
	}{
		{"a red row", func(m *store.MemStore) string { _ = m.AppendNodeAudit(ctx, row("r", promoNode, "red")); return "r" }},
		{"another node's green row", func(m *store.MemStore) string { _ = m.AppendNodeAudit(ctx, row("o", "other", "green")); return "o" }},
		{"an unknown row", func(m *store.MemStore) string { return "no-such-row" }},
		{"an empty audit id", func(m *store.MemStore) string { _ = m.AppendNodeAudit(ctx, row("g", promoNode, "green")); return "" }},
		{"a node that is not provisioning", func(m *store.MemStore) string {
			_ = m.AppendNodeAudit(ctx, row("g", promoNode, "green"))
			_ = m.UpsertNodeState(ctx, promoNode, promoBase, "quarantined", true) // not provisioning
			return "g"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMem()
			id := tc.setup(m)
			ok, err := m.SettleProvisioningNode(ctx, promoNode, id, true)
			if err != nil || ok {
				t.Fatalf("must be refused: ok=%v err=%v", ok, err)
			}
			if h := health(t, m); h.Status == "active" || h.PromotedByAuditID != "" {
				t.Fatalf("a refusal must change nothing: %+v", h)
			}
		})
	}
	// CONTROL: the same fixture with a green row of this node, recorded after the registration, promotes.
	m := newMem()
	_ = m.AppendNodeAudit(ctx, row("g", promoNode, "green"))
	if ok, err := m.SettleProvisioningNode(ctx, promoNode, "g", true); err != nil || !ok {
		t.Fatalf("control: ok=%v err=%v", ok, err)
	}
}

// ── the pure decision ──────────────────────────────────────────────────────────────────────────────────────────────────────

func provisioningSnapshot() NodeSnapshot {
	return NodeSnapshot{NodeID: "n1", Status: LifecycleProvisioning, AuditKnown: true, AuditGreen: true, Live: true, AuditID: "audit-1", EgressConfirmed: true}
}

func actionFor(t *testing.T, s NodeSnapshot) Action {
	t.Helper()
	res := Evaluate([]NodeSnapshot{s}, Params{})
	if len(res.Actions) != 1 {
		t.Fatalf("%d actions for one node", len(res.Actions))
	}
	return res.Actions[0]
}

// TestEvaluate_ProvisioningPromotion_NeedsEveryConjunct: the promotion needs a known green audit, a recorded audit row, an egress-confirmed audit and a
// live node, and each missing conjunct alone is refused (the full snapshot is the control). A red audit on a provisioning node is NOT a quarantine.
func TestEvaluate_ProvisioningPromotion_NeedsEveryConjunct(t *testing.T) {
	full := actionFor(t, provisioningSnapshot())
	if !full.Apply || full.To != LifecycleActive || full.Reason != "audit_promote" || full.AuditID != "audit-1" {
		t.Fatalf("control: a green, egress-confirmed, live provisioning node with a recorded audit must be promoted by that audit: %+v", full)
	}
	for _, tc := range []struct {
		name   string
		mutate func(s *NodeSnapshot)
	}{
		{"audit red", func(s *NodeSnapshot) { s.AuditGreen = false }},
		{"no audit known", func(s *NodeSnapshot) { s.AuditKnown = false }},
		{"egress not confirmed", func(s *NodeSnapshot) { s.EgressConfirmed = false }},
		{"not live", func(s *NodeSnapshot) { s.Live = false }},
		{"no audit row recorded this cycle", func(s *NodeSnapshot) { s.AuditID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := provisioningSnapshot()
			tc.mutate(&s)
			a := actionFor(t, s)
			if a.Apply || a.To != LifecycleProvisioning || a.Reason == "audit_promote" {
				t.Fatalf("%s must not promote and must not change the node: %+v", tc.name, a)
			}
		})
	}
	// The red audit case, stated on its own: provisioning + red is neither a promotion nor a quarantine (an ACTIVE node with the same audit IS quarantined).
	red := provisioningSnapshot()
	red.AuditGreen = false
	if a := actionFor(t, red); a.To == LifecycleQuarantined {
		t.Fatalf("a red audit must not quarantine a provisioning node: %+v", a)
	}
	active := red
	active.Status = LifecycleActive
	if a := actionFor(t, active); !a.Apply || a.To != LifecycleQuarantined {
		t.Fatalf("control: the same red audit quarantines an ACTIVE node: %+v", a)
	}
}

// An unreadable promotion log line must not exist: the watcher names the audit row it promoted by.
func TestPromotion_TheWatcherLogsTheAuditRowItPromotedBy(t *testing.T) {
	w, mem, pool, simAudit, _ := promoFixture(t)
	var logged []string
	w.deps.Log = func(f string, a ...any) { logged = append(logged, f) }
	simAudit.Set(promoBase, node.SimGreen(promoIP, wt))
	w.Tick(context.Background(), pool)
	if h := health(t, mem); h.Status != "active" {
		t.Fatalf("status = %q", h.Status)
	}
	found := false
	for _, l := range logged {
		if strings.Contains(l, "audit_promote") && strings.Contains(l, "audit=") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line named the promotion and its audit: %v", logged)
	}
}
