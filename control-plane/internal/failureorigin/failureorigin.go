// Package failureorigin is the ONE place that decides who failed when M1 could not produce an answer: signals.failure_origin ∈
// {own, remote, undetermined} (vocab.FailureOrigin). The full mapping, with the code line of every branch and the test that covers it,
// is Docs/failure-origin.md; the table below IS that mapping in code, and the doc is checked against it.
//
// THE RULE THE PLATFORM BILLS ON. An unknown caused by the recipient's server is charged; one caused by us, or that cannot be proven
// either way, is free. So the failure mode that matters is a false `remote`: every row of the table is `remote` only where the evidence
// in hand PROVES the recipient's side spoke about the recipient (an SMTP reply to RCPT TO in a transcript) or stayed silent after we
// proved the TCP session open, or our own healthy resolver relayed a SERVFAIL for the domain. Anything M1 cannot prove is
// `undetermined`, never a guess toward `remote`.
//
// WHAT THIS PACKAGE IS NOT. It never changes a status, reason, confidence, recommendation, caching decision or retry. It is a pure
// function of (reason, cause): the stage that saw the failure records the CAUSE (the evidence it holds: a DNS error class, a node
// error class, a transcript shape), and verdict.Assemble — the single place a verdict becomes a response and a LOG row — turns it into
// signals.failure_origin. Nothing else writes the key.
package failureorigin

