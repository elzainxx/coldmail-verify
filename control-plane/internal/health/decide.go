// Package health is the Phase-4 continuous health-watcher (§5) — the control loop P3 explicitly
// deferred (P3 §4.5 SEAM → P4, GAP-18). P3 built the audit-driven quarantine (a clean binary
// signal) + the lifecycle machine skeleton; P4 adds the CONTINUOUS watcher and the BLOCK-RATE
// trigger — a noisy, lagging, statistical signal that demands control-loop discipline the audit
// signal did not.
//
// This file holds the PURE decision logic (decide.go) — no I/O — so the phase's two irreversible
// surfaces are exhaustively, deterministically testable:
//   - the FLEET-WIDE CASCADE BRAKE (§5.5/GAP-28, "the single most important line in the phase"):
//     auto-quarantine redistributes a blocked node's load onto peers, whose block-rate then rises,
//     cascading until the pool empties — on a SHARED cause that was never any single IP's fault. The
//     brake refuses to auto-quarantine when MOST nodes trip at once (alert instead). "Auto-heal must
//     not become auto-outage." Owner directive: when in doubt, err toward ALERT.
//   - the three GUARDS on the block-rate trigger (§5.3): the sample-size gate (GAP-27), hysteresis
//     (quick to quarantine, slow to trust), and redistribution-awareness (the fleet brake).
//
// SHADOW-MODE (DR-2 / history/shadow-mode.md): the BLOCK-RATE (statistical, A1) quarantine trigger DEFAULTS
// to recommend-only — it emits a RECOMMENDATION, never an auto-mutation, until an owner promotes it.
// The AUDIT-RED (A2, deterministic binary) and the CANARY recovery (A3, conservative) stay AUTO.
// This file returns BOTH the decided transitions AND the recommendations; the caller applies the
// former and logs/surfaces the latter (§5.4). The fresh-system invariant (DR-2): at mode-default the
// block-rate trigger performs ZERO live mutation.
package health

import (
	"sort"
)

// Mode is the per-mechanism shadow-mode dial for the block-rate (A1) trigger (DR-2). Auto mutates;
// RecommendOnly (the default) only proposes. It is passed in from config (not decided here).
type Mode string

const (
	ModeRecommendOnly Mode = "recommend_only" // DEFAULT for A1 (block-rate) — propose, don't mutate
	ModeAuto          Mode = "auto"           // owner-promoted — the watcher quarantines on block-rate
)

// Lifecycle mirrors the node lifecycle values (kept as strings to avoid importing the audit package
// here; the watcher maps to/from store.NodeHealth.Status). recovering is the P4 canary sub-state.
const (
	LifecycleProvisioning = "provisioning"
	LifecycleActive       = "active"
	LifecycleQuarantined  = "quarantined"
	LifecycleRecovering   = "recovering"
)

// NodeSnapshot is one node's health inputs for a watcher cycle (§5.2), assembled by the actor from
// the three signals. It is the pure evaluator's per-node input.
type NodeSnapshot struct {
	NodeID     string
	Status     string // current lifecycle (active|quarantined|recovering|provisioning)
	AuditGreen bool   // latest audit signal (§5.2 audit input) — true iff green
	AuditKnown bool   // whether an audit signal exists (false => don't audit-quarantine on absence)
	Live       bool   // latest liveness poll (§5.2 liveness input) — true iff up
	LiveMisses int    // consecutive liveness failures observed so far (for the M-consecutive trigger)

	// Block-rate inputs (§5.2/§5.6), measured from the LOG over the window (from_cache=false ∧
	// verifier_ip set — PB1). Rate = Blocked/Attempts, meaningful only when Attempts >= MinSamples.
	Attempts int
	Blocked  int

	GreenStreak int // consecutive clean cycles (for hysteretic recovery, §5.4)

	// RecentlyBlockQuarantined is the CROSS-CYCLE cascade memory (§5.5/GAP-28, staggered form): true
	// iff THIS node was block-rate-quarantined within the last CascadeWindow cycles. The Watcher
	// supplies it from its own ephemeral memory. It exists because the fleet-wide brake was originally
	// a single-cycle snapshot — a subnet blocklist that climbs the pool ONE node at a time (nodes
	// cross the rolling threshold at different moments) never shows ≥2 tripped in the SAME cycle, so
	// each looks like a lone outlier and the pool drains one-at-a-time with no fleet_wide alert. This
	// flag lets Evaluate see "a block-quarantine just happened AND another node is tripping now" — the
	// cascade signature — and brake. (Found by the Phase-4 adversarial review; the staggered bypass.)
	RecentlyBlockQuarantined bool

	// SELF-PROMOTION INPUTS (0019, owner ruling OD-4). AuditID is the id of the audit row THIS cycle's observation recorded ("" when none was: the
	// fetch failed). EgressConfirmed is true iff the node has a configured public egress IP AND that audit's own current_host_ip equals it
	// (GAP-24, ADR-0009: a green on the wrong IP never promotes). Only a `provisioning` node reads them.
	AuditID         string
	EgressConfirmed bool
}

