// Package config loads the control-plane's runtime configuration. It wires the
// config-manifest.md flags/dials as REAL, READABLE settings from day one — even
// where the feature a flag gates is still a stub. (Phase-0 rule: everything the
// manifest calls a flag is a config entry, never hardcoded.)
//
// Design:
//   - LOCKED contracts (Door 1..5) are NOT config — they live in code (vocab, hash).
//     A "flag" that would let you turn a frozen door off does not exist here by design.
//   - DIALs and feature-flags from the manifest are fields here, each with the manifest's
//     documented default (or explicitly Unset when the manifest says "owner OQ").
//   - The kill-switch ladder (history/kill-switch-ladder.md) and shadow-mode (history/shadow-mode.md)
//     are first-class config: every rung / mode is a readable setting with a SAFE default,
//     so the "cheap-layers-only" rung actually works at Phase 0 (the only rung reachable
//     without SMTP).
//
// Sources of values (highest precedence last): built-in safe defaults -> config file
// (YAML/JSON, added at P1) -> environment overrides. Phase 0 ships defaults + env so the
// skeleton is runnable and testable with zero external files.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Kill-switch ladder (history/kill-switch-ladder.md) ──────────────────────────────────────
// Each rung is a readable flag. Defaults put the system at FULL capability EXCEPT that,
// at Phase 0, no SMTP infrastructure exists — so the effective reachable state is
// CHEAP-LAYERS-ONLY (Rung 3), which the manifest calls "Phase 1's steady state". The
// flags below make that rung real and pullable by config, not code (Door 5 discipline).

// SMTPMode is Rung 1 (SMTP-off) at global scope. Per-node/per-provider scoping is later.
type SMTPMode string

const (
	SMTPEnabled  SMTPMode = "enabled"  // SMTP probing allowed on eligible nodes (P2+ real; P0: no nodes exist)
	SMTPDisabled SMTPMode = "disabled" // Rung 1 global: cheap + fallback only
)

// FallbackMode is Rung 2 (fallback-off) + the Global-A panic button.
type FallbackMode string

const (
	FallbackEnabled FallbackMode = "enabled"
	FallbackOff     FallbackMode = "off" // Rung 2 global / global-fallback-kill (PN-16; rides DR-3 audit)
)

// FrontDoorMode is Rung 4 (paused): accept-and-defer vs reject vs serving.
type FrontDoorMode string

const (
	FrontDoorServing FrontDoorMode = "serving"
	FrontDoorDefer   FrontDoorMode = "accept_and_defer" // queue-only (P5+) — see FrontDoorDeferring
	FrontDoorReject  FrontDoorMode = "reject"
)

// KillSwitch is the operational degradation ladder as config.
type KillSwitch struct {
	SMTP      SMTPMode      // Rung 1
	Fallback  FallbackMode  // Rung 2 / Global A
	FrontDoor FrontDoorMode // Rung 4
	// Rung 3 (cheap-only) is the DERIVED state SMTP=disabled AND Fallback=off; see CheapLayersOnly().
}

// CheapLayersOnly reports whether the ladder is at Rung 3 — the only rung reachable
// without SMTP infrastructure, and Phase 1's designed steady state. Phase-0 verification
// asserts this rung actually works from config alone.
func (k KillSwitch) CheapLayersOnly() bool {
	return k.SMTP == SMTPDisabled && k.Fallback == FallbackOff
}

// FrontDoorRejecting reports whether Rung 4's HARD sub-mode is pulled: nothing is accepted, every
// request-path surface returns an explicit unavailable envelope (history/kill-switch-ladder.md Rung 4).
func (k KillSwitch) FrontDoorRejecting() bool { return k.FrontDoor == FrontDoorReject }

// FrontDoorDeferring reports whether Rung 4's SOFT sub-mode is pulled: accept-and-defer, i.e.
// "the front door accepts submissions and returns a job handle (P5 §7), but the drainer is stopped —
// work sits durably in work_items, nothing is processed" (history/kill-switch-ladder.md Rung 4).
//
// This predicate exists because the value used to have NO consumer (fix-plan item 4 / D2): both
// readers of KillSwitch.FrontDoor compared only against FrontDoorReject, so TM_FRONTDOOR_MODE=
// accept_and_defer validated and then behaved EXACTLY like serving. A silently inert safety control
// is worse than an absent one: an operator reaching for the middle rung during a data-integrity
// incident got full serving, with no error and no warning. Both sub-modes now have a named
// predicate, so a future surface that forgets one is visible as a missing call rather than as a
// missing string comparison.
//
// Two consequences, both required by the rung's own definition:
//
//	(1) DEFER MEANS THE DRAINER DOES NOT RUN. The drainer is what turns a durable work-item into an
//	    SMTP probe and a LOG row; "verdict production is disabled" is precisely "the drainer is
//	    stopped". See queue.Drainer.tick.
//	(2) THERE IS NO DEFERRED FORM OF A SYNCHRONOUS VERIFY. /v1/verify has no handle to hand back and
//	    no queue row to defer into — its answer IS the verdict. Producing one at this rung would be
//	    producing a verdict while verdict production is disabled, and NOT producing one while
//	    returning 200 would be a fabricated verdict, which Door 1/2 forbid outright. So the sync
//	    surface returns the same explicit unavailable envelope it returns under reject. The
//	    asymmetry is the point of the rung: bulk degrades to "accepted, paused", sync degrades to
//	    "unavailable", and neither ever invents an answer.
func (k KillSwitch) FrontDoorDeferring() bool { return k.FrontDoor == FrontDoorDefer }

// VerdictProductionPaused reports whether Rung 4 is pulled AT ALL (either sub-mode). It is the
// predicate for "no new verdict may be produced right now" — the thing both sub-modes share, and
// what the sync surface and the drainer key off. Submission acceptance is where they DIFFER, so
// the bulk submit path deliberately uses FrontDoorRejecting instead.
func (k KillSwitch) VerdictProductionPaused() bool {
	return k.FrontDoorRejecting() || k.FrontDoorDeferring()
}

// ── Shadow-mode (history/shadow-mode.md, DR-2) ──────────────────────────────────────────────
// Every data-driven mutator carries a mode; the four day-one mutators DEFAULT to
// recommend-only (never auto-mutate routing/confidence/spend without a human). This is
// a config surface at Phase 0; the mechanisms themselves are later-phase stubs.

type Mode string

const (
	ModeShadow        Mode = "shadow"         // compute, log only; never emit an actionable proposal
	ModeRecommendOnly Mode = "recommend_only" // DEFAULT for the four mutators; propose, human applies
	ModeAuto          Mode = "auto"           // mutate within the mechanism's own guards
)

// ShadowModes holds the per-mechanism mode dial. Defaults per DR-2 / shadow-mode §2.
type ShadowModes struct {
	A1BlockRateQuarantine Mode // P4 block-rate (statistical trigger). recommend-only by default.
	A6ConfidenceRecalib   Mode // P7 recalibration (sharpest). recommend-only.
	A8ProviderPromotion   Mode // P7 provider auto-promote to :mx. recommend-only.
	A9FallbackWeaning     Mode // P7 per-provider wean. recommend-only.
	// A2/A3/A4/A5/A7/A10/A11 stay auto/alert per shadow-mode §3 — not owner dials here.
}

// ── DIALs from the manifest (values tunable; existence locked) ───────────────────────
// Unset means the manifest deliberately left it an owner OQ (config-manifest §8.2); the
// zero value is NOT silently substituted for a real threshold — features read Unset and
// refuse to act on an unset critical dial rather than guessing.

// Dial is an optional int dial: Set distinguishes "0" from "owner has not chosen".
type Dial struct {
	Value int
	Set   bool
}

func (d Dial) String() string {
	if !d.Set {
		return "UNSET(owner-OQ)"
	}
	return strconv.Itoa(d.Value)
}

