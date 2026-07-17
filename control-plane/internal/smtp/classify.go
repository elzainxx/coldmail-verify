package smtp

import (
	"regexp"
	"strings"

	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// This file: the 5xx address-fault-vs-your-fault classifier (§6.1) and the SMTP-signal → verdict
// map (§7.2) — the most correctness-critical code in Phase 2. Every branch descends from §0.4:
// harden toward `unknown`. `invalid` is minted ONLY by positive proof of non-deliverability
// (a no-such-user address-fault); every ambiguous / policy / transient / timeout / blocked
// outcome is `unknown`, never `invalid` (P1 §7.5 invariant, unchanged).

// Verdict is the SMTP layer's status+reason contribution. Confidence banding is applied by the
// scorer (§7.4); this decides status/reason only. Signals carries the additive §8 keys.
type Verdict struct {
	Status  vocab.Status
	Reason  vocab.Reason
	Signals map[string]any
}

// addressFaultBodyRe mirrors truemail's own smtp_error_body_pattern (Appendix A.1) — the engine's
// address-fault discriminator: a 550 whose body mentions user/account/customer/mailbox is a
// no-such-user (address-fault). Anchoring on the engine's OWN heuristic (not an invented one)
// keeps us consistent with what drove the RCPT. Extended (DIAL) with common no-such-user phrasings.
var addressFaultBodyRe = regexp.MustCompile(`(?i)(user|account|customer|mailbox|recipient|address).*(unknown|not\s*found|no\s*such|does\s*not\s*exist|rejected|invalid|no\s*longer|disabled|unavailable)|(unknown|no\s*such|invalid|nonexistent)\s*(user|account|mailbox|recipient|address)`)

// ClassifyResult is the pure §6.1 classification of a parsed transcript into a status/reason,
// BEFORE the catch-all truth table (§5.5) is applied. The scorer combines the two.
type ClassifyResult struct {
	Status vocab.Status
	Reason vocab.Reason
}

// Classify maps a parsed target transcript to a status/reason per §6.1. It does NOT apply the
// catch-all probe (that is Score's job, §5.5) — it answers "what does the TARGET's own RCPT say?"
func Classify(p Parsed) ClassifyResult {
	// No port ever opened → couldn't connect → connection_failed (unknown). §6.2 step 5.
	if !p.PortOpened && !p.Reached && p.Stage != "mailfrom" {
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonConnectionFailed}
	}
	// A timeout anywhere decisive → smtp_timeout (unknown). §6.5.
	if p.Timeout {
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPTimeout}
	}
	// A pre-RCPT failure (connection/mailfrom stage) with no usable code → connection/smtp error.
	if p.Stage == "connection" {
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonConnectionFailed}
	}

	// A clean success (success && smtp_debug==null, §6.8) has no transcript and thus no code, but
	// it IS an acceptance (the target was 250'd). Treat it as accepted — the catch-all probe
	// (§5.5) then decides valid vs risky. This precedes the code switch (a clean 250 has no code).
	if p.CleanSuccess {
		return ClassifyResult{vocab.StatusValid, vocab.ReasonAccepted}
	}

	code := p.Code
	switch {
	case code == "":
		// Reached a stage but produced no numeric code (e.g. dropped connection) → ambiguous.
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPError}

	case strings.HasPrefix(code, "2"):
		// A 2xx on RCPT means the mailbox was ACCEPTED. This is NOT yet `valid` — the catch-all
		// probe (§5.5) decides valid vs risky/catch_all. Signal "accepted", let Score finalize.
		return ClassifyResult{vocab.StatusValid, vocab.ReasonAccepted}

	case code == "552" || code == "452":
		// Mailbox full / over quota = POSITIVE existence proof (§6.1.1, owner override): valid /
		// mailbox_full, high confidence, rec=review carries the "full now" caveat. Never invalid.
		return ClassifyResult{vocab.StatusValid, vocab.ReasonMailboxFull}

	case code == "550":
		// The pivotal split: a 550 is address-fault (→ invalid) ONLY if the body positively says
		// no-such-user (the engine's smtp_error_body_pattern). Otherwise (policy/blocked/ambiguous
		// "550 rejected") it is your-fault-side → unknown. Harden toward unknown.
		if addressFaultBodyRe.MatchString(p.Body) {
			return ClassifyResult{vocab.StatusInvalid, vocab.ReasonMailboxNotFound}
		}
		if isPolicyBlockBody(p.Body) {
			return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPBlocked}
		}
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPError} // ambiguous 550 → conservative

	case code == "551" || code == "553":
		// Recipient-rejected in address form (user not local / mailbox name not allowed) →
		// address-fault → invalid/mailbox_not_found (§6.1).
		return ClassifyResult{vocab.StatusInvalid, vocab.ReasonMailboxNotFound}

	case code == "554":
		// 554 is transaction-failed: on RCPT it is almost always policy/spam/blocked (your-fault)
		// UNLESS the body positively indicates no-such-user. Harden toward unknown/smtp_blocked.
		if addressFaultBodyRe.MatchString(p.Body) {
			return ClassifyResult{vocab.StatusInvalid, vocab.ReasonMailboxNotFound}
		}
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPBlocked}

	case strings.HasPrefix(code, "4"):
		// 4xx transient. 450 is the canonical greylist; 421 (service not available — the REAL
		// smtpmock block delta, ground-truth §3.4) is a transient block. Both → unknown, retry-
		// eligible; a 450 is greylisted, other 4xx are timeout-shaped transients. Never invalid.
		if code == "450" || strings.Contains(strings.ToLower(p.Body), "greylist") {
			return ClassifyResult{vocab.StatusUnknown, vocab.ReasonGreylisted}
		}
		// 421/4xx transient block — see §6.1 421-vs-554 reconciliation (ground-truth §3.4):
		// the stock smtpmock blacklist knob returns 421, which classifies as transient (not
		// smtp_blocked). Both still map to unknown (harden-toward-unknown holds); reason differs.
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonGreylisted}

	case strings.HasPrefix(code, "5"):
		// Any other unrecognized 5xx → ambiguous → unknown/smtp_error (§6.1).
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPError}

	default:
		return ClassifyResult{vocab.StatusUnknown, vocab.ReasonSMTPError}
	}
}

// policyBlockRe recognizes explicit policy/spam/blocklist language in a 5xx body → your-fault
// (smtp_blocked), distinct from an ambiguous bare "550 rejected" (smtp_error). DIAL.
var policyBlockRe = regexp.MustCompile(`(?i)(blocked|blacklist|black\s*list|denied|spam|policy|reputation|rbl|dnsbl|not\s*allowed|refused|access\s*denied|prohibited)`)

func isPolicyBlockBody(body string) bool {
	return policyBlockRe.MatchString(body)
}

// isTransientCode reports whether a code is transient (4xx). Used by the cache-settled predicate.
func isTransientCode(code string) bool {
	return strings.HasPrefix(code, "4")
}
