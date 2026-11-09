// Package flywheel is the Phase-7 close-the-loop layer (spec history/phase-7-flywheel-spec.md): it joins a
// real send OUTCOME back to the verdict that authorized the send, classifies what that outcome PROVES
// about the address, and — for real-signal outcomes only — recalibrates confidence, promotes
// providers, measures accuracy, and weans off the P6 fallback. It is ENTIRELY brain-side and additive
// (Door 5): the gem/box never learn an outcome came back; the sender is a separate system feeding an
// inbound signal (ADR-0013's mutually-authenticated pull), exactly as the P6 vendor was.
//
// This file is the CORRECTNESS HEART of the phase (the P7 analogue of P5's GAP-37 / P6's
// anti-laundering). One rule everything else defends (§4, GAP-49):
//
//	A bounce tells you about the ADDRESS only when it is ABOUT the address.
//
// Most bounce classes are about your SENDER (reputation), the receiving POLICY, or a TRANSIENT — they
// prove NOTHING about whether the mailbox exists and MUST be excluded, never counted either way. A
// classifier that treats every bounce as "invalid" permanently poisons the exact training data the
// flywheel exists to build. The two rules a naive "bounce = bad" implementation gets wrong:
//
//  1. mailbox_full → VALID-signal (§4.2, the counterintuitive one). A full mailbox is a mailbox that
//     EXISTS — positive existence proof, consistent with the P2 552 ruling ([[truemail-552-mailbox-full-valid]]).
//     Counting it as invalid is the classic poisoning error.
//  2. blocked / policy / spam-reject / greylist → ZERO-INFORMATION (§4.3, "the exclusions are the
//     discipline, not the leftovers"). The zero-information bucket is the MAJORITY of real bounces;
//     counting even a fraction as invalid-signal biases every bucket toward "worse than reality," and
//     because low confidence → fewer sends → fewer outcomes, a bucket gets STUCK looking bad
//     ([[truemail-exclusion-is-the-discipline-stuck-bucket]], GAP-49).
//
// The three-class taxonomy and WHICH class each event maps to are LOCKED; the DSN-string → event
// mapping (NormalizeEvent) is a DIAL (§4.3) — provider-specific and tunable. calibration_class is a
// P7-INTERNAL taxonomy: it decides what an outcome COUNTS AS when calibrating; it is NOT the frozen
// verdict vocabulary (Door 4) and NEVER reaches the /verify response.
package flywheel

import (
	"regexp"
	"strings"
)

// Class is the P7 §4.1 calibration class — what an outcome PROVES about the address. Exactly three
// values (LOCKED). This is NOT a status/reason/recommendation (Door 4): the classifier never mints a
// verdict value; it decides how an outcome is counted.
type Class string

const (
	// ClassInvalidSignal — positive proof the address does not exist / cannot receive. The gold
	// negative signal; the ONLY class that counts as "the verdict was wrong if it said valid" (§4.3).
	ClassInvalidSignal Class = "invalid_signal"
	// ClassValidSignal — positive proof the mailbox EXISTS (delivered cleanly, or bounced BECAUSE full).
	ClassValidSignal Class = "valid_signal"
	// ClassZeroInformation — about your SENDER / the receiving POLICY / a transient; proves NOTHING
	// about the address. EXCLUDED from calibration entirely (never numerator or denominator, §4.3).
	ClassZeroInformation Class = "zero_information"
)

// Event is the NORMALIZED send-outcome event (§4.2) — the classifier maps DSN-code/text semantics to
// this closed set at ingestion (§3.2 normalizes the sender's RAW event; it never trusts a
// pre-classified label). The raw provider event is retained (send_outcomes.raw) so any mapping can be
// re-audited (Appendix C.1: classification quality is bounded by DSN fidelity — the GAP-48 seam).
type Event string

