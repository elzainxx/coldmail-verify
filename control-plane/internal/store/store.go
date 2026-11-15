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

// ── Phase 5 (§3.3/§5/§7) — the durable async queue types ─────────────────────────────────────

// WorkItemRow is one durable queue item (§3.3). It carries the plaintext address as ENCRYPTED
// bytes (AddrCiphertext — DR-4/ADR-0002, sealed by the queue layer with a key separate from the
// pepper); the store is crypto-AGNOSTIC (it persists/returns the ciphertext, never plaintext, and
// never holds the DR-4 key). The plaintext lives only in the queue/orchestrator layer at attempt
// time; AddrCiphertext is NULLed in the settlement transaction (§5.3 step 4).
type WorkItemRow struct {
	ID                string    // UUIDv7 — the lease unit AND the AEAD AAD (queue layer seals against it)
	IdempotencyKey    []byte    // sha256(email_hash‖policy_key‖policy_version‖config_fingerprint) — §5.1
	JobID             string    // owning bulk job; "" => a singular queued check
	Domain            string    // in-clear (Door 3) — group-by-domain ordering + P4 sticky key
	EmailHash         []byte    // Door-3 identity for cache lookup + LOG join
	PolicyKey         string
	PolicyVersion     int
	ConfigFingerprint string
	Status            string // ready | in_flight | waiting_retry | settled | dead (§3.5)
	Attempt           int
	NotBefore         time.Time
	LockedBy          string
	LockedAt          time.Time
	LastReason        string
	VerificationID    string    // FK-less; set at settle (GAP-33). Non-empty => already settled.
	AddrCiphertext    []byte    // DR-4 AES-256-GCM ciphertext of the plaintext address; NULL post-settle
	CreatedAt         time.Time
	SettledAt         time.Time
}

// JobRow is one bulk job handle (§7.2) — brain-side verification-work state ONLY (no user id,
// credits, CSV, filename — §7.6/GAP-36). caller_ref is opaque, echoed, never interpreted.
type JobRow struct {
	ID            string
	PolicyKey     string
	PolicyVersion int
	Status        string // submitted | running | complete | complete_with_unknowns (§7.3)
	TotalCount    int
	SettledCount  int // denormalized progress (GAP-38); completion is authoritative, never this
	SubmittedAt   time.Time
	CompletedAt   time.Time
	CallerRef     string // opaque (§7.6); the sole platform coupling
	// OwnerUserID is the PLATFORM user that owns this job (0013 / fix-plan item 1, D1). Stamped at
	// submit from the AUTHENTICATED key's api_callers.user_id — never from the request body, so a
	// caller cannot assert ownership. Every read path predicates on it. Empty string models a NULL
	// owner (a job submitted before ownership was recorded): it matches NO caller, so legacy rows
	// are read-CLOSED, not read-open. DISTINCT from CallerRef, which is untrusted body text.
	OwnerUserID string
}

// WorkItemAttemptRow is one per-attempt transcript (OQ-P5-8) — append-only, distinct from the
// verdict LOG. Any transcript in Signals must already be Door-3 address-redacted (ADR-0008).
type WorkItemAttemptRow struct {
	ID          string
	WorkItemID  string
	Attempt     int
	AttemptedAt time.Time
	NodeID      string
	VerifierIP  string
	Reason      string
	Signals     map[string]any
	LatencyMS   int
}

// WorkItemResult is one settled work-item paired with its verdict (for ListJobResults, §7.1).
type WorkItemResult struct {
	WorkItemID string
	EmailHash  []byte
	Verdict    VerificationRow // the settled verdict from the LOG (joined via verification_id)
}

// ── Phase 6 (§5/§6) — the external-verifier spend record + provenance sidecar ──────────────────

