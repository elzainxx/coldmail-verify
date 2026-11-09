package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/ratelimit"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/smtp"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the Phase-2 SMTP stage (§7.1). It runs AFTER the cheap layers, ONLY when the cheap
// path produced the non-terminal residue (risky/plausible — no gate stopped it) AND SMTP is
// enabled AND a node is routable AND the provider isn't :mx-routed. It calls the node's
// /v1/verify, parses the transcript (§6.2), scores it (§7.2/§5.5), reconciles DNS (§6.6), and
// returns a replacement verdict + the raw engine blob for the sidecar. Harden-toward-unknown:
// any node/transport failure degrades to unknown, NEVER to a fabricated valid/invalid.

// SMTPDeps carries what the SMTP stage needs. All are interfaces/values so tests inject fakes.
type SMTPDeps struct {
	Client         node.Client                // live HTTP client (or a fake in tests)
	Registry       *router.Registry           // node LIST + Strategy (Door 5)
	TokenFor       func(nodeID string) string // resolves a node's token from secrets (never in config)
	SafeCheckOn    bool                       // whether the engine runs smtp_safe_check (§6.8; config_fingerprint)
	ClientDeadline time.Duration              // brain-side deadline mirror (used by the caller when building Client)

	// Phase 3 (§4.4/§4.5/§5.2). Health drives the eligibility gate; MaxAuditAge is the §4.5
	// staleness window; WarmupCap is the §5.2 per-window ascending cap (<=0 => NOT enforced — a
	// mature node past warmup). Health is usually the store itself; injected so tests use a fake.
	Health        router.HealthSource
	MaxAuditAge   time.Duration // §4.5 staleness window (DIAL); <=0 disables the freshness conjunct
	WarmupCap     int           // §5.2 per-node/per-day :smtp cap; <=0 => warmup not enforced
	CanaryCap     int           // §5.4 the LOW cap a `recovering` (canary) node is metered by; <=0 => fall back to WarmupCap
	CanarySampleN int           // §5.4/GAP-30 canary domain-sample rate (≈1/N domains reach a recovering node in a healthy pool, so it accrues real samples)

	// Phase 4 (§4) — the per-provider rate limiter. nil => no rate limiting (the P2/P3 posture; the
	// limiter is inert). When set, runSMTP jitters + admits before the node call and releases the
	// in-flight slot after; a saturated cap degrades to cheap with smtp_skipped_reason="rate_limited"
	// (§4.6, PROVISIONAL/non-cacheable, never invalid/queued). Sleep is the jitter-sleeper (injected
	// so tests don't actually sleep); nil => time.Sleep.
	Limiter RateLimiter
	Sleep   func(time.Duration)
}

// RateLimiter is the narrow §4 admission capability runSMTP needs (satisfied by *ratelimit.Limiter;
// injected so orchestrator tests use a fake without a store). Admit consumes one session-start slot
// (rate + concurrency); Release frees the concurrency slot after the session closes; JitterDelay is
// the anti-fingerprint pre-start delay (§4.2).
type RateLimiter interface {
	Admit(ctx context.Context, nodeID, providerBucket string) (ratelimit.Decision, error)
	Release(nodeID, providerBucket string)
	JitterDelay(providerBucket string) time.Duration
}

// smtpStageOutcome is runSMTP's result. When Ran, the SMTP verdict + its provenance replace the
// cheap residue. When !Ran, SMTP was eligible-but-couldn't-run: the cheap verdict STANDS, SkipReason
// (a frozen §8 token, "" if none) is attached to signals, and Provisional=true marks a CAPACITY-GAP
// fallback that must NOT be cached (PB1 — caching it would freeze a lower-quality answer and
// suppress the SMTP verdict that becomes available once the node recovers / the cap lifts). Never
// invalid (OQ-P3-6).
type smtpStageOutcome struct {
	Ran         bool
	Result      smtp.Result
	RawBlob     []byte
	NodeID      string
	VerifierIP  string // the node's public egress IP (== audited IP, GAP-24-verified); logged as verifier_ip
	SkipReason  string // frozen §8 signals.smtp_skipped_reason ("" => none)
	Provisional bool   // PB1: capacity-gap fallback → non-cacheable
}

