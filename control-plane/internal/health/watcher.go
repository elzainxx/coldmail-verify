package health

import (
	"context"
	"log"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/audit"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// This file: the Watcher ACTOR (§5.1) — the background control loop that gathers the three health
// signals per node, calls the pure Evaluate (decide.go), and APPLIES the decided transitions. It is
// the SINGLE WRITER of the audit/block-driven lifecycle (§5.1 LOCKED single-writer discipline — the
// router only READS nodes.status). It runs IN-PROCESS in the single brain (OQ-P4-14). The cadence
// lives in the caller (a ticker in main), keeping this actor a deterministic Tick() for tests — the
// same discipline as the P3 scheduler.
//
// It CONSUMES what P3 built (does not rebuild it): the audit signal + node_audits + the gate. It
// ADDS the continuous poll of /healthcheck (liveness) + the block-rate LOG query, and the block-rate
// trigger + hysteretic canary recovery + the fleet-wide brake — all decided in the pure Evaluate.

// BlockRateSource reads per-node block-rate from the LOG (§5.2/§5.6). Satisfied by store.Store.
type BlockRateSource interface {
	BlockRateByVerifierIP(ctx context.Context, since time.Time) ([]store.NodeBlockStats, error)
}

// StatusStore applies the watcher's transitions (§5.4). Satisfied by store.Store. SetNodeStatus is
// the single-writer lifecycle write; ResetWarmup re-warms a recovered node gently (PM3).
type StatusStore interface {
	GetNodeHealth(ctx context.Context, nodeID string) (store.NodeHealth, bool, error)
	SetNodeStatus(ctx context.Context, nodeID, status string) error
	ResetWarmup(ctx context.Context, nodeID string, windowStart time.Time, cap int) error
}

// WatchedNode is one pool member the watcher polls (§5.2). VerifierIP anchors the block-rate query
// (the P3 GAP-24 payoff: per-node block-rate is GROUP BY verifier_ip — §5.6). Empty VerifierIP means
// the node has no real egress yet (local build) → no block-rate signal, only audit/liveness.
type WatchedNode struct {
	audit.NodeRef        // ID, BaseURL, Token, PublicEgressIP
	VerifierIP    string // the node's verifier_ip for the block-rate GROUP BY (== PublicEgressIP on a real box)
}

// Deps wire the watcher (all interfaces so it tests against the sim surfaces + MemStore).
type Deps struct {
	Audit     *audit.Scheduler     // reuse the P3 audit actor for the audit signal + transition history
	Liveness  node.LivenessProbe   // /healthcheck poll (real or SimLivenessSurface)
	BlockRate BlockRateSource      // LOG block-rate query
	Status    StatusStore          // apply transitions
	Now       func() time.Time     // deterministic in tests
	Log       func(string, ...any) // structured log sink (defaults to log.Printf); alerts + transitions
}

// Watcher is the control loop. It holds the ephemeral per-node counters the pure evaluator needs
// (consecutive liveness misses + clean-canary green streak) — derived state, restart-safe by
// starting at 0 (a node re-earns its streak — fail-safe, the correct direction, mirrors the P3
// scheduler's greenStreaks).
type Watcher struct {
	deps         Deps
	params       Params
	blockWindow  time.Duration // §5.2 rolling block-rate window (DIAL)
	canaryCap    int           // §5.4 warmup cap to reset a recovered node to (re-warm gently, PM3)
	liveMisses   map[string]int
	greenStreaks map[string]int
	// blockQuarMemory is the CROSS-CYCLE cascade memory (§5.5/GAP-28 staggered fix): node_id →
	// cycles-remaining since it was block-quarantined. Nonzero for ANY node means "a block-quarantine
	// happened recently" — fed to Evaluate as RecentlyBlockQuarantined so a staggered, one-at-a-time
	// systemic drain (which never shows ≥2 tripped in one cycle) still trips the fleet brake. Decays
	// each cycle so a long-ago quarantine doesn't brake forever.
	blockQuarMemory map[string]int
}

// cascadeMemoryCycles is how many watcher cycles a block-quarantine stays "recent" for the cascade
// brake (§5.5). A DIAL in spirit; a few cycles is enough to see the next node in a staggered drain
// (each node crosses the rolling window within a handful of cadences of its peers).
const cascadeMemoryCycles = 3

// NewWatcher builds a watcher. blockWindow<=0 defaults to 30m (§5.2 DIAL, start conservative).
func NewWatcher(deps Deps, params Params, blockWindow time.Duration, canaryCap int) *Watcher {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Log == nil {
		deps.Log = log.Printf
	}
	if blockWindow <= 0 {
		blockWindow = 30 * time.Minute
	}
	if canaryCap <= 0 {
		canaryCap = 5 // a low re-warm start (§5.4/PM3)
	}
	return &Watcher{
		deps:            deps,
		params:          withDefaults(params),
		blockWindow:     blockWindow,
		canaryCap:       canaryCap,
		liveMisses:      map[string]int{},
		greenStreaks:    map[string]int{},
		blockQuarMemory: map[string]int{},
	}
}

// Tick runs ONE control-loop cycle over the pool (§5.1): (1) refresh the audit signal via the P3
// scheduler (which also drives the audit-red first-red quarantine + writes node_audits history);
// (2) poll liveness; (3) read block-rate from the LOG; (4) build the snapshot; (5) Evaluate (pure);
// (6) apply the AUTO transitions + log the recommendations/alerts. Returns the cycle Result so a
// test asserts on it directly. Deterministic given the injected surfaces + Now.
func (w *Watcher) Tick(ctx context.Context, pool []WatchedNode) Result {
	now := w.deps.Now()

	// (0) Decay the cross-cycle cascade memory (§5.5 staggered-brake fix): each countdown ticks toward
	//     0; a block-quarantine set it to cascadeMemoryCycles. While ANY node's memory is >0, Evaluate
	//     sees RecentlyBlockQuarantined and treats a fresh trip as a cascade-in-progress.
	for id, c := range w.blockQuarMemory {
		if c <= 1 {
			delete(w.blockQuarMemory, id)
		} else {
			w.blockQuarMemory[id] = c - 1
		}
	}

	// (1) Audit pass: OBSERVE each node's audit signal (fetch /v1/audit, derive green/red, GAP-24
	//     egress cross-check, append the history row + summary) — but WITHOUT a lifecycle transition.
	//     The P4 watcher is the SINGLE WRITER of the lifecycle (§5.1); the pure Evaluate below owns
	//     every transition (including the audit-red quarantine, A2). This is how P4 "replaces P3's
	//     minimal scheduler" (§1.2) without two writers racing on nodes.status. Observe returns the
	//     EFFECTIVE audit status (a green-on-wrong-IP is effectively red — GAP-24 preserved).
	auditByID := map[string]audit.Observation{}
	if w.deps.Audit != nil {
		refs := make([]audit.NodeRef, 0, len(pool))
		for _, n := range pool {
			refs = append(refs, n.NodeRef)
		}
		for _, obs := range w.deps.Audit.ObserveAll(ctx, refs) {
			auditByID[obs.NodeID] = obs
			if obs.Err != nil {
				w.deps.Log("health: node=%s audit error: %v", obs.NodeID, obs.Err)
			} else if obs.EgressMismatch {
				w.deps.Log("health: node=%s WARNING egress-IP != audited current_host_ip (GAP-24) — treated as red", obs.NodeID)
			}
		}
	}

	// (2)+(3) Liveness poll + block-rate query.
	live := map[string]bool{}
	for _, n := range pool {
		if w.deps.Liveness != nil {
			l := w.deps.Liveness.FetchLiveness(ctx, n.BaseURL, n.Token)
			live[n.ID] = l.Up
			if l.Up {
				w.liveMisses[n.ID] = 0
			} else {
				w.liveMisses[n.ID]++
			}
		} else {
			live[n.ID] = true // no probe wired → assume up (audit/block-rate still apply)
		}
	}
	blockByIP := map[string]store.NodeBlockStats{}
	if w.deps.BlockRate != nil {
		stats, err := w.deps.BlockRate.BlockRateByVerifierIP(ctx, now.Add(-w.blockWindow))
		if err != nil {
			w.deps.Log("health: block-rate query error: %v", err)
		} else {
			for _, s := range stats {
				blockByIP[s.VerifierIP] = s
			}
		}
	}

	// (4) Build the snapshot from the current (post-audit) health + the polled signals.
	fleet := make([]NodeSnapshot, 0, len(pool))
	for _, n := range pool {
		h, known, err := w.deps.Status.GetNodeHealth(ctx, n.ID)
		if err != nil {
			w.deps.Log("health: node=%s health read error: %v", n.ID, err)
			continue
		}
		if !known {
			continue // never registered → not part of the pool the watcher manages
		}
		snap := NodeSnapshot{
			NodeID:                   n.ID,
			Status:                   h.Status,
			AuditGreen:               h.AuditStatus == "green",
			AuditKnown:               h.AuditStatus != "",
			Live:                     live[n.ID],
			LiveMisses:               w.liveMisses[n.ID],
			GreenStreak:              w.greenStreaks[n.ID],
			RecentlyBlockQuarantined: w.blockQuarMemory[n.ID] > 0, // cross-cycle cascade memory (§5.5)
		}
		// Prefer THIS cycle's fresh observation over the stored summary when available (the summary is
		// what Observe just wrote, but a fetch error leaves the summary stale — an errored observation
		// means "no fresh audit" this cycle, which must NOT read as green). GAP-24 is folded in:
		// EffectiveGreen is false on an egress mismatch.
		if obs, ok := auditByID[n.ID]; ok {
			if obs.Err != nil {
				snap.AuditGreen = false // unreachable/errored audit is not green (harden-toward-unknown)
			} else {
				snap.AuditGreen, snap.AuditKnown = obs.EffectiveGreen, true
			}
		}
		if bs, ok := blockByIP[n.VerifierIP]; ok && n.VerifierIP != "" {
			snap.Attempts, snap.Blocked = bs.Attempts, bs.Blocked
		}
		fleet = append(fleet, snap)
	}

	// (5) Decide (pure — the irreversible surfaces live here).
	res := Evaluate(fleet, w.params)

	// (6) Apply auto-transitions; log recommendations + alerts.
	for _, a := range res.Actions {
		// Maintain the clean-canary green streak for recovering nodes (hysteresis input next cycle).
		w.updateStreak(a)
		if a.Apply && a.To != a.From {
			if err := w.deps.Status.SetNodeStatus(ctx, a.NodeID, a.To); err != nil {
				w.deps.Log("health: node=%s FAILED to apply %s->%s (%s): %v", a.NodeID, a.From, a.To, a.Reason, err)
				continue
			}
			w.deps.Log("health: node=%s %s->%s (%s, auto)", a.NodeID, a.From, a.To, a.Reason)
			// Record a block-rate quarantine in the cross-cycle memory so a STAGGERED systemic drain
			// (the next peer crossing the rolling threshold a few cycles later) trips the fleet brake
			// (§5.5). Only block_rate counts — audit_red/liveness are deterministic per-node faults, not
			// the redistribution cascade the brake guards.
			if a.Reason == "block_rate" && a.To == LifecycleQuarantined {
				w.blockQuarMemory[a.NodeID] = cascadeMemoryCycles
			}
			if a.ResetWarmup {
				// §5.4/PM3: a recovered node re-warms from a low start — reset the durable counter.
				if err := w.deps.Status.ResetWarmup(ctx, a.NodeID, warmupWindow(now), w.canaryCap); err != nil {
					w.deps.Log("health: node=%s warmup reset failed: %v", a.NodeID, err)
				}
			}
		} else if a.Recommend != "" {
			// Recommend-only (A1 default) or a fleet-brake hold — surfaced, NEVER auto-applied (DR-2).
			w.deps.Log("health: node=%s RECOMMENDATION (%s): %s", a.NodeID, a.Reason, a.Recommend)
		}
	}
	for _, al := range res.Alerts {
		w.deps.Log("health: ALERT [%s/%s] node=%s: %s", al.Severity, al.Kind, al.NodeID, al.Detail)
	}
	return res
}

// updateStreak advances/resets the ephemeral clean-canary green streak used by the next cycle's
// hysteresis (§5.4). Only the `recovering` state accumulates it (a canary earning its way back); any
// applied transition away from recovering resets it.
func (w *Watcher) updateStreak(a Action) {
	switch {
	case a.From == LifecycleRecovering && !a.Apply && a.To == LifecycleRecovering:
		// Still recovering, no transition this cycle → the canary stayed clean → advance the streak.
		w.greenStreaks[a.NodeID]++
	case a.To != a.From:
		// Any transition (recover, re-block, quarantine) resets the streak.
		w.greenStreaks[a.NodeID] = 0
	}
}

// warmupWindow truncates to the UTC day (matches the orchestrator's warmup window, §5.2).
func warmupWindow(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// Run drives Tick on a ticker until ctx is cancelled (§5.1 background loop). The cadence T is a DIAL
// (§5.1, recommend 30–60s). It ticks once immediately, then every T. This is the ONLY goroutine; the
// pure decision + single-writer discipline make it safe (§5.1). Block-rate-driven quarantine is
// recommend-only by default (DR-2) so this loop performs zero live block-rate mutation until A1 is
// promoted — the fresh-system invariant.
func (w *Watcher) Run(ctx context.Context, pool []WatchedNode, cadence time.Duration) {
	w.Tick(ctx, pool) // immediate first cycle
	if cadence <= 0 {
		return
	}
	t := time.NewTicker(cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx, pool)
		}
	}
}
