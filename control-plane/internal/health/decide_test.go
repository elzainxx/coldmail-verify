package health

import "testing"

// The pure evaluator tests — the two irreversible surfaces (fleet brake, guards) proven
// deterministically. These are the P4 equivalent of P3's GAP-24 fail-verification: they assert the
// pool does NOT self-empty on a systemic spike, and that the block-rate trigger is guarded.

func autoParams() Params {
	// A permissive test config: block-rate quarantine in AUTO so we can observe the trigger; small
	// sample gate; a majority fleet-wide brake; explicit elevated-fraction + floor so these tests
	// don't silently shift if the defaults change.
	return Params{
		MinSamples: 10, QuarThreshold: 0.30, RecoverThreshold: 0.05, RecoverGreens: 2,
		MinCanarySamples: 10, LivenessMisses: 3, FleetWideFraction: 0.5, ElevatedFraction: 0.6,
		MinActiveFloor: 1, BlockMode: ModeAuto,
	}
}

func active(id string, attempts, blocked int) NodeSnapshot {
	return NodeSnapshot{NodeID: id, Status: LifecycleActive, AuditGreen: true, AuditKnown: true, Live: true, Attempts: attempts, Blocked: blocked}
}

func findAction(res Result, id string) Action {
	for _, a := range res.Actions {
		if a.NodeID == id {
			return a
		}
	}
	return Action{}
}

func hasAlert(res Result, kind string) bool {
	for _, a := range res.Alerts {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// ── THE irreversible surface (§5.5/GAP-28) — fail-verified ──
// A simulated fleet-wide block spike: ALL active nodes are over threshold at once. The brake MUST
// refuse to auto-quarantine (the pool must NOT self-empty) and MUST raise a CRITICAL fleet_wide
// alert. This is "auto-heal must not become auto-outage."
func TestEvaluate_FleetWideSpike_DoesNotEmptyPool_GAP28(t *testing.T) {
	fleet := []NodeSnapshot{
		active("n1", 100, 90), // 90% blocked
		active("n2", 100, 88),
		active("n3", 100, 92),
	}
	res := Evaluate(fleet, autoParams())
	// NOT ONE node may be auto-quarantined — that would begin the cascade the brake exists to stop.
	for _, a := range res.Actions {
		if a.Apply && a.To == LifecycleQuarantined {
			t.Fatalf("GAP-28 VIOLATION: node %s auto-quarantined on a fleet-wide spike — the pool self-empties", a.NodeID)
		}
	}
	if !hasAlert(res, "fleet_wide") {
		t.Fatal("a fleet-wide spike MUST raise a critical fleet_wide alert (§5.5/§8.2 — the human backstop)")
	}
	// The critical severity is the human backstop to the deliberate non-action (OQ-P4-9).
	var crit bool
	for _, al := range res.Alerts {
		if al.Kind == "fleet_wide" && al.Severity == "critical" {
			crit = true
		}
	}
	if !crit {
		t.Fatal("the fleet_wide alert MUST be critical")
	}
}

// The counterpart: a SINGLE dirty node against a healthy pool IS auto-quarantined (the brake only
// suppresses the systemic case; a lone outlier is a genuine per-node fault). This proves the brake
// isn't just "never quarantine."
func TestEvaluate_SingleOutlier_IsQuarantined(t *testing.T) {
	fleet := []NodeSnapshot{
		active("dirty", 100, 90), // the one bad IP
		active("clean1", 100, 1),
		active("clean2", 100, 0),
	}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "dirty"); !(a.Apply && a.To == LifecycleQuarantined) {
		t.Fatalf("a single block-rate outlier against a healthy pool must be quarantined, got %+v", a)
	}
	// The clean peers are untouched (failure isolation, §5.6).
	for _, id := range []string{"clean1", "clean2"} {
		if a := findAction(res, id); a.To != LifecycleActive {
			t.Fatalf("clean peer %s must stay active, got %s", id, a.To)
		}
	}
}

// Sample-size gate (§5.3 guard 1 / GAP-27): a node 100% blocked over only 2 samples is NOT
// quarantined — the rate is noise below MinSamples.
func TestEvaluate_SampleGate_SmallSampleNotQuarantined_GAP27(t *testing.T) {
	fleet := []NodeSnapshot{active("n1", 2, 2)} // 100% blocked but only 2 attempts
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); a.To == LifecycleQuarantined {
		t.Fatalf("GAP-27: a 2-sample node must NOT be quarantined (rate is noise), got quarantined")
	}
}