// BlockRate is Blocked/Attempts, or 0 when there are no attempts (avoids div-by-zero; a node with no
// attempts has no measurable rate — the sample gate handles it).
func (n NodeSnapshot) BlockRate() float64 {
	if n.Attempts <= 0 {
		return 0
	}
	return float64(n.Blocked) / float64(n.Attempts)
}

// Params are the §5.3/§5.4 DIALs (all owner-tunable; existence locked). Thresholds are asymmetric
// (quick to quarantine, slow to trust — hysteresis). FleetWideFraction is the §5.5 brake trigger.
type Params struct {
	MinSamples        int     // §5.3 guard 1 (GAP-27): block-rate trigger inert below this attempt count
	QuarThreshold     float64 // §5.4: block-rate ≥ this (with samples, pool healthy) → quarantine-eligible
	RecoverThreshold  float64 // §5.4: canary block-rate ≤ this (sustained) → recover (asymmetric, lower)
	RecoverGreens     int     // §5.4: consecutive clean cycles before recovering→active
	MinCanarySamples  int     // §5.4/GAP-30: MIN real canary attempts before a block-rate recovery may promote (no zero-sample promotion)
	LivenessMisses    int     // §5.4: M consecutive liveness failures → quarantine (box down/wedged)
	FleetWideFraction float64 // §5.5 (GAP-28): if this fraction+ of ACTIVE nodes are ELEVATED at once → BRAKE (alert, don't quarantine)
	ElevatedFraction  float64 // §5.5: the LOWER-bar "block-rate climbing" fraction (of QuarThreshold) counted toward the fleet-wide signal — catches a systemic spike BEFORE every node crosses the hard trip (the staggered bypass)
	MinActiveFloor    int     // §5.5 backstop: block-rate quarantine may NEVER drop the active pool below this floor (the absolute "cannot self-empty" guarantee — the last healthy node is never auto-block-quarantined)
	BlockMode         Mode    // A1 shadow-mode: recommend_only (default) vs auto (DR-2)
}

// Action is what the watcher should do to one node this cycle. Apply=true means the caller writes
// the transition (SetNodeStatus To); Apply=false with a non-empty Recommend means a recommend-only
// proposal (block-rate at mode recommend_only, or the fleet brake) that is LOGGED/surfaced, never
// auto-applied. ResetWarmup marks a recovery that must re-warm gently (§5.4/PM3).
type Action struct {
	NodeID      string
	From        string
	To          string // the target lifecycle (== From when no change)
	Apply       bool   // true => auto-transition (caller mutates); false => recommendation only
	Recommend   string // human-readable proposal when !Apply (recommend-only / brake-held), else ""
	Reason      string // why: audit_red | liveness | block_rate | canary_recover | canary_reblock | fleet_brake_hold | audit_promote
	ResetWarmup bool   // §5.4/PM3: on recovery, reset the durable warmup counter to a low start
	// AuditID is set only on an `audit_promote` action: the audit row the promotion rests on, which the watcher hands to the store so the
	// promotion is one atomic step recorded with that row's id (0019).
	AuditID string
}

// Alert is a watcher alert for the observability layer (§8.2). Severity: warn|high|critical. The
// fleet-wide block spike is CRITICAL (§5.5/§8.2 — the human backstop to the brake's deliberate
// non-action; OQ-P4-9).
type Alert struct {
	Kind     string // quarantine | block_spike | audit_red | fleet_wide | recovering | rate_saturation | promoted
	Severity string // warn | high | critical
	NodeID   string // "" for fleet-wide
	Detail   string
}