// smtpEligible reports whether the cheap verdict is the non-terminal residue that SMTP should
// resolve (§7.1). It is the EXACT condition, read off the existing engine output — no change to
// P1 gate mechanics: a gate that stopped (invalid/no_mx, null_mx, disposable, spam_trap) has a
// non-empty StoppedAt and is NOT eligible; a :mx-routed provider produces provider_no_smtp (also
// StoppedAt set) and is NOT eligible. Only the clean residue (StoppedAt=="" → risky/plausible)
// is SMTP-eligible.
func smtpEligible(v policy.Verdict) bool {
	return v.StoppedAt == "" && v.Reason == vocab.ReasonPlausible
}

// runSMTP performs the Phase-3 SMTP stage. It applies the §4.4 eligibility gate (via the router's
// health-aware SMTPRoute), the §5.2 warmup cap, then calls the node and scores the transcript. Its
// outcome tells the caller exactly what happened: Ran (SMTP verdict replaces the residue) or not
// (cheap verdict stands, with a frozen §8 smtp_skipped_reason and the PB1 cacheability flag). It
// NEVER fabricates valid/invalid — a node/transport failure degrades to unknown; a gate/cap skip
// keeps the honest cheap verdict.
func (o *Orchestrator) runSMTP(ctx context.Context, normalized, domain, requestID string, cheap policy.Verdict) smtpStageOutcome {
	if o.smtp == nil || o.smtp.Client == nil || o.smtp.Registry == nil {
		return smtpStageOutcome{} // SMTP not wired → cheap verdict stands (no skip signal in P1/Rung3)
	}

	// §4.4 eligibility gate + Door-5 routing: only nodes that are active ∧ green ∧ port25_open ∧
	// fresh ∧ (GAP-24) egress-IP-matched are candidates; the Strategy picks one. No eligible node →
	// degrade to cheap with a frozen §8 skip token (audit_red vs no_eligible_node); this is a
	// CAPACITY-GAP fallback → PROVISIONAL/non-cacheable (PB1). Never invalid (OQ-P3-6).
	rr := o.smtp.Registry.SMTPRoute(ctx,
		router.Plan{Domain: domain, Provider: mxProviderOf(cheap.Signals)},
		o.smtp.Health,
		// AllowRecovering lets a `recovering` node serve the METERED canary (§5.4/GAP-30): a
		// deterministic ≈1/CanarySampleN slice of domains reaches it even in a healthy pool (so it
		// accrues real samples), reserved against the LOW canary cap below.
		router.SMTPFilterConfig{MaxAuditAge: o.smtp.MaxAuditAge, Now: o.now, AllowRecovering: true, CanarySampleN: o.smtp.CanarySampleN},
	)
	if rr.Err != nil {
		return smtpStageOutcome{SkipReason: rr.SkipToken, Provisional: true}
	}
	n := rr.Node

	// §5.2 warmup ramp: reserve one :smtp slot against the node's per-window ascending cap. Over the
	// cap → degrade to cheap with smtp_skipped_reason="warmup_cap" (PROVISIONAL/non-cacheable, PB1 —
	// the SMTP verdict becomes available once the window rolls). NEVER queue (P5) or invalid (§5.2).
	// A CANARY (recovering) node is metered by the LOW canary cap (§5.4) — a recovered IP re-warms
	// gently; this is what keeps the canary a "trickle, well under warmup start". Cap<=0 means warmup
	// is not enforced (a mature node) — but a canary ALWAYS uses its cap (its whole point is metering).
	if ok, skip := o.reserveWarmup(ctx, n, rr.IsCanary); !ok {
		// A CANARY node at its low cap must NOT drag its sampled domain down to cheap when a healthy
		// ACTIVE peer could serve it (the residual starvation the P4→P5 review caught). Re-route once
		// FORCING active (recovering excluded); only degrade if active also can't serve. A non-canary
		// warmup-cap denial has no better peer to divert to → degrade as before.
		if rr.IsCanary {
			rr2 := o.smtp.Registry.SMTPRoute(ctx,
				router.Plan{Domain: domain, Provider: mxProviderOf(cheap.Signals)},
				o.smtp.Health,
				router.SMTPFilterConfig{MaxAuditAge: o.smtp.MaxAuditAge, Now: o.now, AllowRecovering: true, ForceActiveOnly: true},
			)
			if rr2.Err != nil {
				return smtpStageOutcome{SkipReason: rr2.SkipToken, Provisional: true}
			}
			n = rr2.Node
			if ok2, skip2 := o.reserveWarmup(ctx, n, false /* now a normal active route */); !ok2 {
				return smtpStageOutcome{SkipReason: skip2, Provisional: true}
			}
		} else {
			return smtpStageOutcome{SkipReason: skip, Provisional: true}
		}
	}

	// §4 per-provider rate limiter: pace the concentration sticky routing created (§3.1/§4.1). The
	// bucket is (node × provider), refined to "other:<domain>" for honest-corporate traffic (§4.1,
	// OQ-P4-2). We jitter the START (§4.2 anti-fingerprint), then admit (consume a durable token +
	// an ephemeral in-flight slot). A saturated cap degrades to cheap with
	// smtp_skipped_reason="rate_limited" — PROVISIONAL/non-cacheable (PB1), never queued (P5), never
	// invalid (§4.6). Admitted sessions MUST release the in-flight slot; we defer that below. The
	// limiter is nil in the P2/P3 posture (no rate limiting) — the stage runs unchanged then.
	providerBucket := ""
	if o.smtp.Limiter != nil {
		providerBucket = ratelimit.BucketForDomain(mxProviderOf(cheap.Signals), domain)
		if d := o.smtp.Limiter.JitterDelay(providerBucket); d > 0 {
			o.sleep(d) // spread starts so they don't arrive as a periodic, fingerprintable burst
		}
		adm, aerr := o.smtp.Limiter.Admit(ctx, n.ID, providerBucket)
		if aerr != nil {
			// A limiter/store error must NOT silently sprint past the cap (reputation hazard) — fail
			// closed to the cheap verdict, rate_limited/provisional. The verdict stays honest (cheap).
			return smtpStageOutcome{SkipReason: reasonRateLimited, Provisional: true}
		}
		if !adm.Admitted {
			// Every relevant cap exhausted for this (node × provider) → degrade (§4.6). Non-cacheable
			// (PB1: the SMTP verdict becomes available once the cap frees; caching would freeze a
			// lower-quality answer). NEVER invalid, NEVER queued (the P5 seam leaves retry-eligible in
			// signals but nothing retries here).
			return smtpStageOutcome{SkipReason: reasonRateLimited, Provisional: true}
		}
		// The concurrency slot is held for the life of the session; release it on EVERY return path
		// below (the node call, its error paths, and the scored result).
		defer o.smtp.Limiter.Release(n.ID, providerBucket)
	}

	token := ""
	if o.smtp.TokenFor != nil {
		token = o.smtp.TokenFor(n.ID)
	}

	// Call the node. The client applies its own deadline; a timeout/transport error → unknown.
	raw, callErr := o.smtp.Client.Verify(ctx, n.BaseURL, token, normalized)
	// DOOR 3 (the raw full email is NEVER persisted): the wrapper echoes the checked address in
	// the response `email` field, and a real SMTP server can echo it inside a rejection body
	// (e.g. "550 <jane@acme.com> User unknown"). The wrapper response becomes the verification_raw
	// sidecar blob — a durable, retention-managed store — so the plaintext address must be redacted
	// at THIS boundary (the brain owns Door 3; the box is dumb) BEFORE the blob is ever returned for
	// persistence. The row already carries `email_hash` as the join key, so redaction loses nothing
	// joinable. Everything else (smtp_debug transcript, mx, catchall, configuration) stays verbatim
	// — the spec's "join gold" (§6.4) minus only the frozen-door leak the spec itself missed
	// (see ADR-0008). Applied to EVERY return path below, including the error paths.
	raw = redactAddress(raw, normalized)
	// §5.4/OQ-P3-11: verifier_ip is sourced from the BRAIN REGISTRY (n.PublicEgressIP), never
	// self-reported by the box. Because the §4.4 gate already required the audit's current_host_ip
	// == n.PublicEgressIP (GAP-24), the logged verifier_ip is the IP that actually made the SMTP
	// connection. Empty in the local build (PENDING-REAL-BOX) — a real box sets it at provisioning.
	verifierIP := n.PublicEgressIP
	if callErr != nil {
		return smtpStageOutcome{Ran: true, Result: o.smtpFromNodeError(callErr, cheap), RawBlob: raw, NodeID: n.ID, VerifierIP: verifierIP}
	}

	var wr smtp.WrapperResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		// Unparseable node response → unknown/smtp_error (never a guessed verdict).
		return smtpStageOutcome{Ran: true, Result: unknownResult(vocab.ReasonSMTPError), RawBlob: raw, NodeID: n.ID, VerifierIP: verifierIP}
	}

	// ADR-0021: the node's destination guard refused to open a session (the domain's mail host resolves to a non-public address).
	// That is a FACT about the domain, reported with no transcript: risky/non_public_mx — never invalid, never a guess, never cached.
	if wr.ProbeRefused != nil {
		return smtpStageOutcome{Ran: true, Result: smtp.ProbeRefusedResult(wr.ProbeRefused), RawBlob: raw, NodeID: n.ID, VerifierIP: verifierIP}
	}

	parsed := smtp.ParseTranscript(wr.SMTPDebug, wr.Success)
	in := smtp.Input{
		Target:        parsed,
		EngineSuccess: wr.Success,
		SafeCheckOn:   o.smtp.SafeCheckOn,
		Catchall:      wr.Catchall,
		BrainProvider: mxProviderOf(cheap.Signals),
		BrainSawMX:    boolSignal(cheap.Signals, "mx_found"),
	}
	if wr.MX != nil {
		in.EngineMXHosts = wr.MX.MailServers
	}
	res := smtp.Score(in)
	return smtpStageOutcome{Ran: true, Result: res, RawBlob: raw, NodeID: n.ID, VerifierIP: verifierIP}
}

