// Package store is the Supabase/Postgres boundary. It defines the APPEND-ONLY intent
// (Door 2) as an interface shape, plus a pgx-backed implementation (postgres.go) and an
// in-memory fake (mem.go) for tests.
//
// LOCKED intents encoded here (enforced for real by the DB migrations + revoked grants):
//   - the LOG (verifications) is INSERT/SELECT only; UPDATE/DELETE are revoked from the
//     runtime service role (P1 §3.2.1). This interface offers AppendVerification + reads,
//     but NO update/delete of a verdict — the shape refuses mutation by construction.
//   - a separate grant-admin credential holds DDL (PN-9 / DR-3); the runtime store does not.
//   - the cache is keyed on the COMPOSITE (email_hash, config_fingerprint) (P1 §4.2, OQ-9),
//     never email_hash alone — a per-config verdict must not be served across configs.
package store

import (
	"context"
	"errors"
	"time"
)

// VerificationRow is the append-only LOG row (P1 §3.2.2), full provenance surface. This is
// what the verdict assembler writes for EVERY result (cheap verdicts + cache hits included).
// Pointer/typed-nil fields map to SQL NULL (invalid_syntax rows carry NULL email_hash — §3.2.7;
// no empty-string sentinels — a "" domain is written NULL).
type VerificationRow struct {
	ID        string    // UUIDv7 (idgen) — frozen join anchor (§4.7)
	RequestID string    // correlates to meta.request_id; NOT unique (§3.2.7)
	CheckedAt time.Time // verdict time, stored UTC
	CreatedAt time.Time // insert time (== CheckedAt normally)

	// Door 3 identity. EmailHash nil => NULL (+ HashScheme "" and Domain "" also NULL).
	EmailHash             []byte
	HashScheme            string
	Domain                string
	ProviderCanonicalHash []byte // OQ-2; nil => NULL (provider unknown)
	LocalPartLen          int    // 0 => NULL-ish; stored as-is (shape signal, DIAL)

	// Door 4 verdict (validated against vocab before this struct is ever built).
	Status         string
	Reason         string
	Confidence     int
	Recommendation string

	Flags   map[string]bool // -> jsonb
	Signals map[string]any  // -> jsonb

	MXProvider string // promoted typed column; "" => NULL

	PolicyKey     string
	PolicyVersion int
	APIVersion    string

	// Engine provenance — all NULL in P1 (no engine consulted on the cheap path).
	VerifierIP           string // "" => NULL
	NodeID               string // "" => NULL; FK-less
	EngineVersion        string // "" => NULL
	EngineCoreVersion    string // "" => NULL
	ConfigFingerprint    string // NOT NULL (§3.2.2)
	EngineValidationType string // "" => NULL
	EngineSuccessSet     bool   // whether EngineSuccess is meaningful (else NULL)
	EngineSuccess        bool

	FromCache bool
	LatencyMS int
}

// RawRow is the 1:1 sidecar (verification_raw) holding the untouched engine blob. Written
// only when an engine was consulted; nil in P1 (cheap path never has a blob).
type RawRow struct {
	VerificationID string
	Raw            []byte // raw engine JSON; the ONLY retention-managed payload (§3.2.6)
}

// CachedVerdict is a cache row (P1 §4.2). The key is the COMPOSITE (EmailHash, ConfigFingerprint).
type CachedVerdict struct {
	EmailHash         []byte
	ConfigFingerprint string
	Domain            string
	MXProvider        string
	HashScheme        string
	Status            string
	Reason            string
	Confidence        int
	Recommendation    string
	Flags             map[string]bool
	Signals           map[string]any
	CachedAt          time.Time
	ExpiresAt         time.Time
}

// PolicyRow is a stored policy definition (P1 §4.6). Definition is the open jsonb object.
type PolicyRow struct {
	Key        string
	Version    int
	Definition []byte // raw jsonb (open object — OQ-25)
	IsDefault  bool
}

// ── Phase 3: node health, audit history, warmup (all ADDITIVE — new tables, no frozen schema) ──