// Result is one cycle's full decision: the per-node actions + the alerts. Deterministic given the
// fleet + params (no clock, no randomness) — the two irreversible surfaces are proven by asserting
// on this.
type Result struct {
	Actions []Action
	Alerts  []Alert
}

// Evaluate runs one control-loop cycle over the fleet snapshot (§5.4/§5.5). It is the PURE core:
// same input → same output, no I/O. The order of precedence per node (§5.4):
//  1. audit-red   → quarantine AUTO (A2 deterministic; §5.4 first-red fail-safe, inherited P3).
//  2. liveness    → quarantine AUTO after M consecutive misses (box down/wedged).
//  3. block-rate  → quarantine-ELIGIBLE, but GUARDED (§5.3): sample gate, hysteresis, and the
//     FLEET-WIDE BRAKE (§5.5). At mode recommend_only (default) it only RECOMMENDS.
//  4. recovering  → canary evaluation: clean over RecoverGreens → active (reset warmup, AUTO A3);
//     re-block → quarantined.
//  5. quarantined + audit-green + live → move to `recovering` (start the canary, AUTO).
//
// The fleet-wide brake is computed FIRST (it gates whether ANY block-rate quarantine may fire this
// cycle): if the fraction of active nodes that are block-rate-tripped ≥ FleetWideFraction, the cause
// is systemic → NO node is block-quarantined this cycle; a CRITICAL fleet_wide alert fires and
// membership holds (§5.5/OQ-P4-9). Audit/liveness quarantines are NOT gated by the brake (they are
// per-node deterministic faults, not the redistribution cascade).
func Evaluate(fleet []NodeSnapshot, p Params) Result {
	p = withDefaults(p)
	var res Result

	// ── Fleet-wide brake pre-check (§5.5/GAP-28) — hardened against the STAGGERED cascade ──
	// The original brake was a single-cycle snapshot keyed on TRIPPED (sample-gated ≥QuarThreshold)
	// nodes. The Phase-4 adversarial review found the hole: a subnet blocklist (the exact shared
	// cause) rarely pushes every node's 30-min rolling rate over the hard trip in the SAME 45s cycle —
	// higher-traffic nodes cross first. So the pool drains ONE node at a time, each looking like a
	// "lone outlier," and the critical alert never fires. Four defenses now, all err-toward-ALERT:
	//
	//   (a) ELEVATED signal, not just TRIPPED. Count active nodes whose rate is CLIMBING toward the
	//       trip (≥ ElevatedFraction × QuarThreshold, with samples) — a lower bar than the hard trip.
	//       A systemic spike shows as many ELEVATED nodes before they all cross. This catches the
	//       staggered spike while peers are still climbing.
	//   (b) CROSS-CYCLE cascade memory. If ANY node was block-quarantined in the last few cycles
	//       (RecentlyBlockQuarantined) AND another node is now tripping, that IS the cascade in
	//       progress — brake, regardless of the single-cycle count.
	//   (c) the ≥2 rule stays (a lone tripped node in an otherwise-quiet pool is a genuine per-node
	//       fault), but is now evaluated on the ELEVATED count, not the tripped count.
	//   (d) the ACTIVE-FLOOR backstop (applied per-node below): block-rate quarantine may NEVER drop
	//       the active pool below MinActiveFloor — the absolute "the pool cannot self-empty" guarantee.
	activeCount, trippedCount, elevatedCount := 0, 0, 0
	recentBlockQuar := false
	for _, n := range fleet {
		if n.RecentlyBlockQuarantined {
			recentBlockQuar = true
		}
		if n.Status == LifecycleActive {
			activeCount++
			if blockTripped(n, p) {
				trippedCount++
			}
			if blockElevated(n, p) {
				elevatedCount++
			}
		}
	}
	// (a)+(c): a systemic spike = a majority of active nodes ELEVATED (climbing), ≥2 of them.
	fleetWide := activeCount > 0 && elevatedCount >= 2 &&
		float64(elevatedCount) >= p.FleetWideFraction*float64(activeCount)
	// (b): a recent block-quarantine + a fresh trip = the cascade already underway → brake even if
	// only one node is over the line THIS cycle (the staggered, one-at-a-time drain).
	if recentBlockQuar && trippedCount >= 1 {
		fleetWide = true
	}
	if fleetWide {
		res.Alerts = append(res.Alerts, Alert{
			Kind: "fleet_wide", Severity: "critical", NodeID: "",
			Detail: "fleet-wide/cascading block spike: multiple active nodes elevated (or a cascade is underway) — block-rate auto-quarantine SUPPRESSED (§5.5/GAP-28); holding membership, alerting a human (OQ-P4-9)",
		})
	}
	// (d) the active-floor backstop, computed once: how many active nodes exist right now. A block-rate
	// quarantine is only ever allowed while activeCount > MinActiveFloor (checked per-node below), so
	// the LAST healthy node(s) can never be auto-block-quarantined — the pool cannot self-empty even if
	// every other guard somehow missed. Audit-red/liveness (deterministic faults) are NOT floor-gated
	// (a genuinely-down box must leave regardless; that's honest degradation, not a cascade).

	for _, n := range fleet {
		a := Action{NodeID: n.NodeID, From: n.Status, To: n.Status, Reason: ""}

		switch {
		// 1. Audit-red → quarantine AUTO (A2 deterministic — §5.4 first-red, inherited P3). Applies from
		//    any non-quarantined state. This is NOT gated by the fleet brake (deterministic fault).
		//    A PROVISIONING node is NOT quarantined by a red audit (0019, owner ruling OD-4): it is not serving, so there is nothing to protect, and
		//    quarantine would put a node that merely has not finished its DNS/PTR setup on the slow canary path instead of letting its next green
		//    audit promote it. A red audit leaves it provisioning (case 6 below); it is quarantined only after it has been active.
		case n.AuditKnown && !n.AuditGreen && n.Status != LifecycleQuarantined && n.Status != LifecycleProvisioning:
			a.To, a.Apply, a.Reason = LifecycleQuarantined, true, "audit_red"
			res.Alerts = append(res.Alerts, Alert{Kind: "audit_red", Severity: "high", NodeID: n.NodeID, Detail: "audit went red — quarantined"})
			res.Alerts = append(res.Alerts, Alert{Kind: "quarantine", Severity: "warn", NodeID: n.NodeID, Detail: "active→quarantined (audit_red)"})

		// 2. Liveness fail × M consecutive → quarantine AUTO (box down/wedged). Not brake-gated.
		case n.Status == LifecycleActive && !n.Live && n.LiveMisses >= p.LivenessMisses:
			a.To, a.Apply, a.Reason = LifecycleQuarantined, true, "liveness"
			res.Alerts = append(res.Alerts, Alert{Kind: "quarantine", Severity: "warn", NodeID: n.NodeID, Detail: "active→quarantined (liveness: box down/wedged)"})

		// 3. Block-rate trip on an ACTIVE node (§5.3/§5.4), audit still green + live. GUARDED.
		case n.Status == LifecycleActive && n.AuditGreen && n.Live && blockTripped(n, p):
			// The fleet brake overrides: a systemic spike (or a cascade underway) is NOT N independent
			// dirty IPs (§5.5). Alert-and-hold, never quarantine.
			if fleetWide {
				a.Apply, a.Reason = false, "fleet_brake_hold"
				a.Recommend = "HOLD (fleet-wide brake): block-rate tripped but the pool is systemically affected / a cascade is underway — NOT quarantining (§5.5)"
				break
			}
			// (d) ACTIVE-FLOOR backstop: never block-quarantine below the serving floor. If quarantining
			// this node would drop the active pool to/under MinActiveFloor, HOLD (alert) — the pool must
			// not self-empty on block-rate even if this looks like a lone outlier. Block-quarantine is a
			// reputation-protection nicety; keeping SOME node serving (honest verdicts) outranks it.
			if activeCount <= p.MinActiveFloor {
				a.Apply, a.Reason = false, "active_floor_hold"
				a.Recommend = "HOLD (active-floor backstop): block-rate tripped but quarantining would drop the pool to/under the serving floor — NOT quarantining (§5.5); the pool never self-empties on block-rate"
				res.Alerts = append(res.Alerts, Alert{Kind: "block_spike", Severity: "high", NodeID: n.NodeID, Detail: "block-rate over threshold but held by the active-floor backstop (pool at the serving floor)"})
				break
			}
			// A single dirty node relative to a healthy pool. block_spike alert always fires.
			res.Alerts = append(res.Alerts, Alert{Kind: "block_spike", Severity: "high", NodeID: n.NodeID, Detail: "block-rate over threshold (single-node outlier)"})
			if p.BlockMode == ModeAuto {
				a.To, a.Apply, a.Reason = LifecycleQuarantined, true, "block_rate"
				res.Alerts = append(res.Alerts, Alert{Kind: "quarantine", Severity: "warn", NodeID: n.NodeID, Detail: "active→quarantined (block_rate, auto)"})
			} else {
				// DR-2 default: recommend-only. Propose, do NOT mutate (the fresh-system invariant).
				a.Apply, a.Reason = false, "block_rate"
				a.Recommend = "QUARANTINE-ELIGIBLE (block-rate): node is a block-rate outlier — recommend-only (A1 default); an owner promotes A1 to auto to enforce"
			}

		// 4. Recovering (canary) node (§5.4/GAP-30): audit-green + live + clean canary over the window
		//    → active (reset warmup, AUTO A3); a re-block sends it back to quarantined.
		case n.Status == LifecycleRecovering:
			switch {
			case !n.AuditGreen || !n.Live:
				// Lost audit/liveness during recovery → back to quarantined (fail-safe).
				a.To, a.Apply, a.Reason = LifecycleQuarantined, true, "canary_reblock"
			case blockTripped(n, p):
				// The metered canary re-blocked → back to quarantined (not recovered).
				a.To, a.Apply, a.Reason = LifecycleQuarantined, true, "canary_reblock"
				res.Alerts = append(res.Alerts, Alert{Kind: "quarantine", Severity: "warn", NodeID: n.NodeID, Detail: "recovering→quarantined (canary re-blocked)"})
			case n.GreenStreak+1 >= p.RecoverGreens && n.Attempts >= p.MinCanarySamples && n.BlockRate() <= p.RecoverThreshold:
				// Sustained clean canary → promote to active, re-warm gently (§5.4/PM3 reset warmup).
				// GAP-30 fix (Phase-4 adversarial review): promotion requires MinCanarySamples REAL
				// canary attempts — NOT just audit-green cycles. Without this, a recovering node (which
				// the eligibility gate excludes from normal :smtp) accrued ZERO attempts, so BlockRate()
				// was trivially 0 ≤ RecoverThreshold and the node promoted back to active on audit-green
				// alone, re-warming a still-blocklisted IP with NO evidence its reputation recovered. The
				// canary must actually SEND (the metered trickle, admitted by the gate for recovering
				// nodes — see router.SMTPRoute) and come back clean. No samples ⇒ stay recovering.
				a.To, a.Apply, a.Reason, a.ResetWarmup = LifecycleActive, true, "canary_recover", true
				res.Alerts = append(res.Alerts, Alert{Kind: "recovering", Severity: "warn", NodeID: n.NodeID, Detail: "recovering→active (canary clean over real samples; warmup reset — re-warm gently)"})
			}
			// else: stay recovering (canary still accumulating clean samples, or not enough yet) — no action.

		// 5. Quarantined node that regained audit-green + liveness → start the canary (→ recovering).
		//    This makes block-quarantine reversible (§5.4/GAP-30): without it a quarantined node gets
		//    no traffic → no samples → stuck forever. AUTO (A3, conservative — a metered canary only).
		case n.Status == LifecycleQuarantined && n.AuditGreen && n.Live:
			a.To, a.Apply, a.Reason = LifecycleRecovering, true, "canary_start"
			res.Alerts = append(res.Alerts, Alert{Kind: "recovering", Severity: "warn", NodeID: n.NodeID, Detail: "quarantined→recovering (audit-green + live; starting metered canary, §5.4)"})

		// 6. SELF-PROMOTION of a provisioning node (0019, owner ruling OD-4; Docs/install-and-deploy.md "d.4"): provisioning → active ONLY on a
		//    green audit that THIS cycle recorded (AuditID names the row), on the node's configured egress IP (EgressConfirmed: GAP-24), from a node that
		//    answers its liveness probe. Anything less leaves it provisioning: no audit this cycle, a red one, a green on the wrong IP, a node that
		//    does not answer. Whether the row was recorded AFTER this boot's registration, and that it is green, is re-checked by the store in the
		//    same statement that writes the promotion (Store.SettleProvisioningNode), so a stale row can never promote even if this decision were wrong.
		case n.Status == LifecycleProvisioning && n.AuditKnown && n.AuditGreen && n.EgressConfirmed && n.Live && n.AuditID != "":
			a.To, a.Apply, a.Reason, a.AuditID = LifecycleActive, true, "audit_promote", n.AuditID
			res.Alerts = append(res.Alerts, Alert{Kind: "promoted", Severity: "warn", NodeID: n.NodeID, Detail: "provisioning→active (green audit on the configured egress IP; self-promotion, OD-4)"})
		}

		res.Actions = append(res.Actions, a)
	}

	sort.Slice(res.Alerts, func(i, j int) bool {
		if res.Alerts[i].NodeID != res.Alerts[j].NodeID {
			return res.Alerts[i].NodeID < res.Alerts[j].NodeID
		}
		return res.Alerts[i].Kind < res.Alerts[j].Kind
	})
	return res
}

