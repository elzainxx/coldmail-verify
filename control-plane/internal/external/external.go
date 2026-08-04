// Package external is the Phase-6 third-party fallback layer (spec phase-6-fallback-spec.md):
// the brain-side call to a paid external verifier, routed ONLY the residue every prior phase
// structurally could not answer (catch-all, :mx-routed big providers, retry-exhausted greylisted,
// gave-up rate-limited). It is the FIRST deliberate egress of plaintext PII to a third party, and
// it ships DISABLED by default — enabling it is an owner action gated on the §7.4 DPA checklist,
// never a code default (see cmd/control-plane: buildExternal wires it only when Fallback==enabled
// AND a vendor key is present AND the DPA is confirmed).
//
// This file is the CORRECTNESS HEART of the phase. Two load-bearing, irreversible rules live here:
//
//  1. ANTI-LAUNDERING (§3.4, LOCKED) — Resolve(): a vendor "valid" becomes status:risky at a CAPPED
//     confidence, NEVER SMTP-grade valid. A rented verdict is someone else's answer about someone
//     else's evidence; laundering it into our own valid would poison P7's calibration with the
//     vendor's errors and destroy the ability to measure & wean the rented population (GAP-41). Every
//     external verdict carries reason=external_verified (reserved in P1 — Door 4, zero new vocab).
//
//  2. OQ-P6-3 ASYMMETRY (DECIDED) — Resolve(): a vendor "invalid" is accepted as invalid ONLY where
//     an INDEPENDENT non-existence is structurally plausible (retry-exhausted, gave-up-capacity), and
//     is DOWNGRADED to risky on catch-all/:mx residue (an accept-all domain accepts everything, so a
//     vendor "invalid" there is itself a guess — and a false "invalid" IRREVERSIBLY discards a real
//     address). Trust the vendor's "no" more than their "yes," and only where "no" is possible.
//
// The residue routing (§4, the cost firewall) is Classify(): a CLOSED set of four classes; nothing
// else is ever eligible. unknown stays first-class — a vendor unknown/down/over-budget keeps the
// residue verdict exactly as P5 left it (KeepResidue), never fabricates, never degrades (principle 2).
//
// The vendor is an INTERFACE (Vendor), not a hardwired provider (§8) — swappable, A/B-able later
// (SEAM → P7). One vendor at launch. No gem/box surface (Door 5): the vendor is a brain-side HTTP
// call, never a node, never health-watched or rate-limited by the P4 pool (it has its OWN budget +
// breaker, §5, distinct from the P4 IP-reputation limiter — GAP-44).
package external

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// DeriveVendorIdempotencyKey computes the vendor-call idempotency key (§5.4/OQ-P6-4):
// hex(sha256(email_hash ‖ vendor)). It REUSES the Door-3 artifact (email_hash — already HMAC'd under
// the pepper), so there is NO new hashing scheme and NO new pepper exposure (Door 3). Passing it to a
// vendor that supports an idempotency key makes a P5-reclaim retry return the SAME billed result
// (not a new charge); where the vendor offers no such key it is still a useful audit/dedup trace.
// Deterministic (same address × vendor ⇒ same key), so it never leaks time/randomness.
func DeriveVendorIdempotencyKey(emailHash []byte, vendor string) string {
	h := sha256.New()
	h.Write(emailHash)
	h.Write([]byte{0}) // domain separator so field boundaries can't collide
	h.Write([]byte(vendor))
	return hex.EncodeToString(h.Sum(nil))
}

// Vendor is the swappable external-verifier adapter (§8.1). A concrete adapter (the mock here; a
// real MillionVerifier-like HTTP client later) holds its own API key, enforces the vendor's
// request/idempotency contract, and NORMALIZES the vendor's status strings into the system
// vocabulary at THIS boundary (§8.2) — no vendor-specific string ever reaches status/reason.
//
// idempotencyKey is sha256-derived (email_hash ‖ vendor) — passed so an adapter that supports a
// vendor idempotency key (OQ-P6-4) returns the SAME billed result on a P5-reclaim retry, not a new
// charge. Where the vendor offers no such key the CAS still guarantees settle-once/cost-record-once
// (§5.4/GAP-42); the key only tightens the bounded worst case.
type Vendor interface {
	// Name is the adapter handle (matches policy external.vendor + the budget/breaker key, §5.3).
	Name() string
	// Verify calls the vendor for ONE plaintext address. It returns a normalized VendorResult, or a
	// non-nil error for a transport failure/timeout/vendor-down (the caller degrades to keep_residue
	// and trips the breaker — §5.3; it NEVER fabricates a verdict on an error).
	Verify(ctx context.Context, plaintextAddr, idempotencyKey string) (VendorResult, error)
}