// Dials groups the manifest's numeric dials that a Phase-0 reader should see wired.
// (Illustrative subset — the FULL registry is Docs/config-reference.md, generated from
// the manifest. Every one here is readable and defaulted-or-explicitly-unset.)
type Dials struct {
	CacheTTLSeconds        Dial // P1 OQ-9: expires_at value. UNSET (owner). NON-NULL required at write time.
	RawBlobRetentionDays   int  // P1 OQ-10: ~90d default.
	RecommendationSend     int  // P1: 70
	RecommendationReview   int  // P1: 30
	SMTPClientDeadlineMS   Dial // P2 §6.5: UNSET; P5 lease TTL must EXCEED this (X-7 coupling).

	// Phase 3 (§4.5/§5.2) — the audit/warmup DIALs (volumes are dials; existence is locked).
	// NOTE (Phase-8 dial sweep): the P3 re-audit cadence + N-green recover count are NOT separate
	// dials here — the P4 health-watcher SUBSUMED the P3 re-audit ticker (main.go startHealthWatcher),
	// so the live re-audit cadence is TM_WATCHER_CADENCE_SECONDS and the recover-green count is
	// TM_CANARY_RECOVER_GREENS. The former TM_AUDIT_INTERVAL_SECONDS / TM_AUDIT_RECOVER_GREENS were
	// settable-but-ignored aliases (read+validated, never consumed) — removed as the rate_ceiling-class
	// footgun (same as the P6 TM_EXTERNAL_CONFIDENCE_CAP removal). AuditMaxAge (staleness) IS consumed.
	AuditMaxAgeSeconds int // §4.5 staleness window; recommend 2× the P4 watcher cadence. A green older than this → not fresh → excluded.
	WarmupDailyCap     int // §5.2 per-node/per-day :smtp verification ceiling; 0 => warmup NOT enforced (a mature node). Counts verifications (catch-all ≈ 2 connections — GAP-13/§5.2).

	// Phase 4 (§5) — the health-watcher control-loop DIALs (volumes are dials; existence is locked).
	// The watcher REPLACES the P3 minimal re-audit ticker: it owns the audit cadence + adds liveness
	// + block-rate. Start conservative (§5.4 "longer window = less twitchy"). OQ-P4-13 recommends
	// T=30–60s, block-rate window 15–30m. The block-rate quarantine is recommend-only by default
	// (DR-2, ShadowModes.A1BlockRateQuarantine) so the watcher performs ZERO live block-rate mutation
	// until an owner promotes A1 to auto (the fresh-system invariant).
	WatcherCadenceSeconds  int // §5.1 T — control-loop cadence (0 => one pass at boot, no loop)
	BlockRateWindowSeconds int // §5.2 rolling block-rate window
	BlockMinSamples        int // §5.3/GAP-27 — block-rate trigger inert below this attempt count
	BlockQuarThresholdPct  int // §5.4 — block-rate ≥ this % (with samples, pool healthy) → quarantine-eligible
	BlockRecoverThreshPct  int // §5.4 — canary block-rate ≤ this % (sustained) → recover (asymmetric, lower)
	CanaryRecoverGreens    int // §5.4 — consecutive clean canary cycles before recovering→active
	CanaryMinSamples       int // §5.4/GAP-30 — MIN real canary attempts before a block-rate recovery may promote (no zero-sample promotion — the Phase-4 review finding)
	CanarySampleN          int // §5.4/GAP-30 — canary domain-sample rate: ≈1/N of distinct domains route to a recovering node even in a healthy pool, so the canary accrues REAL samples (else stuck-forever); the low canary cap bounds total volume
	LivenessMisses         int // §5.4 — M consecutive liveness failures → quarantine (box down/wedged)
	FleetWidePct           int // §5.5/GAP-28 — if this % of ACTIVE nodes trip at once → BRAKE (alert, don't quarantine)
	CanaryWarmupCap        int // §5.4/PM3 — the low warmup cap a recovered node re-warms from
	PoolEligibleFloor      int // §8.2 — alert when the eligible set ≤ this floor (recommend 1)
	RateSaturationCount    int // §8.2 — alert when rate_limited degrades in the window ≥ this

	// Phase 5 (§4.2/§4.5/§7.5) — the queue's retry/backoff/lease/bulk DIALs (existence locked;
	// values tunable). These were code-defaults in buildQueue; promoted here so PROJECT_RULES §6
	// ("anything the manifest calls a dial is a readable config entry, never hardcoded") holds. The
	// backoff/attempt/age dials carry the spec's §4.2/§4.5 recommended defaults; the submission-
	// backpressure dials (§7.5) are owner-OQ (UNSET) — they must read UNSET, never a guessed number.
	QueueBackoffBaseSeconds   int  // §4.2 attempt-0 backoff floor (greylist windows are minutes); 300 (5m)
	QueueBackoffFactor        int  // §4.2 exponential factor; 2
	QueueBackoffCapSeconds    int  // §4.2 per-step ceiling; 3600 (60m)
	QueueBackoffJitterPct     int  // §4.2 ± jitter % (anti thundering-herd); 15
	QueueRateWaitSeconds      int  // §4.6 SHORT capacity-wait backoff (rate_limited/no-eligible); 30
	QueueMaxAttempts          int  // §4.5 retry ceiling; 4 (spec 3–5)
	QueueMaxRetryAgeSeconds   int  // §4.5 total retry-budget-in-time ceiling; 21600 (6h)
	QueueLeaseMarginSeconds   int  // §5.2/GAP-37 — lease TTL = max-SMTP-deadline + this margin (must be >0 so TTL > deadline); 120 (2m)
	QueueDrainBatchSize       int  // §6 items leased per drain tick; 10
	QueueDrainIdlePollSeconds int  // drain loop idle sleep when the queue is empty; 2
	// §7.5 submission backpressure — owner OQ (manifest §8.2: "UNSET"). A Dial so it reads UNSET,
	// never a guessed number; when UNSET the endpoint uses a safe built-in hard cap (documented) but
	// the *tuned* value is deliberately the owner's, not invented here. This is the ONE backpressure
	// dial that is CONSUMED (bounds the bulk surface). The other two manifest §7.5/§6.5 backpressure
	// dials (max-in-flight-jobs, per-job-in-flight-%) have NO enforcement path yet (per-job
	// concurrency / WFQ is a deferred SEAM — OQ-P5-3); they stay in the manifest §8.2 UNSET inventory
	// and are deliberately NOT wired as settable config here — a settable-but-ignored knob is a silent
	// footgun (the P5→P6 review finding). They land in the phase that actually enforces them.
	QueueBulkMaxAddresses Dial // §7.5 max addresses per /v1/verify/bulk submission (backpressure)

	// Phase 6 (§5.2/§5.3) — the external-fallback spend/breaker DIALs (existence locked; the budget
	// ceilings are owner-OQ → UNSET, "start conservative and raise from observed residue-tail volume",
	// never loosen blind). The caps are Dials so an UNSET ceiling reads UNSET (owner-OQ), never a
	// guessed number — a 0-cap means "not enforced", NOT "no spend"; the layer only egresses at all
	// when deliberately enabled (Fallback==enabled + a vendor key + DPA). ExternalErrorRatePct/
	// CooldownSeconds gate the circuit-breaker (§5.3 cond 2).
	//
	// NOTE (P6→P7 review): the §3.4 anti-laundering confidence cap is NOT a config dial — it lives in
	// the PER-POLICY `external.confidence_cap` (policy JSON, spec §3.2), which is where runExternal
	// reads it (external_stage.go → external.Resolve). A global TM_EXTERNAL_CONFIDENCE_CAP was removed
	// as a settable-but-ignored footgun (the rate_ceiling-class finding): the runtime never read it,
	// so it stayed here only to mislead. The cap is per-policy by design (different policies may cap
	// differently), so the policy key is the single correct home.
	ExternalDailyCap          Dial // §5.2 cost-units/day; UNSET (owner-OQ) — 0/unset => not enforced
	ExternalMonthlyCap        Dial // §5.2 cost-units/month; UNSET
	ExternalPerJobCap         Dial // §5.2/§4.4 cost-units per bulk job (bounds a pathological list); UNSET
	ExternalRateCeiling       Dial // §5.2 max vendor calls/min; UNSET
	ExternalErrorRatePct      int  // §5.3 cond-2 error-rate trip threshold %; 0 => error-rate condition disabled
	ExternalErrorMinSamples   int  // §5.3/GAP-27 min calls in the window before error-rate can trip; 20
	ExternalBreakerCooldownSeconds int // §5.3 breaker open-duration before a half-open probe; 300 (5m)

	// Phase 7 (§4.6/§5/§6) — the flywheel DIALs (existence locked; values tunable). The feedback-loop
	// guards (§6) carry the spec's recommended defaults; the historical-bounce gate (§4.6) and the
	// recalibration cadence are owner/P7-time DIALs. The GATE dials (TTL/MinN/Penalty) are CONSUMED NOW
	// by buildFlywheel → the orchestrator's FlywheelDeps (§4.6 gate + §5.4 read). The RECALIBRATION-PASS
	// guard dials (MinBucketSamples/MaxStep/UpStepPct/Brake*/Cadence) are VALIDATED at startup here
	// (the brake range-guards below — the P4 lesson-b) and are consumed when the recalibration PASS is
	// wired as a scheduled job (flywheel.RecalibrateDeps.GuardParams); that wiring maps the integer
	// percents to the float GuardParams (UpStepPct/100 → UpStepFactor, BrakeBucketPct/100 →
	// BrakeBucketFraction) so config.Validate and flywheel.ValidateGuardParams agree. The two LIVE-PATH
	// touches (gate + calibrated read) default OFF (FlywheelGateEnabled/CalibrationEnabled false) so a
	// fresh P7 system reads the P1 baseline + never gates until an owner opts in — the DR-2 fresh-system
	// invariant expressed as config, mirroring the P6 DPA/kill-switch "enabling is hard".
	HistoricalBounceTTLDays  int // §4.6/OQ-P7-2 TTL window on a confirmed mailbox_not_found (6–12mo); 270 (9mo)
	HistoricalBounceMinN     int // §4.6/F-17/PN-11 ≥N independent invalid-signal outcomes before the gate fires; 2
	HistoricalBouncePenalty  int // §4.6 the post-TTL confidence penalty (a strong nudge, recoverable); 30
	RecalMinBucketSamples    int // §6.2/OQ-P7-3 sample-size gate; 500 (build-plan upper ~200–500)
	RecalMaxStep             int // §6.3 bounded step per cycle (points); 10
	RecalUpStepPct           int // §6.3 hysteresis: UP moves scaled to this % of max_step (down-fast/up-slow); 50
	RecalBrakeWindowCycles   int // §6.4 rolling window (>1 catches STAGGERED drift — the P4 lesson a); 3
	RecalBrakeBucketPct      int // §6.4 fraction of touched buckets at max-step across the window → hold; 50 (must be 1..99)
	RecalBrakeMinBuckets     int // §6.4 denominator floor before the brake may trip (systemic-layer GAP-27); 20
	RecalCadenceSeconds      int // §5/OQ-P7-7 recalibration cadence (batch, not per-outcome); 86400 (daily)

	// ADR-0017 — verification_raw partition maintenance. The raw sidecar is RANGE-partitioned by month and
	// a row dated in an uncovered month is rejected (SQLSTATE 23514); these two dials are what keep that
	// from happening. MonthsAhead MUST be >= 1 (0 would re-create the cliff at every month boundary, so the
	// loader rejects it) and <= 120 (the migration's own bound).
	RawPartitionMonthsAhead             int // how many FUTURE months of partitions to keep created, beyond the current one; 3
	RawPartitionMaintainIntervalSeconds int // how often the maintenance loop re-checks; 21600 (6h). Idempotent, so cheap.
}