// ExternalCallRow is one append-only external-verifier call transcript (P6 §6.1) — the spend record
// AND the P7 provenance the weaning decision runs on. It is DISTINCT from the verdict LOG (a sidecar,
// the P5 work_item_attempts pattern): it records what the vendor said + what the call cost, without
// bloating the verdict row. It stores NO plaintext — only email_hash (Door 3), for SAR/deletion
// queryability (PN-10). Recorded INSIDE the settlement CAS for a queued escalation (spend-once,
// GAP-42) so a lost-lease attempt's spend record rolls back with its (discarded) settlement.
type ExternalCallRow struct {
	ID                  string    // UUIDv7
	VerificationID      string    // FK-less pointer to the settled verdict (join the rented population); "" if none
	WorkItemID          string    // "" for a sync call; the queue item id for a queued escalation
	JobID               string    // "" for a singular/sync check; the bulk job (per_job budget, §5.2)
	Vendor              string    // which adapter answered — the per-vendor budget/breaker key (§5.3)
	ResidueClass        string    // which of the 4 §4.1 classes triggered escalation (§6.2)
	EmailHash           []byte    // Door-3 identity for SAR/deletion (PN-10); NEVER plaintext
	VendorStatusRaw     string    // the vendor's own status string, verbatim (§6.2 — P7 measures their grade)
	VendorConfidenceRaw int       // the vendor's own confidence/score if any (§6.2)
	RequestID           string    // vendor call id (idempotency + audit trace to a billed call, §5.4)
	CostUnits           int       // what the call cost — spend accounting (§5, §6.2). 0 for an errored call.
	Outcome             string    // "ok" | "error" — feeds the error-rate breaker condition (§5.3)
	LatencyMS           int       // vendor round-trip (§6.2)
	CalledAt            time.Time // when the call was made (passed in — no wall-clock in the store)
}

// ExternalSpend is the durable spend/error state the breaker reasons over (§5.2/§5.3), derived from
// windowed reads of the append-only external_calls table (so it survives a brain restart — the
// PM4-analogue hazard for MONEY). The store returns store-local counts; the orchestrator maps them to
// external.SpendFacts (the store never imports the external package — layering).
type ExternalSpend struct {
	SpentDay        int // cost-units in the rolling day window (ok calls)
	SpentMonth      int // cost-units in the rolling month window (ok calls)
	SpentJob        int // cost-units this bulk job has spent (0 when jobID == "")
	Calls           int // total calls in the error-rate window (ok + error) — the denominator
	Errors          int // errored calls in the error-rate window — the numerator
	CallsLastMinute int // total calls in the last rolling minute — the rate_ceiling brake (§5.2)
}

// ── Phase 7 (§3/§5/§8) — the flywheel: outcomes, the calibration store, the recalibration CAS ──

// SendOutcomeRow is one arriving send outcome (P7 §3.2 shape, filled into the P1 send_outcomes table).
// The sender supplies VerificationID/MessageID/Event/SentAt/OccurredAt/Raw; P7 backfills the rollup
// dims + the classifier fields at ingestion. Door 3: it stores email_hash (rollup) + domain, NEVER
// plaintext. CalibrationClass/IngestError are the P7-owned annotations (migration 0012) — the
// classifier's decision + the reject-and-flag integrity marker (§3.4/GAP-51).
type SendOutcomeRow struct {
	ID             string    // UUIDv7 (time-ordered)
	VerificationID string    // the authoritative verdict-level join anchor (§3.1); "" => no prior verdict (GAP-51 flag)
	EmailHash      []byte    // address rollup (§3.1 — NOT the primary join); the historical-bounce rollup key (§4.6)
	Domain         string    // rollup dimension (captured at outcome time)
	MXProvider     string    // the P1-promoted provider dimension (§5.2 bucket key)
	MessageID      string    // sender's message id — idempotency (§3.4, with Event)
	Event          string    // the RAW provider event → normalized by the classifier (never trusted as-is, §3.2)
	SentAt         time.Time // temporal attribution (§3.3) — must be >= the verdict's created_at
	OccurredAt     time.Time // when the bounce/delivery happened
	Raw            []byte    // untouched provider event (jsonb) — the fidelity copy exclusions re-audit against

	// P7-owned classifier annotations (migration 0012). Set at ingestion, AFTER the join classifies it.
	CalibrationClass string // "invalid_signal"|"valid_signal"|"zero_information"|"excluded"|"" (unclassified)
	IngestError      string // "" (clean) | "unknown_verification_id"|"sent_before_verdict"|"malformed"|"unauthenticated"
	IsExternalVerified bool // the joined verdict was reason=external_verified (bucket SEPARATELY, §5.2/GAP-54)
	ResidueClass     string // the P6 residue class of the joined rented verdict (external bucket key); "" if SMTP-earned
	Vendor           string // the vendor of the joined rented verdict (external bucket key); "" if SMTP-earned
	FromCache        bool   // the joined verdict was from_cache=true (calibration-excluded, §4.4)
	IngestedAt       time.Time
}