const (
	EventMailboxNotFound Event = "mailbox_not_found" // hard 550 no-such-user — invalid-signal (the gold)
	EventMailboxFull     Event = "mailbox_full"      // 452/552 over-quota — VALID-signal (existence proof!)
	EventDelivered       Event = "delivered"         // accepted, no bounce — WEAK valid-signal (silent-drop-prone)
	EventBlocked         Event = "blocked"           // reputation/IP-blocklist — zero-information (about the SENDER)
	EventPolicyReject    Event = "policy_reject"     // receiver policy/content — zero-information
	EventSpamReject      Event = "spam_reject"       // spam filter rejected the MESSAGE — zero-information
	EventGreylisted      Event = "greylisted"        // transient 4xx greylist — zero-information (retry would deliver)
	EventDeferred        Event = "deferred"          // transient 4xx non-quota — zero-information
	EventComplaint       Event = "complaint"         // spam report — zero-information FOR VALIDITY (sender-health signal)
	EventUnknown         Event = "unknown"           // unclassifiable/ambiguous DSN — harden toward exclusion (§4.2)
)

// Strength is the evidence-weight toward the class (§4.5). The ORDERING is LOCKED
// (mailbox_full ≥ discriminating-delivery > catch-all-delivery ≈ 0); the numeric VALUES are DIALs.
// A StrengthZero outcome nudges no bucket — it is present for completeness but weightless, the
// near-worthless "delivered on a catch-all" case (§4.5, P2 §5.5: the domain 250s everything).
type Strength int

const (
	StrengthZero     Strength = 0 // near-worthless (delivered on a catch-all) — weight ≈ 0, no bucket nudge
	StrengthWeak     Strength = 1 // delivered, no bounce (silent drops / spam-folder look identical)
	StrengthModerate Strength = 2 // delivered on a DISCRIMINATING (non-catch-all) server — decent, not proof
	StrengthStrong   Strength = 3 // mailbox_full existence proof / mailbox_not_found non-existence proof
)

// Classification is the classifier's output for one outcome: the class + the evidence strength.
type Classification struct {
	Class    Class
	Strength Strength
}

// Classify maps a NORMALIZED event to its calibration class + evidence strength (§4.2/§4.5) — the
// LOCKED heart. onCatchAll is the joined verdict's domain catch-all state (read from catchall_domains,
// P1 §4.3, written by the P2 probe): a "delivered" on a catch-all domain proves almost nothing about
// the specific mailbox (the domain accepts every RCPT), so it is down-weighted to StrengthZero.
//
// The exclusions ARE the discipline (§4.3): blocked/policy/spam/greylist/deferred/complaint/unknown all
// map to ClassZeroInformation — NEVER invalid-signal. This is the single most important behavior in the
// phase; the fail-verify test removes an exclusion (maps a blocked event to invalid-signal) and asserts
// the "blocked never enters a calibration numerator" test then breaks.
func Classify(ev Event, onCatchAll bool) Classification {
	switch ev {
	case EventMailboxNotFound:
		// The gold signal — the receiving server authoritatively denies the mailbox exists. The one
		// outcome that proves invalid. Strong negative (§4.2).
		return Classification{Class: ClassInvalidSignal, Strength: StrengthStrong}
	case EventMailboxFull:
		// THE counterintuitive rule (§4.2): a full mailbox EXISTS. Positive existence proof, strong.
		return Classification{Class: ClassValidSignal, Strength: StrengthStrong}
	case EventDelivered:
		// Delivered is only WEAK evidence — silent drops, spam-folder delivery, accept-then-discard all
		// look like "delivered" (§4.5). On a CATCH-ALL domain it is near-worthless (the domain 250s
		// everything) → StrengthZero, effectively excluded. On a discriminating server → moderate.
		if onCatchAll {
			return Classification{Class: ClassValidSignal, Strength: StrengthZero}
		}
		return Classification{Class: ClassValidSignal, Strength: StrengthModerate}
	case EventBlocked, EventPolicyReject, EventSpamReject, EventGreylisted, EventDeferred, EventComplaint, EventUnknown:
		// The zero-information MAJORITY (§4.3). About the sender's reputation / the receiver's policy /
		// a transient / a human's "don't want" (complaint) — NEVER about whether the address exists.
		// EXCLUDED from calibration. A complaint proves a human read it (valid-adjacent) but is a
		// sender-reputation signal, not validity evidence — logged for sender-health (§10), not counted.
		return Classification{Class: ClassZeroInformation, Strength: StrengthZero}
	default:
		// Harden toward exclusion, never toward invalid (§4.2 — the "harden toward unknown" instinct).
		return Classification{Class: ClassZeroInformation, Strength: StrengthZero}
	}
}