// warmupWindow returns the start of the current warmup window (the UTC day, §5.2). The window is a
// DIAL shape; a day is the minimal ascending-cap unit. Truncating to the UTC day makes the durable
// per-window counter deterministic across brain restarts.
func (o *Orchestrator) warmupWindow() time.Time {
	n := o.now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// reserveWarmup reserves one :smtp slot against node n's per-window ascending cap (§5.2). A canary
// (recovering) node uses the LOW canary cap (§5.4); an active node uses the normal warmup cap. It
// returns ok=true when the slot was granted (or no cap is enforced), else ok=false with the frozen
// §8 skip token the caller degrades on. A store error fails CLOSED (no over-cap send — reputation
// hazard). Extracted so the canary-cap-denial re-route (force-active fallback) can reuse it.
func (o *Orchestrator) reserveWarmup(ctx context.Context, n router.Node, isCanary bool) (ok bool, skip string) {
	cap := o.smtp.WarmupCap
	if isCanary && o.smtp.CanaryCap > 0 {
		cap = o.smtp.CanaryCap
	}
	if cap <= 0 {
		return true, "" // warmup not enforced (a mature node); a canary always has CanaryCap>0 wired
	}
	dec, err := o.store.ReserveWarmupSlot(ctx, n.ID, o.warmupWindow(), cap)
	if err != nil || !dec.Allowed {
		return false, reasonWarmupCap
	}
	return true, ""
}

// sleep is the jitter sleeper (§4.2), injected via SMTPDeps.Sleep so tests don't block on real time.
func (o *Orchestrator) sleep(d time.Duration) {
	if o.smtp != nil && o.smtp.Sleep != nil {
		o.smtp.Sleep(d)
		return
	}
	time.Sleep(d)
}

// reasonWarmupCap / reasonSMTPSkipped is the signals.smtp_skipped_reason enum (additive to the open
// signals bag — Door 1). P3 shipped four tokens; P4 adds ONE additive value: rate_limited (§4.6/§9,
// the phase's only Door-adjacent addition — an additive value on the already-open string enum, NOT a
// new key/reason/status). Only these five tokens are ever written.
const (
	reasonNoEligibleNode = "no_eligible_node"
	reasonAuditRed       = "audit_red"
	reasonProviderMX     = "provider_mx_routed"
	reasonWarmupCap      = "warmup_cap"
	reasonRateLimited    = "rate_limited" // P4 §4.6: every eligible node at its per-provider cap
	signalSMTPSkipped    = "smtp_skipped_reason"
)

// smtpFromNodeError maps a node call failure to an honest unknown (§4.1 failure modes / §6.5):
// a transport timeout → smtp_timeout; a non-2xx HTTP status → connection_failed/unknown. It never
// fabricates valid/invalid. The cheap verdict's provider signal is preserved for provenance.
func (o *Orchestrator) smtpFromNodeError(err error, cheap policy.Verdict) smtp.Result {
	var transport *node.TransportError
	if errors.As(err, &transport) {
		return unknownResult(vocab.ReasonSMTPTimeout)
	}
	var status *node.HTTPStatusError
	if errors.As(err, &status) {
		if status.Status == 401 || status.Status == 400 {
			// Auth/protocol bug — not a verdict; degrade to unknown, surfaced in logs.
			return unknownResult(vocab.ReasonSMTPError)
		}
		return unknownResult(vocab.ReasonConnectionFailed)
	}
	return unknownResult(vocab.ReasonSMTPError)
}

// unknownResult builds a non-cacheable unknown SMTP result (harden-toward-unknown). retry_eligible
// is marked for the transient classes so a P5 queue can act (nothing retries in P2).
func unknownResult(reason vocab.Reason) smtp.Result {
	signals := map[string]any{}
	switch reason {
	case vocab.ReasonSMTPTimeout, vocab.ReasonConnectionFailed:
		signals["retry_eligible"] = true
	}
	return smtp.Result{Status: vocab.StatusUnknown, Reason: reason, Confidence: 10, Signals: signals, Cacheable: false}
}

func boolSignal(signals map[string]any, key string) bool {
	b, _ := signals[key].(bool)
	return b
}

// redactAddress removes the plaintext checked address from the raw engine blob before it is
// persisted to the verification_raw sidecar (Door 3 — the raw full email is NEVER persisted).
// Two passes: (1) the full address (longest, most specific) — covers the echoed `email` field and a
// "<local@domain>" body echo; (2) the bare local-part at WORD BOUNDARIES — covers a body that prints
// only the local part ("User jo does not exist"). There is deliberately NO length guard: Door 3
// forbids persisting the raw address at ANY length, so a short local-part (jo@x.com) must be scrubbed
// too; word boundaries are what make an unguarded replace safe (a standalone "jo" is redacted, but the
// "jo" inside "json"/"rejected" is not — so we never under-redact the identity nor shred the transcript).
// The domain is Door-3-clear (stored plaintext in `verifications.domain`), so it is left intact. The
// row's email_hash remains the join key, so nothing joinable is lost. Everything else (smtp_debug
// codes/bodies, mx, catchall, configuration) is preserved verbatim — the spec's "join gold" (§6.4).
func redactAddress(raw []byte, normalized string) []byte {
	if len(raw) == 0 || normalized == "" {
		return raw
	}
	at := strings.LastIndexByte(normalized, '@')
	if at <= 0 {
		return raw // no local part to leak (shouldn't happen post-normalize; defensive)
	}
	const token = "[redacted-local]" // fixed marker; the address identity lives in email_hash
	local, domain := normalized[:at], normalized[at+1:]
	out := string(raw)
	// Redaction is CASE-INSENSITIVE. normalized is lowercased (Door 3), but a real SMTP server may
	// echo the address in a DIFFERENT case in a bounce body ("550 <SecretLocal@Acme.com> rejected",
	// RFC 5321 permits it). A case-sensitive replace would leak that variant → a Door-3 breach. Both
	// passes therefore use (?i). (Found during ADR-0008 ratification, locked by a regression test.)
	// (1) Full address first (longest, most specific match) — covers the echoed `email` field and a
	//     "<local@domain>" body echo. Domain is intentionally KEPT (Door-3-clear: stored plaintext in
	//     verifications.domain by design; analytics/joins rely on it) — so the replacement re-emits it.
	if reFull, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(normalized)); err == nil {
		out = reFull.ReplaceAllString(out, token+"@"+domain)
	} else {
		out = caseInsensitiveReplaceAll(out, normalized, token+"@"+domain) // fallback stays case-insensitive (Door 3)
	}
	// (2) Bare local-part — covers a body that prints only the local part ("User jo does not exist").
	//     NO length guard: Door 3 forbids persisting the raw address at ANY length, so a short
	//     local-part (jo@x.com) must be scrubbed too. Word boundaries (\b) keep an unguarded replace
	//     from shredding substrings ("\bjo\b" hits a standalone "jo" but NOT the "jo" in "json").
	//     BUT \b only works when the local-part STARTS and ENDS with a word char: a leading/trailing
	//     non-word char (".foo", "foo.") breaks the anchor and would leak (found in ratification). So
	//     when the local-part has a non-word char at either end, fall back to an UNGUARDED substring
	//     replace — over-redaction is acceptable for Door 3; under-redaction is not. Internal special
	//     chars ("foo.bar", "foo+tag") are fine with \b (they're literal via QuoteMeta, anchored only
	//     at the ends) — verified empirically.
	if edgeIsNonWord(local) {
		out = caseInsensitiveReplaceAll(out, local, token) // no safe boundary → unguarded (safe dir)
	} else if re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(local) + `\b`); err == nil {
		out = re.ReplaceAllString(out, token)
	} else {
		out = caseInsensitiveReplaceAll(out, local, token)
	}
	return []byte(out)
}

