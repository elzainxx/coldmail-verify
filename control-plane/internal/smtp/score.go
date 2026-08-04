package smtp

import (
	"strings"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: Score — the top-level SMTP-signal → verdict map (§7.2). It combines the §6.1
// classification (Classify) with the §5.5 catch-all truth table, the §6.8 smtp_safe_check
// distinction, and §6.6 DNS reconciliation, producing the final (status, reason, signals) the
// policy gate consumes. It also owns the settled-only cache predicate (§6.5 / OQ-P2-6) and the
// confidence band (§7.4). It NEVER mints a value outside the frozen vocab (Door 4).

// Catch-all conclusion tokens (mirror the wrapper §4.1 and P2 §5.5).
const (
	ConclusionCatchAll     = "catchall"
	ConclusionNotCatchAll  = "not_catchall"
	ConclusionInconclusive = "inconclusive"
	ConclusionNotProbed    = "not_probed"
)

// Input is everything the scorer needs, all derived from the wrapper response + brain routing.
type Input struct {
	Target        Parsed         // parsed target transcript (§6.2)
	EngineSuccess bool           // truemail's own top-level success (for the safe-check split)
	SafeCheckOn   bool           // whether smtp_safe_check was enabled (config, §6.8)
	Catchall      *CatchallBlock // the probe outcome (nil => not probed)
	BrainProvider string         // brain's MX provider classification (advisory, §6.6)
	EngineMXHosts []string       // MX the box actually used (authoritative, §6.6)
	BrainSawMX    bool           // did the brain's cheap path find any MX? (for divergence)
}

// Result is the scored SMTP verdict plus the additive §8 signals and the cache-settled flag.
type Result struct {
	Status     vocab.Status
	Reason     vocab.Reason
	Confidence int
	Signals    map[string]any
	Cacheable  bool // §6.5/OQ-P2-6: only SETTLED verdicts are cacheable
}

// Confidence bands (§7.4 DIAL). Placeholders until P7 recalibrates per (reason × provider).
const (
	confCleanValid     = 90 // clean 250 + probe-rejected (§5.5 #4) — strong, not 100
	confSafePassValid  = 65 // safe-check pass (§6.8) — capped below clean-250
	confMailboxFull    = 90 // 552 = positive existence proof (§6.1.1)
	confAddressFault   = 95 // 550 no-user = positive proof of non-deliverability
	confCatchAll       = 40 // risky/catch_all — low
	confRiskyPlausible = 40 // risky/plausible (unprobed accepted) — low
	confUnknown        = 10 // all unknowns — low, expresses uncertainty
)

// Score produces the final SMTP verdict. The precedence (§5.5): address-fault and transient
// outcomes on the TARGET outrank the catch-all conclusion; a probe never upgrades an unknown/
// invalid target. Only a clean-accepted target is subject to the catch-all decision.
func Score(in Input) Result {
	signals := map[string]any{}
	dnsReconcile(in, signals)

	cls := Classify(in.Target)

	// Populate the always-present transcript signals (§8).
	if in.Target.Code != "" {
		signals["smtp_code"] = in.Target.Code // string token, Door-1 type preserved (§6.2)
	}
	if in.Target.EnhancedCode != "" {
		signals["smtp_enhanced_code"] = in.Target.EnhancedCode
	}

	switch cls.Status {
	case vocab.StatusInvalid:
		// Positive address-fault. Decisive — the probe is irrelevant (§5.5 row 1). Settled → cacheable.
		return settled(cls.Status, cls.Reason, confAddressFault, signals)

	case vocab.StatusUnknown:
		// Transient / blocked / timeout / ambiguous. Never cache (§6.5) — a retry may resolve it.
		markTransient(cls.Reason, signals)
		return transient(cls.Status, cls.Reason, confUnknown, signals)

	case vocab.StatusValid:
		// The target was ACCEPTED (2xx) or is 552 mailbox_full. mailbox_full is settled valid
		// regardless of catch-all (existence is proven by the 552 itself, §6.1.1).
		if cls.Reason == vocab.ReasonMailboxFull {
			return settled(vocab.StatusValid, vocab.ReasonMailboxFull, confMailboxFull, signals)
		}
		// A clean 250 (accepted) is subject to the catch-all truth table (§5.5).
		return scoreAccepted(in, signals)
	}
	// Unreachable (Classify only returns the three above), but harden: unknown.
	return transient(vocab.StatusUnknown, vocab.ReasonSMTPError, confUnknown, signals)
}

// scoreAccepted applies the §5.5 catch-all truth table to a target that returned a clean 2xx.
// valid is minted ONLY on target-250 + probe-explicitly-rejected (row #4). Every other accepted
// path is at best risky. Also applies the §6.8 safe-check confidence cap.
func scoreAccepted(in Input, signals map[string]any) Result {
	// §6.8: distinguish a clean 250 (success && smtp_debug==null) from a safe-check pass
	// (success && smtp_debug!=null). The parser sets CleanSuccess only for a true clean 250.
	safePass := in.EngineSuccess && !in.Target.CleanSuccess
	if in.SafeCheckOn && safePass {
		signals["smtp_safe_pass"] = true
		signals["smtp_debug_ref"] = true
	}

	conclusion := catchallConclusion(in.Catchall)
	signals["catchall_conclusion"] = conclusion

	switch conclusion {
	case ConclusionNotCatchAll:
		// Row #4: server distinguishes real vs fake → the target 250 is MEANINGFUL → valid.
		conf := confCleanValid
		if signals["smtp_safe_pass"] == true {
			conf = confSafePassValid // §6.8 cap
		}
		return settled(vocab.StatusValid, vocab.ReasonAccepted, conf, signals)

	case ConclusionCatchAll:
		// Row #3: target 250 + probe 250 → catch-all → existence unprovable → risky/catch_all.
		return settled(vocab.StatusRisky, vocab.ReasonCatchAll, confCatchAll, signals)

	case ConclusionInconclusive:
		// Rows #5/#7: probe unreliable / session died → cannot rule out catch-all → risky/catch_all
		// (conservative — harden toward risky, never mint valid). Settled (a re-probe is a new check).
		return settled(vocab.StatusRisky, vocab.ReasonCatchAll, confCatchAll, signals)

	default: // ConclusionNotProbed (row #6): can't claim valid on a maybe-catch-all domain.
		return settled(vocab.StatusRisky, vocab.ReasonPlausible, confRiskyPlausible, signals)
	}
}

// catchallConclusion normalizes the probe block into a truth-table token. A nil/absent probe or
// an empty conclusion is "not_probed" (§5.5 row 6).
func catchallConclusion(cb *CatchallBlock) string {
	if cb == nil || !cb.Probed || cb.Conclusion == "" {
		return ConclusionNotProbed
	}
	switch cb.Conclusion {
	case ConclusionCatchAll, ConclusionNotCatchAll, ConclusionInconclusive:
		return cb.Conclusion
	default:
		return ConclusionInconclusive // unknown token → conservative
	}
}

// dnsReconcile applies §6.6: engine-MX is authoritative for the SMTP verdict, brain-MX advisory.
// Divergence is LOGGED (never silently reconciled). The verdict-affecting case — brain routed to
// :smtp but the engine found NO MX — is handled by the caller (it wouldn't have a transcript);
// here we record the divergence signals.
func dnsReconcile(in Input, signals map[string]any) {
	if in.BrainProvider != "" {
		signals["brain_mx_provider"] = in.BrainProvider
	}
	if len(in.EngineMXHosts) > 0 {
		signals["engine_mx_hosts"] = in.EngineMXHosts
	}
	// Divergence: brain saw MX but engine used none, or vice-versa, or provider mismatch shape.
	engineSawMX := len(in.EngineMXHosts) > 0
	if in.BrainSawMX != engineSawMX {
		signals["dns_divergence"] = true
	}
}

// markTransient adds the requeue-shape signals (§6.5) to a transient/blocked unknown so a future
// P5 queue can act without a contract change. NOTHING retries it in P2 (queue is SEAM → P5).
func markTransient(reason vocab.Reason, signals map[string]any) {
	switch reason {
	case vocab.ReasonGreylisted:
		signals["greylisted"] = true
		signals["retry_eligible"] = true
		// retry_after_hint_s: a policy default (DIAL). Parsed from the 4xx body when present is a
		// P5 refinement; P1 store note flags the int-vs-float64 fidelity for this key (§8).
		signals["retry_after_hint_s"] = 300
	case vocab.ReasonSMTPTimeout, vocab.ReasonSMTPBlocked, vocab.ReasonConnectionFailed:
		signals["retry_eligible"] = true
	}
}

// settled marks a verdict cacheable (§6.5): valid, invalid, and risky/catch_all are settled facts.
func settled(status vocab.Status, reason vocab.Reason, conf int, signals map[string]any) Result {
	return Result{Status: status, Reason: reason, Confidence: conf, Signals: signals, Cacheable: true}
}

// transient marks a verdict NON-cacheable (§6.5/OQ-P2-6): your-fault/transient unknowns are
// expected to change on retry — caching one would suppress the retry that resolves it.
func transient(status vocab.Status, reason vocab.Reason, conf int, signals map[string]any) Result {
	return Result{Status: status, Reason: reason, Confidence: conf, Signals: signals, Cacheable: false}
}

// CacheableReason reports the settled-only cache predicate as a standalone check (§6.5): true iff
// the (status, reason) is a SETTLED verdict. Exposed so the orchestrator can apply the identical
// rule to cheap verdicts (never cache a transient unknown), a single testable predicate.
func CacheableReason(status vocab.Status, reason vocab.Reason) bool {
	switch status {
	case vocab.StatusValid, vocab.StatusInvalid:
		return true
	case vocab.StatusRisky:
		// risky/catch_all and address-fault-adjacent risky are settled; a risky that is really a
		// transient placeholder would carry an unknown reason — but risky reasons here are settled.
		return true
	case vocab.StatusUnknown:
		return false // every unknown is a non-answer → never cache (OQ-P2-6 generalized)
	default:
		return false
	}
}

// isYourFault reports whether a reason is a your-fault/transient class (for tests + docs parity).
func isYourFault(reason vocab.Reason) bool {
	switch reason {
	case vocab.ReasonSMTPBlocked, vocab.ReasonSMTPTimeout, vocab.ReasonGreylisted,
		vocab.ReasonConnectionFailed, vocab.ReasonSMTPError:
		return true
	default:
		return false
	}
}

// normalizeCode is a tiny helper for tests/log parity (kept internal).
func normalizeCode(code string) string { return strings.TrimSpace(code) }