// NodeAuditRow is one APPEND-ONLY /v1/audit result (P3 §4.3, OQ-P3-5; migration 0007 node_audits).
// The gate reads the LATEST row per node; the flap history feeds the P4 health-watcher (SEAM → P4).
// CurrentHostIP is host_audit's self-detected IP — the GAP-24 anchor (§6.6.1): the brain
// cross-checks it against the node's expected public egress IP so a multi-homed box cannot pass
// audit-green on its PRIVATE IP while the real egress IP is unaudited.
type NodeAuditRow struct {
	ID            string         // UUIDv7
	NodeID        string         // FK-less (must survive node deletion)
	AuditedAt     time.Time      // audit time, UTC
	AuditStatus   string         // "green" (warnings=={}) | "red" — P3 §4.1
	Warnings      map[string]any // raw host_audit warnings ({} == green)
	CurrentHostIP string         // host_audit.current_host_ip; "" if undetectable (=> NULL)
}

// NodeHealth is the gate-relevant runtime state of a node (P3 §4.3/§4.4). It is read at routing
// time to enforce the LOCKED eligibility rule (§4.4): :smtp-eligible iff status==active AND
// audit_status==green AND port25_open AND audit-fresh AND (GAP-24) the audit ran on the expected
// public egress IP. Status is the lifecycle (provisioning|active|quarantined); the operator on/off
// switch is the config NodeDescriptor.Enabled (config-list authoritative, P1 OQ-5), NOT a status
// value — so the spec's "disabled" state maps to Enabled=false, needing no schema/CHECK change.
type NodeHealth struct {
	NodeID          string
	Status          string    // provisioning | active | quarantined (lifecycle, §4.5)
	AuditStatus     string    // "green" | "red" | "" (never audited)
	LastAuditAt     time.Time // zero => never audited
	Port25Open      bool      // set by the egress smoke test (§3.4), NOT by /audit (GAP-15)
	LastAuditHostIP string    // current_host_ip of the latest audit (the GAP-24 cross-check anchor)
}

// WarmupDecision is the outcome of reserving one :smtp slot against the ascending cap (P3 §5.2).
// Allowed=false means the window is already at its cap → the caller sets
// signals.smtp_skipped_reason="warmup_cap" and degrades to the cheap verdict (never queues — P5;
// never invalid). The counter is DURABLE (Supabase), so a brain restart cannot reset a fresh IP's
// ramp to full allowance mid-warmup (§5.2 LOCKED / PM4).
type WarmupDecision struct {
	Allowed     bool
	Count       int       // count AFTER a granted reservation, or the current count when denied
	Cap         int       // this window's ceiling
	WindowStart time.Time // start of the window the reservation fell in (UTC)
}

// ── Phase 4 (§4.4/§5.2/§5.4/§5.6) — durable rate budget, block-rate, lifecycle transitions ──

// RateParams are the token-bucket DIALs for one (node × provider) bucket, sourced from the policy
// rate_limits key (§4.2/§4.7). Capacity is the max burst (the bucket ceiling); RefillPerSec is the
// steady-state refill. They are passed on EVERY call (config-authoritative), mirroring how
// ReserveWarmupSlot takes `cap` each call — a policy edit takes effect on the next reservation.
type RateParams struct {
	Capacity     float64 // bucket ceiling (max burst); a DIAL
	RefillPerSec float64 // steady-state refill rate (tokens/sec); a DIAL
}

// RateDecision is the outcome of a rate-budget check (§4.4). On a RESERVE, Allowed=false means the
// bucket is empty → the caller degrades to cheap with signals.smtp_skipped_reason="rate_limited"
// (PROVISIONAL/non-cacheable, PB1; never queues — P5; never invalid — §4.6). TokensAfter is the
// remaining tokens (post-decrement on a granted reserve; the refilled count on a peek/deny). The
// budget is DURABLE (§4.4/OQ-P4-3): a brain restart refills ONLY by REAL elapsed wall-clock (never
// resets to full), so a freshly-restarted brain cannot let the pool sprint at a provider (the PM4
// hazard, mirrored from the P3 warmup counter). Contrast in-flight (ephemeral, §4.3/GAP-26).
type RateDecision struct {
	Allowed     bool
	TokensAfter float64
	Capacity    float64
}