// ── Policy / structural feature-flags (shape frozen, contents dial) ─────────────────
type Policy struct {
	DefaultPolicyKey string // "cold-email"
	// provider_routing, gates, order, confidence tables live in the policies store (jsonb),
	// not here — this is just which policy is the system default (P1 §4.6, single-default).
}

// Config is the whole readable configuration surface.
type Config struct {
	Env         string // "development" | "production"
	ListenAddr  string // control-plane front door bind
	DatabaseURL string // Postgres/Supabase DSN (empty => /v1/verify not-ready). Value is a secret-ish connection string, loaded from env, never committed.
	KillSwitch  KillSwitch
	ShadowModes ShadowModes
	Dials       Dials
	Policy      Policy

	// FlywheelGateEnabled turns ON the P7 §4.6 historical-bounce gate (the live-path short-circuit to
	// invalid/historical_bounce on a confirmed ≥N in-TTL bounce history). Default FALSE — a fresh P7
	// system does NOT gate until an owner opts in (the reserved layer stays dormant), so the fresh-
	// system invariant (DR-2) holds: behavior identical to end-of-P6.
	FlywheelGateEnabled bool
	// FlywheelCalibrationEnabled turns ON the P7 §5.4 calibrated-confidence read on the verdict path.
	// Default FALSE — the verdict path uses the P1 baseline (cold-start) until an owner opts in. Even
	// when true, only an APPROVED (effective) calibration version overrides the baseline (recommend-
	// only proposals are inert), so day-one behavior is unchanged until a human approves a version.
	FlywheelCalibrationEnabled bool
	// FlywheelRecalibrateMode is the A6 recalibration-pass mode when the (future) recalibration job runs
	// (mirrors ShadowModes.A6ConfidenceRecalib; kept as the pass's own mode for clarity). Default
	// recommend-only (DR-2). This does NOT gate the live read above — it governs whether a recalibration
	// PASS writes proposals (recommend-only), approves them (auto), or only observes (shadow).

	// ExternalDPAConfirmed is the P6 §7.4 legal enablement gate: the external fallback layer ships
	// DISABLED and MUST NOT egress a single real address until the vendor DPA / due-diligence
	// checklist has passed (DPA signed, retention understood, TLS, key custody). Enabling pre-DPA is
	// UNLAWFUL SUB-PROCESSING. Defaults FALSE — so even Fallback==enabled + a vendor key present does
	// NOT turn the layer on; an operator must ALSO deliberately assert this flag (TM_EXTERNAL_DPA_
	// CONFIRMED=true) after the checklist passes. This makes accidental enablement HARD: three
	// independent switches (kill-switch rung, vendor key, this DPA flag) must all be affirmative, and
	// the global-fallback-kill (Fallback==off) overrides all of them (§ kill-switch-ladder Global A).
	ExternalDPAConfirmed bool

	// Node registry is a LIST from config (Door 5 — never a single hardcoded endpoint).
	// Phase 0 ships zero or one mock entry; the list shape is what matters.
	Nodes []NodeDescriptor
}

// ExternalFallbackEnabled reports whether the P6 fallback layer may egress to a vendor — the AND of
// three independent switches (§7.4 "enabling is hard"): the kill-switch fallback rung is ON
// (Fallback==enabled — DEFAULT off), the operator confirmed the DPA (ExternalDPAConfirmed — DEFAULT
// false), and (checked by the caller) a vendor key is loaded. The global-fallback-kill
// (Fallback==off) is the panic button that forces this false regardless of the DPA flag. This is the
// code-level guard that makes accidental pre-DPA egress hard; buildExternal also requires the key.
func (c Config) ExternalFallbackEnabled() bool {
	return c.KillSwitch.Fallback == FallbackEnabled && c.ExternalDPAConfirmed
}

