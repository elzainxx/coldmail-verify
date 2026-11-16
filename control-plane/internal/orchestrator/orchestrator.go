// Package orchestrator runs the /v1/verify request lifecycle (P1 §5.2, SVG ①):
//
//	normalize (Door 3) → cache lookup → policy plan → cheap layers → score
//	→ verdict assembler (single source: Response + LOG row) → LOG → cache write → respond
//
// It is the seam that turns the Phase-0 honest-unknown stub into a real scored+logged verdict
// using cheap layers only — NO port 25 anywhere (Phase-1 done-when). Every result is LOGged
// before it is returned, including cache hits (from_cache=true) and un-addressable rejects
// (invalid_syntax, NULL hash — §3.2.7). The LOG write failing FAILS the request: an unlogged
// verdict violates Door 2 ("the log is the point"), so we never return one we couldn't log.
package orchestrator

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/smtp"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// Orchestrator wires the cheap-path dependencies. It holds no per-request state.
type Orchestrator struct {
	hasher   *hash.Hasher
	store    store.Store
	resolver layers.Resolver
	engine   *policy.Engine
	layerSet map[string]layers.Layer
	now      func() time.Time

	// cacheTTL is the DIAL for cache.expires_at (P1 OQ-9). Zero means "unset" → caching is
	// SKIPPED entirely (we never write a row with a guessed/absent TTL — expires_at is NOT
	// NULL, §4.2). The verdict is still logged; only the cache write is skipped.
	cacheTTL time.Duration

	// cfgFingerprint is the config fingerprint (§3.2.5). In P1 the engine inputs are
	// "unconfigured"; in P2 the caller recomputes it over the fork's /v1/config effective config
	// (§4.6, GAP-3) and passes it here. Precomputed at wire time.
	cfgFingerprint string

	// smtp is the OPTIONAL Phase-2 SMTP stage (§7.1). nil => cheap-layers-only (Phase-1 steady
	// state / kill-switch Rung 3). When wired AND the cheap verdict is the non-terminal residue
	// AND a node is routable, the SMTP stage resolves it against the verifier-node.
	smtp *SMTPDeps

	// external is the OPTIONAL Phase-6 third-party-fallback stage (§3). nil => fallback fully OFF
	// (the P1–P5 posture and the default — enabling is a deliberate owner action gated on the §7.4
	// DPA checklist, never a code default). When wired AND the settled verdict is an escalatable
	// residue class AND the policy opts in AND the budget/breaker is open, it rents a verdict at
	// CAPPED confidence (never SMTP-grade — anti-laundering §3.4). See external_stage.go.
	external *ExternalDeps

	// flywheel is the OPTIONAL Phase-7 live-path touches (§4.6 historical-bounce gate + §5.4 calibrated-
	// confidence read). nil => the flywheel is fully OFF (the P1–P6 posture and the default). Even when
	// wired it is inert on a fresh system (no historical bounces, no effective calibration versions) —
	// behavior IDENTICAL to end-of-P6 until real outcomes accrue (the DR-2 fresh-system invariant). It
	// NEVER mutates the LOG or mints vocab (uses the reserved historical_bounce). See flywheel_stage.go.
	flywheel *FlywheelDeps
}

// Deps carries the orchestrator's dependencies.
type Deps struct {
	Hasher            *hash.Hasher
	Store             store.Store
	Resolver          layers.Resolver
	Now               func() time.Time
	CacheTTL          time.Duration
	ConfigFingerprint string
	// SMTP is the OPTIONAL Phase-2 stage. nil => cheap-layers-only (P1 steady state). Wire it
	// only when SMTP is enabled (kill-switch not SMTP-off) and a node list exists.
	SMTP *SMTPDeps
	// External is the OPTIONAL Phase-6 fallback stage. nil => fallback OFF (default). Wire it only
	// when an owner enabled it (Fallback==enabled) AND a vendor key + DPA are configured (§7.4).
	External *ExternalDeps
	// Flywheel is the OPTIONAL Phase-7 live-path touches (§4.6 gate + §5.4 calibrated read). nil =>
	// flywheel OFF (default). Wire it in P7; both touches are inert on a fresh system regardless.
	Flywheel *FlywheelDeps
}