// NodeBlockStats is the per-verifier_ip block-rate aggregation the health-watcher reads from the
// LOG (§5.2/§5.6). It counts ONLY node-attributable SMTP attempts: from_cache=false AND
// verifier_ip IS NOT NULL — a cache hit ran no SMTP on any node, and a cheap-degrade has
// verifier_ip=null (SMTP skipped, §4.6). Counting either would quarantine a node for verdicts it
// never produced (PB1, the same from_cache-excluded discipline P1 §3.2.1 locked, applied to the
// control loop). Blocked (smtp_blocked) is the PRIMARY numerator; Timeouts
// (smtp_timeout/connection_failed) are a SECONDARY, separately-thresholded signal (may be network,
// not reputation); greylisted is NOT counted (expected transient) — OQ-P4-6. block-rate =
// Blocked/Attempts, meaningful only above the sample-size gate (GAP-27).
type NodeBlockStats struct {
	VerifierIP string
	Attempts   int // node-attributable SMTP attempts this window (denominator)
	Blocked    int // smtp_blocked (primary numerator)
	Timeouts   int // smtp_timeout + connection_failed (secondary signal)
}

// MetricRow is the slim projection of a LOG row the observability layer needs (§8) — derived from
// EXISTING verifications columns (Door 2, no schema change). It is NOT the full VerificationRow (the
// metrics layer doesn't need the hash/provenance surface); just the fields the build-plan's metrics
// (block-rate, unknown-rate, latency, cache-hit) + pool/rate-pressure aggregate on.
type MetricRow struct {
	CheckedAt  time.Time
	Status     string
	Reason     string
	MXProvider string
	VerifierIP string
	FromCache  bool
	LatencyMS  int
	Signals    map[string]any // for smtp_skipped_reason (rate-saturation metric)
}

// ErrNotWired marks the no-DB stub. ErrCacheMiss is a normal (non-error) cache absence.
var (
	ErrNotWired  = errors.New("store: no database wired")
	ErrCacheMiss = errors.New("store: cache miss")
)

// applyRefill is the SINGLE source of the token-bucket math (§4.4/OQ-P4-3). Given a bucket's stored
// tokens + last-refill instant and the caller's params, it returns the refilled token count at
// `now` (capped at Capacity) and whether at least one whole token is available. It is used by BOTH
// the PG store and the Mem fake so the two can never diverge (the P3 review's test-double-fidelity
// lesson — divergences hid there twice). A NEGATIVE elapsed (clock skew, or a last_refill_at ahead
// of now) contributes ZERO refill — never grants free tokens. Because refill is derived from
// wall-clock elapsed since last_refill_at, a brain RESTART refills only by REAL time passed; it
// never resets the bucket to full (§4.4 restart-safe — the PM4 hazard the durable state prevents).
func applyRefill(stored float64, lastRefill time.Time, p RateParams, now time.Time) (refilled float64, ok bool) {
	elapsed := now.Sub(lastRefill).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	refilled = stored + p.RefillPerSec*elapsed
	if refilled > p.Capacity {
		refilled = p.Capacity
	}
	return refilled, refilled >= 1.0
}