// edgeIsNonWord reports whether the local-part starts or ends with a non-word character (anything
// other than [A-Za-z0-9_]). For such local-parts the regexp `\b` anchor at that end never matches
// (a boundary needs a word/non-word transition), so word-boundary redaction would UNDER-redact and
// leak — the caller falls back to an unguarded (over-redacting, Door-3-safe) replace instead.
func edgeIsNonWord(s string) bool {
	if s == "" {
		return true
	}
	isWord := func(b byte) bool {
		return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	return !isWord(s[0]) || !isWord(s[len(s)-1])
}

// caseInsensitiveReplaceAll replaces every case-insensitive occurrence of old with repl. Used on
// the redaction fallback paths so a case-variant address echo cannot leak even when the primary
// regexp path is unavailable (Door 3 — the raw full email is never persisted, in any case).
func caseInsensitiveReplaceAll(s, old, repl string) string {
	if old == "" {
		return s
	}
	if re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(old)); err == nil {
		return re.ReplaceAllString(s, repl)
	}
	return strings.ReplaceAll(s, old, repl) // last resort (regexp can't fail on a QuoteMeta'd literal)
}

// persistCatchAll writes the domain-level catch-all conclusion to the store (§5.6 — brain-side).
// It is best-effort provenance: a write failure is logged by the caller but never fails the
// verdict (the catch-all table is analytics/routing state, not the append-only LOG). Inconclusive
// is a no-op inside the store (never overwrites a known value with NULL).
func (o *Orchestrator) persistCatchAll(ctx context.Context, domain string, res smtp.Result) {
	concl, _ := res.Signals["catchall_conclusion"].(string)
	var isCatchall *bool
	switch concl {
	case smtp.ConclusionCatchAll:
		t := true
		isCatchall = &t
	case smtp.ConclusionNotCatchAll:
		f := false
		isCatchall = &f
	default:
		return // not_probed / inconclusive → nothing to persist (§5.6)
	}
	_ = o.store.SetCatchAll(ctx, domain, isCatchall, "probe", o.now().Add(catchAllTTL))
}

// catchAllTTL is the re-probe cadence DIAL (§5.6 SEAM → P4). A conservative default; the
// re-probe LOOP that reads expires_at is a later-phase owner call (FLAG-C, open-decisions §5).
const catchAllTTL = 30 * 24 * time.Hour
