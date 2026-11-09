// Package vocab is the SINGLE authoritative source for the frozen Door 4
// vocabularies: status, reason, and recommendation. (PROJECT_RULES.md Door 4.)
//
// RULE (LOCKED, additive-only): new values may be added forever; a value is
// NEVER renamed, removed, or repurposed. Consumers branch on these strings, and
// P7 calibration buckets on `reason` — a rename silently corrupts history.
//
// Values reserved by a later phase (P2..P7) are declared NOW so the enum never
// needs a breaking addition later; they simply won't be emitted until that phase.
// This file is the one place the enums live — never duplicated per package.
//
// Phase 0 note: these are typed constants + validation sets, not behavior. The
// scoring that MAPS signals to these values is later-phase; here we only freeze
// the vocabulary and provide the "is this a legal value" guard the storage-layer
// CHECK constraints (P1 §4.1) mirror.
package vocab

import "sort"

// Status is the verdict. Exactly four values, frozen (P1 §3.4.1).
type Status string

const (
	StatusValid   Status = "valid"   // positive proof of deliverability (SMTP-earned; P2+)
	StatusInvalid Status = "invalid" // positive proof of non-deliverability ONLY
	StatusRisky   Status = "risky"   // deliverable-but-dangerous / low-trust
	StatusUnknown Status = "unknown" // FIRST-CLASS: could not determine; never guessed away
)

// Recommendation is the action hint. Three values, frozen (P1 §3.4.2).
type Recommendation string

const (
	RecSend   Recommendation = "send"
	RecReview Recommendation = "review"
	RecSkip   Recommendation = "skip"
)

// Reason is the single dominant cause of a verdict. Frozen enum, additive-only
// (P1 §3.4.3). Grouped by the phase that first EMITS it; all are reserved now.
type Reason string

const (
	// Phase 1 — cheap layers (no port 25).
	ReasonInvalidSyntax     Reason = "invalid_syntax"
	ReasonInvalidDomain     Reason = "invalid_domain"
	ReasonNoMX              Reason = "no_mx"
	ReasonNullMX            Reason = "null_mx"
	ReasonDNSError          Reason = "dns_error"
	ReasonDisposable        Reason = "disposable" // -> risky, never invalid (OQ-7)
	ReasonBlacklistedDomain Reason = "blacklisted_domain"
	ReasonBlacklistedMX     Reason = "blacklisted_mx"
	ReasonSpamTrap          Reason = "spam_trap"
	ReasonRoleAccount       Reason = "role_account"
	ReasonWhitelisted       Reason = "whitelisted"
	ReasonProviderNoSMTP    Reason = "provider_no_smtp"
	ReasonPlausible         Reason = "plausible"
	ReasonUnknownError      Reason = "unknown_error"

	// Phase 2 — SMTP (reserved in P1, emitted from P2).
	ReasonAccepted         Reason = "accepted"
	ReasonMailboxNotFound  Reason = "mailbox_not_found"
	ReasonMailboxFull      Reason = "mailbox_full" // 552 = positive existence proof -> valid (P2 §6.1.1)
	ReasonCatchAll         Reason = "catch_all"
	ReasonAcceptAll        Reason = "accept_all"
	ReasonGreylisted       Reason = "greylisted"
	ReasonSMTPBlocked      Reason = "smtp_blocked"
	ReasonSMTPTimeout      Reason = "smtp_timeout"
	ReasonSMTPError        Reason = "smtp_error"
	ReasonConnectionFailed Reason = "connection_failed"

	// Phase 6 — third-party fallback (reserved in P1).
	ReasonExternalVerified Reason = "external_verified"

	// Phase 7 — flywheel (reserved in P1).
	ReasonHistoricalBounce Reason = "historical_bounce"

	// ADR-0021 (additive): the verifier node refused to open an SMTP session because the domain's mail host resolves to a
	// non-public address (loopback, RFC 1918, link-local/cloud-metadata, CGNAT, ...). risky, never invalid; no probe was made.
	ReasonNonPublicMX Reason = "non_public_mx"
)

// The authoritative legal-value sets. Storage-layer CHECK constraints (P1 §4.1)
// must mirror these; adding a value = update here + an additive CHECK migration.
var (
	statuses = map[Status]struct{}{
		StatusValid: {}, StatusInvalid: {}, StatusRisky: {}, StatusUnknown: {},
	}
	recommendations = map[Recommendation]struct{}{
		RecSend: {}, RecReview: {}, RecSkip: {},
	}
	reasons = map[Reason]struct{}{
		ReasonInvalidSyntax: {}, ReasonInvalidDomain: {}, ReasonNoMX: {}, ReasonNullMX: {},
		ReasonDNSError: {}, ReasonDisposable: {}, ReasonBlacklistedDomain: {}, ReasonBlacklistedMX: {},
		ReasonSpamTrap: {}, ReasonRoleAccount: {}, ReasonWhitelisted: {}, ReasonProviderNoSMTP: {},
		ReasonPlausible: {}, ReasonUnknownError: {},
		ReasonAccepted: {}, ReasonMailboxNotFound: {}, ReasonMailboxFull: {}, ReasonCatchAll: {},
		ReasonAcceptAll: {}, ReasonGreylisted: {}, ReasonSMTPBlocked: {}, ReasonSMTPTimeout: {},
		ReasonSMTPError: {}, ReasonConnectionFailed: {},
		ReasonExternalVerified: {}, ReasonHistoricalBounce: {}, ReasonNonPublicMX: {},
	}
)

// ValidStatus / ValidReason / ValidRecommendation guard the frozen vocabulary at
// the boundary. A value outside the set is a Door-4 violation, never emitted.
func ValidStatus(s Status) bool                 { _, ok := statuses[s]; return ok }
func ValidReason(r Reason) bool                 { _, ok := reasons[r]; return ok }
func ValidRecommendation(r Recommendation) bool { _, ok := recommendations[r]; return ok }

// ComposabilityOK enforces the LOCKED status->recommendation invariants (P1 §3.1.3):
// invalid⇒{skip}; valid⇒{send,review}; risky⇒{review,skip}; unknown⇒{review,skip}.
// invalid+send is a forbidden combination. Directional invariant is LOCKED;
// the threshold VALUES that pick send-vs-review are a DIAL (config).
func ComposabilityOK(s Status, rec Recommendation) bool {
	switch s {
	case StatusInvalid:
		return rec == RecSkip
	case StatusValid:
		return rec == RecSend || rec == RecReview
	case StatusRisky, StatusUnknown:
		return rec == RecReview || rec == RecSkip
	default:
		return false
	}
}

// Reasons returns every legal reason, sorted. It exists so a test can check the storage-layer CHECK constraints against this set
// (the comment above says they must mirror it; until ADR-0021 nothing verified that).
func Reasons() []Reason {
	out := make([]Reason, 0, len(reasons))
	for r := range reasons {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