// Store is the control-plane's only path to state. Note the deliberate ABSENCE of an
// UpdateVerification / DeleteVerification method — Door 2 append-only is enforced by the
// interface shape, not just by DB grants. A correction is a new AppendVerification row.
type Store interface {
	// AppendVerification INSERTs one LOG row (+ optional raw sidecar). INSERT only.
	AppendVerification(ctx context.Context, row VerificationRow, raw *RawRow) error

	// Cache: composite-keyed read/write (§4.2). GetCachedVerdict returns ErrCacheMiss when
	// absent or expired (never a stale verdict). PutCachedVerdict upserts.
	GetCachedVerdict(ctx context.Context, emailHash []byte, configFingerprint string) (CachedVerdict, error)
	PutCachedVerdict(ctx context.Context, v CachedVerdict) error

	// List membership reads (brain-owned lists, §4.4). Domains are norm/v1 (lowercased).
	IsDisposable(ctx context.Context, domain string) (bool, error)
	IsSpamTrap(ctx context.Context, domain string) (bool, error)
	// CatchAllState returns (isCatchall, known). known=false => never probed (§4.3 tri-state).
	CatchAllState(ctx context.Context, domain string) (isCatchall bool, known bool, err error)

	// SetCatchAll upserts a domain's catch-all conclusion (P2 §5.6 — the BRAIN persists, never
	// the box). isCatchall is tri-state via a *bool: nil == inconclusive, which is a NO-OP that
	// NEVER overwrites a previously-determined value with unknown (§5.6). method ∈ probe|seed|manual.
	// It is a mutation of brain STATE, not the append-only LOG — permitted (migration 0006 grant).
	SetCatchAll(ctx context.Context, domain string, isCatchall *bool, method string, expiresAt time.Time) error

	// GetDefaultPolicy / GetPolicy load policy definitions (§4.6).
	GetDefaultPolicy(ctx context.Context) (PolicyRow, error)
	GetPolicy(ctx context.Context, key string, version int) (PolicyRow, error)

	// ── Phase 3 (§4.3/§4.4/§4.5/§5.2) — all against NEW tables; the verdict LOG is untouched ──

	// AppendNodeAudit INSERTs one audit-history row (append-only, node_audits) AND upserts the
	// node's summary health columns (audit_status, last_audit_at, current_host_ip) on `nodes` in
	// the SAME transaction — so the latest-audit summary and the history can never diverge. It does
	// NOT touch status/port25_open (those move via the lifecycle transition / the egress test).
	AppendNodeAudit(ctx context.Context, row NodeAuditRow) error

	// GetNodeHealth returns the gate-relevant runtime state of a node (§4.4). known=false => the
	// node has no `nodes` row yet (never mirrored/audited) — the router treats it as NOT eligible.
	GetNodeHealth(ctx context.Context, nodeID string) (health NodeHealth, known bool, err error)

	// UpsertNodeState mirrors the config node descriptor + writes the lifecycle status/port25_open
	// (§3.6, §4.5). Used at boot (register provisioning) and on a lifecycle transition
	// (active/quarantined). It NEVER deletes; DELETE is a cp_admin action (migration 0007).
	UpsertNodeState(ctx context.Context, nodeID, baseURL, status string, port25Open bool) error

	// ReserveWarmupSlot atomically reserves one :smtp slot for a node in the given window against
	// `cap` (§5.2). It is the DURABLE ascending-cap gate: if the window is at cap it returns
	// Allowed=false (caller degrades to cheap, smtp_skipped_reason="warmup_cap"); otherwise it
	// increments the durable counter and returns Allowed=true. windowStart identifies the ramp
	// window (e.g. the UTC day); cap is that window's ceiling (a DIAL). Restart-safe (§5.2 LOCKED).
	ReserveWarmupSlot(ctx context.Context, nodeID string, windowStart time.Time, cap int) (WarmupDecision, error)

	// ── Phase 4 (§4.4/§5.2/§5.4/§5.6) ──

	// ReserveRateToken atomically consumes one token from the (node × provider) durable token
	// bucket (§4.4). It refills by REAL elapsed wall-clock since last_refill_at (never on restart —
	// OQ-P4-3 restart-safe), caps at p.Capacity, and — iff ≥1 token is available — decrements and
	// returns Allowed=true. Empty bucket → Allowed=false (caller degrades to cheap with
	// smtp_skipped_reason="rate_limited"; never queues/invalid — §4.6). now is passed so tests are
	// deterministic and the refill anchor is caller-controlled. It NEVER refills the allowance to
	// full on a restart — the whole point of durable budget state (the PM4 reputation hazard).
	ReserveRateToken(ctx context.Context, nodeID, providerBucket string, p RateParams, now time.Time) (RateDecision, error)

	// PeekRateBudget refills-and-reads the (node × provider) bucket WITHOUT consuming (§3.3 eligibility
	// "has rate-limit headroom for the target provider" — the router filter checks headroom before
	// selection). It never mutates the durable spend; it does advance last_refill_at as a read-side
	// refill (idempotent — the same tokens result on the next call). Allowed reports whether ≥1 token
	// is currently available.
	PeekRateBudget(ctx context.Context, nodeID, providerBucket string, p RateParams, now time.Time) (RateDecision, error)

	// BlockRateByVerifierIP aggregates per-node block-rate from the LOG over [since, now] (§5.2/§5.6).
	// It counts ONLY node-attributable SMTP attempts (from_cache=false AND verifier_ip NOT NULL) so
	// cache hits and cheap-degrades never inflate/deflate a node's rate (PB1). One row per verifier_ip
	// that has ≥1 attempt in the window. The watcher applies the sample-size gate (GAP-27) + fleet
	// brake (§5.5) on top — this method only measures, it never quarantines.
	BlockRateByVerifierIP(ctx context.Context, since time.Time) ([]NodeBlockStats, error)

	// MetricSince reads a bounded window of LOG rows (checked_at >= since, newest first, capped at
	// limit) for the observability metrics derivation (§8) — the "human dashboard" view (unknown-rate,
	// latency percentiles, cache-hit), derived from EXISTING columns (Door 2, no schema change). It is
	// deliberately bounded: at Phase-4 volumes (a small pool, honest-corporate :smtp) a windowed read
	// is proportional (GAP-25); limit<=0 uses a safe default. Truncated=true when the window had MORE
	// rows than limit (the caller LOGS the truncation — no silent cap). Control decisions never depend
	// on this read (the LOG is authoritative for block-rate via BlockRateByVerifierIP; OQ-P4-11).
	MetricRowsSince(ctx context.Context, since time.Time, limit int) (rows []MetricRow, truncated bool, err error)

	// SetNodeStatus writes ONLY the lifecycle status column (§5.4) — the health-watcher's single-
	// writer transition (active⇄quarantined⇄recovering). It does NOT touch port25_open/audit columns
	// (separate write paths), so the watcher can transition a node without clobbering the egress/audit
	// facts. status ∈ provisioning|active|quarantined|recovering (the 0008 CHECK). No row → no-op
	// (matches 0 rows; mirrors AppendNodeAudit's summary-update posture).
	SetNodeStatus(ctx context.Context, nodeID, status string) error

	// ResetWarmup resets a node's durable warmup counter for the given window to a LOW start (§5.4,
	// PM3): a block-quarantined node that recovers has a cooling IP and must re-warm gradually like a
	// fresh IP — never resume its pre-quarantine allowance. It upserts count=0 at the given cap for
	// the window so the next reservation starts the ramp over. Idempotent.
	ResetWarmup(ctx context.Context, nodeID string, windowStart time.Time, cap int) error

	Ping(ctx context.Context) error
	Close()
}