// SMTPClientDeadline returns the brain-side SMTP client deadline (§6.5), or 0 when the dial is
// UNSET (the node client falls back to its own safe default). This is the brain's own budget on a
// /v1/verify call so a hung box can't hang the brain; a timeout maps to smtp_timeout/unknown.
func (c Config) SMTPClientDeadline() time.Duration {
	if !c.Dials.SMTPClientDeadlineMS.Set {
		return 0
	}
	return time.Duration(c.Dials.SMTPClientDeadlineMS.Value) * time.Millisecond
}

// SMTPEnabled reports whether the SMTP path may run (kill-switch Rung 1 not pulled). Even when
// true, SMTP only runs if a node is actually routable — otherwise the brain degrades to the cheap
// verdict (never invents SMTP).
func (c Config) SMTPEnabled() bool { return c.KillSwitch.SMTP == SMTPEnabled }

// AuditMaxAge is the Phase-3 §4.5 audit-staleness window as a typed accessor (the router
// eligibility filter reads it: a green older than this is not fresh → not :smtp-eligible).
// (The re-audit CADENCE accessor was removed in the Phase-8 sweep — the P4 watcher owns the
// cadence via WatcherCadence(); see the Dials.AuditMaxAgeSeconds comment.)
func (c Config) AuditMaxAge() time.Duration {
	return time.Duration(c.Dials.AuditMaxAgeSeconds) * time.Second
}

// WatcherCadence / BlockRateWindow are the Phase-4 §5 health-watcher DIALs as typed durations.
func (c Config) WatcherCadence() time.Duration {
	return time.Duration(c.Dials.WatcherCadenceSeconds) * time.Second
}
func (c Config) BlockRateWindow() time.Duration {
	return time.Duration(c.Dials.BlockRateWindowSeconds) * time.Second
}

// ── Phase 5 queue DIAL accessors (§4.2/§4.5/§5.2) — typed durations the queue wiring reads ──
func (c Config) QueueBackoffBase() time.Duration {
	return time.Duration(c.Dials.QueueBackoffBaseSeconds) * time.Second
}
func (c Config) QueueBackoffCap() time.Duration {
	return time.Duration(c.Dials.QueueBackoffCapSeconds) * time.Second
}
func (c Config) QueueRateWait() time.Duration {
	return time.Duration(c.Dials.QueueRateWaitSeconds) * time.Second
}
func (c Config) QueueMaxRetryAge() time.Duration {
	return time.Duration(c.Dials.QueueMaxRetryAgeSeconds) * time.Second
}
func (c Config) QueueDrainIdlePoll() time.Duration {
	return time.Duration(c.Dials.QueueDrainIdlePollSeconds) * time.Second
}

// ExternalBreakerCooldown is the P6 §5.3 breaker open-duration (before a half-open probe) as a typed
// duration; it also anchors the error-rate measurement window (external_stage.externalErrWindow).
func (c Config) ExternalBreakerCooldown() time.Duration {
	return time.Duration(c.Dials.ExternalBreakerCooldownSeconds) * time.Second
}

// HistoricalBounceTTL / RecalCadence are the P7 §4.6/§5 DIALs as typed durations.
func (c Config) HistoricalBounceTTL() time.Duration {
	return time.Duration(c.Dials.HistoricalBounceTTLDays) * 24 * time.Hour
}
// RawPartitionMaintainInterval is the ADR-0017 maintenance-loop cadence as a duration.
func (c Config) RawPartitionMaintainInterval() time.Duration {
	return time.Duration(c.Dials.RawPartitionMaintainIntervalSeconds) * time.Second
}

func (c Config) RecalCadence() time.Duration {
	return time.Duration(c.Dials.RecalCadenceSeconds) * time.Second
}

// QueueLeaseTTL derives the GAP-37 lease TTL from the brain's max SMTP deadline PLUS the configured
// margin — the lease MUST exceed the deadline so a live attempt never loses its lease (a reclaim
// then provably means dead). maxSMTPDeadline is passed in (the caller resolves it from
// SMTPClientDeadline with its own fallback). The margin is a dial (>0, enforced by Validate).
func (c Config) QueueLeaseTTL(maxSMTPDeadline time.Duration) time.Duration {
	return maxSMTPDeadline + time.Duration(c.Dials.QueueLeaseMarginSeconds)*time.Second
}

// CacheTTL returns the configured cache TTL as a duration, or 0 when the dial is UNSET
// (OQ-9 — no owner value). Zero means "do not cache" (the orchestrator never writes a
// guessed expires_at; §4.2 requires it NOT NULL). Value is in seconds (Dials.CacheTTLSeconds).
func (c Config) CacheTTL() time.Duration {
	if !c.Dials.CacheTTLSeconds.Set {
		return 0
	}
	return time.Duration(c.Dials.CacheTTLSeconds.Value) * time.Second
}