// Shadow-mode (DR-2): with the block-rate trigger at recommend-only (the DEFAULT), a single outlier
// is NOT auto-quarantined — it emits a RECOMMENDATION. This is the fresh-system zero-mutation
// invariant for A1.
func TestEvaluate_BlockRate_RecommendOnlyByDefault_DR2(t *testing.T) {
	p := autoParams()
	p.BlockMode = ModeRecommendOnly // the DR-2 default
	fleet := []NodeSnapshot{active("dirty", 100, 90), active("clean", 100, 0)}
	res := Evaluate(fleet, p)
	a := findAction(res, "dirty")
	if a.Apply {
		t.Fatal("DR-2: at recommend-only the block-rate trigger must NOT auto-mutate (fresh-system invariant)")
	}
	if a.Recommend == "" || a.Reason != "block_rate" {
		t.Fatalf("recommend-only must emit a block_rate RECOMMENDATION, got %+v", a)
	}
	// The block_spike alert still fires (a human sees it), just no mutation.
	if !hasAlert(res, "block_spike") {
		t.Fatal("recommend-only still alerts on the block spike (compute + alert, never auto-change)")
	}
}

// Audit-red stays AUTO regardless of the block-rate shadow-mode (A2 is deterministic, not
// statistical) — and is NOT gated by the fleet brake.
func TestEvaluate_AuditRed_AutoQuarantine_EvenAtRecommendOnly(t *testing.T) {
	p := autoParams()
	p.BlockMode = ModeRecommendOnly
	fleet := []NodeSnapshot{
		{NodeID: "n1", Status: LifecycleActive, AuditGreen: false, AuditKnown: true, Live: true},
	}
	res := Evaluate(fleet, p)
	a := findAction(res, "n1")
	if !(a.Apply && a.To == LifecycleQuarantined && a.Reason == "audit_red") {
		t.Fatalf("audit-red must AUTO-quarantine even when block-rate is recommend-only, got %+v", a)
	}
}

// Even during a fleet-wide block spike, an audit-red node still auto-quarantines (deterministic
// fault, not the redistribution cascade the brake guards).
func TestEvaluate_AuditRed_NotSuppressedByFleetBrake(t *testing.T) {
	fleet := []NodeSnapshot{
		active("n1", 100, 90), active("n2", 100, 90), // fleet-wide block spike
		{NodeID: "bad", Status: LifecycleActive, AuditGreen: false, AuditKnown: true, Live: true},
	}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "bad"); !(a.Apply && a.To == LifecycleQuarantined && a.Reason == "audit_red") {
		t.Fatalf("audit-red must quarantine even amid a fleet-wide spike, got %+v", a)
	}
	// ...but the block-rate nodes are held (brake).
	for _, id := range []string{"n1", "n2"} {
		if a := findAction(res, id); a.Apply && a.To == LifecycleQuarantined {
			t.Fatalf("block-rate node %s must be held by the brake, got quarantined", id)
		}
	}
}

// Liveness: M consecutive misses → quarantine (box down/wedged).
func TestEvaluate_Liveness_QuarantineAfterMMisses(t *testing.T) {
	p := autoParams()
	down := NodeSnapshot{NodeID: "n1", Status: LifecycleActive, AuditGreen: true, AuditKnown: true, Live: false, LiveMisses: 3}
	res := Evaluate([]NodeSnapshot{down}, p)
	if a := findAction(res, "n1"); !(a.Apply && a.To == LifecycleQuarantined && a.Reason == "liveness") {
		t.Fatalf("M consecutive liveness misses must quarantine, got %+v", a)
	}
	// Below M, no quarantine (debounce a transient blip).
	blip := down
	blip.LiveMisses = 1
	res2 := Evaluate([]NodeSnapshot{blip}, p)
	if a := findAction(res2, "n1"); a.To == LifecycleQuarantined {
		t.Fatal("a single liveness miss must NOT quarantine (debounce)")
	}
}

// ── Canary recovery (§5.4/GAP-30) — block-quarantine is NOT a one-way door ──

