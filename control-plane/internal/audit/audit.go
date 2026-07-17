// Package audit is the Phase-3 /audit gate + node-lifecycle logic (P3 §4). It is the phase's
// spine — the go/no-go that decides whether a node may run :smtp — so it is small, pure, and
// exhaustively tested. It holds NO I/O: it maps a captured audit result + a node's expected
// identity to (a) the green/red signal, (b) the GAP-24 four-way IP cross-check, (c) the LOCKED
// eligibility rule, and (d) the lifecycle state transition. The scheduler/store/router call these
// pure functions; only the real IP *values* are deferred to a real box (PENDING-REAL-BOX) — the
// logic and its failing-case tests are done here, against a simulated audit surface.
//
// LOCKED rules encoded here (P3 §4.4 "non-negotiable", §6.6/§6.6.1 GAP-24):
//   - green ⇔ host_audit warnings == {} — nothing else (§4.1).
//   - a node is :smtp-eligible IFF status==active AND audit_status==green AND port25_open AND
//     audit-fresh AND (GAP-24) the audit ran on the node's EXPECTED public egress IP.
//   - quarantine on the FIRST red; require N-consecutive-green to auto-recover (OQ-P3-7 asymmetric).
//   - never mints valid/invalid — this package produces an eligibility decision, not a verdict.
package audit

import (
	"time"
)

// Status is the audit signal. Exactly two values (P3 §4.1): green iff warnings=={}, else red.
type Status string

const (
	StatusGreen Status = "green"
	StatusRed   Status = "red"
)

// Lifecycle is the SVG-⑦ node state machine (§4.5). "disabled" is NOT here — the operator on/off
// switch is the config descriptor's Enabled flag (config-list authoritative, P1 OQ-5), orthogonal
// to the lifecycle; the router requires Enabled AND status==active (§4.4), so a disabled node is
// simply Enabled=false regardless of lifecycle state. This keeps the `nodes.status` CHECK (0003:
// provisioning|active|quarantined) unchanged — no schema/vocab addition.
type Lifecycle string

const (
	LifecycleProvisioning Lifecycle = "provisioning"
	LifecycleActive       Lifecycle = "active"
	LifecycleQuarantined  Lifecycle = "quarantined"
)

// Result is a captured /v1/audit response, engine-agnostic (the node client fills it — from a real
// box or the simulated surface). Warnings=={} is the ONLY green (§4.1). CurrentHostIP is the
// audit's self-detected IP — the GAP-24 anchor (§6.6.1).
type Result struct {
	CurrentHostIP string            // host_audit.current_host_ip ("" => undetectable => a red-worthy IP warning)
	Warnings      map[string]string // host_audit warnings; empty => green (dns/ptr keys when red)
	AuditedAt     time.Time         // the wrapper's audited_at (RFC3339 UTC)
}

// DeriveStatus applies the ONLY green rule (§4.1): warnings must be empty. An undetectable
// current_host_ip is itself an audit failure (the gem reports it as a warning; we also treat an
// empty IP defensively as red so a box that can't determine its own IP is never eligible).
func DeriveStatus(r Result) Status {
	if len(r.Warnings) > 0 {
		return StatusRed
	}
	if r.CurrentHostIP == "" {
		return StatusRed // can't determine own IP → not trustworthy for :smtp (defensive; §4.1 IP dim)
	}
	return StatusGreen
}

// EligibilityInput is everything the LOCKED gate rule (§4.4) needs, all read off node state + the
// node's EXPECTED public egress IP (from the config descriptor / registry, §5.4 OQ-P3-11). The
// brain's registry is authoritative for the expected IP; the audit's self-detected IP is
// cross-checked against it (GAP-24). now + freshness give the staleness conjunct (§4.5).
type EligibilityInput struct {
	Status           Lifecycle // node lifecycle (must be active)
	AuditStatus      Status    // latest audit signal (must be green)
	Port25Open       bool      // egress smoke-test result (§3.4; separate from audit — GAP-15)
	LastAuditAt      time.Time // freshness anchor (zero => never audited => not fresh)
	LastAuditHostIP  string    // current_host_ip of the latest audit (the GAP-24 cross-check value)
	ExpectedEgressIP string    // the node's known public egress IP (brain registry authoritative)
	Now              time.Time
	MaxAuditAge      time.Duration // staleness window (§4.5 DIAL); <=0 => skip the freshness conjunct
}

