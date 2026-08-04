package health

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/audit"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// End-to-end watcher tests against the LOCAL multi-node substrate the P4 kickoff mandates: a
// simulated pool (N≥3) with controllable per-node audit (SimAuditSurface), liveness
// (SimLivenessSurface), and block-rate (seeded LOG rows in MemStore). This is the Phase-3
// simulated-box approach extended to a pool — the block-rate/quarantine behaviors run against a
// controllable verdict stream (Appendix B rows 4/5/6).

const wt = "2026-07-18T12:00:00Z"

var wnow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// poolFixture wires N nodes: registers them active+green+port25 in MemStore, a sim audit surface
// (all green on their egress IP), a sim liveness surface (all up), and a block-rate source (the LOG).
func poolFixture(t *testing.T, ids []string, params Params, mode Mode) (*Watcher, *store.MemStore, []WatchedNode, *node.SimLivenessSurface, *node.SimAuditSurface) {
	t.Helper()
	mem := store.NewMem(func() time.Time { return wnow })
	simAudit := node.NewSimAuditSurface()
	simLive := node.NewSimLivenessSurface()
	var pool []WatchedNode
	for i, id := range ids {
		ip := ipFor(i)
		base := "http://box-" + id
		// Register active + green + port25 on its egress IP.
		_ = mem.UpsertNodeState(context.Background(), id, base, "active", true)
		_ = mem.AppendNodeAudit(context.Background(), store.NodeAuditRow{
			ID: "a" + id, NodeID: id, AuditedAt: wnow, AuditStatus: "green", Warnings: map[string]any{}, CurrentHostIP: ip,
		})
		simAudit.Set(base, node.SimGreen(ip, wt))
		pool = append(pool, WatchedNode{
			NodeRef:    audit.NodeRef{ID: id, BaseURL: base, Token: "t", PublicEgressIP: ip},
			VerifierIP: ip,
		})
	}
	sched := audit.NewScheduler(audit.Deps{Client: simAudit, Store: mem, Now: func() time.Time { return wnow }}, params.RecoverGreens)
	params.BlockMode = mode
	w := NewWatcher(Deps{
		Audit: sched, Liveness: simLive, BlockRate: mem, Status: mem,
		Now: func() time.Time { return wnow }, Log: func(string, ...any) {},
	}, params, 30*time.Minute, 5)
	return w, mem, pool, simLive, simAudit
}

func ipFor(i int) string {
	return []string{"203.0.113.10", "203.0.113.11", "203.0.113.12", "203.0.113.13"}[i]
}

// seedBlocks appends n LOG rows for verifierIP: `blocked` of them smtp_blocked, the rest accepted,
// all real (from_cache=false, verifier_ip set) so the block-rate query counts them (PB1).
func seedBlocks(mem *store.MemStore, verifierIP string, attempts, blocked int) {
	for i := 0; i < attempts; i++ {
		reason := "accepted"
		status := "valid"
		if i < blocked {
			reason, status = "smtp_blocked", "unknown"
		}
		_ = mem.AppendVerification(context.Background(), store.VerificationRow{
			ID: "v", CheckedAt: wnow, CreatedAt: wnow, Status: status, Reason: reason, VerifierIP: verifierIP,
		}, nil)
	}
}

func params() Params {
	return Params{MinSamples: 10, QuarThreshold: 0.30, RecoverThreshold: 0.05, RecoverGreens: 2, MinCanarySamples: 10, LivenessMisses: 2, FleetWideFraction: 0.5, ElevatedFraction: 0.6, MinActiveFloor: 1}
}

// THE fleet-brake fail-verification, end-to-end (§5.5/GAP-28): seed a FLEET-WIDE block spike in the
// LOG (all 3 nodes 90% blocked), tick the watcher IN AUTO mode, and assert the pool does NOT
// self-empty — every node stays active, a critical fleet_wide alert fires.
func TestWatcher_FleetWideSpike_PoolDoesNotSelfEmpty_GAP28(t *testing.T) {
	w, mem, pool, _, _ := poolFixture(t, []string{"n1", "n2", "n3"}, params(), ModeAuto)
	for i := range pool {
		seedBlocks(mem, ipFor(i), 100, 90) // every node 90% blocked → systemic
	}
	res := w.Tick(context.Background(), pool)
	for _, p := range pool {
		h, _, _ := mem.GetNodeHealth(context.Background(), p.ID)
		if h.Status != "active" {
			t.Fatalf("GAP-28 VIOLATION: node %s left active (%s) on a fleet-wide spike — the pool self-emptied", p.ID, h.Status)
		}
	}
	if !hasAlert(res, "fleet_wide") {
		t.Fatal("a fleet-wide spike must raise the critical fleet_wide alert")
	}
}

