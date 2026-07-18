package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/idgen"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// This file: the Phase-3 MINIMAL scheduled re-audit + auto-quarantine actor (§4.5). It is the
// state-machine ACTOR the spec allows in P3 — NOT the P4 continuous health-watcher. The distinction
// is sharp (PM3): P3 builds only (a) the audit signal, (b) the gate (internal/audit + the router
// filter), and (c) a minimal scheduler that re-runs /v1/audit on a cadence and drives the lifecycle
// transition. It does NOT poll /health, does NOT watch block-rate (GAP-18 — block-rate-driven
// quarantine is P4), and does NOT manage a pool. Auto-quarantine-on-red is a state transition here,
// fail-safe (quarantine on the first red; recover only after N consecutive greens — OQ-P3-7).

// Deps are the scheduler's collaborators (all interfaces so it tests without a real box or DB).
type Deps struct {
	Client node.AuditClient // fetches /v1/audit (real HTTP or the simulated audit surface)
	Store  AuditStore       // persists the append-only history + reads/writes node health
	Now    func() time.Time
}

// AuditStore is the narrow store capability the scheduler needs. Satisfied by store.Store; kept
// minimal so the scheduler depends only on the audit-history + node-state reads/writes.
type AuditStore interface {
	AppendNodeAudit(ctx context.Context, row store.NodeAuditRow) error
	GetNodeHealth(ctx context.Context, nodeID string) (store.NodeHealth, bool, error)
	UpsertNodeState(ctx context.Context, nodeID, baseURL, status string, port25Open bool) error
}

// NodeRef is what the scheduler needs to audit one node: its id, base URL, token, and — critically —
// its EXPECTED public egress IP (the GAP-24 anchor: an audit whose current_host_ip != this is a red
// flag). RecoverGreens is the §4.5 N-consecutive-green recover count (OQ-P3-7).
type NodeRef struct {
	ID             string
	BaseURL        string
	Token          string
	PublicEgressIP string
}

// Scheduler runs re-audit cycles. It holds no goroutine — the caller (a ticker in main, or a test)
// invokes AuditNode / AuditOnce; keeping the loop OUT of this type makes it deterministic to test
// and keeps the "not a P4 daemon" boundary honest (the cadence lives in the caller, §4.5 DIAL).
type Scheduler struct {
	deps          Deps
	recoverGreens int
	// greenStreaks tracks per-node consecutive greens for the recover debounce (§4.5). It is derived
	// state (rebuildable from node_audits history), kept in-memory for the minimal scheduler; a
	// restart re-derives conservatively by starting at 0 (a node must re-earn its recover streak —
	// fail-safe, the correct direction). SEAM → P4 persists/derives it from history if needed.
	greenStreaks map[string]int
}

// NewScheduler builds a scheduler. recoverGreens<1 is clamped to 1 (§4.5 / OQ-P3-7).
func NewScheduler(deps Deps, recoverGreens int) *Scheduler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if recoverGreens < 1 {
		recoverGreens = 1
	}
	return &Scheduler{deps: deps, recoverGreens: recoverGreens, greenStreaks: map[string]int{}}
}

// AuditOutcome is one re-audit cycle's result (for logging/tests). Status is the derived audit
// signal; From/To are the lifecycle transition; EgressMismatch flags the GAP-24 red flag (§5.4
// cross-check) so an operator sees "audit went green but on the WRONG IP".
type AuditOutcome struct {
	NodeID         string
	Status         Status
	From           Lifecycle
	To             Lifecycle
	EgressMismatch bool
	Err            error
}

