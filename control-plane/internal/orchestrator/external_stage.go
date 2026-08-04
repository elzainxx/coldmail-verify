package orchestrator

import (
	"context"
	"time"

	"github.com/coldmail-verify/control-plane/internal/external"
	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the Phase-6 external-fallback stage (§3). It runs AFTER the SMTP fold, ONLY when the
// settled verdict is one of the four §4.1 residue classes the policy opts to escalate (route_when),
// AND the fallback is wired (an owner enabled it + a vendor key + DPA), AND the per-vendor budget/
// breaker is open. It calls the vendor for the PLAINTEXT address, applies the anti-laundering +
// OQ-P6-3 rules (external.Resolve), and returns a replacement verdict + the spend/provenance record.
//
// The MASTER INVARIANT (§8) is preserved by WHERE this runs: the shared compute() gate path
// (eligibility→sticky→rate-limit→warmup) is BYTE-IDENTICAL to P5 — the external stage is an
// ADDITIVE step the SETTLER runs, reached identically whether the check came in sync or through the
// queue. compute() only CLASSIFIES the residue (no vendor call in the shared path); the paid call
// happens in the settle path so spend-once (GAP-42) is atomic with the CAS.
//
// unknown stays first-class (principle 2): a vendor unknown/down/over-budget/breaker-open returns
// keepResidue — the residue verdict stands UNCHANGED. The fallback can only ever IMPROVE a residue
// verdict, never degrade it, and its absence never makes the system worse than P5 left it.
//
// Door 5: the vendor is a brain-side HTTP call, never a node — no gem/box surface. Door 3: the
// plaintext egresses transiently (reused from the P5 queue plaintext / the sync raw input) and is
// NEVER persisted by P6 (the external_calls record stores email_hash only).

// ExternalDeps wires the fallback stage. nil => the layer is fully OFF (no escalation ever) — the
// P1–P5 posture and the default (enabling is a deliberate owner action, not a code default). All
// fields are interfaces/values so tests inject a mock vendor + fake store without a real vendor/DB.
type ExternalDeps struct {
	Vendor external.Vendor // the swappable adapter (§8); the mock in tests, an HTTP client at launch

	// Budget + breaker DIALs (§5). Read on every escalation against the durable spend facts.
	Budget  external.Budget
	Breaker external.BreakerParams

	// GloballyDisabled is the global-fallback-kill (§5-adjacent / kill-switch-ladder Global A): one
	// operator switch that stops ALL vendor egress across every policy in one action, INDEPENDENT of
	// per-policy config and breaker state (the panic button for a billing runaway / privacy incident).
	// Set from the FallbackOff kill-switch rung; when true, runExternal never calls the vendor.
	GloballyDisabled bool

	// Now anchors the budget windows + breaker cooldown (injected for deterministic tests).
	Now func() time.Time
}

// externalStageOutcome is runExternal's result. Escalated=false => the residue verdict stands
// UNCHANGED (not eligible, layer off, breaker open, or vendor unknown — keep_residue). When
// Escalated, Verdict replaces the residue, RawBlob is the vendor JSON for the sidecar, and Call is
// the spend/provenance record the settler persists (queued: inside the CAS — spend-once; sync: a
// plain append). Call is recorded EVEN on a vendor error (Outcome="error", CostUnits=0) so the
// error-rate breaker window is measurable — but on an error Escalated stays false (keep_residue).
type externalStageOutcome struct {
	Escalated bool
	Verdict   policy.Verdict
	RawBlob   []byte
	Call      *store.ExternalCallRow
}

// runExternal is the fallback stage. cheap is the settled residue verdict (post-SMTP-fold); class is
// its residue class (from external.Classify); addr is the plaintext (reused from the sync input /
// the queue's decrypted plaintext — never a new plaintext store). It returns whether the residue was
// escalated, and if so the replacement verdict + spend record. It NEVER fabricates a verdict: a
// vendor error/timeout/down or a vendor unknown yields keep_residue (Escalated=false).
func (o *Orchestrator) runExternal(ctx context.Context, addr string, cheap policy.Verdict, class external.ResidueClass, def *policy.Definition, jobID, workItemID string, emailHash []byte) externalStageOutcome {
	ext := o.external
	// Layer OFF entirely, or the global kill-switch pulled (§ global-fallback-kill): no egress, ever.
	if ext == nil || ext.Vendor == nil || ext.GloballyDisabled {
		return externalStageOutcome{}
	}
	// The policy must OPT IN to fallback (the `external` block) AND to THIS residue class (route_when).
	// A policy without the block, or one whose route_when omits this class, never escalates it — the
	// mechanical opt-out lever (§7.5) and the per-policy cost dial (§3.2).
	pe := def.External
	if pe == nil || !pe.Enabled || !external.RouteWhenAllows(pe.RouteWhen, class) {
		return externalStageOutcome{}
	}

	vendor := ext.Vendor.Name()
	now := o.extNow()

	// §5.3 budget + circuit breaker: read the durable per-vendor spend facts and decide. Breaker
	// open / budget exhausted → skip → keep_residue (LOCKED §5.3; never a fabricated verdict). A
	// facts-read error also fails to keep_residue (never sprint past an unknown budget — the money
	// analogue of the SMTP limiter's fail-closed posture).
	facts, ferr := o.externalSpendFacts(ctx, vendor, jobID, now)
	if ferr != nil {
		return externalStageOutcome{}
	}
	dec := external.Decide(ext.Budget, ext.Breaker, facts, now)
	if !dec.Allow {
		// keep_residue on budget-exhausted / breaker-open (§3.2 on_budget_exhausted). No vendor call,
		// no charge, no record — the residue settles exactly as P5 left it.
		return externalStageOutcome{}
	}

	// The paid call. idempotencyKey (email_hash ‖ vendor) lets an adapter that supports a vendor
	// idempotency key (OQ-P6-4) return the same billed result on a P5-reclaim retry.
	idem := external.DeriveVendorIdempotencyKey(emailHash, vendor)
	vr, callErr := ext.Vendor.Verify(ctx, addr, idem)
	if callErr != nil {
		// Vendor down/timeout/rate-limited (§5.3 cond 2/3): record the ERROR (Outcome="error",
		// cost 0) so the error-rate window is measurable + the breaker can trip, then keep_residue.
		// NEVER a fabricated verdict on an error (principle 2).
		return externalStageOutcome{Call: o.externalCallRow(vendor, class, jobID, workItemID, emailHash, external.VendorResult{}, "error", now)}
	}

	// Anti-laundering (§3.4) + OQ-P6-3 asymmetry: the single place a rented verdict becomes a system
	// verdict. It CANNOT produce SMTP-grade valid (a vendor valid → risky, capped).
	resn := external.Resolve(vr, class, pe.ConfidenceCap)
	callRow := o.externalCallRow(vendor, class, jobID, workItemID, emailHash, vr, "ok", now)
	if resn.KeepResidue {
		// Vendor unknown — the residue stays first-class (§3.2 on_vendor_unknown). We DID pay (a call
		// was made), so the spend/provenance record is still written (Outcome="ok", real cost), but
		// the verdict is unchanged. (The vendor charging for an "unknown" is real; P7 measures it.)
		return externalStageOutcome{Call: callRow}
	}

	folded := foldExternal(cheap, resn, vr, vendor, class)
	return externalStageOutcome{Escalated: true, Verdict: folded, RawBlob: vr.RawJSON, Call: callRow}
}

// foldExternal replaces the residue verdict with the resolved external verdict, MERGING the
// signals.external.* provenance bag into the existing signals (additive — Door 1, §6.2) so P7 can
// isolate + measure the rented population per (residue_class × provider × vendor). The vendor's raw
// strings live ONLY in signals.external.* + the `raw` sidecar — NEVER in status/reason (§8.1). The
// resolved status/reason/confidence (never valid, always external_verified, always ≤ cap) win.
func foldExternal(cheap policy.Verdict, resn external.Resolution, vr external.VendorResult, vendor string, class external.ResidueClass) policy.Verdict {
	signals := cheap.Signals
	if signals == nil {
		signals = map[string]any{}
	}
	// The §6.2 provenance bag — the parsed, queryable projection (the `raw` sidecar holds fidelity).
	signals["external"] = map[string]any{
		"vendor":                vendor,
		"vendor_status_raw":     vr.StatusRaw,
		"vendor_confidence_raw": vr.ConfidenceRaw,
		"residue_class":         string(class),
		"request_id":            vr.RequestID,
		"cost_units":            vr.CostUnits,
		"latency_ms":            vr.LatencyMS,
		"mx_provider":           mxProviderOf(cheap.Signals),
	}
	rec := recommendationFor(resn.Status, resn.Reason)
	return policy.Verdict{
		Status: resn.Status, Reason: resn.Reason, Confidence: resn.Confidence,
		Recommendation: rec, Flags: cheap.Flags, Signals: signals,
		StoppedAt: "external",
	}
}

// externalCallRow builds the append-only spend/provenance record (§6.1). outcome ∈ {ok, error}.
// It stores email_hash ONLY (Door 3 — never plaintext, §7.3). verification_id is stamped LATER by
// the settler (it owns the LOG row id); here we leave it "" and the settler fills it.
func (o *Orchestrator) externalCallRow(vendor string, class external.ResidueClass, jobID, workItemID string, emailHash []byte, vr external.VendorResult, outcome string, now time.Time) *store.ExternalCallRow {
	id, err := idgen.NewV7String(now)
	if err != nil {
		return nil // no id → no spend record (the settle/log still holds; a missing record is safe-fail)
	}
	cost := vr.CostUnits
	if outcome == "error" {
		cost = 0 // an errored call produced no verdict → no charge (still counts to the error window)
	}
	return &store.ExternalCallRow{
		ID: id, WorkItemID: workItemID, JobID: jobID, Vendor: vendor, ResidueClass: string(class),
		EmailHash: emailHash, VendorStatusRaw: vr.StatusRaw, VendorConfidenceRaw: vr.ConfidenceRaw,
		RequestID: vr.RequestID, CostUnits: cost, Outcome: outcome, LatencyMS: vr.LatencyMS, CalledAt: now,
	}
}

// externalSpendFacts reads the durable per-vendor spend facts for the breaker (§5.2/§5.3), anchoring
// the day/month/error windows at now. The windows are fixed here (rolling day/month; error window =
// the breaker cooldown, a reasonable recent-failure horizon) — their tuning is a dial via the caps
// themselves. Maps the store's counts onto external.SpendFacts (the store never imports external).
func (o *Orchestrator) externalSpendFacts(ctx context.Context, vendor, jobID string, now time.Time) (external.SpendFacts, error) {
	dayStart := now.Add(-24 * time.Hour)
	monthStart := now.Add(-30 * 24 * time.Hour)
	errWindow := now.Add(-o.externalErrWindow())
	minuteStart := now.Add(-time.Minute) // the rate_ceiling burst window (§5.2)
	s, err := o.store.ExternalSpendSince(ctx, vendor, jobID, dayStart, monthStart, errWindow, minuteStart, now)
	if err != nil {
		return external.SpendFacts{}, err
	}
	return external.SpendFacts{
		SpentDay: s.SpentDay, SpentMonth: s.SpentMonth, SpentJob: s.SpentJob,
		Calls: s.Calls, Errors: s.Errors, CallsLastMinute: s.CallsLastMinute,
	}, nil
}

// externalErrWindow is the error-rate measurement window: the breaker cooldown, or a safe 15m
// default when the cooldown is unset. (A short window makes the error-rate responsive without a
// separate dial — the cooldown already expresses "how long a bad vendor stays out".)
func (o *Orchestrator) externalErrWindow() time.Duration {
	if o.external != nil && o.external.Breaker.Cooldown > 0 {
		return o.external.Breaker.Cooldown
	}
	return 15 * time.Minute
}

func (o *Orchestrator) extNow() time.Time {
	if o.external != nil && o.external.Now != nil {
		return o.external.Now()
	}
	return o.now()
}

// classifyResidue reports the residue class of a settled verdict and whether it is escalatable at
// this point (§4.1/§4.2). exhausted distinguishes the inline path (compute/sync: only the terminal
// catch_all/provider_no_smtp classes) from the drainer's give-up path (retry-exhausted greylisted /
// gave-up capacity). It reads off the EXISTING verdict — no change to gate mechanics.
func classifyResidue(v policy.Verdict, exhausted bool) (external.ResidueClass, bool) {
	return external.Classify(v.Status, v.Reason, v.Signals, exhausted)
}

// EscalateExhausted is the drainer's give-up escalation (§2.2, §4.1 classes c/d). When the P5 retry
// budget is spent (max_attempts / max_retry_age), the drainer is about to settle the residue as
// "dead" (the reserved unknown/greylisted verdict, or the gave-up-capacity risky/plausible). BEFORE
// it does, it calls this to give the fallback ONE chance to rent a verdict for the residue the pool
// exhausted — the retry-exhausted greylisted (c) and gave-up rate_limited (d) classes, which are
// escalatable ONLY at exhaustion (before that they retry — the queue absorbs the transient).
//
// It takes the already-computed QueueOutcome (its Row is the settled residue verdict, id assigned),
// escalates if eligible, and returns a POSSIBLY-REPLACED QueueOutcome: on escalation the Row is
// rebuilt from the external verdict (id preserved so the spend record + CAS still point at it) and
// ExternalCall is set (recorded inside the settle CAS — spend-once). Not eligible / off / breaker
// open / vendor unknown → the outcome is returned UNCHANGED (settle the residue as-is, keep_residue).
// The plaintext addr is the drainer's already-decrypted address (reused, never a new store).
func (o *Orchestrator) EscalateExhausted(ctx context.Context, addr string, def *policy.Definition, out QueueOutcome, jobID, workItemID string) QueueOutcome {
	if o.external == nil {
		return out
	}
	// Reconstruct the settled residue verdict from the Row (the drainer's out.Row is what it would
	// settle). We classify off the Row's status/reason/signals with exhausted=true.
	status := vocab.Status(out.Row.Status)
	reason := vocab.Reason(out.Row.Reason)
	rc, ok := external.Classify(status, reason, out.Row.Signals, true /* exhausted give-up path */)
	if !ok {
		return out // not an escalatable residue class → settle as-is
	}
	cheap := policy.Verdict{
		Status: status, Reason: reason, Confidence: out.Row.Confidence,
		Recommendation: vocab.Recommendation(out.Row.Recommendation),
		Flags:          out.Row.Flags, Signals: out.Row.Signals,
	}
	res := o.runExternal(ctx, addr, cheap, rc, def, jobID, workItemID, out.Row.EmailHash)
	if res.Call != nil {
		res.Call.VerificationID = out.Row.ID // point the spend record at the (same-id) settled verdict
		res.Call.WorkItemID = workItemID
		out.ExternalCall = res.Call
	}
	if res.Escalated {
		// Rebuild the LOG row from the external verdict, PRESERVING the id (so the CAS + spend record
		// still reference it) and the queue provenance. The verdict becomes external_verified at capped
		// confidence — the residue the pool couldn't answer is now a rented (separately-measurable) one.
		out.Row = rebuildRowFromVerdict(out.Row, res.Verdict)
		out.LastReason = out.Row.Reason
		if len(res.RawBlob) > 0 {
			out.Raw = &store.RawRow{VerificationID: out.Row.ID, Raw: res.RawBlob}
		}
		// The escalated verdict is settled (external_verified) — it caches like any settled verdict
		// (§4.3/OQ-P6-9). Rebuild the cache row so a re-verify of the same identity returns it with NO
		// second vendor call (GAP-43). Only when a TTL is configured + hashable (mirrors compute).
		out.Cache = o.rebuildCacheFromVerdict(out.Row, res.Verdict)
	}
	return out
}

// rebuildRowFromVerdict overwrites a settled LOG row's verdict fields with the external-folded
// verdict, keeping the id + all provenance (email_hash, domain, policy, config_fingerprint, etc.).
// The row was already assembled by compute(); we replace only status/reason/confidence/recommendation
// /flags/signals with the rented verdict (Door 4 values, all valid by construction — Resolve()).
func rebuildRowFromVerdict(row store.VerificationRow, v policy.Verdict) store.VerificationRow {
	row.Status = string(v.Status)
	row.Reason = string(v.Reason)
	row.Confidence = v.Confidence
	row.Recommendation = string(v.Recommendation)
	row.Flags = v.Flags
	row.Signals = v.Signals
	return row
}

// rebuildCacheFromVerdict builds the cache row for an escalated (external_verified) queued verdict,
// or nil when caching is off/unhashable (mirrors ComputeForQueue's cache gate). external_verified is
// a settled verdict → cacheable (§4.3/OQ-P6-9); caching it is what makes escalate-once hold under
// re-upload (GAP-43).
func (o *Orchestrator) rebuildCacheFromVerdict(row store.VerificationRow, v policy.Verdict) *store.CachedVerdict {
	if o.cacheTTL <= 0 || len(row.EmailHash) == 0 {
		return nil
	}
	now := o.now()
	return &store.CachedVerdict{
		EmailHash: row.EmailHash, ConfigFingerprint: o.cfgFingerprint, Domain: row.Domain,
		MXProvider: row.MXProvider, HashScheme: row.HashScheme,
		Status: string(v.Status), Reason: string(v.Reason), Confidence: v.Confidence,
		Recommendation: string(v.Recommendation), Flags: v.Flags, Signals: v.Signals,
		CachedAt: now, ExpiresAt: now.Add(o.cacheTTL),
	}
}
