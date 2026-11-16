package smtp

import "github.com/coldmail-verify/control-plane/internal/failureorigin"

// CauseOf is the evidence a parsed transcript gives about WHO could not answer, for an `unknown` verdict (signals.failure_origin,
// Docs/failure-origin.md). It reads exactly the fields Classify reads, in the same order, and never changes what Classify decides.
//
// The principle: the recipient's side is proven only by
//
//	(a) an SMTP reply at the RCPT stage — their server answered about THIS recipient — or
//	(b) a timeout in a session whose TCP connection the transcript shows open — their server accepted the connection and then did not
//	    answer.
//
// Everything else is left to the table's undetermined default, because the transcript cannot attribute it:
//   - no session at all (nothing, or port_opened=false everywhere): our egress (port 25 blocked) and the recipient refusing look
//     identical;
//   - a reply at the GREETING or MAIL FROM stage: it answers the connecting client — our IP's reputation, our sender identity — not the
//     mailbox under test (postscreen deferrals and DNSBL refusals are sent here), so it is not proof the recipient is at fault;
//   - an open session that ended with no reply code and no timeout (a dropped connection, a TLS error): the raw string cannot be
//     attributed to their server or to our client.
func CauseOf(p Parsed) failureorigin.Cause {
	sessionOpen := p.PortOpened || p.Reached
	switch {
	case !p.PortOpened && !p.Reached && p.Stage != "mailfrom":
		// Classify rule 1 (connection_failed): no port ever opened / no transcript at all.
		return failureorigin.CauseSMTPNoSession
	case p.Timeout:
		if sessionOpen {
			return failureorigin.CauseSMTPSilentAfterConnect
		}
		return failureorigin.CauseSMTPNoSession
	case p.Code != "":
		if p.Stage == "rcptto" {
			return failureorigin.CauseSMTPReply
		}
		return failureorigin.CauseSMTPPreRecipientReply
	default:
		return failureorigin.CauseSMTPSessionLost
	}
}