// AuditNode runs ONE re-audit cycle for a node (§4.5): fetch /v1/audit → derive green/red → append
// the history row (which also updates the node's audit summary) → compute the lifecycle transition
// → persist the new status. port25_open is NOT changed here (the egress smoke test owns it, §3.4);
// the scheduler reads the current port25_open from health to gate promotion.
//
// A fetch failure is NOT green: it records nothing and leaves the node's state as-is (an unreachable
// box stays whatever it was; the freshness conjunct in the gate will exclude it once stale — harden-
// toward-unknown). It returns the fetch error so the caller can log/alert.
func (s *Scheduler) AuditNode(ctx context.Context, n NodeRef) AuditOutcome {
	now := s.deps.Now()
	ar, err := s.deps.Client.FetchAudit(ctx, n.BaseURL, n.Token)
	if err != nil {
		return AuditOutcome{NodeID: n.ID, Err: fmt.Errorf("audit fetch: %w", err)}
	}

	result := Result{CurrentHostIP: ar.CurrentHostIP, Warnings: strMap(ar.Warnings), AuditedAt: parseAuditedAt(ar.AuditedAt, now)}
	status := DeriveStatus(result)

	// GAP-24 (§5.4/§6.6.1): the audit's self-detected IP must equal the node's expected public
	// egress IP. A mismatch means a green is meaningless (a multi-homed box audited the wrong iface).
	// We record the audit verbatim (history is truth) but flag the mismatch; a green-on-wrong-IP is
	// forced to NOT promote (treated as red for the transition — fail-safe). This is the runtime
	// cross-check the spec requires (Appendix B row 14).
	egressMismatch := n.PublicEgressIP != "" && ar.CurrentHostIP != "" && ar.CurrentHostIP != n.PublicEgressIP

	// Append the append-only history row (also updates nodes.audit_status/last_audit_at/host_ip).
	id, _ := idgen.NewV7String(now)
	if aerr := s.deps.Store.AppendNodeAudit(ctx, store.NodeAuditRow{
		ID: id, NodeID: n.ID, AuditedAt: result.AuditedAt, AuditStatus: string(status),
		Warnings: anyMap(ar.Warnings), CurrentHostIP: ar.CurrentHostIP,
	}); aerr != nil {
		return AuditOutcome{NodeID: n.ID, Status: status, Err: fmt.Errorf("append audit: %w", aerr)}
	}

	// Read current health for the transition inputs (current lifecycle + port25_open). If the node
	// has no row yet, treat it as provisioning (the boot upsert should have created it).
	h, known, herr := s.deps.Store.GetNodeHealth(ctx, n.ID)
	if herr != nil {
		return AuditOutcome{NodeID: n.ID, Status: status, Err: fmt.Errorf("read health: %w", herr)}
	}
	cur := LifecycleProvisioning
	port25 := false
	if known {
		if h.Status != "" {
			cur = Lifecycle(h.Status)
		}
		port25 = h.Port25Open
	}

	// A green-on-wrong-IP must NOT promote/recover — drive the transition as if RED (fail-safe).
	effective := status
	if egressMismatch {
		effective = StatusRed
	}
	tr := NextState(cur, s.greenStreaks[n.ID], effective, port25, s.recoverGreens)
	s.greenStreaks[n.ID] = tr.GreenStreak

	if tr.NextStatus != cur {
		if uerr := s.deps.Store.UpsertNodeState(ctx, n.ID, n.BaseURL, string(tr.NextStatus), port25); uerr != nil {
			return AuditOutcome{NodeID: n.ID, Status: status, From: cur, To: cur, EgressMismatch: egressMismatch, Err: fmt.Errorf("persist state: %w", uerr)}
		}
	}
	return AuditOutcome{NodeID: n.ID, Status: status, From: cur, To: tr.NextStatus, EgressMismatch: egressMismatch}
}

// Observation is one Observe cycle's result (P4 §5.1): the EFFECTIVE audit status (green only if
// derived-green AND no GAP-24 egress mismatch — a green-on-wrong-IP is effectively red) plus the
// raw derived status and the mismatch flag. It carries NO lifecycle transition — Observe records the
// audit FACT (history + summary) but leaves the state machine to the P4 watcher (internal/health),
// which is the single writer of the lifecycle (§5.1). Fetch failure → Err set, EffectiveGreen false.
type Observation struct {
	NodeID         string
	EffectiveGreen bool // green derived AND egress matches (GAP-24) — what the watcher trusts
	RawStatus      Status
	EgressMismatch bool
	Err            error
}