// InInvalidNumerator reports whether this classification contributes to a bucket's / accuracy metric's
// INVALID numerator (§4.3/§8.1 LOCKED): ONLY an invalid-signal (mailbox_not_found) hard bounce. This is
// the predicate the §4.3 exclusion discipline enforces — a zero-information outcome is NEVER in the
// numerator. The fail-verify test asserts a blocked event (→ zero-information) returns false here;
// neuter the classifier's exclusion (map blocked → invalid-signal) and this starts returning true → the
// test breaks. It is the single guard that keeps calibration measuring the ADDRESS, not the SENDER.
func (c Classification) InInvalidNumerator() bool {
	return c.Class == ClassInvalidSignal
}

// Calibrates reports whether this outcome enters a calibration bucket at all (numerator OR denominator).
// zero-information is excluded entirely (§4.3); a StrengthZero valid-signal (delivered-on-catch-all) is
// near-worthless and also does not calibrate (it would add denominator noise proving nothing — §4.5,
// Appendix B row 5). Everything with real signal (invalid-signal, or a valid-signal with Strength>0)
// calibrates. This is the denominator gate for the §8 accuracy metric's "any calibratable outcome".
func (c Classification) Calibrates() bool {
	if c.Class == ClassZeroInformation {
		return false
	}
	if c.Class == ClassValidSignal && c.Strength == StrengthZero {
		return false // delivered-on-catch-all: near-worthless, excluded (§4.5)
	}
	return true
}

// dsnBasicCode extracts the leading 3-digit SMTP basic code from the FRONT of the DSN line (the reply
// code is always the first token, per RFC 5321). It matches ONLY at the start (optionally after a
// leading MTA name), so an incidental "551" inside a queue-id/IP/byte-count later in the string can
// never be read as a reply code — the CRITICAL fix (a substring match poisoned the classifier).
var dsnBasicCode = regexp.MustCompile(`(?m)^\s*(?:[0-9a-z._-]+\s+said:?\s*)?([245][0-9][0-9])(?:[ \t.-]|$)`)

// dsnEnhancedCode extracts an RFC-3463 enhanced status code (class.subject.detail) as a WHOLE token —
// bounded by non-digit-dot on both sides — so "5.1.1" is matched as a code but "125.1.10.7" (an IP) or
// "15510" (a byte count) is not. This is the structural replacement for the free-substring matching.
var dsnEnhancedCode = regexp.MustCompile(`(?:^|[^0-9.])([245]\.[0-9]{1,3}\.[0-9]{1,3})(?:[^0-9.]|$)`)