// SkipReason is the diagnostic reason SMTP was NOT run for a node — it maps directly to the
// additive signals.smtp_skipped_reason enum (P3 §8: no_eligible_node|audit_red|provider_mx_routed|
// warmup_cap), PLUS internal-only finer reasons the router uses to explain a single node's
// exclusion. Only the four §8 tokens are ever written to signals; the finer ones fold into
// "no_eligible_node" at the routing layer (they explain WHY no node was eligible). Additive-only.
type SkipReason string

const (
	SkipNone           SkipReason = ""                   // eligible — SMTP runs
	SkipAuditRed       SkipReason = "audit_red"          // §8 token: node's audit is red (or IP undetectable)
	SkipNotActive      SkipReason = "not_active"         // lifecycle provisioning/quarantined
	SkipPort25Closed   SkipReason = "port25_closed"      // GAP-15: audit-green but egress 25 unproven/blocked
	SkipAuditStale     SkipReason = "audit_stale"        // §4.5: last audit older than MaxAuditAge
	SkipEgressMismatch SkipReason = "egress_ip_mismatch" // GAP-24: audit IP != expected public egress IP
	SkipNeverAudited   SkipReason = "never_audited"      // no audit yet (provisioning entry)
)

// Eligibility applies the LOCKED §4.4 rule and returns the FIRST failing conjunct as a SkipReason
// (SkipNone => eligible). The order surfaces the most decisive failure first for diagnosis, but the
// rule is a strict AND — any non-None reason means NOT eligible. This is the single source of the
// gate; the router calls it per candidate node. Because it is pure, its failing cases (red hard-
// blocks, port25-closed-but-green, egress mismatch, stale) are all provable locally against the
// simulated surface — only the real IP values are PENDING-REAL-BOX.
func Eligibility(in EligibilityInput) SkipReason {
	// 1. Lifecycle: only an active node runs :smtp (provisioning/quarantined are excluded, §4.4/§4.5).
	if in.Status != LifecycleActive {
		return SkipNotActive
	}
	// 2. Never audited → not fresh → excluded (a stale/absent green is not trusted, §4.5).
	if in.LastAuditAt.IsZero() {
		return SkipNeverAudited
	}
	// 3. Audit must be green — the hard PTR/DNS gate (§4.2/§4.4). A red audit is a HARD block.
	if in.AuditStatus != StatusGreen {
		return SkipAuditRed
	}
	// 4. Egress-25 must be proven — GAP-15: audit-green is necessary but NOT sufficient. A box can
	//    be green and unable to send a single SMTP packet; port25_open is the separate egress proof.
	if !in.Port25Open {
		return SkipPort25Closed
	}
	// 5. GAP-24 (§6.6.1) — the audit must have run on the node's EXPECTED public egress IP. If the
	//    audit self-detected a DIFFERENT IP (e.g. a multi-homed box audited its private interface),
	//    the green is meaningless: real MX see an un-PTR'd source and verifier_ip would lie. Cross-
	//    check ONLY when an expected IP is known (registry-configured); if unknown we cannot assert
	//    the invariant and must NOT run :smtp (fail-safe — the whole point of GAP-24 is to not trust
	//    a green whose IP we can't confirm). An empty audited IP also fails here (defensive).
	if in.ExpectedEgressIP == "" || in.LastAuditHostIP == "" || in.LastAuditHostIP != in.ExpectedEgressIP {
		return SkipEgressMismatch
	}
	// 6. Staleness — a green older than the window is not trusted until re-audited (§4.5). Skipped
	//    when MaxAuditAge<=0 (freshness conjunct disabled — used only where a scheduler guarantees
	//    freshness out of band).
	if in.MaxAuditAge > 0 && in.Now.Sub(in.LastAuditAt) > in.MaxAuditAge {
		return SkipAuditStale
	}
	return SkipNone
}