// Observe fetches /v1/audit, derives green/red, applies the GAP-24 egress cross-check, and appends
// the append-only history row (which also updates the node's audit_status/last_audit_at summary) —
// but does NOT compute or persist a lifecycle transition (P4 §5.1: the health-watcher owns the
// lifecycle; the scheduler here is reduced to the audit FETCH + record). This is how P4 "replaces
// P3's minimal scheduler" (§1.2) without a second writer racing on nodes.status. The GAP-24 invariant
// is preserved: a green whose current_host_ip != the expected egress IP yields EffectiveGreen=false,
// so the watcher treats it as red (never promotes it) — exactly as the P3 scheduler forced red for
// the transition. History records the RAW status (truth); the effective flag drives the decision.
//
// A fetch failure records nothing and returns Err — an unreachable box is NOT green (the watcher
// treats an errored observation as "no fresh audit"); harden-toward-unknown.
func (s *Scheduler) Observe(ctx context.Context, n NodeRef) Observation {
	now := s.deps.Now()
	ar, err := s.deps.Client.FetchAudit(ctx, n.BaseURL, n.Token)
	if err != nil {
		return Observation{NodeID: n.ID, Err: fmt.Errorf("audit fetch: %w", err)}
	}
	result := Result{CurrentHostIP: ar.CurrentHostIP, Warnings: strMap(ar.Warnings), AuditedAt: parseAuditedAt(ar.AuditedAt, now)}
	status := DeriveStatus(result)
	egressMismatch := n.PublicEgressIP != "" && ar.CurrentHostIP != "" && ar.CurrentHostIP != n.PublicEgressIP

	// Append the append-only history row (also updates nodes.audit_status/last_audit_at/host_ip). We
	// write the EFFECTIVE status to the summary so a green-on-wrong-IP surfaces as red to any reader of
	// the summary too (GAP-24 fail-safe) — while the raw warnings/current_host_ip stay in the row for
	// forensics. This differs from the P3 AuditNode (which wrote the raw status + used `effective` only
	// for its own transition); the P4 watcher reads the summary/observation, so the summary must not lie.
	effective := status
	if egressMismatch {
		effective = StatusRed
	}
	id, _ := idgen.NewV7String(now)
	if aerr := s.deps.Store.AppendNodeAudit(ctx, store.NodeAuditRow{
		ID: id, NodeID: n.ID, AuditedAt: result.AuditedAt, AuditStatus: string(effective),
		Warnings: anyMap(ar.Warnings), CurrentHostIP: ar.CurrentHostIP,
	}); aerr != nil {
		return Observation{NodeID: n.ID, RawStatus: status, EgressMismatch: egressMismatch, Err: fmt.Errorf("append audit: %w", aerr)}
	}
	return Observation{NodeID: n.ID, EffectiveGreen: effective == StatusGreen, RawStatus: status, EgressMismatch: egressMismatch}
}

// ObserveAll runs one Observe pass across all nodes (P4 §5.1 — the watcher's audit-signal refresh).
func (s *Scheduler) ObserveAll(ctx context.Context, nodes []NodeRef) []Observation {
	out := make([]Observation, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.Observe(ctx, n))
	}
	return out
}

// AuditAll runs one re-audit cycle across all nodes (§4.5 scheduled sweep). The caller drives it on
// a cadence (a ticker); AuditAll itself is a single pass so it is deterministic to test.
func (s *Scheduler) AuditAll(ctx context.Context, nodes []NodeRef) []AuditOutcome {
	out := make([]AuditOutcome, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.AuditNode(ctx, n))
	}
	return out
}

// ── small helpers ─────────────────────────────────────────────────────────────────────────

func strMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// parseAuditedAt parses the wrapper's RFC3339 audited_at; on any parse failure it falls back to the
// scheduler's now (freshness must never be a zero time, which the gate would treat as never-audited).
func parseAuditedAt(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return fallback
}
