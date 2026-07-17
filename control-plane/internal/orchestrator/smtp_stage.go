package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/router"
	"github.com/mnfi/codmailling/control-plane/internal/smtp"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
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

// runSMTP performs the SMTP stage. It returns (smtpResult, rawBlob, ok). ok=false means the stage
// did not run or could not reach a node — the caller keeps the cheap verdict. A node failure that
// DID occur mid-flight returns an unknown verdict (ok=true) so the honest degradation is logged.
func (o *Orchestrator) runSMTP(ctx context.Context, normalized, domain, requestID string, cheap policy.Verdict) (smtp.Result, []byte, string, bool) {
	if o.smtp == nil || o.smtp.Client == nil || o.smtp.Registry == nil {
		return smtp.Result{}, nil, "", false // SMTP not wired → cheap verdict stands
	}

	// Route to a node (Door 5: a LIST via Strategy; empty list → degrade, never invalid).
	n, err := o.smtp.Registry.Route(ctx, router.Plan{Domain: domain, Provider: mxProviderOf(cheap.Signals)})
	if err != nil {
		// No eligible node → cheap verdict stands (harden-toward-unknown: we do NOT invent SMTP).
		return smtp.Result{}, nil, "", false
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
	if callErr != nil {
		return o.smtpFromNodeError(callErr, cheap), raw, n.ID, true
	}

	var wr smtp.WrapperResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		// Unparseable node response → unknown/smtp_error (never a guessed verdict).
		return unknownResult(vocab.ReasonSMTPError), raw, n.ID, true
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
	return res, raw, n.ID, true
}

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
// re-probe LOOP that reads expires_at is a later-phase owner call (FLAG-C, DECISIONS §5).
const catchAllTTL = 30 * 24 * time.Hour