// VendorResult is an adapter's answer for one address: the vendor's verdict NORMALIZED into the
// system vocabulary (§8.2) PLUS the full provenance the P7 flywheel needs to measure & wean the
// rented population (§6.2). The adapter fills Result via its normalization table; the raw strings
// live only in StatusRaw + RawJSON (never in status/reason).
type VendorResult struct {
	Result        vocab.Status // normalized (§8.2): valid|invalid|risky|unknown. Unrecognized => unknown.
	StatusRaw     string       // the vendor's own status string, verbatim (§6.2 — P7 measures THEIR grade)
	ConfidenceRaw int          // the vendor's own confidence/score if any (0 => none reported)
	RequestID     string       // vendor call id — idempotency + audit trace to a billed call (§5.4/§6.2)
	CostUnits     int           // what this call cost (spend accounting — §5, §6.2)
	LatencyMS     int           // vendor round-trip (§6.2 — sync-deadline pressure signal, OQ-P6-7)
	RawJSON       []byte        // the untouched vendor JSON → the reserved `raw` sidecar (§3.3, Door 1)
}

// ResidueClass is one of the four structurally-unanswerable residue classes (§4.1). This is a
// CLOSED set (LOCKED): a policy's route_when may escalate a SUBSET, never add a fifth. It is a
// provenance tag (logged in signals.external.residue_class) AND the OQ-P6-3 asymmetry key
// (Resolve() trusts a vendor "invalid" only on the two classes where non-existence is plausible).
type ResidueClass string

const (
	// ResidueCatchAll — risky/catch_all|accept_all (P2 §5.5): the domain accepts every RCPT; SMTP
	// cannot prove a specific mailbox exists. A vendor "invalid" here is a GUESS (OQ-P6-3 → risky).
	ResidueCatchAll ResidueClass = "catch_all"
	// ResidueProviderNoSMTP — risky/provider_no_smtp (P1 §6.9/P2 §5.5): Gmail/Yahoo/O365 are
	// :mx-routed, never reach SMTP; their 250s are pure risk. Vendor "invalid" here is a guess too.
	ResidueProviderNoSMTP ResidueClass = "provider_no_smtp"
	// ResidueRetryExhausted — unknown/greylisted settled at max_attempts/max_retry_age (P5 §4.5):
	// the pool retried through backoff and never resolved. An INDEPENDENT invalid IS plausible here
	// (the address may genuinely not exist) → a vendor "invalid" is accepted (OQ-P6-3).
	ResidueRetryExhausted ResidueClass = "retry_exhausted_greylisted"
	// ResidueGaveUpRateLimited — risky/plausible carrying smtp_skipped_reason=rate_limited that a P5
	// retry could not clear within max_retry_age (P4 §4.6/P5 §4.6): every eligible node stayed capped;
	// we never ran SMTP. Independent invalid plausible here too → vendor "invalid" accepted (OQ-P6-3).
	ResidueGaveUpRateLimited ResidueClass = "gave_up_rate_limited"
)

// Classify maps a TERMINAL verdict to its residue class (§4.1), and whether it is escalatable NOW.
// It is the entry predicate of the whole cost firewall — anything it returns false for NEVER reaches
// the vendor (§4.2). Nothing outside the four classes is ever eligible.
//
// exhausted distinguishes WHEN a class is escalatable, preserving the P5 master invariant:
//   - catch_all (a) and provider_no_smtp (b) are TERMINAL the moment they occur — escalatable inline
//     on both sync /verify and the queue's settle path (§2.2), never retried.
//   - greylisted (c) and gave-up-capacity (d) are escalatable ONLY when the P5 retry budget is spent
//     (exhausted=true — the drainer's "give up" path). Before exhaustion they still RETRY (the queue
//     absorbs the transient); escalating a still-retryable greylist would rent an answer the pool was
//     about to earn for free. So compute() calls Classify(..., false) — only a/b fire inline; the
//     drainer calls Classify(..., true) at exhaustion — c/d fire.
//
// A false return is the harden-toward-unknown safe direction: not-a-residue-class => settle as-is.
func Classify(status vocab.Status, reason vocab.Reason, signals map[string]any, exhausted bool) (ResidueClass, bool) {
	switch reason {
	case vocab.ReasonCatchAll, vocab.ReasonAcceptAll:
		return ResidueCatchAll, true
	case vocab.ReasonProviderNoSMTP:
		return ResidueProviderNoSMTP, true
	case vocab.ReasonGreylisted:
		if exhausted {
			return ResidueRetryExhausted, true
		}
		return "", false
	}
	// (d) gave-up capacity: a residue verdict (risky/plausible) that carries the frozen §8
	// smtp_skipped_reason=rate_limited and could not be cleared by a retry within max_retry_age.
	// Only at exhaustion (the drainer gave up waiting for capacity), never inline.
	if exhausted && skipReasonRateLimited(signals) {
		return ResidueGaveUpRateLimited, true
	}
	return "", false
}

// skipReasonRateLimited reports whether the verdict's signals mark it as a gave-up capacity residue
// (the frozen §8 smtp_skipped_reason token — set by the P4 rate limiter, §4.6). Read off the
// existing signals bag; no new key.
func skipReasonRateLimited(signals map[string]any) bool {
	if signals == nil {
		return false
	}
	s, _ := signals["smtp_skipped_reason"].(string)
	return s == "rate_limited"
}

