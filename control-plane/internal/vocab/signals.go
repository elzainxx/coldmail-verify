package vocab

import "sort"

// This file: the vocabulary of the OPEN `signals` bag keys that other systems code against, kept beside the frozen enums so they are
// declared once. Nothing here is a Door-4 enum (status / reason / recommendation) and nothing here changes one: these are additive
// values inside `signals`, which Door 1 leaves open (consumers ignore what they do not know). Rule of the door still applies: a value is
// never renamed, removed or repurposed once shipped.

// SignalFailureOrigin is the signals key that says WHO failed when an answer could not be produced (see internal/failureorigin and
// Docs/failure-origin.md). It is present on every `unknown` verdict and on a `risky` answer that is a degraded cheap-only fallback
// caused by us (SignalSMTPSkippedReason carrying one of the own-failure tokens below).
const SignalFailureOrigin = "failure_origin"

// SignalErrorCapped is the signals key the drainer sets (true) when a work item was settled because its compute / decrypt / policy
// error repeated up to the attempt cap (TM_QUEUE_ERROR_MAX_ATTEMPTS) rather than because a check produced an answer.
const SignalErrorCapped = "error_capped"

// SignalSMTPSkippedReason is the signals key that says why the SMTP stage did not run (the tokens are the Skip* constants below).
const SignalSMTPSkippedReason = "smtp_skipped_reason"

// FailureOrigin is the value of signals.failure_origin. Exactly three values; the platform bills on it: an unknown caused by the
// recipient's server (OriginRemote) is charged; one caused by us (OriginOwn), or that cannot be proven either way (OriginUndetermined),
// is free. The default for anything not proven is OriginUndetermined, never OriginRemote.
type FailureOrigin string

const (
	// OriginOwn: the cause is on our side of the wire (our node, our egress, our resolver, our queue, a policy or reputation block about
	// OUR IP).
	OriginOwn FailureOrigin = "own"
	// OriginRemote: the recipient's side provably caused it (their server spoke, or was silent after we proved the session open; their
	// domain's DNS failed behind a healthy resolver of ours).
	OriginRemote FailureOrigin = "remote"
	// OriginUndetermined: M1 cannot prove either side. Never a guess toward remote.
	OriginUndetermined FailureOrigin = "undetermined"
)

var failureOrigins = map[FailureOrigin]struct{}{
	OriginOwn: {}, OriginRemote: {}, OriginUndetermined: {},
}

// ValidFailureOrigin guards the three legal values.
func ValidFailureOrigin(o FailureOrigin) bool { _, ok := failureOrigins[o]; return ok }

// The signals.smtp_skipped_reason tokens (an OPEN string enum, additive-only: P3 shipped four, P4 added rate_limited, the P2-research
// close-out added smtp_unwired). The orchestrator writes them; they are listed ONCE, here, so a test can enumerate them.
const (
	SkipNoEligibleNode = "no_eligible_node"   // no node is :smtp-eligible (a health / routing gap) — provisional, never cached
	SkipAuditRed       = "audit_red"          // the pool is red — provisional, never cached
	SkipProviderMX     = "provider_mx_routed" // the provider is :mx-routed (Gmail/O365/Yahoo): a SETTLED answer (risky/provider_no_smtp), cacheable
	SkipWarmupCap      = "warmup_cap"         // the node is at its warmup cap — provisional, never cached
	SkipRateLimited    = "rate_limited"       // every eligible node is at its per-provider cap — provisional, never cached
	// SkipSMTPUnwired: the SMTP stage is not wired in this process at all (SMTP disabled, or buildSMTP returned nil because no node or
	// no config could be fetched). Our own degraded state, not an answer: provisional, never cached.
	SkipSMTPUnwired = "smtp_unwired"
)

// SkipTokens returns every legal signals.smtp_skipped_reason token, sorted.
func SkipTokens() []string {
	out := []string{SkipNoEligibleNode, SkipAuditRed, SkipProviderMX, SkipWarmupCap, SkipRateLimited, SkipSMTPUnwired}
	sort.Strings(out)
	return out
}

// SkipIsOwnFailure reports whether a skip token records a failure (or a deliberate degradation) on OUR side, i.e. the cheap-only answer
// that carries it is not what a working M1 would have said. provider_mx_routed is the exception: it is a settled routing outcome.
func SkipIsOwnFailure(token string) bool {
	switch token {
	case SkipNoEligibleNode, SkipAuditRed, SkipWarmupCap, SkipRateLimited, SkipSMTPUnwired:
		return true
	}
	return false
}