// NormalizeEvent maps a sender's RAW event/DSN string to the normalized Event (§3.2/§4.3). This is the
// DIAL layer — the DSN-code/text → event mapping is provider-specific and tunable; the three-class
// taxonomy Classify applies to the RESULT is what is LOCKED. It NEVER trusts a pre-classified label:
// it matches on DSN semantics (codes + well-known text), and anything unrecognized → EventUnknown
// (harden toward exclusion, never toward invalid). Classification quality is bounded by DSN fidelity
// (Appendix C.1, the GAP-48 seam) — a genuinely mislabeled upstream DSN is the honest limit, flagged
// not hidden; the raw event is retained (send_outcomes.raw) so a mapping fix can re-audit history.
//
// CODES ARE PARSED STRUCTURALLY, NEVER free-substring-matched (the §4.3/GAP-49 correctness fix): a bare
// "551"/"5.1.1" inside a queue-id, IP, or byte-count must NEVER be read as a reply code and pull a
// transient/policy bounce into invalid-signal. The basic code is read from the FRONT of the line; the
// enhanced code is read as a whole dotted token. TEXT tokens (user-unknown, greylist, …) are still
// substring-matched, but those phrases don't collide the way bare digits do.
//
// Matching order: the MOST specific existence signals (not-found, full) win over the broader policy/
// reputation buckets, and a strong ADDRESS signal in the TEXT wins over a mere transient/policy CODE —
// because a "451 greylisted ... user unknown" composite should resolve to what it proves about the
// address. But a bare transient/policy code with NO address text never becomes invalid-signal.
func NormalizeEvent(raw string) Event {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return EventUnknown
	}
	basic := firstSubmatch(dsnBasicCode, s)     // "550" | "451" | "250" | "" (front-of-line only)
	enh := firstSubmatch(dsnEnhancedCode, s)     // "5.1.1" | "5.2.2" | … | "" (whole dotted token only)

	// mailbox_not_found (invalid-signal, the gold) — the strongest ADDRESS signal. Fires on the
	// authoritative no-such-user TEXT, or on the not-found ENHANCED code (5.1.1), or a bare 551 basic
	// code. A 550 alone is NOT enough (550 is a generic permanent reject — policy/spam/blocked all use
	// it), so 550 only counts here WITH not-found text or the 5.1.1 enhanced code (checked via text/enh).
	if containsAny(s, "mailbox_not_found", "user unknown", "no such user", "no such recipient",
		"recipient not found", "recipient rejected", "unknown recipient", "does not exist",
		"no mailbox", "recipient address rejected") || enh == "5.1.1" || basic == "551" {
		return EventMailboxNotFound
	}
	// mailbox_full (VALID-signal — existence proof). Over-quota TEXT, or the over-quota enhanced codes.
	if containsAny(s, "mailbox_full", "mailbox full", "over quota", "over-quota", "quota exceeded",
		"insufficient storage", "mailbox is full") || enh == "4.2.2" || enh == "5.2.2" || basic == "552" {
		return EventMailboxFull
	}
	// spam/content reject (zero-information) — before generic blocked/policy so it's attributed to
	// content, not reputation (both zero-information; the §10 classifier-distribution monitor wants the split).
	if containsAny(s, "spam", "content rejected", "message rejected as", "message content rejected") {
		return EventSpamReject
	}
	// greylist (zero-information, transient — retry would deliver).
	if containsAny(s, "greylist", "grey-list", "gray list", "try again later") || enh == "4.7.1" {
		return EventGreylisted
	}
	// blocked / reputation / IP-blocklist (zero-information — about the SENDER).
	if containsAny(s, "blocked", "blocklist", "blacklist", "block list", "reputation",
		"poor reputation", "listed on", "rbl", "dnsbl", "banned") || enh == "5.7.606" {
		return EventBlocked
	}
	// generic policy reject (zero-information).
	if containsAny(s, "policy", "not allowed", "rejected due to", "administrative prohibition", "denied") || enh == "5.7.0" || enh == "5.7.1" {
		return EventPolicyReject
	}
	// complaint / abuse / FBL (zero-information for validity).
	if containsAny(s, "complaint", "abuse report", "feedback loop", "fbl", "spam report") {
		return EventComplaint
	}
	// deferred / transient (zero-information). Any 4xx basic code with no stronger signal above lands
	// here (transient), NOT in invalid-signal — the harden-toward-exclusion direction.
	if containsAny(s, "deferred", "temporary failure", "temporarily", "try later", "connection timed out") ||
		strings.HasPrefix(enh, "4.") || strings.HasPrefix(basic, "4") {
		return EventDeferred
	}
	// delivered / accepted (weak valid-signal). Delivery TEXT, or a 2xx basic/enhanced code.
	if containsAny(s, "delivered", "delivery", "accepted", "relayed") ||
		strings.HasPrefix(enh, "2.") || strings.HasPrefix(basic, "2") {
		return EventDelivered
	}
	return EventUnknown
}

// firstSubmatch returns the first capture group of re against s, or "" if no match.
func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