// RouteWhenAllows reports whether a policy's route_when list (the residue classes it opts to
// escalate, §3.2 — a per-policy DIAL) includes this class. An empty/nil list escalates NONE (the
// safe default — a policy without a configured route_when never spends). This is expressed as DATA
// (the policy), not code: which classes a policy escalates is tunable per policy, never a fifth
// class (the closed set is enforced by Classify only ever RETURNING one of the four).
func RouteWhenAllows(routeWhen []string, class ResidueClass) bool {
	for _, c := range routeWhen {
		if c == string(class) {
			return true
		}
	}
	return false
}

// Resolution is the outcome of applying the anti-laundering + OQ-P6-3 rules to a vendor result. It
// is what the orchestrator's fold step maps onto the final verdict. KeepResidue=true means the
// fallback could not help (vendor unknown) — the residue verdict stands UNCHANGED (principle 2:
// unknown stays first-class; the fallback can only ever IMPROVE a residue verdict, never degrade it).
type Resolution struct {
	KeepResidue bool         // true => leave the residue verdict as-is (vendor unknown; §3.2 keep_residue)
	Status      vocab.Status // NEVER valid (§3.4 anti-laundering) when a verdict is produced
	Reason      vocab.Reason // always external_verified when a verdict is produced (Door 4, reserved P1)
	Confidence  int          // ALWAYS ≤ confidenceCap — a rented verdict is never SMTP-grade (§3.4)
}

// Resolve is the ANTI-LAUNDERING resolver (§3.4) + the OQ-P6-3 asymmetry — the single place a rented
// verdict becomes a system verdict. It CANNOT produce SMTP-grade valid, by construction: the valid
// case maps to risky. This is the phase's headline correctness demo (acceptance criterion 2) — the
// fail-verify test removes the valid→risky mapping and asserts a vendor valid can never be valid.
//
//   - vendor valid   → risky/external_verified, capped (§3.4: a rented "valid" is a weaker claim than
//     a 250; capping keeps it honest AND separately measurable so P7 can wean — GAP-41).
//   - vendor invalid → invalid/external_verified ONLY on retry_exhausted/gave_up_capacity (independent
//     non-existence plausible); DOWNGRADED to risky/external_verified on catch_all/provider_no_smtp
//     (a vendor "invalid" on an accept-all domain is a guess, and a false invalid irreversibly
//     discards a real address — OQ-P6-3, the one place a rented verdict does permanent harm).
//   - vendor risky   → risky/external_verified.
//   - vendor unknown (or any unrecognized status normalized to unknown) → KeepResidue (the residue
//     verdict stays; the fallback couldn't help — never a fabricated invalid, §3.2 on_vendor_unknown).
//
// confidenceCap (§3.2 DIAL, recommend 60) is the CEILING on every external-derived confidence — the
// whole rented population stays below any SMTP-earned valid (baseline 90). A vendor that reports a
// lower own confidence lowers it further; it can never raise it above the cap.
func Resolve(vr VendorResult, class ResidueClass, confidenceCap int) Resolution {
	conf := cappedConfidence(vr.ConfidenceRaw, confidenceCap)
	switch vr.Result {
	case vocab.StatusValid:
		// §3.4 THE anti-laundering rule: a vendor "valid" is NEVER SMTP-grade valid. risky, capped.
		return Resolution{Status: vocab.StatusRisky, Reason: vocab.ReasonExternalVerified, Confidence: conf}
	case vocab.StatusInvalid:
		// OQ-P6-3: accept invalid only where independent non-existence is structurally plausible.
		if class == ResidueRetryExhausted || class == ResidueGaveUpRateLimited {
			return Resolution{Status: vocab.StatusInvalid, Reason: vocab.ReasonExternalVerified, Confidence: conf}
		}
		// catch_all / provider_no_smtp: a vendor "invalid" is itself a guess → downgrade to risky.
		return Resolution{Status: vocab.StatusRisky, Reason: vocab.ReasonExternalVerified, Confidence: conf}
	case vocab.StatusRisky:
		return Resolution{Status: vocab.StatusRisky, Reason: vocab.ReasonExternalVerified, Confidence: conf}
	default:
		// unknown — the vendor couldn't answer either. The residue stays first-class (principle 2).
		return Resolution{KeepResidue: true}
	}
}

// cappedConfidence returns the external verdict's confidence: the cap, lowered by the vendor's own
// reported confidence when that is smaller and positive. NEVER above the cap (the anti-laundering
// ceiling), never below 0. A non-positive cap is treated as 0 (defensive — a mis-set cap can only
// make a rented verdict LESS confident, never launder it into a high-confidence answer).
func cappedConfidence(vendorConf, cap int) int {
	if cap < 0 {
		cap = 0
	}
	c := cap
	if vendorConf > 0 && vendorConf < c {
		c = vendorConf
	}
	return c
}