// A quarantined node that regained audit-green + liveness moves to `recovering` (starts the canary).
func TestEvaluate_QuarantinedToRecovering(t *testing.T) {
	fleet := []NodeSnapshot{{NodeID: "n1", Status: LifecycleQuarantined, AuditGreen: true, AuditKnown: true, Live: true}}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); !(a.Apply && a.To == LifecycleRecovering) {
		t.Fatalf("a quarantined node that is audit-green + live must start recovering (canary), got %+v", a)
	}
}

// A recovering node whose metered canary stays clean over RecoverGreens → active, WITH a warmup
// reset (re-warm gently, PM3).
func TestEvaluate_Recovering_CleanCanaryPromotesAndResetsWarmup(t *testing.T) {
	p := autoParams() // RecoverGreens=2
	// GreenStreak=1 this cycle + this clean cycle = 2 → promote. Clean canary: block-rate under recover.
	fleet := []NodeSnapshot{{
		NodeID: "n1", Status: LifecycleRecovering, AuditGreen: true, AuditKnown: true, Live: true,
		Attempts: 20, Blocked: 0, GreenStreak: 1,
	}}
	res := Evaluate(fleet, p)
	a := findAction(res, "n1")
	if !(a.Apply && a.To == LifecycleActive && a.Reason == "canary_recover") {
		t.Fatalf("a sustained-clean canary must promote to active, got %+v", a)
	}
	if !a.ResetWarmup {
		t.Fatal("PM3: canary recovery MUST reset the warmup counter (re-warm gently, don't re-burn)")
	}
}

// A recovering node whose canary RE-BLOCKS goes back to quarantined (not stuck, not promoted).
func TestEvaluate_Recovering_ReblockReturnsToQuarantine(t *testing.T) {
	fleet := []NodeSnapshot{{
		NodeID: "n1", Status: LifecycleRecovering, AuditGreen: true, AuditKnown: true, Live: true,
		Attempts: 20, Blocked: 18, GreenStreak: 1, // canary re-blocked
	}}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); !(a.Apply && a.To == LifecycleQuarantined && a.Reason == "canary_reblock") {
		t.Fatalf("a re-blocked canary must return to quarantined, got %+v", a)
	}
}

// Recovery is not premature: a recovering node with an insufficient green streak STAYS recovering
// (hysteresis — slow to trust).
func TestEvaluate_Recovering_StaysUntilStreakMet(t *testing.T) {
	fleet := []NodeSnapshot{{
		NodeID: "n1", Status: LifecycleRecovering, AuditGreen: true, AuditKnown: true, Live: true,
		Attempts: 20, Blocked: 0, GreenStreak: 0, // needs 2 (RecoverGreens), has 0 → +1 = 1 < 2
	}}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); a.Apply {
		t.Fatalf("recovery must not fire before the green streak is met (hysteresis), got %+v", a)
	}
}

// ── Regression: the Phase-4 adversarial-review findings (staggered cascade + zero-sample canary) ──

// Finding A (staggered cascade, §5.5/GAP-28): a subnet blocklist climbs the pool ONE node at a time
// — only one node is over the hard trip in a given cycle, but peers are ELEVATED (climbing). The
// original snapshot brake (keyed on ≥2 TRIPPED in one cycle) missed this and drained the pool
// one-at-a-time. The elevated-fraction guard must brake here.
func TestEvaluate_StaggeredSpike_ElevatedGuardBrakes_FindingA(t *testing.T) {
	// n1 is over the 30% trip; n2/n3 are ELEVATED (climbing, ≥60% of 30% = 18%) but not yet tripped.
	fleet := []NodeSnapshot{
		active("n1", 100, 35), // 35% — tripped
		active("n2", 100, 22), // 22% — elevated (>18%), not tripped
		active("n3", 100, 20), // 20% — elevated, not tripped
	}
	res := Evaluate(fleet, autoParams())
	if !hasAlert(res, "fleet_wide") {
		t.Fatal("Finding A: a staggered systemic spike (1 tripped + peers elevated) MUST trip the fleet brake")
	}
	if a := findAction(res, "n1"); a.Apply && a.To == LifecycleQuarantined {
		t.Fatalf("Finding A: n1 must be HELD by the brake (staggered cascade), not quarantined, got %+v", a)
	}
}