// blockTripped reports whether a node's block-rate crosses the quarantine threshold WITH enough
// samples (§5.3 guard 1 + §5.4). Below MinSamples the rate is noise → never tripped (GAP-27).
func blockTripped(n NodeSnapshot, p Params) bool {
	if n.Attempts < p.MinSamples {
		return false
	}
	return n.BlockRate() >= p.QuarThreshold
}

// blockElevated reports whether a node's block-rate is CLIMBING toward the trip — a LOWER bar than
// blockTripped (≥ ElevatedFraction × QuarThreshold), used ONLY for the fleet-wide systemic-spike
// signal (§5.5). It catches a subnet-wide spike while peers are still climbing, BEFORE they all cross
// the hard trip — the fix for the staggered cascade the single-cycle snapshot missed. A tripped node
// is always elevated. Same sample gate (below MinSamples the rate is noise).
func blockElevated(n NodeSnapshot, p Params) bool {
	if n.Attempts < p.MinSamples {
		return false
	}
	return n.BlockRate() >= p.ElevatedFraction*p.QuarThreshold
}

// withDefaults fills safe, conservative defaults for any unset param (start twitchy-averse — §5.4:
// "start conservative, longer window = less twitchy"). A zero FleetWideFraction would disable the
// brake, which is unsafe (GAP-28), so it defaults to a majority (0.5).
func withDefaults(p Params) Params {
	if p.MinSamples <= 0 {
		p.MinSamples = 20 // §5.3 GAP-27 recommendation
	}
	if p.QuarThreshold <= 0 {
		p.QuarThreshold = 0.30 // §5.4 recommend ≥30%
	}
	if p.RecoverThreshold <= 0 {
		p.RecoverThreshold = 0.05 // §5.4 recommend ≤5% (asymmetric, lower)
	}
	if p.RecoverGreens <= 0 {
		p.RecoverGreens = 3
	}
	if p.MinCanarySamples <= 0 {
		p.MinCanarySamples = 10 // §5.4/GAP-30: require real canary evidence before promoting (no zero-sample)
	}
	if p.LivenessMisses <= 0 {
		p.LivenessMisses = 3
	}
	if p.FleetWideFraction <= 0 {
		p.FleetWideFraction = 0.5 // §5.5: a majority of active nodes ELEVATED = systemic
	}
	if p.ElevatedFraction <= 0 {
		p.ElevatedFraction = 0.6 // §5.5: "climbing" = ≥60% of the trip threshold — catches the staggered spike early
	}
	if p.ElevatedFraction > 1 {
		p.ElevatedFraction = 1 // never require MORE than the hard trip to count as elevated
	}
	// MinActiveFloor defaults to 0 — meaning "no floor" would let block-rate empty a pool. That is
	// unsafe, so the SAFE default is 1: the last healthy active node is never auto-block-quarantined
	// (the pool can always still serve honest verdicts). An owner may raise it; 0 is coerced to 1.
	if p.MinActiveFloor < 1 {
		p.MinActiveFloor = 1
	}
	if p.BlockMode == "" {
		p.BlockMode = ModeRecommendOnly // DR-2 default
	}
	return p
}