// A single dirty node against a healthy pool IS quarantined end-to-end (block-rate from the LOG,
// AUTO mode) — and the clean peers are untouched (failure isolation §5.6, the GAP-24 payoff).
func TestWatcher_SingleDirtyNode_QuarantinedCleanPeersIsolated(t *testing.T) {
	w, mem, pool, _, _ := poolFixture(t, []string{"n1", "n2", "n3"}, params(), ModeAuto)
	seedBlocks(mem, ipFor(0), 100, 90) // n1 dirty
	seedBlocks(mem, ipFor(1), 100, 1)  // n2 clean
	seedBlocks(mem, ipFor(2), 100, 0)  // n3 clean
	w.Tick(context.Background(), pool)
	if h, _, _ := mem.GetNodeHealth(context.Background(), "n1"); h.Status != "quarantined" {
		t.Fatalf("the dirty node n1 must be quarantined, got %s", h.Status)
	}
	for _, id := range []string{"n2", "n3"} {
		if h, _, _ := mem.GetNodeHealth(context.Background(), id); h.Status != "active" {
			t.Fatalf("clean peer %s must stay active (isolation), got %s", id, h.Status)
		}
	}
}

// Full canary cycle (§5.4/GAP-30) end-to-end: quarantine (audit-red) → restore green → recovering →
// clean canary over the streak → active with warmup reset. Proves block-quarantine is not a one-way
// door and recovery re-warms gently.
func TestWatcher_CanaryRecoveryCycle_NotAOneWayDoor_GAP30(t *testing.T) {
	w, mem, pool, _, simAudit := poolFixture(t, []string{"n1"}, params(), ModeAuto)
	ctx := context.Background()
	// 1) Node goes audit-red → the audit pass quarantines it (A2 auto).
	simAudit.Set("http://box-n1", node.SimPTRWarning(ipFor(0), wt))
	w.Tick(ctx, pool)
	if h, _, _ := mem.GetNodeHealth(ctx, "n1"); h.Status != "quarantined" {
		t.Fatalf("audit-red must quarantine n1, got %s", h.Status)
	}
	// Seed the warmup counter high, so we can prove the recovery reset re-warms gently (PM3).
	mem.SeedWarmup("n1", warmupWindow(wnow), 50, 50)

	// 2) Restore green → next tick moves quarantined → recovering (canary starts).
	simAudit.Set("http://box-n1", node.SimGreen(ipFor(0), wt))
	w.Tick(ctx, pool)
	if h, _, _ := mem.GetNodeHealth(ctx, "n1"); h.Status != "recovering" {
		t.Fatalf("a green+live quarantined node must start recovering, got %s", h.Status)
	}

	// 3) The canary must send REAL checks and come back clean before promotion (GAP-30 fix: no
	//    zero-sample promotion). Seed MinCanarySamples (10) clean attempts on n1's IP — this models the
	//    metered canary trickle the router now routes to a recovering node — then two clean recovering
	//    ticks accrue the green streak and, with real clean samples, promote to active + reset warmup.
	seedBlocks(mem, ipFor(0), 12, 0) // 12 clean canary attempts, 0 blocked → block-rate 0 over real samples
	w.Tick(ctx, pool)                // streak 1 (has samples, clean)
	w.Tick(ctx, pool)                // streak 2 → promote
	if h, _, _ := mem.GetNodeHealth(ctx, "n1"); h.Status != "active" {
		t.Fatalf("a sustained-clean canary WITH real samples must promote to active, got %s", h.Status)
	}
	// The warmup counter was reset to a low start (re-warm gently) — a reservation now succeeds at
	// the low canary cap, proving the pre-quarantine full window did not carry over.
	d, _ := mem.ReserveWarmupSlot(ctx, "n1", warmupWindow(wnow), 5)
	if !d.Allowed {
		t.Fatalf("PM3: after recovery the warmup must have been reset to a low start (ramp reopened), got denied %+v", d)
	}
}