// Finding A, cross-cycle memory: a node block-quarantined a cycle ago + a fresh trip now = the
// cascade underway → brake, even if only one node is over the line THIS cycle.
func TestEvaluate_CascadeMemory_BrakesOnStaggeredDrain_FindingA(t *testing.T) {
	fleet := []NodeSnapshot{
		{NodeID: "n1", Status: LifecycleQuarantined, AuditGreen: true, AuditKnown: true, Live: true, RecentlyBlockQuarantined: true},
		active("n2", 100, 40), // freshly tripping — the next domino
	}
	res := Evaluate(fleet, autoParams())
	if !hasAlert(res, "fleet_wide") {
		t.Fatal("Finding A: a recent block-quarantine + a fresh trip is a cascade-in-progress → must brake")
	}
	if a := findAction(res, "n2"); a.Apply && a.To == LifecycleQuarantined {
		t.Fatalf("Finding A: n2 must be held (cascade underway), not quarantined, got %+v", a)
	}
}

// Finding A, active-floor backstop: block-rate quarantine may NEVER drop the pool below the serving
// floor. A single tripped node that is the ONLY active node is held (the pool cannot self-empty on
// block-rate). (This is distinct from a healthy multi-node pool where a lone outlier IS quarantined.)
func TestEvaluate_ActiveFloorBackstop_NeverSelfEmpties_FindingA(t *testing.T) {
	// One active node, tripped, plus quarantined peers → quarantining it would empty the pool.
	fleet := []NodeSnapshot{
		active("last", 100, 90),
		{NodeID: "q1", Status: LifecycleQuarantined, AuditGreen: false, AuditKnown: true, Live: true},
	}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "last"); a.Apply && a.To == LifecycleQuarantined {
		t.Fatal("Finding A: the last active node must NOT be block-quarantined (active-floor backstop — pool cannot self-empty)")
	}
	if a := findAction(res, "last"); a.Reason != "active_floor_hold" {
		t.Fatalf("Finding A: the held node should carry reason active_floor_hold, got %q", a.Reason)
	}
}

// Finding B (zero-sample canary, §5.4/GAP-30): a recovering node with ZERO real canary attempts must
// NOT promote — BlockRate()==0 is trivially ≤ RecoverThreshold, but there is no evidence the IP
// recovered. It must stay recovering until it has ≥ MinCanarySamples real attempts.
func TestEvaluate_Recovering_ZeroSamplesDoesNotPromote_FindingB(t *testing.T) {
	fleet := []NodeSnapshot{{
		NodeID: "n1", Status: LifecycleRecovering, AuditGreen: true, AuditKnown: true, Live: true,
		Attempts: 0, Blocked: 0, GreenStreak: 5, // green streak long past RecoverGreens, but NO real samples
	}}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); a.Apply && a.To == LifecycleActive {
		t.Fatal("Finding B: a recovering node with ZERO canary samples must NOT promote (no reputation evidence)")
	}
}

// Finding B, positive: with ≥ MinCanarySamples clean real attempts, the canary DOES promote.
func TestEvaluate_Recovering_RealCleanSamplesPromotes_FindingB(t *testing.T) {
	p := autoParams() // MinCanarySamples defaults to 10 via withDefaults
	fleet := []NodeSnapshot{{
		NodeID: "n1", Status: LifecycleRecovering, AuditGreen: true, AuditKnown: true, Live: true,
		Attempts: 15, Blocked: 0, GreenStreak: 1, // 15 clean real canary attempts, streak reaches RecoverGreens
	}}
	res := Evaluate(fleet, p)
	a := findAction(res, "n1")
	if !(a.Apply && a.To == LifecycleActive && a.ResetWarmup) {
		t.Fatalf("Finding B: a canary with real clean samples over the streak must promote + reset warmup, got %+v", a)
	}
}

// A never-audited node is not audit-quarantined on the ABSENCE of a signal (AuditKnown=false).
func TestEvaluate_NeverAudited_NotAuditQuarantined(t *testing.T) {
	fleet := []NodeSnapshot{{NodeID: "n1", Status: LifecycleActive, AuditGreen: false, AuditKnown: false, Live: true}}
	res := Evaluate(fleet, autoParams())
	if a := findAction(res, "n1"); a.Apply && a.Reason == "audit_red" {
		t.Fatal("absence of an audit signal must NOT be treated as red (AuditKnown=false)")
	}
}