// StubStore satisfies Store without a DB, for building/smoke-testing the skeleton. Every
// method returns ErrNotWired (never a fabricated result) so a control-plane with no DB is
// honestly not-ready rather than silently returning fake state.
type StubStore struct{}

func (StubStore) AppendVerification(context.Context, VerificationRow, *RawRow) error {
	return ErrNotWired
}
func (StubStore) GetCachedVerdict(context.Context, []byte, string) (CachedVerdict, error) {
	return CachedVerdict{}, ErrNotWired
}
func (StubStore) PutCachedVerdict(context.Context, CachedVerdict) error { return ErrNotWired }
func (StubStore) IsDisposable(context.Context, string) (bool, error)    { return false, ErrNotWired }
func (StubStore) IsSpamTrap(context.Context, string) (bool, error)      { return false, ErrNotWired }
func (StubStore) CatchAllState(context.Context, string) (bool, bool, error) {
	return false, false, ErrNotWired
}
func (StubStore) SetCatchAll(context.Context, string, *bool, string, time.Time) error {
	return ErrNotWired
}
func (StubStore) GetDefaultPolicy(context.Context) (PolicyRow, error) {
	return PolicyRow{}, ErrNotWired
}
func (StubStore) GetPolicy(context.Context, string, int) (PolicyRow, error) {
	return PolicyRow{}, ErrNotWired
}
func (StubStore) AppendNodeAudit(context.Context, NodeAuditRow) error { return ErrNotWired }
func (StubStore) GetNodeHealth(context.Context, string) (NodeHealth, bool, error) {
	return NodeHealth{}, false, ErrNotWired
}
func (StubStore) UpsertNodeState(context.Context, string, string, string, bool) error {
	return ErrNotWired
}
func (StubStore) ReserveWarmupSlot(context.Context, string, time.Time, int) (WarmupDecision, error) {
	return WarmupDecision{}, ErrNotWired
}
func (StubStore) ReserveRateToken(context.Context, string, string, RateParams, time.Time) (RateDecision, error) {
	return RateDecision{}, ErrNotWired
}
func (StubStore) PeekRateBudget(context.Context, string, string, RateParams, time.Time) (RateDecision, error) {
	return RateDecision{}, ErrNotWired
}
func (StubStore) BlockRateByVerifierIP(context.Context, time.Time) ([]NodeBlockStats, error) {
	return nil, ErrNotWired
}
func (StubStore) MetricRowsSince(context.Context, time.Time, int) ([]MetricRow, bool, error) {
	return nil, false, ErrNotWired
}
func (StubStore) SetNodeStatus(context.Context, string, string) error       { return ErrNotWired }
func (StubStore) ResetWarmup(context.Context, string, time.Time, int) error { return ErrNotWired }
func (StubStore) Ping(context.Context) error                                { return ErrNotWired }
func (StubStore) Close()                                                    {}