// ConfigFingerprint returns the P1 config fingerprint (§3.2.5): a stable SHA-256 over the
// verdict-affecting brain+policy config. In P1 there is no live engine, so the engine inputs
// are marked "unconfigured"; the fingerprint covers the policy default + the enabled cheap
// layers + the kill-switch rung. Recomputed deterministically from the config each call.
func (c Config) ConfigFingerprint() string {
	// Canonical, sorted-key serialization of the verdict-affecting inputs (§3.2.5).
	parts := []string{
		"engine=unconfigured", // P1: no live engine (SEAM → P2 adds the fork /config)
		"policy=" + c.Policy.DefaultPolicyKey,
		"layers=syntax,mx,null_mx,spam_trap,disposable,provider_classify,role,typo,free_corp",
		"rung=" + rungName(c.KillSwitch),
		"send=" + strconv.Itoa(c.Dials.RecommendationSend),
		"review=" + strconv.Itoa(c.Dials.RecommendationReview),
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "cfg_sha256:" + hex.EncodeToString(sum[:])[:16]
}

// EngineConfigFingerprint composes the P2 config fingerprint (§4.6, GAP-3): the same canonical
// brain+policy inputs as ConfigFingerprint, but with the engine marker REPLACED by the engine's
// own canonical fingerprint (fetched from the fork's /v1/config, computed brain-side). This
// retires the P1 "engine=unconfigured" placeholder — from P2 the fingerprint covers the real
// engine config, so a config-affecting engine change yields a new fingerprint that naturally
// invalidates the (email_hash, config_fingerprint) cache. engineFP is the brain-computed hash.
func (c Config) EngineConfigFingerprint(engineFP string) string {
	parts := []string{
		"engine=" + engineFP, // was "engine=unconfigured" in P1 (§4.6 resolves GAP-3)
		"policy=" + c.Policy.DefaultPolicyKey,
		"layers=syntax,mx,null_mx,spam_trap,disposable,provider_classify,role,typo,free_corp,smtp",
		"rung=" + rungName(c.KillSwitch),
		"send=" + strconv.Itoa(c.Dials.RecommendationSend),
		"review=" + strconv.Itoa(c.Dials.RecommendationReview),
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "cfg_sha256:" + hex.EncodeToString(sum[:])[:16]
}

func rungName(k KillSwitch) string {
	if k.CheapLayersOnly() {
		return "cheap-layers-only"
	}
	return "other"
}

// NodeDescriptor is one entry in the node LIST (Door 5). Tokens are referenced by id
// and resolved from secrets — never stored here in clear.
type NodeDescriptor struct {
	ID       string
	BaseURL  string
	TokenRef string // key into secrets.NodeToken(...)
	Enabled  bool

	// PublicEgressIP is the node's KNOWN public egress IP — the IP its outbound SMTP actually
	// originates from and that PTR resolves to (P3 §5.4 OQ-P3-11: the brain registry is authoritative
	// for verifier_ip; the box never self-reports its reputation-bearing identity). It is the GAP-24
	// cross-check anchor (§6.6.1): the router refuses :smtp unless the audit's self-detected
	// current_host_ip EQUALS this value, so a multi-homed box can't pass green on its private IP.
	// PENDING-REAL-BOX: the VALUE is set once a real host is provisioned; empty here (local build) →
	// the GAP-24 conjunct fails safe (no :smtp on a green whose IP we can't confirm).
	PublicEgressIP string
}

// Defaults returns the built-in safe configuration. Notably it places the kill-switch at
// CHEAP-LAYERS-ONLY (Rung 3) and all four mutators at recommend-only (DR-2) — the correct
// Phase-0/Phase-1 steady state. These are the manifest's documented defaults, not invented.
func Defaults() Config {
	return Config{
		Env:        "development",
		ListenAddr: ":8080",
		KillSwitch: KillSwitch{
			// No SMTP infra exists at P0/P1, and no vendor is configured — so the honest,
			// SAFE default IS cheap-layers-only. Promoting SMTP/fallback is a later, deliberate flip.
			SMTP:      SMTPDisabled,
			Fallback:  FallbackOff,
			FrontDoor: FrontDoorServing,
		},
		ShadowModes: ShadowModes{
			A1BlockRateQuarantine: ModeRecommendOnly,
			A6ConfidenceRecalib:   ModeRecommendOnly,
			A8ProviderPromotion:   ModeRecommendOnly,
			A9FallbackWeaning:     ModeRecommendOnly,
		},
		Dials: Dials{
			CacheTTLSeconds:      Dial{Set: false}, // owner OQ-9 — refuse to invent a TTL
			RawBlobRetentionDays: 90,               // P1 OQ-10 recommendation
			RecommendationSend:   70,               // P1 §7.2
			RecommendationReview: 30,               // P1 §7.2
			SMTPClientDeadlineMS: Dial{Set: false}, // P2 §6.5 owner OQ (X-7 coupling)
			// Phase-3 audit/warmup dials (§4.5/§5.2). Conservative defaults (OQ-P3-10). (Re-audit
			// cadence + recover-greens are the P4 watcher's TM_WATCHER_CADENCE_SECONDS /
			// TM_CANARY_RECOVER_GREENS — see the Dials.AuditMaxAgeSeconds comment.)
			AuditMaxAgeSeconds: 7200, // 2× the P4 watcher cadence → a stale green is excluded (§4.5)
			WarmupDailyCap:     50,   // §5.2 conservative day-1 start (~50/day, OQ-P3-10 dial)
			// Phase-4 health-watcher dials (§5). Conservative defaults (OQ-P4-13; §5.4 asymmetric):
			WatcherCadenceSeconds:  45,   // §5.1 T (30–60s recommended)
			BlockRateWindowSeconds: 1800, // §5.2 30-minute rolling window (start conservative)
			BlockMinSamples:        20,   // §5.3/GAP-27 sample gate
			BlockQuarThresholdPct:  30,   // §5.4 quarantine at ≥30% block-rate
			BlockRecoverThreshPct:  5,    // §5.4 recover at ≤5% (asymmetric, lower)
			CanaryRecoverGreens:    3,    // §5.4 consecutive clean canary cycles
			CanaryMinSamples:       10,   // §5.4/GAP-30 real canary attempts before promotion (no zero-sample)
			CanarySampleN:          5,    // §5.4/GAP-30 ≈1/5 of domains canary-sampled in a healthy pool (so a canary accrues real samples); low canary cap bounds total volume
			LivenessMisses:         3,    // §5.4 M consecutive liveness misses
			FleetWidePct:           50,   // §5.5/GAP-28 majority of active nodes = systemic → brake
			// CanaryWarmupCap is the per-window meter for a recovering node's canary trickle. It MUST be
			// >= CanaryMinSamples, or the canary can never accrue enough real attempts to promote —
			// re-introducing the GAP-30 "stuck forever" one-way door (the Phase-4 round-2 review found
			// this exact inconsistency when the cap was below the gate). Validate() enforces it; the
			// default sits comfortably above CanaryMinSamples so a recovering node can actually recover.
			CanaryWarmupCap:     20, // §5.4/PM3 low re-warm start — but >= CanaryMinSamples so the canary can promote
			PoolEligibleFloor:   1,  // §8.2 alert one-quarantine-from-all-degrade
			RateSaturationCount: 50, // §8.2 sustained rate_limited degrades in the window
			// Phase-5 queue dials (§4.2/§4.5/§7.5). Backoff/attempt/age carry the spec's recommended
			// defaults; the §7.5 submission-backpressure dials are owner-OQ → UNSET (never guessed).
			QueueBackoffBaseSeconds:   300,   // §4.2 5m attempt-0 floor (greylist windows are minutes)
			QueueBackoffFactor:        2,     // §4.2 ×2 exponential
			QueueBackoffCapSeconds:    3600,  // §4.2 60m per-step ceiling
			QueueBackoffJitterPct:     15,    // §4.2 ±15% jitter (anti thundering-herd)
			QueueRateWaitSeconds:      30,    // §4.6 short capacity-wait backoff
			QueueMaxAttempts:          4,     // §4.5 retry ceiling (spec 3–5)
			QueueMaxRetryAgeSeconds:   21600, // §4.5 6h total retry-budget-in-time
			QueueLeaseMarginSeconds:   120,   // §5.2/GAP-37 lease TTL = maxSMTPdeadline + 2m (>0 so TTL > deadline)
			QueueDrainBatchSize:       10,    // §6 items per drain tick
			QueueDrainIdlePollSeconds: 2,     // idle sleep when the queue is empty
			QueueBulkMaxAddresses:     Dial{Set: false}, // §7.5 owner OQ — UNSET (safe hard cap applies)
			// Phase-6 external-fallback dials (§5.2/§5.3). The spend ceilings are owner-OQ → UNSET
			// (start conservative, raise from observed volume — never loosen blind). The breaker
			// error-rate is off-by-default (0%) with a sample gate + a 5m cooldown, so a first
			// deployment relies on the budget ceilings until an owner tunes the error-rate condition.
			// (The §3.4 anti-laundering cap is the per-policy external.confidence_cap, not a dial here.)
			ExternalDailyCap:               Dial{Set: false}, // §5.2 owner OQ — UNSET (not enforced until set)
			ExternalMonthlyCap:             Dial{Set: false}, // §5.2 owner OQ — UNSET
			ExternalPerJobCap:              Dial{Set: false}, // §5.2/§4.4 owner OQ — UNSET
			ExternalRateCeiling:            Dial{Set: false}, // §5.2 owner OQ — UNSET
			ExternalErrorRatePct:           0,                // §5.3 cond-2 disabled until tuned (0 => off)
			ExternalErrorMinSamples:        20,               // §5.3/GAP-27 sample gate (mirrors block-rate)
			ExternalBreakerCooldownSeconds: 300,              // §5.3 5m open before a half-open probe
			// Phase-7 flywheel dials (§4.6/§5/§6). Feedback-loop guards carry the spec's conservative
			// defaults; the historical-bounce gate + recalibration cadence are owner/P7-time DIALs.
			HistoricalBounceTTLDays: 270, // §4.6/OQ-P7-2 9-month TTL (within the 6–12mo band)
			HistoricalBounceMinN:    2,   // §4.6/F-17/PN-11 ≥2 independent — one forged outcome can't blacklist
			HistoricalBouncePenalty: 30,  // §4.6 post-TTL confidence penalty (recoverable)
			RecalMinBucketSamples:   500, // §6.2/OQ-P7-3 start at the build-plan upper bound
			RecalMaxStep:            10,  // §6.3 ≤10 points/cycle
			RecalUpStepPct:          50,  // §6.3 up moves at half rate (down-fast/up-slow hysteresis)
			RecalBrakeWindowCycles:  3,   // §6.4 rolling 3-cycle window (>1 → catches staggered drift)
			RecalBrakeBucketPct:     50,  // §6.4 ≥50% of touched buckets at max-step across the window → hold
			RecalBrakeMinBuckets:    20,  // §6.4 denominator floor (systemic-layer GAP-27)
			RecalCadenceSeconds:     86400, // §5/OQ-P7-7 daily rollup (batch, not per-outcome)

			RawPartitionMonthsAhead:             3,     // ADR-0017: three months of headroom; a failing loop has ~3 months of warning in its logs before any write fails
			RawPartitionMaintainIntervalSeconds: 21600, // ADR-0017: 6h — the call is idempotent and cheap, so frequent is fine and shortens recovery after an outage
		},
		Policy: Policy{DefaultPolicyKey: "cold-email"},
		Nodes:  nil, // node LIST, empty at P0. Door 5: a list even when length 0/1.
	}
}

// Load builds config from defaults, then applies environment overrides. (File-based
// overrides land at P1; the precedence order is defaults -> file -> env.) It VALIDATES
// the result so a nonsensical kill-switch/mode can't start the control-plane.
func Load(getenv func(string) string) (Config, error) {
	c := Defaults()

	if v := getenv("TM_ENV"); v != "" {
		c.Env = v
	}
	if v := getenv("TM_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := getenv("TM_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	// Cache TTL dial (OQ-9): an operator MAY set it; absent => stays UNSET (no caching).
	if v := getenv("TM_CACHE_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("config: invalid TM_CACHE_TTL_SECONDS %q", v)
		}
		c.Dials.CacheTTLSeconds = Dial{Value: n, Set: true}
	}
	// SMTP client deadline dial (P2 §6.5): the brain's own budget on a /v1/verify call.
	if v := getenv("TM_SMTP_CLIENT_DEADLINE_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_SMTP_CLIENT_DEADLINE_MS %q", v)
		}
		c.Dials.SMTPClientDeadlineMS = Dial{Value: n, Set: true}
	}
	// Kill-switch env overrides (operator can pull a rung without a redeploy).
	if v := getenv("TM_SMTP_MODE"); v != "" {
		c.KillSwitch.SMTP = SMTPMode(v)
	}
	if v := getenv("TM_FALLBACK_MODE"); v != "" {
		c.KillSwitch.Fallback = FallbackMode(v)
	}
	if v := getenv("TM_FRONTDOOR_MODE"); v != "" {
		c.KillSwitch.FrontDoor = FrontDoorMode(v)
	}
	// The node LIST from env (Door 5). Node #1 uses the bare TM_NODE_* keys (the P0/P3 shape); nodes
	// #2..#N use numbered keys TM_NODE_<i>_* (i≥2). This is how P4 proves "add node #2 = +1 config
	// line" (§7/GAP-31): standing up a second node is a CONFIG edit — a numbered env block — with ZERO
	// brain code change. The router/watcher/limiter are all list-shaped/per-node (built that way since
	// P1), so the same code serves N=1 and N=2. (A real #2 also needs the §6 provisioning — the
	// honest bound of GAP-31: the brain is N-agnostic; the world still makes you earn each clean IP.)
	if id := getenv("TM_NODE_ID"); id != "" {
		c.Nodes = append(c.Nodes, NodeDescriptor{
			ID:       id,
			BaseURL:  getenv("TM_NODE_BASE_URL"),
			TokenRef: id,
			Enabled:  getenv("TM_NODE_ENABLED") == "true",
			// P3 §5.4/GAP-24: the node's KNOWN public egress IP (== the PTR'd/audited IP). PENDING-
			// REAL-BOX — set once a real host is provisioned; empty in the local build → the GAP-24
			// conjunct fails safe (no :smtp on a green whose IP the brain can't confirm).
			PublicEgressIP: getenv("TM_NODE_PUBLIC_EGRESS_IP"),
		})
	}
	// Nodes #2..#N — numbered, config-only (the GAP-31 proof). Stops at the first gap in the sequence.
	for i := 2; ; i++ {
		p := "TM_NODE_" + strconv.Itoa(i) + "_"
		id := getenv(p + "ID")
		if id == "" {
			break
		}
		c.Nodes = append(c.Nodes, NodeDescriptor{
			ID:             id,
			BaseURL:        getenv(p + "BASE_URL"),
			TokenRef:       id,
			Enabled:        getenv(p+"ENABLED") == "true",
			PublicEgressIP: getenv(p + "PUBLIC_EGRESS_IP"),
		})
	}

	// Phase-3 audit/warmup dial overrides (§4.5/§5.2). Each is optional; absent => the safe default.
	// (TM_AUDIT_INTERVAL_SECONDS and TM_AUDIT_RECOVER_GREENS are intentionally NOT read: the P4
	// health-watcher subsumed the P3 re-audit ticker, so the live re-audit cadence is
	// TM_WATCHER_CADENCE_SECONDS and the recover-green count is TM_CANARY_RECOVER_GREENS. The two
	// TM_AUDIT_* vars were settable-but-ignored aliases — removed in the Phase-8 sweep so an operator
	// can't set a knob that silently does nothing. Only the consumed staleness window remains.)
	if v := getenv("TM_AUDIT_MAX_AGE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_AUDIT_MAX_AGE_SECONDS %q", v)
		}
		c.Dials.AuditMaxAgeSeconds = n
	}
	if v := getenv("TM_WARMUP_DAILY_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("config: invalid TM_WARMUP_DAILY_CAP %q", v)
		}
		c.Dials.WarmupDailyCap = n
	}

	// Phase-4 health-watcher dial overrides (§5). Each optional; absent => the safe default. All are
	// non-negative (a 0 cadence disables the loop; 0 fleet-pct is clamped to the safe default in the
	// health package's withDefaults, so the brake is never accidentally disabled — GAP-28).
	for _, d := range []struct {
		env  string
		dst  *int
		zero bool // true => allow 0 (e.g. cadence 0 = one-shot)
	}{
		{"TM_WATCHER_CADENCE_SECONDS", &c.Dials.WatcherCadenceSeconds, true},
		{"TM_BLOCK_RATE_WINDOW_SECONDS", &c.Dials.BlockRateWindowSeconds, false},
		{"TM_BLOCK_MIN_SAMPLES", &c.Dials.BlockMinSamples, false},
		{"TM_BLOCK_QUAR_THRESHOLD_PCT", &c.Dials.BlockQuarThresholdPct, false},
		{"TM_BLOCK_RECOVER_THRESHOLD_PCT", &c.Dials.BlockRecoverThreshPct, false},
		{"TM_CANARY_RECOVER_GREENS", &c.Dials.CanaryRecoverGreens, false},
		{"TM_CANARY_MIN_SAMPLES", &c.Dials.CanaryMinSamples, false},
		{"TM_CANARY_SAMPLE_N", &c.Dials.CanarySampleN, false},
		{"TM_LIVENESS_MISSES", &c.Dials.LivenessMisses, false},
		{"TM_FLEET_WIDE_PCT", &c.Dials.FleetWidePct, false},
		{"TM_CANARY_WARMUP_CAP", &c.Dials.CanaryWarmupCap, false},
		{"TM_POOL_ELIGIBLE_FLOOR", &c.Dials.PoolEligibleFloor, true},
		{"TM_RATE_SATURATION_COUNT", &c.Dials.RateSaturationCount, true},
		// Phase-5 queue dials (§4.2/§4.5/§5.2). All must be > 0 (a zero backoff/attempt/lease is
		// nonsensical or unsafe); the drainer additionally fails fast if the lease TTL doesn't exceed
		// the SMTP deadline (GAP-37), and Validate() guards the margin below.
		{"TM_QUEUE_BACKOFF_BASE_SECONDS", &c.Dials.QueueBackoffBaseSeconds, false},
		{"TM_QUEUE_BACKOFF_FACTOR", &c.Dials.QueueBackoffFactor, false},
		{"TM_QUEUE_BACKOFF_CAP_SECONDS", &c.Dials.QueueBackoffCapSeconds, false},
		{"TM_QUEUE_BACKOFF_JITTER_PCT", &c.Dials.QueueBackoffJitterPct, true}, // 0 jitter is legal (tests/degenerate)
		{"TM_QUEUE_RATE_WAIT_SECONDS", &c.Dials.QueueRateWaitSeconds, false},
		{"TM_QUEUE_MAX_ATTEMPTS", &c.Dials.QueueMaxAttempts, false},
		{"TM_QUEUE_MAX_RETRY_AGE_SECONDS", &c.Dials.QueueMaxRetryAgeSeconds, false},
		{"TM_QUEUE_LEASE_MARGIN_SECONDS", &c.Dials.QueueLeaseMarginSeconds, false},
		{"TM_QUEUE_DRAIN_BATCH_SIZE", &c.Dials.QueueDrainBatchSize, false},
		{"TM_QUEUE_DRAIN_IDLE_POLL_SECONDS", &c.Dials.QueueDrainIdlePollSeconds, false},
		// Phase-6 external-fallback int dials (§5.3). ErrorRatePct 0 disables the error-rate breaker
		// condition (budget ceilings still apply). Cooldown/samples 0 legal (degenerate; tests). All
		// non-negative. (The §3.4 confidence cap is NOT here — it is the per-policy external.confidence_cap.)
		{"TM_EXTERNAL_ERROR_RATE_PCT", &c.Dials.ExternalErrorRatePct, true},
		{"TM_EXTERNAL_ERROR_MIN_SAMPLES", &c.Dials.ExternalErrorMinSamples, true},
		{"TM_EXTERNAL_BREAKER_COOLDOWN_SECONDS", &c.Dials.ExternalBreakerCooldownSeconds, true},
		// Phase-7 flywheel int dials (§4.6/§5/§6). All non-negative; the brake fraction/window/up-step
		// are additionally range-guarded in Validate (a silently-disabled brake is the P4 lesson-b hole).
		{"TM_HISTORICAL_BOUNCE_TTL_DAYS", &c.Dials.HistoricalBounceTTLDays, true},
		{"TM_HISTORICAL_BOUNCE_MIN_N", &c.Dials.HistoricalBounceMinN, true},
		{"TM_HISTORICAL_BOUNCE_PENALTY", &c.Dials.HistoricalBouncePenalty, true},
		{"TM_RECAL_MIN_BUCKET_SAMPLES", &c.Dials.RecalMinBucketSamples, false},
		{"TM_RECAL_MAX_STEP", &c.Dials.RecalMaxStep, false},
		{"TM_RECAL_UP_STEP_PCT", &c.Dials.RecalUpStepPct, false},
		{"TM_RECAL_BRAKE_WINDOW_CYCLES", &c.Dials.RecalBrakeWindowCycles, false},
		{"TM_RECAL_BRAKE_BUCKET_PCT", &c.Dials.RecalBrakeBucketPct, false},
		{"TM_RECAL_BRAKE_MIN_BUCKETS", &c.Dials.RecalBrakeMinBuckets, false},
		{"TM_RECAL_CADENCE_SECONDS", &c.Dials.RecalCadenceSeconds, true},
		// ADR-0017: verification_raw partition maintenance. Both must be > 0 (zero headroom IS the cliff;
		// a zero interval would never re-check). MonthsAhead's upper bound is range-guarded in Validate.
		{"TM_RAW_PARTITION_MONTHS_AHEAD", &c.Dials.RawPartitionMonthsAhead, false},
		{"TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS", &c.Dials.RawPartitionMaintainIntervalSeconds, false},
	} {
		if v := getenv(d.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (!d.zero && n == 0) {
				return c, fmt.Errorf("config: invalid %s %q", d.env, v)
			}
			*d.dst = n
		}
	}

	// Phase-5 submission-backpressure dial (§7.5) — owner OQ, stays UNSET unless an operator sets it
	// (reads UNSET, never a guessed number). Only the CONSUMED one is wired: TM_QUEUE_BULK_MAX_ADDRESSES
	// bounds the bulk surface. Max-in-flight-jobs / per-job-% are NOT wired (no enforcement path yet —
	// a settable-but-ignored knob is a silent footgun; they land with the phase that enforces them).
	if v := getenv("TM_QUEUE_BULK_MAX_ADDRESSES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_QUEUE_BULK_MAX_ADDRESSES %q (must be > 0)", v)
		}
		c.Dials.QueueBulkMaxAddresses = Dial{Value: n, Set: true}
	}

	// Phase-6 external-spend ceiling dials (§5.2) — owner OQ, stay UNSET unless an operator sets them
	// (read UNSET, never a guessed number; an UNSET cap is "not enforced", never "unbounded spend" —
	// the layer only egresses when deliberately enabled). Each must be > 0 when set.
	for _, d := range []struct {
		env string
		dst *Dial
	}{
		{"TM_EXTERNAL_DAILY_CAP", &c.Dials.ExternalDailyCap},
		{"TM_EXTERNAL_MONTHLY_CAP", &c.Dials.ExternalMonthlyCap},
		{"TM_EXTERNAL_PER_JOB_CAP", &c.Dials.ExternalPerJobCap},
		{"TM_EXTERNAL_RATE_CEILING", &c.Dials.ExternalRateCeiling},
	} {
		if v := getenv(d.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("config: invalid %s %q (must be > 0)", d.env, v)
			}
			*d.dst = Dial{Value: n, Set: true}
		}
	}

	// Phase-6 §7.4 DPA enablement gate (default FALSE): the layer stays disabled until the operator
	// asserts the DPA/due-diligence checklist has passed. Enabling pre-DPA = unlawful sub-processing,
	// so this is deliberately a separate, explicit affirmative flag — NOT implied by the kill-switch
	// rung or the vendor key. Only the exact string "true" turns it on.
	if getenv("TM_EXTERNAL_DPA_CONFIRMED") == "true" {
		c.ExternalDPAConfirmed = true
	}

	// Phase-7 flywheel live-path opt-ins (default FALSE — the fresh-system invariant / "enabling is
	// explicit"). Only the exact string "true" turns each on. The gate short-circuits verdicts to
	// invalid/historical_bounce; the calibrated read overrides the P1 baseline with an APPROVED version.
	if getenv("TM_FLYWHEEL_GATE_ENABLED") == "true" {
		c.FlywheelGateEnabled = true
	}
	if getenv("TM_FLYWHEEL_CALIBRATION_ENABLED") == "true" {
		c.FlywheelCalibrationEnabled = true
	}

	return c, c.Validate()
}