// VerdictJoin is the projection of a verifications row the ingestion join needs (§3.1/§3.3/§4.4) — the
// verdict fields that decide how an outcome is classified/excluded, WITHOUT reading the full row. It is
// read by verification_id (the authoritative anchor). Known=false => no such verdict (GAP-51: reject).
type VerdictJoin struct {
	Status       string
	Reason       string
	Confidence   int
	MXProvider   string
	FromCache    bool      // §4.4: a cache-hit verdict's outcome is calibration-EXCLUDED (not a fresh check)
	IsOverride   bool      // §4.4: a whitelist/blacklist override verdict is calibration-EXCLUDED
	CreatedAt    time.Time // §3.3: an outcome whose sent_at precedes this is a temporal contradiction (reject)
	EmailHash    []byte
	Domain       string
	ResidueClass string // from signals.external.residue_class when reason=external_verified (GAP-54 bucket)
	Vendor       string // from signals.external.vendor when reason=external_verified
}

// CalibrationRow is one versioned confidence-store row (P7 §5.4 / PN-14 sketch). The bucket is
// (Reason × MXProvider); Vendor+ResidueClass are set ONLY for the parallel external-verified bucket
// space (both-null or both-set, §5.2/GAP-54). EffectiveFrom==zero => a PROPOSAL (recommend-only, not
// read by the verdict path); the verdict path reads the CURRENT effective row (EffectiveFrom set,
// EffectiveTo zero). Versioned so a bad cycle is revertible (§6.4).
type CalibrationRow struct {
	ID            string
	Reason        string
	MXProvider    string
	Vendor        string // "" => SMTP-earned bucket; non-"" => rented (external) bucket (GAP-54)
	ResidueClass  string // "" => SMTP-earned; non-"" => the P6 residue class (external bucket)
	Confidence    int
	SampleN       int
	Version       int
	Mode          string // "shadow"|"recommend_only"|"auto" — the mechanism mode at write time
	Approved      bool
	EffectiveFrom time.Time // zero => proposal only (not effective)
	EffectiveTo   time.Time // zero => still effective (when EffectiveFrom set)
	CreatedAt     time.Time
}

// BucketAggregate is the recalibration read (§5.2/§5.3): calibratable outcomes grouped by bucket, with
// the counts the four guards + the accuracy metric need. It EXCLUDES zero-information, from_cache,
// override, and ingest_error rows at the SQL layer (§4.3/§4.4) — the exclusion discipline enforced at
// read time. External-verified rows come back with Vendor/ResidueClass set (bucketed separately).
type BucketAggregate struct {
	Reason       string
	MXProvider   string
	Vendor       string // non-"" => an external-verified bucket (measured separately, GAP-54)
	ResidueClass string
	QualifyingN  int     // calibratable outcomes in this bucket (invalid_signal + weighted valid_signal)
	InvalidN     int     // invalid_signal (mailbox_not_found) outcomes — the numerator (§8.1)
}

// AccuracyRow is the §8 per-provider accuracy read: of valid-labeled verdicts that got a calibratable
// outcome, how many were false-valid (invalid-signal). Carries the coverage denominator (ValidLabeled)
// so the §8.2 "never a bare number" rule is enforceable (see flywheel.Accuracy).
type AccuracyRow struct {
	MXProvider        string
	Vendor            string // "" => SMTP-earned; non-"" => rented population (weaning compares the two)
	ResidueClass      string
	ValidLabeled      int // valid-labeled verdicts in scope (coverage denominator)
	CalibratableValid int // of those, how many got a calibratable outcome (accuracy denominator)
	FalseValid        int // of those, how many hard-bounced invalid-signal (accuracy numerator)
}

// RecalRun is the recalibration lease/CAS record (P7 §5.5/GAP-53) — the exactly-once writer's lease.
// BucketsTouched (denominator) AND BucketsAtMax (numerator) are BOTH persisted so the §6.4 cascade
// brake's ROLLING window can sum both across prior cycles — without the numerator, prior cycles would
// contribute 0 to the at-max count and the staggered-drift detector would be defeated (the P4 lesson-a).
type RecalRun struct {
	ID             string
	Status         string // leased | committed | discarded | failed
	LeasedBy       string
	LeasedAt       time.Time
	WindowFrom     time.Time
	WindowTo       time.Time
	BucketsTouched int
	BucketsAtMax   int
	BrakeHeld      bool
}