// Eligible is the boolean form of Eligibility (SkipNone).
func Eligible(in EligibilityInput) bool { return Eligibility(in) == SkipNone }

// SignalToken maps a router-internal SkipReason to the frozen §8 signals.smtp_skipped_reason enum.
// Only four tokens are ever persisted (Door-1 additive contract); the finer internal reasons
// (not_active, port25_closed, audit_stale, egress_ip_mismatch, never_audited) all explain WHY a
// node was excluded and fold into "audit_red" when they are the node's own health failure — EXCEPT
// that the router, when NO node is eligible at all, emits "no_eligible_node" (the capacity-gap
// token). This function returns the per-node token; the router decides no_eligible_node vs this.
func SignalToken(r SkipReason) string {
	switch r {
	case SkipAuditRed, SkipNotActive, SkipPort25Closed, SkipAuditStale, SkipEgressMismatch, SkipNeverAudited:
		// All are node-health/eligibility failures. The §8 enum's node-health token is "audit_red";
		// the finer distinction is preserved in logs/metrics, not in the frozen signal (additive-safe).
		return "audit_red"
	default:
		return ""
	}
}

// ── Lifecycle state machine (§4.5, OQ-P3-7 asymmetric: quarantine-on-first-red, N-green-recover) ──

// Transition is one state-machine step's decision. NextStatus is the lifecycle to persist;
// GreenStreak is the updated consecutive-green counter (reset to 0 on red; incremented on green).
type Transition struct {
	NextStatus  Lifecycle
	GreenStreak int
}

// NextState computes the lifecycle transition from the current state + the latest audit signal
// (§4.5). It is asymmetric by owner ruling (OQ-P3-7): a SINGLE red quarantines immediately
// (fail-safe — a red audit is cheap to recover from; the downside of running on a genuinely-red
// box is IP damage), but recovery requires recoverGreens CONSECUTIVE greens (debounce flapping).
// port25Open is required for the provisioning→active promotion (§4.5: "first audit green ∧
// port25_open"); SPF is a separate provisioning checklist item (GAP-17), checked out-of-band.
//
//   - provisioning + green ∧ port25_open (+ streak reached) → active
//   - active       + red                                    → quarantined (streak reset)
//   - quarantined  + green × recoverGreens                  → active
//   - any          + red                                    → quarantined (first red is fail-safe)
func NextState(cur Lifecycle, greenStreak int, latest Status, port25Open bool, recoverGreens int) Transition {
	if recoverGreens < 1 {
		recoverGreens = 1
	}
	if latest == StatusRed {
		// First red → quarantine, everywhere (fail-safe). Streak resets to 0.
		return Transition{NextStatus: LifecycleQuarantined, GreenStreak: 0}
	}
	// latest == green: advance the streak.
	streak := greenStreak + 1
	switch cur {
	case LifecycleProvisioning:
		// Promote to active on the first green ONLY if egress-25 is proven (§4.5, GAP-15). Without
		// port25_open the node stays provisioning (audit-green but can't send — not :smtp-ready).
		if port25Open && streak >= 1 {
			return Transition{NextStatus: LifecycleActive, GreenStreak: streak}
		}
		return Transition{NextStatus: LifecycleProvisioning, GreenStreak: streak}
	case LifecycleQuarantined:
		// Auto-recover only after N consecutive greens (debounce flapping, OQ-P3-7). Also require
		// egress still proven before returning to active.
		if port25Open && streak >= recoverGreens {
			return Transition{NextStatus: LifecycleActive, GreenStreak: streak}
		}
		return Transition{NextStatus: LifecycleQuarantined, GreenStreak: streak}
	case LifecycleActive:
		// Stays active on green (streak keeps climbing, harmless).
		return Transition{NextStatus: LifecycleActive, GreenStreak: streak}
	default:
		// Unknown state → be conservative: a green alone doesn't grant :smtp; treat as provisioning.
		if port25Open && streak >= recoverGreens {
			return Transition{NextStatus: LifecycleActive, GreenStreak: streak}
		}
		return Transition{NextStatus: LifecycleProvisioning, GreenStreak: streak}
	}
}