// LoadFromOS is the production entry point.
func LoadFromOS() (Config, error) { return Load(os.Getenv) }

// Validate rejects illegal combinations. It enforces the composability of the ladder and
// modes, not the frozen doors (those are compile-time in vocab/hash).
func (c Config) Validate() error {
	switch c.KillSwitch.SMTP {
	case SMTPEnabled, SMTPDisabled:
	default:
		return fmt.Errorf("config: invalid SMTP mode %q", c.KillSwitch.SMTP)
	}
	switch c.KillSwitch.Fallback {
	case FallbackEnabled, FallbackOff:
	default:
		return fmt.Errorf("config: invalid fallback mode %q", c.KillSwitch.Fallback)
	}
	switch c.KillSwitch.FrontDoor {
	case FrontDoorServing, FrontDoorDefer, FrontDoorReject:
	default:
		return fmt.Errorf("config: invalid front-door mode %q", c.KillSwitch.FrontDoor)
	}
	for name, m := range map[string]Mode{
		"A1": c.ShadowModes.A1BlockRateQuarantine, "A6": c.ShadowModes.A6ConfidenceRecalib,
		"A8": c.ShadowModes.A8ProviderPromotion, "A9": c.ShadowModes.A9FallbackWeaning,
	} {
		switch m {
		case ModeShadow, ModeRecommendOnly, ModeAuto:
		default:
			return fmt.Errorf("config: invalid shadow-mode for %s: %q", name, m)
		}
	}
	// ADR-0017: the headroom must be a real, bounded number. Checked here as well as in Load so a Config
	// built any other way (tests, a future file source) cannot carry the cliff or an unbounded request
	// past startup. 120 is the migration's own cap on ensure_verification_raw_partitions.
	if c.Dials.RawPartitionMonthsAhead < 1 || c.Dials.RawPartitionMonthsAhead > 120 {
		return fmt.Errorf("config: raw_partition_months_ahead (%d) must be in 1..120 (0 would re-create the partition cliff at every month boundary)",
			c.Dials.RawPartitionMonthsAhead)
	}
	if c.Dials.RawPartitionMaintainIntervalSeconds < 1 {
		return fmt.Errorf("config: raw_partition_maintain_interval_seconds (%d) must be >= 1", c.Dials.RawPartitionMaintainIntervalSeconds)
	}
	if c.Dials.RecommendationSend < c.Dials.RecommendationReview {
		return fmt.Errorf("config: recommendation.send (%d) must be >= review (%d)",
			c.Dials.RecommendationSend, c.Dials.RecommendationReview)
	}
	// §5.4/GAP-30: the canary meter must allow enough real attempts to satisfy the promotion sample
	// gate, or a recovering node can NEVER accrue MinCanarySamples → stuck-forever (the round-2 review
	// finding). CanaryWarmupCap>0 (a canary is always metered) AND >= CanaryMinSamples. Both dials are
	// per the block-rate window in practice; require the meter to cover the gate with margin=equality.
	if c.Dials.CanaryWarmupCap > 0 && c.Dials.CanaryMinSamples > c.Dials.CanaryWarmupCap {
		return fmt.Errorf("config: canary_warmup_cap (%d) must be >= canary_min_samples (%d) or a recovering node can never accrue enough samples to promote (GAP-30 stuck-forever)",
			c.Dials.CanaryWarmupCap, c.Dials.CanaryMinSamples)
	}
	// §5.5/GAP-28 (round-2 review finding D): the fleet-wide brake fraction is FleetWidePct/100, and
	// the systemic-spike test is `elevated >= fraction × active`. A percentage ≥ 100 yields a fraction
	// ≥ 1.0, so the test needs EVERY active node elevated in one cycle — with staggered onset that
	// essentially never happens, silently DISABLING "the single most important safety line" with no
	// startup error. The loader already rejects 0/negative; here we bound the top. Require 1..99 so the
	// brake can always fire on a genuine majority. (The active-floor backstop still bounds the worst
	// case, but a dead fraction-brake + no fleet_wide alert is exactly the auto-heal→auto-outage hole.)
	if c.Dials.FleetWidePct < 1 || c.Dials.FleetWidePct > 99 {
		return fmt.Errorf("config: fleet_wide_pct (%d) must be in 1..99 — a value ≥100 makes the fleet-wide cascade brake (GAP-28) mathematically unable to fire, silently disabling it",
			c.Dials.FleetWidePct)
	}
	// §5.2/GAP-37: the queue lease TTL = maxSMTPdeadline + QueueLeaseMarginSeconds. The margin MUST be
	// > 0 so the lease strictly EXCEEDS the deadline — a live (slow-not-dead) attempt must never lose
	// its lease, or a reclaim double-runs it and the CAS exactly-once guarantee degrades to a race.
	// (NewDrainer also fails fast on TTL<=deadline; this catches a zero/negative margin at startup.)
	if c.Dials.QueueLeaseMarginSeconds <= 0 {
		return fmt.Errorf("config: queue_lease_margin_seconds (%d) must be > 0 — the lease TTL must strictly exceed the max SMTP deadline (GAP-37), else a slow attempt loses its lease and is double-run",
			c.Dials.QueueLeaseMarginSeconds)
	}
	// §4.5: at least one retry ceiling must bound an item so it can't linger forever (attempt count
	// OR age). The drainer enforces the same, but a startup guard is clearer than a runtime surprise.
	if c.Dials.QueueMaxAttempts <= 0 && c.Dials.QueueMaxRetryAgeSeconds <= 0 {
		return fmt.Errorf("config: at least one of queue_max_attempts / queue_max_retry_age_seconds must be > 0 (§4.5 — an item must have a retry ceiling)")
	}
	// §6.4 (P7, the P4 fleet-brake lesson-b, applied to the recalibration cascade brake): a brake
	// bucket-fraction outside 1..99 makes the systemic-swing brake mathematically unable to fire (>=100%
	// needs EVERY touched bucket to swing at once — staggered poisoning never does), SILENTLY disabling
	// the safety valve. The window must be >=1 (0 disables the brake). The up-step % must be 1..100
	// (>100 would invert the down-fast/up-slow hysteresis). All fail at STARTUP, never a silent no-op.
	// (flywheel.ValidateGuardParams re-checks these when the recalibration pass runs; this is the
	// earliest, config-level backstop so a nonsensical brake can't even start.)
	if c.Dials.RecalBrakeBucketPct < 1 || c.Dials.RecalBrakeBucketPct > 99 {
		return fmt.Errorf("config: recal_brake_bucket_pct (%d) must be in 1..99 — a value outside makes the P7 cascade brake (§6.4) unable to fire, silently disabling the systemic-swing safety valve (the P4 fleet-brake lesson-b)", c.Dials.RecalBrakeBucketPct)
	}
	if c.Dials.RecalBrakeWindowCycles < 1 {
		return fmt.Errorf("config: recal_brake_window_cycles (%d) must be >= 1 — 0 disables the cascade brake; >1 is what catches STAGGERED drift the P4 single-snapshot brake missed (§6.4 lesson-a)", c.Dials.RecalBrakeWindowCycles)
	}
	if c.Dials.RecalUpStepPct < 1 || c.Dials.RecalUpStepPct > 100 {
		return fmt.Errorf("config: recal_up_step_pct (%d) must be in 1..100 — >100 inverts the down-fast/up-slow hysteresis asymmetry (§6.3)", c.Dials.RecalUpStepPct)
	}
	return nil
}

// Summary is a one-line, secret-free description for boot logs and the readiness probe.
func (c Config) Summary() string {
	rung := "full"
	if c.KillSwitch.CheapLayersOnly() {
		rung = "cheap-layers-only(Rung3)"
	}
	// Rung 4 is HIGHER than Rung 3 and subsumes it in the summary: with verdict production paused,
	// which cheap layers are wired is no longer the operative fact. Naming the sub-mode is the whole
	// point (fix-plan item 4) — an operator who pulled accept-and-defer must be able to SEE that the
	// process agrees, in the boot log and on /readyz, rather than inferring it from a config echo
	// the code ignores.
	switch {
	case c.KillSwitch.FrontDoorRejecting():
		rung = "paused-reject(Rung4)"
	case c.KillSwitch.FrontDoorDeferring():
		rung = "paused-accept-and-defer(Rung4)"
	}
	return fmt.Sprintf("env=%s listen=%s ladder=%s smtp=%s fallback=%s frontdoor=%s nodes=%d mutators=recommend-only",
		c.Env, c.ListenAddr, rung, c.KillSwitch.SMTP, c.KillSwitch.Fallback, c.KillSwitch.FrontDoor, len(c.Nodes))
}