// ErrNotWired marks the no-DB stub. ErrCacheMiss is a normal (non-error) cache absence.
var (
	ErrNotWired  = errors.New("store: no database wired")
	ErrCacheMiss = errors.New("store: cache miss")
	// ErrOutcomeDuplicate is returned by AppendSendOutcome when the (message_id, event) dedup key
	// already exists (§3.4): a replayed webhook delivery is a NO-OP, not an error to retry — the
	// exactly-once INGESTION guarantee firing (the input-side complement of the recalibration CAS).
	ErrOutcomeDuplicate = errors.New("store: send-outcome duplicate (message_id, event) — no-op")
	// ErrRecalLeaseLost is the recalibration writer's CAS firing (§5.5/GAP-53): a cycle whose lease was
	// reclaimed (slow, not dead) matches zero rows on commit and DISCARDS its bucket writes — the
	// P5/P6 exactly-once invariant transferring a third time (settle-once → spend-once → recalibrate-once).
	ErrRecalLeaseLost = errors.New("store: recalibration lease lost (CAS matched zero rows)")
	// ErrLeaseLost is returned by a CAS settle/requeue when the WHERE (id, locked_by=<me>,
	// status='in_flight') matched ZERO rows (GAP-37): the lease was reclaimed or the item already
	// transitioned. The caller DISCARDS its result — it neither settled nor requeued. This is not a
	// failure to retry; it is the exactly-once guarantee firing (another attempt owns the item).
	ErrLeaseLost = errors.New("store: work-item lease lost (CAS matched zero rows)")
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
	// (active/quarantined). It NEVER deletes; DELETE is a verify_admin action (migration 0007).
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

	// ── Phase 5 (§3.3/§5/§7) — the durable async queue ──

	// EnqueueWorkItem inserts one work-item, DEDUPING on idempotency_key (§5.5): if the key already
	// exists, it is a no-op returning alreadyExists=true (ON CONFLICT DO NOTHING). The caller seals
	// AddrCiphertext (DR-4) before calling — the store persists ciphertext, never plaintext. itemID
	// is the row's id (the caller's WorkItemRow.ID, which is also the AEAD AAD).
	EnqueueWorkItem(ctx context.Context, item WorkItemRow) (itemID string, alreadyExists bool, err error)

	// DequeueReady leases up to `limit` ready items (status IN (ready,waiting_retry) AND
	// now>=not_before) via FOR UPDATE SKIP LOCKED, in group-by-domain order (§6.2), setting
	// status='in_flight', locked_by=workerID, locked_at=now. The caller runs each through the
	// UNCHANGED P4 router+gates (§8) and then settles/requeues via the CAS methods below.
	DequeueReady(ctx context.Context, workerID string, limit int, now time.Time) ([]WorkItemRow, error)

	// SettleWorkItem is the GAP-37/33/38/42 conditional-CAS settlement (§5.3 + P6 §5.4), all in ONE
	// transaction:
	// (1) INSERT the one verifications LOG row (+ optional raw sidecar) — Door 2 append;
	// (2) CAS UPDATE work_items SET status=<terminal>, verification_id=verdict.ID, settled_at,
	//     addr_ciphertext=NULL, lease cleared  WHERE id=itemID AND locked_by=workerID AND
	//     status='in_flight';  (3) if job-owned, jobs.settled_count++;  (4) INSERT the attempt row;
	// (5) P6 SPEND-ONCE (GAP-42): if extCall != nil (a paid vendor call was made for this settle),
	//     INSERT the external_calls spend record IN THE SAME TX. Because the vendor call + its
	//     cost-record + the settle all commit under the ONE CAS on the lease-holder, a lost/reclaimed
	//     attempt matches ZERO rows and the WHOLE tx rolls back — the spend record is discarded with
	//     the (discarded) settlement, so a slow-drainer reclaim can never double-charge. This is the
	//     GAP-37 invariant extended from "settle-once" to "spend-once".
	// terminal is "settled" (a real verdict) or "dead" (retry-exhausted — still logs the reserved
	// unknown/greylisted verdict). If the CAS matches ZERO rows the whole tx ROLLS BACK (no LOG
	// row, no spend row — GAP-33/42) and it returns ErrLeaseLost — the lost attempt discards its result.
	SettleWorkItem(ctx context.Context, itemID, workerID, terminal string, verdict VerificationRow, raw *RawRow, attempt WorkItemAttemptRow, extCall *ExternalCallRow) error

	// AppendExternalCall records a SYNC-path external-verifier call (§6.1) — the append-only spend +
	// provenance record for a /v1/verify escalation (no work-item, no CAS: the sync path settles by
	// logging, so the spend record is a plain append alongside the verdict). INSERT-only (append-only,
	// mirrors work_item_attempts). For a QUEUED escalation the spend record rides SettleWorkItem's CAS
	// instead (spend-once, GAP-42) — never both.
	AppendExternalCall(ctx context.Context, call ExternalCallRow) error

	// ExternalSpendSince returns the durable per-vendor spend/error facts for the breaker (§5.2/§5.3):
	// cost-units + call/error counts over the day, month, error-rate, and last-minute (rate_ceiling)
	// windows. jobID scopes the per-job spend (SpentJob; "" => 0). Derived from the append-only
	// external_calls table so a restart cannot reset the window (the MONEY analogue of the P4
	// restart-safe budget). now anchors the windows; minuteStart is the rate_ceiling minute boundary.
	ExternalSpendSince(ctx context.Context, vendor, jobID string, dayStart, monthStart, errWindowStart, minuteStart, now time.Time) (ExternalSpend, error)

	// RequeueWorkItem is the GAP-37 conditional-CAS requeue (§4.2): CAS UPDATE work_items SET
	// status='waiting_retry', attempt=(bumpAttempt? attempt+1 : attempt), not_before=notBefore,
	// last_reason=reason, lease cleared  WHERE id=itemID AND locked_by=workerID AND
	// status='in_flight'; then INSERT the attempt row. bumpAttempt is false for a rate_limited /
	// no-eligible-node capacity wait (§4.6/OQ-P5-7 — a capacity wait must not burn the greylist
	// retry budget) and true for a provider-transient (greylist/timeout). Zero rows => ErrLeaseLost.
	RequeueWorkItem(ctx context.Context, itemID, workerID string, notBefore time.Time, reason string, bumpAttempt bool, attempt WorkItemAttemptRow) error

	// ReclaimExpiredLeases resets in_flight items whose lease is older than leaseTTL back to
	// dequeueable (ready, or waiting_retry when not_before is still in the future), clearing the
	// lease (§5.2). The lease TTL MUST exceed the brain's max SMTP deadline (GAP-37) so a live
	// attempt never loses its lease — a reclaim then provably means the holder is dead. Returns the
	// count reclaimed (for observability / the drainer-liveness alert).
	ReclaimExpiredLeases(ctx context.Context, leaseTTL time.Duration, now time.Time) (reclaimed int, err error)

	// GetWorkItem reads one work-item by id (progress/debugging). known=false => no such row.
	GetWorkItem(ctx context.Context, itemID string) (item WorkItemRow, known bool, err error)

	// CreateJob inserts a jobs row (§7.2) and returns its id. job.OwnerUserID is the platform user
	// the submitting key acts for (0013) — the value every read below predicates on.
	CreateJob(ctx context.Context, job JobRow) (jobID string, err error)

	// ── THE OWNER PREDICATE (0013 / fix-plan item 1, D1; contract §1.4) ──
	//
	// All three job reads take ownerUserID and scope on it IN THE QUERY. This signature is the fix:
	// before 0013 they took jobID alone, so any authenticated caller could read any caller's job
	// status and verdicts. Passing the owner is therefore not optional plumbing — it is the only
	// thing standing between two tenants, and the compiler now refuses a call that omits it.
	//
	// A job that exists but belongs to someone else, and a job that does not exist, are
	// INDISTINGUISHABLE: both come back known=false / zero rows, and the handler maps both to the
	// SAME 404. Never 403 — a 403 would confirm the id exists, which is exactly the
	// existence-disclosure the senders' tenancy posture forbids.
	//
	// An EMPTY ownerUserID matches NOTHING (not "match all", not "match NULL owners"). That keeps
	// the fail-safe direction: an unauthenticated-or-ownerless caller reads no job at all, and the
	// pre-0013 rows whose owner was never captured stay closed.

	// GetJob reads one job by id, SCOPED TO ITS OWNER (progress polling). known=false => no such
	// job FOR THIS OWNER — which deliberately covers both "no such id" and "not yours".
	GetJob(ctx context.Context, jobID, ownerUserID string) (job JobRow, known bool, err error)

	// JobCompletion judges completion AUTHORITATIVELY (§7.3/GAP-38): complete iff
	// NOT EXISTS(work_items WHERE job_id=jobID AND status NOT IN ('settled','dead')). It also
	// returns the (denormalized) total/settled counts for cheap progress display — but completion
	// is NEVER settled==total (the counter can drift under the reclaim/no-op path). SCOPED TO ITS
	// OWNER: a job owned by another caller returns ErrNotWired, exactly as a nonexistent one does.
	JobCompletion(ctx context.Context, jobID, ownerUserID string) (complete bool, total, settled int, err error)

	// ListJobResults returns settled work-items for a job joined to their verdicts, cursor-paginated
	// (§7.1). cursor is the last-seen work_item.id ("" => from the start); nextCursor is "" at the end.
	// SCOPED TO ITS OWNER via a join back to jobs — a cross-tenant call yields ZERO ROWS, never a
	// partial page. This is the path that leaked full verification results (PII) under D1.
	ListJobResults(ctx context.Context, jobID, ownerUserID, cursor string, limit int) (results []WorkItemResult, nextCursor string, err error)

	// ── Phase 5 (§7.5 / PN-13) — per-caller front-door auth ──

	// LookupCaller resolves an authenticated caller from the sha256 of a presented bearer key
	// (PN-13). It returns the caller_id ONLY when a row matches AND now ∈ [not_before, not_after)
	// — expiry is enforced in the query, so a rotated-out key (not_after in the past) yields
	// ok=false. keySHA256 is sha256(raw key); the raw key is never stored (§7.5). A miss (unknown
	// or expired key) is ok=false, NOT an error — the caller maps that to 401. This is a hot,
	// read-only lookup (verify_app has SELECT only on api_callers; issuance/rotation are verify_admin).
	//
	// It ALSO returns ownerUserID — the platform user the key acts for (api_callers.user_id, 0013).
	// This is where the job-owner value comes from, and the reason it is returned HERE rather than
	// read from the request: the identity is KEY-DERIVED, so a caller cannot assert it (contract
	// §1.4 clause 6, the credential-based option). "" => the key has no platform user recorded
	// (pre-0013 key, or the legacy static-token path); such a caller can still submit, but its
	// jobs are stamped with a NULL owner and are therefore read-closed rather than world-readable.
	LookupCaller(ctx context.Context, keySHA256 []byte, now time.Time) (callerID, ownerUserID string, ok bool, err error)

	// ── Phase 7 (§3/§5/§8) — the flywheel ──

	// GetVerdictForJoin reads the join projection of a verdict by id (§3.1). known=false => no such
	// verdict (the ingestion path flags ingest_error=unknown_verification_id and excludes it — GAP-51).
	GetVerdictForJoin(ctx context.Context, verificationID string) (VerdictJoin, bool, error)

	// AppendSendOutcome inserts one classified send outcome (§3), DEDUPING on (message_id, event) —
	// a replayed delivery returns ErrOutcomeDuplicate (a no-op, §3.4). The caller has already joined +
	// classified the outcome (CalibrationClass/IngestError/rollup dims set). INSERT-only append; the
	// classifier annotations are set at insert (not a later UPDATE) so a duplicate can't reclassify.
	AppendSendOutcome(ctx context.Context, row SendOutcomeRow) error

	// HistoricalBounceCount returns the address-level rollup for the §4.6 gate: the number of DISTINCT
	// CONFIRMED invalid-signal (mailbox_not_found) outcomes for an email_hash, and the latest such time
	// (the TTL anchor). It counts ONLY calibration_class='invalid_signal' rows with no ingest_error —
	// a zero-information bounce never reaches this rollup (§4.6 anti-poisoning). count=0 => the gate
	// never fires. This is the ONE place the email_hash rollup (not verification_id) is the right key.
	HistoricalBounceCount(ctx context.Context, emailHash []byte) (independentCount int, latestAt time.Time, err error)

	// BucketAggregatesSince aggregates calibratable outcomes by bucket over [since, now] for
	// recalibration (§5.2). It EXCLUDES zero-information / from_cache / override / ingest_error rows in
	// SQL (§4.3/§4.4 — the exclusion discipline at read time), and returns external-verified rows with
	// Vendor/ResidueClass set so they bucket SEPARATELY (GAP-54). This is the recalibration read; the
	// caller runs the four guards over it and stages proposals into the calibration store.
	BucketAggregatesSince(ctx context.Context, since, now time.Time) ([]BucketAggregate, error)

	// GetEffectiveCalibration reads the CURRENT effective confidence for a bucket (the verdict-path
	// read, §5.4). vendor/residueClass are "" for a SMTP-earned bucket, non-"" for the external bucket
	// space. known=false => no effective row (cold-start → the caller uses the P1 baseline, §5.6).
	GetEffectiveCalibration(ctx context.Context, reason, mxProvider, vendor, residueClass string) (CalibrationRow, bool, error)

	// AppendCalibrationVersion writes a NEW versioned calibration row (§5.4). At recommend-only it is a
	// PROPOSAL (EffectiveFrom zero) — computed + alertable, NOT read by the verdict path (shadow-mode
	// §2.1). Version is assigned by the store (max(version)+1 for the bucket). Append-only in spirit;
	// approval/supersede/rollback are separate operations. Returns the assigned version.
	AppendCalibrationVersion(ctx context.Context, row CalibrationRow) (version int, err error)

	// ApproveCalibration marks a proposed calibration version effective (§5.4/§6.4) — stamping its
	// EffectiveFrom=at and closing the prior effective row's EffectiveTo=at, atomically. This is the
	// deliberate owner action that promotes a recommend-only proposal to live (or the auto path when a
	// mechanism is owner-promoted). Rolling back is the inverse (RollbackCalibration). Idempotent per id.
	ApproveCalibration(ctx context.Context, calibrationID string, at time.Time) error

	// AccuracyByProvider reads the §8 per-provider accuracy facts over [since, now]: for valid-labeled
	// verdicts, the coverage denominator (ValidLabeled), the accuracy denominator (CalibratableValid),
	// and the numerator (FalseValid = invalid-signal outcomes). SMTP-earned buckets have Vendor="";
	// external-verified buckets are returned separately (Vendor set) so §9 weaning compares the two.
	AccuracyByProvider(ctx context.Context, since, now time.Time) ([]AccuracyRow, error)

	// LeaseRecalibration inserts a 'leased' recalibration_runs row (§5.5) and returns its id — the
	// lease unit for the exactly-once recalibration writer (GAP-53).
	LeaseRecalibration(ctx context.Context, run RecalRun) (runID string, err error)

	// CommitRecalibration is the recalibration writer's CAS (§5.5/GAP-53): it stages the proposed
	// calibration versions AND flips the run 'leased'→'committed' UNDER A SINGLE compare-and-set on the
	// lease-holder (id + status='leased' + leased_by). If the CAS matches ZERO rows (the lease was
	// reclaimed — a slow, not-dead cycle), the whole tx ROLLS BACK (no calibration rows written) and it
	// returns ErrRecalLeaseLost — the discarded cycle applies nothing. brakeHeld records a §6.4 cascade
	// hold. This is settle-once/spend-once transferring a THIRD time to the calibration writer.
	// bucketsTouched (denominator) + bucketsAtMax (numerator) are BOTH recorded so the §6.4 cascade
	// brake's rolling window can sum across prior committed cycles (the staggered-drift detector).
	CommitRecalibration(ctx context.Context, runID, leasedBy string, versions []CalibrationRow, bucketsTouched, bucketsAtMax int, brakeHeld bool, now time.Time) error

	// RecentRecalRuns returns the last `limit` committed recalibration runs (most recent first) — the
	// rolling window the cascade brake reasons over (§6.4, the staggered-drift detector). It carries
	// BucketsTouched per run so CascadeBrake can sum across cycles.
	RecentRecalRuns(ctx context.Context, limit int) ([]RecalRun, error)

	// GetOutcomeCursor / SetOutcomeCursor read/write the durable pull watermark per feed source
	// (ADR-0013 §B.2, mutually-authenticated pull). known=false => never pulled (start from the source's
	// beginning). Replay-safe by construction (complements the (message_id,event) dedup).
	GetOutcomeCursor(ctx context.Context, sourceKey string) (cursor string, known bool, err error)
	SetOutcomeCursor(ctx context.Context, sourceKey, cursor string, at time.Time) error

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
func (StubStore) EnqueueWorkItem(context.Context, WorkItemRow) (string, bool, error) {
	return "", false, ErrNotWired
}
func (StubStore) DequeueReady(context.Context, string, int, time.Time) ([]WorkItemRow, error) {
	return nil, ErrNotWired
}
func (StubStore) SettleWorkItem(context.Context, string, string, string, VerificationRow, *RawRow, WorkItemAttemptRow, *ExternalCallRow) error {
	return ErrNotWired
}
func (StubStore) AppendExternalCall(context.Context, ExternalCallRow) error { return ErrNotWired }
func (StubStore) ExternalSpendSince(context.Context, string, string, time.Time, time.Time, time.Time, time.Time, time.Time) (ExternalSpend, error) {
	return ExternalSpend{}, ErrNotWired
}
func (StubStore) RequeueWorkItem(context.Context, string, string, time.Time, string, bool, WorkItemAttemptRow) error {
	return ErrNotWired
}
func (StubStore) ReclaimExpiredLeases(context.Context, time.Duration, time.Time) (int, error) {
	return 0, ErrNotWired
}
func (StubStore) GetWorkItem(context.Context, string) (WorkItemRow, bool, error) {
	return WorkItemRow{}, false, ErrNotWired
}
func (StubStore) CreateJob(context.Context, JobRow) (string, error) { return "", ErrNotWired }
func (StubStore) GetJob(context.Context, string, string) (JobRow, bool, error) {
	return JobRow{}, false, ErrNotWired
}
func (StubStore) JobCompletion(context.Context, string, string) (bool, int, int, error) {
	return false, 0, 0, ErrNotWired
}
func (StubStore) ListJobResults(context.Context, string, string, string, int) ([]WorkItemResult, string, error) {
	return nil, "", ErrNotWired
}
func (StubStore) LookupCaller(context.Context, []byte, time.Time) (string, string, bool, error) {
	return "", "", false, ErrNotWired
}
func (StubStore) GetVerdictForJoin(context.Context, string) (VerdictJoin, bool, error) {
	return VerdictJoin{}, false, ErrNotWired
}
func (StubStore) AppendSendOutcome(context.Context, SendOutcomeRow) error { return ErrNotWired }
func (StubStore) HistoricalBounceCount(context.Context, []byte) (int, time.Time, error) {
	return 0, time.Time{}, ErrNotWired
}
func (StubStore) BucketAggregatesSince(context.Context, time.Time, time.Time) ([]BucketAggregate, error) {
	return nil, ErrNotWired
}
func (StubStore) GetEffectiveCalibration(context.Context, string, string, string, string) (CalibrationRow, bool, error) {
	return CalibrationRow{}, false, ErrNotWired
}
func (StubStore) AppendCalibrationVersion(context.Context, CalibrationRow) (int, error) {
	return 0, ErrNotWired
}
func (StubStore) ApproveCalibration(context.Context, string, time.Time) error { return ErrNotWired }
func (StubStore) AccuracyByProvider(context.Context, time.Time, time.Time) ([]AccuracyRow, error) {
	return nil, ErrNotWired
}
func (StubStore) LeaseRecalibration(context.Context, RecalRun) (string, error) {
	return "", ErrNotWired
}
func (StubStore) CommitRecalibration(context.Context, string, string, []CalibrationRow, int, int, bool, time.Time) error {
	return ErrNotWired
}
func (StubStore) RecentRecalRuns(context.Context, int) ([]RecalRun, error) { return nil, ErrNotWired }
func (StubStore) GetOutcomeCursor(context.Context, string) (string, bool, error) {
	return "", false, ErrNotWired
}
func (StubStore) SetOutcomeCursor(context.Context, string, string, time.Time) error {
	return ErrNotWired
}
func (StubStore) Ping(context.Context) error { return ErrNotWired }
func (StubStore) Close()                     {}