// New wires an orchestrator. Resolver defaults to the real net resolver; Now to time.Now.
func New(d Deps) *Orchestrator {
	res := d.Resolver
	if res == nil {
		res = layers.NewNetResolver()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Orchestrator{
		hasher:         d.Hasher,
		store:          d.Store,
		resolver:       res,
		engine:         policy.NewEngine(),
		layerSet:       policy.LayerSet(),
		now:            now,
		cacheTTL:       d.CacheTTL,
		cfgFingerprint: d.ConfigFingerprint,
		smtp:           d.SMTP,
		external:       d.External,
		flywheel:       d.Flywheel,
	}
}

// ErrNotReady means a core dependency (hasher or store) is missing — the control-plane
// cannot hash or log, so it must not pretend to verify. The API maps this to 503.
var ErrNotReady = errors.New("orchestrator: not ready (missing hasher or store)")

// QueueClass tells the P5 drainer how to transition a work-item after a queued check's compute
// (§3.5). It is derived (in compute) from data the sync path already produces — NO new state on the
// SMTP stage, and the SYNC path never reads it (inert for sync by construction). It exists so the
// drainer can settle vs requeue while sharing the IDENTICAL P4 gate path (§8 master invariant).
type QueueClass int

const (
	QSettled        QueueClass = iota // terminal verdict → settle + log once (§5.3)
	QGreylisted                       // greylist transient → requeue on backoff, bump attempt (§4.2)
	QTransientNet                     // smtp_timeout/connection_failed → requeue, bump (secondary, OQ-P5-12)
	QRateLimited                      // rate_limited skip → requeue as a CAPACITY wait, NO attempt bump (§4.6/OQ-P5-7)
	QNoEligibleNode                   // audit_red/no_eligible_node/warmup_cap → requeue as a health/capacity wait, no bump
)

// logKind selects which settlement path a computed verdict takes — the three pre-existing sync log
// calls, preserved exactly (byte-identical proof for the sync path).
type logKind int

const (
	logFull      logKind = iota // main path → logAndCacheRaw (LOG + optional cache write)
	logReject                   // un-addressable reject → logOnly (NULL hash)
	logFromCache                // cache hit → logOnly (from_cache=true)
)

// computed is the log-FREE result of the compute path — everything both the sync settler and the
// queue drainer need, produced ONCE. The sync path (settleSync) reads resp/row/kind/cache inputs;
// the queue path (ComputeForQueue) additionally reads class/retryHintS/lastReason. Nothing here is
// logged or cached — that is the caller's decision (sync logs unconditionally; the queue settles
// via CAS or requeues without logging — resolving §5.3 vs Door-2 "the log is the point").
type computed struct {
	resp verdict.Response
	row  store.VerificationRow
	kind logKind

	// logFull inputs (for the sync cache write / the queue post-settle cache).
	emailHash []byte
	domain    string
	prov      verdict.Provenance
	cacheable bool
	rawBlob   []byte

	// Queue classifier (ignored by the sync path).
	class      QueueClass
	retryHintS int    // seed for the greylist backoff (§4.2), from signals.retry_after_hint_s
	lastReason string // work_items.last_reason (operational)

	// extCall is the P6 external-fallback spend/provenance record (§6.1) when compute() escalated a
	// TERMINAL residue (catch_all/provider_no_smtp) to the vendor inline. nil when no vendor call was
	// made (fallback off, not eligible, breaker open, cache hit). The settler persists it: the sync
	// path appends it (AppendExternalCall), the queue path records it INSIDE the settlement CAS
	// (spend-once, GAP-42). Its VerificationID is stamped by the settler (which owns the LOG row id).
	extCall *store.ExternalCallRow
}

// QueueOutcome is what the P5 drainer receives from ComputeForQueue: the transition class plus the
// settle inputs (the LOG row with its id already assigned, the optional raw sidecar, and the
// cache row when cacheable). The drainer maps Class → SettleWorkItem (QSettled) or RequeueWorkItem
// (the transient/capacity classes), NEVER logging outside the CAS settlement.
type QueueOutcome struct {
	Class      QueueClass
	RetryHintS int
	LastReason string
	Row        store.VerificationRow // id assigned; ready for SettleWorkItem's in-tx LOG insert
	Raw        *store.RawRow         // nil unless an engine blob was produced
	Cache      *store.CachedVerdict  // nil unless the settled verdict is cacheable (post-settle write)

	// ExternalCall is the P6 spend/provenance record when this settle escalated a residue to the
	// vendor (§6.1). The drainer passes it to SettleWorkItem so it commits INSIDE the CAS (spend-once,
	// GAP-42): a reclaimed attempt's charge rolls back with its discarded settlement. nil when no
	// vendor call was made. Its VerificationID is already stamped (ComputeForQueue/EscalateExhausted
	// assign the Row id first, then the vendor call, so the record points at the LOG row it produced).
	ExternalCall *store.ExternalCallRow
}

// classifyRan maps a folded SMTP verdict's reason to a queue class (§4.5). Greylist + network
// transients are retryable; everything else (accepted/mailbox_not_found/catch_all/mailbox_full/
// smtp_blocked/…) is terminal — smtp_blocked is NOT retried by the greylist loop (reputation is
// the health-watcher's job, §4.5). retryHintS seeds the backoff from signals.retry_after_hint_s.
func classifyRan(v policy.Verdict) (QueueClass, int, string) {
	switch v.Reason {
	case vocab.ReasonGreylisted:
		return QGreylisted, hintSeconds(v.Signals), string(vocab.ReasonGreylisted)
	case vocab.ReasonSMTPTimeout, vocab.ReasonConnectionFailed:
		return QTransientNet, 0, string(v.Reason)
	default:
		return QSettled, 0, string(v.Reason)
	}
}

// classifySkip maps a frozen §8 smtp_skipped_reason to a queue class (§4.6). rate_limited is a pure
// capacity wait (queue absorbs the P4 backpressure); the health/routing gaps also wait for the
// pool to recover. Both requeue WITHOUT bumping the attempt count (a capacity/health wait must not
// burn the greylist retry budget, §4.6/OQ-P5-7).
func classifySkip(skip string) (QueueClass, string) {
	if skip == reasonRateLimited {
		return QRateLimited, reasonRateLimited
	}
	return QNoEligibleNode, skip
}

// hintSeconds reads signals.retry_after_hint_s tolerating int or float64 (JSON round-trips numbers
// as float64). Absent/zero → 0 (the scheduler uses the policy default first step, §4.2).
func hintSeconds(signals map[string]any) int {
	switch v := signals["retry_after_hint_s"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// Verify runs the full lifecycle for one address under the given policy definition, and
// returns the Door-1 response. The verdict is LOGged (append-only) before returning.
//
// It is compute + settleSync: compute() runs the log-FREE lifecycle (normalize→cache→cheap→SMTP
// gates), settleSync() dispatches to the three pre-existing log calls (logOnly ×2, logAndCacheRaw)
// arg-for-arg. The split is a pure statement-move — the sync path is byte-identical to the prior
// single-function Verify (the queue classifier fields compute() also sets are never read here). It
// exists so the P5 drainer can reuse the IDENTICAL P4 gate path via ComputeForQueue (log-free) and
// settle via CAS, resolving §5.3 (no LOG row for an intermediate greylist) vs Door 2 (the log is
// the point) — see §8 master invariant.
func (o *Orchestrator) Verify(ctx context.Context, rawEmail string, def *policy.Definition) (verdict.Response, error) {
	if o.hasher == nil || o.store == nil {
		return verdict.Response{}, ErrNotReady
	}
	// Sync /verify has no bulk job → jobID "" (the per_job cap applies only to bulk jobs; a sync
	// check's escalation is bounded by the day/month caps).
	c, err := o.compute(ctx, rawEmail, def, "")
	if err != nil {
		return verdict.Response{}, err
	}
	if err := o.settleSync(ctx, c); err != nil {
		return verdict.Response{}, err
	}
	return c.resp, nil
}

// settleSync performs the sync path's settlement — the three pre-existing log calls, unchanged.
// logReject/logFromCache → logOnly (as the old finishReject/finishFromCache did); logFull →
// logAndCacheRaw (as the old main path did), arg-for-arg. This is the ONLY code sync executes
// beyond compute; it is a dispatch, not new logic — the byte-identical proof.
func (o *Orchestrator) settleSync(ctx context.Context, c computed) error {
	switch c.kind {
	case logReject, logFromCache:
		return o.logOnly(ctx, c.resp, c.row)
	default: // logFull
		return o.logAndCacheRaw(ctx, c.resp, c.row, c.emailHash, c.domain, c.prov, c.cacheable, c.rawBlob, c.extCall)
	}
}

// compute runs the log-FREE lifecycle and returns everything a settler needs. It is the body of the
// former Verify with every log/cache CALL removed (moved to settleSync) — the assemble steps and
// the P4 gate path (runSMTP) are unchanged. It additionally derives the QueueClass (ignored by
// sync). Precondition: o.hasher and o.store are non-nil (Verify/ComputeForQueue check ErrNotReady).
// jobID is the owning bulk job for a QUEUED check ("" for a sync /verify), threaded so an INLINE
// external escalation (a terminal residue) enforces the per_job spend cap (§5.2/§4.4) and stamps the
// spend record's job_id — the drainer's exhaustion path already threads it; this closes the same for
// the inline path (the dominant residue class in bulk jobs).
func (o *Orchestrator) compute(ctx context.Context, rawEmail string, def *policy.Definition, jobID string) (computed, error) {
	start := o.now()

	// 1. Normalize + hash (Door 3). An unparseable address is an un-addressable reject:
	//    logged with a NULL hash (§3.2.7), status invalid/invalid_syntax — never dropped.
	normalized, domain, err := o.hasher.Normalize(rawEmail)
	if errors.Is(err, hash.ErrUnparseable) {
		return o.computeReject(rawEmail, start, def), nil
	}
	if err != nil {
		return computed{}, err
	}
	emailHash := o.hasher.EmailHash(normalized)
	localPart := normalized[:indexAt(normalized)]

	// 1b. Phase-7 HISTORICAL-BOUNCE GATE (§4.6), the ONE new P7 gate. If this address has ≥N confirmed
	//     invalid-signal (mailbox_not_found) bounces on record within the TTL, short-circuit to
	//     invalid/historical_bounce — GROUND-TRUTH proof it doesn't exist, stronger than any probe or
	//     cache hit. Nil-safe + inert by default (no history / gate disabled => does not fire). Uses the
	//     RESERVED reason (Door 4, zero new vocab). Reads the address-level email_hash rollup (§3.1).
	//     A post-TTL history degrades to a confidence penalty applied at assembly, not a short-circuit.
	if gv, fired := o.historicalGate(ctx, emailHash, def); fired {
		return o.computeHistoricalBounce(rawEmail, domain, emailHash, localPart, gv, start, def), nil
	}

	// 2. Cache lookup on the COMPOSITE key (email_hash, config_fingerprint) — §4.2. A hit is
	//    still LOGged with from_cache=true (§4.2/OQ-9). A miss/expiry falls through to compute.
	if cv, cerr := o.store.GetCachedVerdict(ctx, emailHash, o.cfgFingerprint); cerr == nil {
		return o.computeFromCache(rawEmail, domain, emailHash, localPart, cv, start, def), nil
	} else if !errors.Is(cerr, store.ErrCacheMiss) {
		return computed{}, cerr
	}

	// 3. Evaluate the cheap layers under the policy (cheapest-sufficient stop-early).
	ev := layers.NewEvidence(rawEmail, normalized, localPart, domain, o.resolver, listAdapter{o.store})
	v, err := o.engine.Evaluate(ctx, def, ev, o.layerSet)
	if err != nil {
		return computed{}, err
	}

	requestID := mustID(o.now())

	// 3b. SMTP stage (§7.1, gated by the Phase-3 §4.4 eligibility rule). Runs ONLY when the cheap
	//     verdict is the non-terminal residue (risky/plausible, no gate stopped it) AND SMTP is
	//     wired AND a node is :smtp-ELIGIBLE (active ∧ green ∧ port25_open ∧ fresh ∧ GAP-24
	//     egress-match) AND under the warmup cap. A definitive cheap gate (invalid/no_mx, null_mx,
	//     disposable, spam_trap) or a :mx-routed provider (provider_no_smtp) is already terminal →
	//     SMTP is skipped. When the residue was SMTP-eligible but no node could serve it (red / no
	//     eligible node / warmup cap), the cheap verdict STANDS with an additive smtp_skipped_reason
	//     signal (§8) and is marked PROVISIONAL → non-cacheable (PB1). Never invalid (OQ-P3-6).
	var rawBlob []byte
	var nodeID, verifierIP string
	provisional := false
	// The queue class defaults to QSettled: a pure cheap-path verdict (no SMTP eligible, or a
	// :mx-routed provider_no_smtp) is terminal — it settles, never retries.
	class := QSettled
	retryHintS := 0
	lastReason := string(v.Reason)
	if smtpEligible(v) {
		out := o.runSMTP(ctx, normalized, domain, requestID, v)
		switch {
		case out.Ran:
			v = foldSMTP(v, out.Result)
			rawBlob, nodeID, verifierIP = out.RawBlob, out.NodeID, out.VerifierIP
			o.persistCatchAll(ctx, domain, out.Result)
			class, retryHintS, lastReason = classifyRan(v)
		case out.SkipReason == reasonSMTPUnwired:
			// SMTP is not wired in this process (M1-G6). The cheap residue stands, annotated so it is no longer indistinguishable from
			// a genuine cheap-layers answer, and PROVISIONAL (our own degraded state is not an answer: never cached). The queue class
			// stays QSettled and lastReason stays the verdict's: this is NOT a capacity wait — nothing in this process will ever make
			// the stage appear, so requeueing would loop forever. Everything else is exactly what it was.
			setSkipSignal(&v, out.SkipReason)
			provisional = out.Provisional
		case out.SkipReason != "":
			// Eligible residue, but no node ran it. Keep the cheap verdict; record WHY SMTP was
			// skipped (frozen §8 token) and carry the PB1 provisional flag to suppress caching.
			setSkipSignal(&v, out.SkipReason)
			provisional = out.Provisional
			class, lastReason = classifySkip(out.SkipReason)
		}
	} else if v.Reason == vocab.ReasonProviderNoSMTP {
		// A :mx-routed big provider (Gmail/O365/Yahoo, §5.1) never reaches the SMTP call — its
		// verdict is risky/provider_no_smtp. Record the frozen §8 token so the LOG shows SMTP was
		// skipped by ROUTING (a SETTLED outcome → remains cacheable, unlike a capacity gap — PB1).
		setSkipSignal(&v, reasonProviderMX)
	}

	// 4. Assemble (single source: response + LOG row). LOG + cache are the SETTLER's job now.
	prov := verdict.Provenance{
		RequestID: requestID, CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: false,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: mxProviderOf(v.Signals),
		ProviderCanonicalHash: o.providerCanonical(normalized, v.Signals),
		NodeID:                nodeID,
		VerifierIP:            verifierIP, // §5.4: real SMTP egress IP (== audited IP); "" when SMTP didn't run
	}
	// 3c. Phase-6 external fallback (§3), INLINE for the TERMINAL residue classes (catch_all,
	//     provider_no_smtp) — the classes that are settled the moment they occur, escalatable on both
	//     sync /verify and the queue's settle path (§2.2). The retry-eligible residue (greylisted,
	//     gave-up capacity) is NOT escalated here — it still RETRIES (the queue absorbs the transient);
	//     the drainer escalates it only at exhaustion via EscalateExhausted (classifyResidue(...,true)).
	//     This runs AFTER the SMTP fold, so `normalized` is the plaintext (reused, not a new store)
	//     and `v` is the settled residue. keep_residue (no change) when not eligible / off / breaker
	//     open / vendor unknown. A vendor call's spend record rides c.extCall to the settler.
	var extCall *store.ExternalCallRow
	if class == QSettled {
		if rc, ok := classifyResidue(v, false /* inline: terminal residue only */); ok {
			// jobID is "" for sync (no per_job cap) and the work-item's job for a queued check (so the
			// per_job cap is enforced + the spend record's job_id is stamped). workItemID is filled by
			// the drainer at settle (it owns the id); "" here.
			out := o.runExternal(ctx, normalized, v, rc, def, jobID, "", emailHash)
			if out.Call != nil {
				extCall = out.Call
			}
			if out.Escalated {
				v = out.Verdict
				if len(out.RawBlob) > 0 {
					rawBlob = out.RawBlob
				}
				lastReason = string(v.Reason)
			}
		}
	}

	// 3d. Phase-7 CALIBRATION READ (§5.4): replace the P1 baseline confidence for this (reason ×
	//     provider) bucket with the CURRENT EFFECTIVE calibrated confidence, if one exists (external_
	//     verified reads the SEPARATE bucket space, GAP-54). Cold-start / disabled / recommend-only-not-
	//     yet-approved => the baseline stands (§5.6 — zero live mutation on a fresh system). Also applies
	//     the §4.6 TTL-EXPIRED historical-bounce penalty (a recoverable confidence nudge). Nil-safe.
	v = o.calibrationConfidence(ctx, v, emailHash, def)

	resp, row := verdict.Assemble(toCore(v), prov)
	// Cache only SETTLED verdicts (§6.5/OQ-P2-6 + PB1): a transient/your-fault unknown from SMTP is
	// non-cacheable, AND a capacity-gap fallback (SMTP was eligible but no node served it —
	// provisional=true) is non-cacheable even though its cheap reason looks settled. Caching a
	// capacity-gap verdict would freeze a lower-quality answer and suppress the SMTP verdict once the
	// node recovers / the cap lifts (PB1). A provider_mx_routed skip is NOT provisional (settled). An smtp_unwired
	// residue (the stage is not wired in this process: our own degraded state, M1-G6) IS provisional too — it is not an answer.
	// An external_verified verdict IS a settled verdict and caches like any other (§4.3/OQ-P6-9) — so
	// a re-uploaded list returns the cached rented verdict with NO second vendor call (escalate-once
	// at verdict-mint, GAP-43): the cache hit path (computeFromCache) never reaches this stage.
	cacheable := smtp.CacheableReason(v.Status, v.Reason) && !provisional
	return computed{
		resp: resp, row: row, kind: logFull,
		emailHash: emailHash, domain: domain, prov: prov, cacheable: cacheable, rawBlob: rawBlob,
		class: class, retryHintS: retryHintS, lastReason: lastReason, extCall: extCall,
	}, nil
}

// setSkipSignal records the frozen §8 signals.smtp_skipped_reason (additive to the open signals
// bag, Door 1 — no reason/status/recommendation change). It never overwrites a status/reason; it
// only annotates WHY SMTP didn't run so the LOG (and P4/P7) can see it.
func setSkipSignal(v *policy.Verdict, reason string) {
	if reason == "" {
		return
	}
	if v.Signals == nil {
		v.Signals = map[string]any{}
	}
	v.Signals[signalSMTPSkipped] = reason
}

// foldSMTP replaces the cheap residue with the scored SMTP verdict, MERGING the SMTP signals into
// the existing signals bag (additive — §8) rather than discarding the cheap evidence. The SMTP
// status/reason/confidence win (SMTP is the Type-1 core layer that resolves the residue, §7.1).
func foldSMTP(cheap policy.Verdict, res smtp.Result) policy.Verdict {
	signals := cheap.Signals
	if signals == nil {
		signals = map[string]any{}
	}
	for k, val := range res.Signals {
		signals[k] = val // additive P2 signal keys (§8)
	}
	rec := recommendationFor(res.Status, res.Reason)
	return policy.Verdict{
		Status: res.Status, Reason: res.Reason, Confidence: res.Confidence,
		Recommendation: rec, Flags: cheap.Flags, Signals: signals,
		StoppedAt: "smtp",
		Cause:     res.Cause, // evidence for signals.failure_origin on an unknown; no other effect
	}
}

// recommendationFor derives the recommendation from the SMTP status honoring the LOCKED
// composability invariants (vocab.ComposabilityOK). 552 mailbox_full is valid but rec=review
// (existence proven, deliver-now uncertain, §6.1.1). valid/accepted → send. Everything else →
// the safe legal value. Never emits the forbidden invalid+send.
func recommendationFor(status vocab.Status, reason vocab.Reason) vocab.Recommendation {
	switch status {
	case vocab.StatusValid:
		if reason == vocab.ReasonMailboxFull {
			return vocab.RecReview // "full now" caveat rides the recommendation (§6.1.1)
		}
		return vocab.RecSend
	case vocab.StatusInvalid:
		return vocab.RecSkip
	default: // risky / unknown
		return vocab.RecReview
	}
}

// computeReject assembles an un-addressable input (invalid/invalid_syntax, NULL hash) WITHOUT
// logging — the settler logs it (logOnly, as the former finishReject did). kind=logReject.
func (o *Orchestrator) computeReject(rawEmail string, start time.Time, def *policy.Definition) computed {
	core := verdict.Core{
		Status: vocab.StatusInvalid, Reason: vocab.ReasonInvalidSyntax,
		Confidence: confidenceForReject(def), Recommendation: vocab.RecSkip,
		Flags: map[string]bool{}, Signals: map[string]any{"syntax_valid": false},
	}
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		RawEmail: rawEmail, NormalizedDomain: "", EmailHash: nil, // NULL hash (§3.2.7)
	}
	resp, row := verdict.Assemble(core, prov)
	// A reject is terminal (settles, never retries) and NOT cached (its key would be NULL).
	return computed{resp: resp, row: row, kind: logReject, class: QSettled, lastReason: string(vocab.ReasonInvalidSyntax)}
}

// computeHistoricalBounce assembles the §4.6 historical-bounce gate verdict (invalid/historical_bounce)
// WITHOUT logging — the settler logs it (logFull → logAndCacheRaw), exactly like a normal terminal
// verdict. The gate fired on ground-truth (≥N confirmed invalid-signal bounces within the TTL), so it
// is a SETTLED verdict; it is NOT cached (cacheable=false) — a historical-bounce is a per-address rollup
// conclusion that must be RE-EVALUATED against the rollup each time (so a TTL expiry / a re-created
// mailbox is seen promptly, not frozen in the cache). It carries full provenance (email_hash/domain) so
// the LOG row joins normally. Zero new vocab (reason=historical_bounce is P1-reserved, Door 4).
func (o *Orchestrator) computeHistoricalBounce(rawEmail, domain string, emailHash []byte, localPart string, gv policy.Verdict, start time.Time, def *policy.Definition) computed {
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: false,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: mxProviderOf(gv.Signals),
	}
	resp, row := verdict.Assemble(toCore(gv), prov)
	// Terminal (settles, never retries); NOT cached (re-evaluate the rollup each time — TTL/recovery).
	return computed{
		resp: resp, row: row, kind: logFull,
		emailHash: emailHash, domain: domain, prov: prov, cacheable: false, rawBlob: nil,
		class: QSettled, lastReason: string(vocab.ReasonHistoricalBounce),
	}
}

// computeFromCache assembles a cache hit (from_cache=true) WITHOUT logging — the settler logs it
// (logOnly, as the former finishFromCache did). kind=logFromCache; a cache hit is never re-cached.
func (o *Orchestrator) computeFromCache(rawEmail, domain string, emailHash []byte, localPart string, cv store.CachedVerdict, start time.Time, def *policy.Definition) computed {
	core := verdict.Core{
		Status: vocab.Status(cv.Status), Reason: vocab.Reason(cv.Reason),
		Confidence: cv.Confidence, Recommendation: vocab.Recommendation(cv.Recommendation),
		Flags: cv.Flags, Signals: cv.Signals,
	}
	prov := verdict.Provenance{
		RequestID: mustID(o.now()), CheckedAt: start, LatencyMS: sinceMS(o.now(), start),
		PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: o.cfgFingerprint,
		FromCache: true,
		RawEmail:  rawEmail, NormalizedDomain: domain, EmailHash: emailHash,
		HashScheme: hash.SchemeTag, LocalPartLen: len(localPart), MXProvider: cv.MXProvider,
	}
	resp, row := verdict.Assemble(core, prov)
	// A cached verdict is (by P2 OQ-P2-6) always a SETTLED verdict → the queue is satisfied without
	// SMTP (§5.5); terminal, never retried.
	return computed{resp: resp, row: row, kind: logFromCache, emailHash: emailHash, domain: domain,
		class: QSettled, lastReason: string(cv.Reason)}
}

// ComputeForQueue runs the log-FREE P4 gate path for a QUEUED check (the P5 drainer's entry point,
// §3/§4.4/§8). It calls the IDENTICAL compute() the sync path uses — same eligibility→sticky→
// rate-limit→warmup gates — and returns a QueueOutcome the drainer maps onto a work-item transition
// (settle via CAS, or requeue), NEVER logging outside the CAS settlement. This is what keeps §5.3
// (no LOG row for an intermediate greylist) compatible with Door 2: the LOG write lives in
// settleSync/SettleWorkItem, and the drainer bypasses settleSync. The Row it returns already has its
// id assigned (so SettleWorkItem's in-tx LOG insert + verification_id stamp are one atomic write).
func (o *Orchestrator) ComputeForQueue(ctx context.Context, rawEmail string, def *policy.Definition, jobID string) (QueueOutcome, error) {
	if o.hasher == nil || o.store == nil {
		return QueueOutcome{}, ErrNotReady
	}
	c, err := o.compute(ctx, rawEmail, def, jobID)
	if err != nil {
		return QueueOutcome{}, err
	}
	// Assign the LOG row id now (mirrors logWithRaw): the drainer hands Row to SettleWorkItem, which
	// inserts it + stamps work_items.verification_id in one tx (GAP-33). A queued reject can't happen
	// (a bulk item is a real address that normalized), but if it did it settles like any terminal.
	row := c.row
	id, ierr := idgen.NewV7String(o.now())
	if ierr != nil {
		return QueueOutcome{}, ierr
	}
	row.ID = id
	var raw *store.RawRow
	if len(c.rawBlob) > 0 {
		raw = &store.RawRow{VerificationID: id, Raw: c.rawBlob}
	}
	var cache *store.CachedVerdict
	if c.class == QSettled && c.cacheable && o.cacheTTL > 0 && len(c.emailHash) > 0 {
		now := o.now()
		cache = &store.CachedVerdict{
			EmailHash: c.emailHash, ConfigFingerprint: o.cfgFingerprint, Domain: c.domain,
			MXProvider: c.prov.MXProvider, HashScheme: hash.SchemeTag,
			Status: string(c.resp.Status), Reason: string(c.resp.Reason), Confidence: c.resp.Confidence,
			Recommendation: string(c.resp.Recommendation), Flags: c.resp.Flags, Signals: c.resp.Signals,
			CachedAt: now, ExpiresAt: now.Add(o.cacheTTL),
		}
	}
	// P6 (§5.4/GAP-42): if compute() escalated a terminal residue to the vendor inline, stamp the
	// spend record with the LOG row id we just assigned and hand it to the drainer, which passes it to
	// SettleWorkItem so it commits INSIDE the CAS (spend-once — a reclaimed attempt's charge rolls
	// back with its discarded settlement).
	extCall := c.extCall
	if extCall != nil {
		extCall.VerificationID = id
		extCall.WorkItemID = "" // stamped by the drainer (it owns the work-item id)
		// JobID is already set (compute threaded it into runExternal → externalCallRow), so both the
		// per_job budget DECISION and the recorded row see the real job — closing the inline-path hole.
	}
	return QueueOutcome{
		Class: c.class, RetryHintS: c.retryHintS, LastReason: c.lastReason,
		Row: row, Raw: raw, Cache: cache, ExternalCall: extCall,
	}, nil
}

// logAndCacheRaw assigns the LOG id, appends the row + optional raw engine sidecar blob (Door 2),
// and — if the verdict is cacheable AND a TTL is configured AND the address is hashable — writes
// the cache. cacheable=false skips the cache write (§6.5: never cache a transient unknown). The
// raw blob (the fork's /v1/verify response incl. smtp_debug) rides the verification_raw sidecar
// (§6.4 — no new column, no append-only violation).
func (o *Orchestrator) logAndCacheRaw(ctx context.Context, resp verdict.Response, row store.VerificationRow, emailHash []byte, domain string, prov verdict.Provenance, cacheable bool, rawBlob []byte, extCall *store.ExternalCallRow) error {
	logID, err := o.logWithRawID(ctx, resp, row, rawBlob)
	if err != nil {
		return err
	}
	// P6 (§6.1): a sync escalation's spend/provenance record appends AFTER the LOG row (the sync path
	// settles by logging, not a CAS). Stamp the verification_id we just assigned so the record points
	// at the verdict it produced (the rented population is joinable — §6.3). Best-effort: a
	// spend-record write failure must NOT drop an already-logged verdict (Door 2), but it IS logged.
	if extCall != nil {
		extCall.VerificationID = logID
		if cerr := o.store.AppendExternalCall(ctx, *extCall); cerr != nil {
			logExternalCallErr(cerr)
		}
	}
	// Cache only when the verdict is settled AND a TTL is set (expires_at is NOT NULL, §4.2). No
	// TTL configured (OQ-9 unset) => skip caching, never write a guessed TTL. A transient unknown
	// (cacheable=false) is never cached even with a TTL (§6.5/OQ-P2-6).
	if !cacheable || o.cacheTTL <= 0 || len(emailHash) == 0 {
		return nil
	}
	now := o.now()
	cv := store.CachedVerdict{
		EmailHash: emailHash, ConfigFingerprint: o.cfgFingerprint, Domain: domain,
		MXProvider: prov.MXProvider, HashScheme: hash.SchemeTag,
		Status: string(resp.Status), Reason: string(resp.Reason), Confidence: resp.Confidence,
		Recommendation: string(resp.Recommendation), Flags: resp.Flags, Signals: resp.Signals,
		CachedAt: now, ExpiresAt: now.Add(o.cacheTTL),
	}
	return o.store.PutCachedVerdict(ctx, cv)
}

// logOnly assigns a fresh UUIDv7 id and appends the LOG row with NO raw sidecar (cheap path,
// rejects, cache re-logs). A failure here FAILS the request (Door 2: never return a verdict we
// couldn't log).
func (o *Orchestrator) logOnly(ctx context.Context, resp verdict.Response, row store.VerificationRow) error {
	return o.logWithRaw(ctx, resp, row, nil)
}

// logWithRaw assigns a fresh UUIDv7 id and appends the LOG row plus an optional raw engine blob
// (the fork's /v1/verify response) into the verification_raw sidecar (§6.4). The append is atomic
// (the store wraps both in one tx). A failure FAILS the request (Door 2).
func (o *Orchestrator) logWithRaw(ctx context.Context, resp verdict.Response, row store.VerificationRow, rawBlob []byte) error {
	_, err := o.logWithRawID(ctx, resp, row, rawBlob)
	return err
}

// logWithRawID is logWithRaw returning the assigned LOG id (so a sync external-call record can stamp
// its verification_id — §6.1). Same append semantics; the id is a plain UUIDv7 (Door 2 join anchor).
func (o *Orchestrator) logWithRawID(ctx context.Context, resp verdict.Response, row store.VerificationRow, rawBlob []byte) (string, error) {
	id, err := idgen.NewV7String(o.now())
	if err != nil {
		return "", err
	}
	row.ID = id
	// Defensive: never persist/emit an out-of-contract verdict (Door 1/4).
	if !resp.Valid() {
		return "", errors.New("orchestrator: refusing to log an out-of-contract verdict")
	}
	var raw *store.RawRow
	if len(rawBlob) > 0 {
		raw = &store.RawRow{VerificationID: id, Raw: rawBlob}
	}
	if err := o.store.AppendVerification(ctx, row, raw); err != nil {
		return "", err
	}
	return id, nil
}

// logExternalCallErr surfaces a best-effort external-call spend-record write failure. The verdict is
// already logged authoritatively (Door 2); a lost spend record is an observability gap, not a
// verdict failure — but it is never silent (§9 duplicate/spend-record health).
func logExternalCallErr(err error) {
	log.Printf("orchestrator: external-call spend record write failed (verdict is logged; spend accounting may under-count): %v", err)
}

func (o *Orchestrator) providerCanonical(normalized string, signals map[string]any) []byte {
	prov := mxProviderOf(signals)
	if prov == "" {
		return nil
	}
	h, ok := o.hasher.ProviderCanonicalHash(normalized, prov)
	if !ok {
		return nil
	}
	return h
}

// listAdapter adapts the store to the layers.ListSource capability interface.
type listAdapter struct{ s store.Store }

func (a listAdapter) IsDisposable(ctx context.Context, d string) (bool, error) {
	return a.s.IsDisposable(ctx, d)
}
func (a listAdapter) IsSpamTrap(ctx context.Context, d string) (bool, error) {
	return a.s.IsSpamTrap(ctx, d)
}

// ── small helpers ────────────────────────────────────────────────────────────────────

func toCore(v policy.Verdict) verdict.Core {
	return verdict.Core{
		Status: v.Status, Reason: v.Reason, Confidence: v.Confidence,
		Recommendation: v.Recommendation, Flags: v.Flags, Signals: v.Signals,
		Cause: v.Cause,
	}
}

func mxProviderOf(signals map[string]any) string {
	if s, ok := signals["mx_provider"].(string); ok {
		return s
	}
	return ""
}

func confidenceForReject(def *policy.Definition) int {
	if v, ok := def.Confidence.BaselineByReason["invalid_syntax"]; ok {
		return v
	}
	return 99
}

func indexAt(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			return i
		}
	}
	return 0
}

// mustID returns a bare UUIDv7 string for request_id. It must be a plain UUID (no prefix):
// the LOG's request_id column is Postgres `uuid` (§3.2.2). A crypto/rand failure is
// vanishingly unlikely; if it happens we still return a valid all-zero-ish UUID rather than
// a non-UUID sentinel that the DB would reject.
func mustID(t time.Time) string {
	id, err := idgen.NewV7String(t)
	if err != nil {
		return "00000000-0000-7000-8000-000000000000"
	}
	return id
}

func sinceMS(now, start time.Time) int {
	ms := int(now.Sub(start) / time.Millisecond)
	if ms < 0 {
		return 0
	}
	return ms
}