import (
	"sort"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// Cause is the evidence a stage holds about WHY it could not answer. It is internal (never serialised): only the origin derived from
// it is. The empty Cause means "the stage recorded no evidence", which maps to undetermined.
type Cause string

const (
	// ── DNS (internal/layers: the mx layer's lookup) ──

	// CauseDNSNoResolver: no resolver is wired into the evidence (our own configuration).
	CauseDNSNoResolver Cause = "dns_no_resolver"
	// CauseDNSResolverTimeout: the resolver we asked did not answer within our budget (a socket timeout or our context's deadline).
	CauseDNSResolverTimeout Cause = "dns_resolver_timeout"
	// CauseDNSResolverUnreachable: a socket-level failure talking to the resolver we configured (connection refused, network
	// unreachable, write error): a DNS client error that carries the server address and is not a DNS reply.
	CauseDNSResolverUnreachable Cause = "dns_resolver_unreachable"
	// CauseDNSServFail: our resolver ANSWERED, with SERVFAIL ("server misbehaving", temporary): it is the report of a working resolver
	// that the domain's name servers gave it no answer (unreachable, lame delegation, DNSSEC failure).
	CauseDNSServFail Cause = "dns_servfail"
	// CauseDNSUnclassified: any other DNS error (a non-SERVFAIL bad rcode such as REFUSED, a malformed reply, a lame REFERRAL from a
	// non-recursive server, a cgo-resolver failure, an error that is not a DNS error at all).
	CauseDNSUnclassified Cause = "dns_unclassified"

	// ── The verifier node (internal/orchestrator/smtp_stage.go: the call to OUR node) ──

	// CauseNodeTransport: no HTTP response from our node (unreachable, refused, brain-side deadline): node.TransportError.
	CauseNodeTransport Cause = "node_transport"
	// CauseNodeRejected: our node answered HTTP 400 or 401 (a protocol or credential bug on our side).
	CauseNodeRejected Cause = "node_http_400_401"
	// CauseNodeHTTPStatus: our node answered any other non-2xx status.
	CauseNodeHTTPStatus Cause = "node_http_status"
	// CauseNodeBadResponse: our node answered 2xx with a body that is not the wrapper's JSON.
	CauseNodeBadResponse Cause = "node_bad_response"
	// CauseNodeClientError: our own client failed before or after the exchange (marshal, request build, body read).
	CauseNodeClientError Cause = "node_client_error"

	// ── The SMTP transcript (internal/smtp: what the node reports the recipient's server did) ──

	// CauseSMTPReply: the recipient's server answered at the RCPT stage with an SMTP reply code — it spoke, about this recipient.
	CauseSMTPReply Cause = "smtp_reply"
	// CauseSMTPPreRecipientReply: the recipient's server answered with a reply code at the GREETING or MAIL FROM stage, before the
	// mailbox was ever named. That is a statement about the connecting client (our IP's reputation, our sender identity), not about the
	// mailbox: postscreen deferrals and DNSBL refusals are sent exactly here. Not proof the recipient is at fault.
	CauseSMTPPreRecipientReply Cause = "smtp_pre_recipient_reply"
	// CauseSMTPSilentAfterConnect: a timeout in a session whose TCP connection the transcript shows OPEN (port_opened or a reached
	// RCPT stage): the recipient's server accepted the connection and then did not answer.
	CauseSMTPSilentAfterConnect Cause = "smtp_silent_after_connect"
	// CauseSMTPNoSession: the transcript shows no session at all (no transcript, or the port never opened, or a timeout with no proof
	// the TCP session opened). Our egress (port 25 blocked) and the recipient refusing look identical.
	CauseSMTPNoSession Cause = "smtp_no_session"
	// CauseSMTPSessionLost: the session was open but nothing attributable came back (no reply code and no timeout: a dropped
	// connection, a TLS error): the raw string cannot be attributed to the recipient's server or to our client.
	CauseSMTPSessionLost Cause = "smtp_session_lost"

	// ── The queue (internal/queue/drainer.go) ──

	// CauseErrorCapped: an item's policy-resolve / decrypt / compute error repeated up to the attempt cap and was settled.
	CauseErrorCapped Cause = "error_capped"
)

// Row is one line of the mapping: a (reason, cause) pair and the origin it proves. The table is the code half of Docs/failure-origin.md.
type Row struct {
	Reason vocab.Reason
	Cause  Cause
	Origin vocab.FailureOrigin
}

// table is the whole mapping. A pair that is not in it is `undetermined` (the default direction), with ONE reason-level rule applied
// before the lookup: smtp_blocked is always own (see OriginFor).
var table = []Row{
	// unknown / dns_error
	{vocab.ReasonDNSError, CauseDNSNoResolver, vocab.OriginOwn},
	{vocab.ReasonDNSError, CauseDNSResolverTimeout, vocab.OriginOwn},
	{vocab.ReasonDNSError, CauseDNSResolverUnreachable, vocab.OriginOwn},
	{vocab.ReasonDNSError, CauseDNSServFail, vocab.OriginRemote},
	{vocab.ReasonDNSError, CauseDNSUnclassified, vocab.OriginUndetermined},

	// unknown / smtp_error
	{vocab.ReasonSMTPError, CauseNodeRejected, vocab.OriginOwn},
	{vocab.ReasonSMTPError, CauseNodeBadResponse, vocab.OriginOwn},
	{vocab.ReasonSMTPError, CauseNodeClientError, vocab.OriginOwn},
	{vocab.ReasonSMTPError, CauseSMTPReply, vocab.OriginRemote},
	{vocab.ReasonSMTPError, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
	{vocab.ReasonSMTPError, CauseSMTPSessionLost, vocab.OriginUndetermined},

	// unknown / smtp_timeout
	{vocab.ReasonSMTPTimeout, CauseNodeTransport, vocab.OriginOwn},
	{vocab.ReasonSMTPTimeout, CauseSMTPSilentAfterConnect, vocab.OriginRemote},
	{vocab.ReasonSMTPTimeout, CauseSMTPNoSession, vocab.OriginUndetermined},

	// unknown / connection_failed
	{vocab.ReasonConnectionFailed, CauseNodeHTTPStatus, vocab.OriginOwn},
	{vocab.ReasonConnectionFailed, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
	{vocab.ReasonConnectionFailed, CauseSMTPSessionLost, vocab.OriginUndetermined},
	{vocab.ReasonConnectionFailed, CauseSMTPNoSession, vocab.OriginUndetermined},

	// unknown / smtp_blocked: always own, whatever the cause (the reason-level rule in OriginFor); the row documents the usual cause.
	{vocab.ReasonSMTPBlocked, CauseSMTPReply, vocab.OriginOwn},

	// unknown / greylisted: a 4xx reply from the recipient's server at the RCPT stage (a 4xx at MAIL FROM is a deferral aimed at us).
	{vocab.ReasonGreylisted, CauseSMTPReply, vocab.OriginRemote},
	{vocab.ReasonGreylisted, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},

	// unknown / unknown_error: the drainer's attempt cap.
	{vocab.ReasonUnknownError, CauseErrorCapped, vocab.OriginOwn},
}

// Rows returns a copy of the mapping table (for the doc check and the tests).
func Rows() []Row {
	out := make([]Row, len(table))
	copy(out, table)
	return out
}

// OriginFor is the pure function. It never returns anything but a valid vocab.FailureOrigin and never returns OriginRemote for a pair
// that is not a table row with that origin.
//
// One reason-level rule is applied before the table: unknown/smtp_blocked is ALWAYS own. A policy or reputation block is a statement
// about OUR IP (it matches M1's own bounce classification), so whatever the transcript looked like, the cause is not the recipient's
// mailbox.
func OriginFor(reason vocab.Reason, cause Cause) vocab.FailureOrigin {
	if reason == vocab.ReasonSMTPBlocked {
		return vocab.OriginOwn
	}
	for _, r := range table {
		if r.Reason == reason && r.Cause == cause {
			return r.Origin
		}
	}
	return vocab.OriginUndetermined
}

// Of is the function verdict.Assemble calls. ok=false means the key does not apply to this result.
//
//   - every `unknown` result gets an origin (OriginFor), whatever its reason: an unknown reason, or an unknown with no recorded cause,
//     is undetermined — so the key is present on EVERY unknown by construction, not by a branch someone must remember;
//   - a non-unknown result that carries a skip token recording OUR failure (no_eligible_node, audit_red, warmup_cap, rate_limited,
//     smtp_unwired) is `own`: the cheap-only answer it carries is a degraded fallback, not what a working M1 would have said. This is
//     additive information only; the verdict, confidence, recommendation and cacheability are decided elsewhere and untouched.
func Of(status vocab.Status, reason vocab.Reason, cause Cause, signals map[string]any) (vocab.FailureOrigin, bool) {
	if status == vocab.StatusUnknown {
		return OriginFor(reason, cause), true
	}
	if token, _ := signals[vocab.SignalSMTPSkippedReason].(string); vocab.SkipIsOwnFailure(token) {
		return vocab.OriginOwn, true
	}
	return "", false
}

// Causes returns every cause constant that appears in the table, sorted (for tests that sweep them).
func Causes() []Cause {
	seen := map[Cause]struct{}{}
	for _, r := range table {
		seen[r.Cause] = struct{}{}
	}
	out := make([]Cause, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