// Liveness end-to-end: a box down for M consecutive ticks is quarantined; the pool is otherwise
// unaffected.
func TestWatcher_LivenessDown_Quarantines(t *testing.T) {
	p := params()
	p.LivenessMisses = 2
	w, mem, pool, simLive, _ := poolFixture(t, []string{"n1", "n2"}, p, ModeAuto)
	simLive.SetDown("http://box-n1", true)
	w.Tick(context.Background(), pool) // miss 1 — below M, no quarantine
	if h, _, _ := mem.GetNodeHealth(context.Background(), "n1"); h.Status != "active" {
		t.Fatalf("one liveness miss must not quarantine (debounce), got %s", h.Status)
	}
	w.Tick(context.Background(), pool) // miss 2 — reaches M → quarantine
	if h, _, _ := mem.GetNodeHealth(context.Background(), "n1"); h.Status != "quarantined" {
		t.Fatalf("M consecutive liveness misses must quarantine, got %s", h.Status)
	}
	if h, _, _ := mem.GetNodeHealth(context.Background(), "n2"); h.Status != "active" {
		t.Fatalf("the live peer n2 must stay active, got %s", h.Status)
	}
}

// Shadow-mode end-to-end (DR-2 fresh-system invariant): with the block-rate trigger at the DEFAULT
// recommend-only, a dirty node is NOT auto-quarantined — the watcher performs ZERO live block-rate
// mutation (it only recommends). This is the fresh-system regression gate for A1.
func TestWatcher_BlockRate_RecommendOnly_ZeroMutation_DR2(t *testing.T) {
	w, mem, pool, _, _ := poolFixture(t, []string{"n1", "n2"}, params(), ModeRecommendOnly)
	seedBlocks(mem, ipFor(0), 100, 90) // n1 dirty
	res := w.Tick(context.Background(), pool)
	if h, _, _ := mem.GetNodeHealth(context.Background(), "n1"); h.Status != "active" {
		t.Fatalf("DR-2: at recommend-only the block-rate trigger must perform ZERO mutation — n1 must stay active, got %s", h.Status)
	}
	// A recommendation + a block_spike alert are surfaced (compute + alert, never auto-change).
	if !hasAlert(res, "block_spike") {
		t.Fatal("recommend-only must still alert on the block spike")
	}
	var recommended bool
	for _, a := range res.Actions {
		if a.NodeID == "n1" && !a.Apply && a.Reason == "block_rate" && a.Recommend != "" {
			recommended = true
		}
	}
	if !recommended {
		t.Fatal("recommend-only must emit a block_rate recommendation for the dirty node")
	}
}

// Block-rate excludes cache hits + degrades end-to-end (PB1): a node whose LOG is all cache hits +
// degrades is NOT quarantined even in AUTO mode (it produced no real SMTP attempts).
func TestWatcher_BlockRate_ExcludesCacheAndDegrades_PB1(t *testing.T) {
	w, mem, pool, _, _ := poolFixture(t, []string{"n1"}, params(), ModeAuto)
	ctx := context.Background()
	// 50 cache-hit "blocks" (from_cache=true) + 50 degrades (verifier_ip="") — none is a real attempt.
	for i := 0; i < 50; i++ {
		_ = mem.AppendVerification(ctx, store.VerificationRow{ID: "c", CheckedAt: wnow, CreatedAt: wnow, Status: "unknown", Reason: "smtp_blocked", VerifierIP: ipFor(0), FromCache: true}, nil)
		_ = mem.AppendVerification(ctx, store.VerificationRow{ID: "d", CheckedAt: wnow, CreatedAt: wnow, Status: "unknown", Reason: "smtp_blocked", VerifierIP: ""}, nil)
	}
	w.Tick(ctx, pool)
	if h, _, _ := mem.GetNodeHealth(ctx, "n1"); h.Status != "active" {
		t.Fatalf("PB1: a node with only cache hits + degrades produced no real attempts — must not quarantine, got %s", h.Status)
	}
}
