package layers

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/coldmail-verify/control-plane/internal/failureorigin"
)

// This file: who failed when the mx layer could not resolve (signals.failure_origin, Docs/failure-origin.md). It only OBSERVES the
// errors the resolver returned; it changes no outcome. resolveMX / impliedMX / NetResolver are untouched.

// causeRecorder wraps a Resolver and remembers the last error either lookup returned. For an mxError outcome that last error is the
// failure that caused it: resolveMX returns mxError either straight from a non-not-found LookupMX error (no further lookup is made) or
// from a non-not-found LookupIP error that follows a not-found LookupMX (so the IP error is the last one recorded).
type causeRecorder struct {
	Resolver
	mu   sync.Mutex
	last error
}

func (r *causeRecorder) note(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.last = err
	r.mu.Unlock()
}

func (r *causeRecorder) lastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func (r *causeRecorder) LookupMX(ctx context.Context, domain string) ([]MXHost, error) {
	hosts, err := r.Resolver.LookupMX(ctx, domain)
	r.note(err)
	return hosts, err
}

func (r *causeRecorder) LookupIP(ctx context.Context, domain string) ([]net.IP, error) {
	ips, err := r.Resolver.LookupIP(ctx, domain)
	r.note(err)
	return ips, err
}

// serverMisbehaving is the text Go's DNS client gives a reply whose rcode is not success and not NXDOMAIN (net/dnsclient_unix.go:
// errServerMisbehaving / errServerTemporarilyMisbehaving). Only the TEMPORARY variant is a SERVFAIL; the permanent one is any other bad
// rcode (REFUSED, FORMERR, NOTIMP, ...).
const serverMisbehaving = "server misbehaving"

// dnsCause classifies a lookup error that was NOT a definitive not-found, into the evidence it gives about who failed:
//
//   - a timeout (the resolver did not answer inside our budget, or our own context ended)    → our resolver / budget: own;
//   - a socket-level failure talking to the configured server (refused, unreachable, write)  → our resolver unreachable: own;
//   - a SERVFAIL REPLY from the resolver (it answered, "server misbehaving", temporary)      → remote: a working resolver of ours
//     reports that the domain's name servers produced no answer (unreachable, lame delegation, DNSSEC failure);
//   - anything else (REFUSED / FORMERR / NOTIMP, a malformed reply, a lame REFERRAL from a server that is not recursive, a cgo
//     resolver error, a non-DNS error)                                                       → unclassified: undetermined.
//
// What M1 cannot see: whether a SERVFAIL really came from the domain's servers or from our resolver's own trouble (Docs/failure-origin.md,
// "Cases that cannot be classified honestly"). A stub resolver only ever sees its own resolver's answer.
func dnsCause(err error) failureorigin.Cause {
	if err == nil {
		// An mxError with no recorded error cannot happen (every mxError path returns one); stay honest if it ever does.
		return failureorigin.CauseDNSUnclassified
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsTimeout:
			return failureorigin.CauseDNSResolverTimeout
		case de.IsTemporary && de.Err == serverMisbehaving:
			return failureorigin.CauseDNSServFail
		case de.IsTemporary && de.Server != "":
			// Go sets IsTemporary on a socket-level (*net.OpError) failure of the DNS client and records the server it was talking to;
			// a reply-level failure was handled above, and a reply with a non-temporary failure falls through to unclassified.
			return failureorigin.CauseDNSResolverUnreachable
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return failureorigin.CauseDNSResolverTimeout
	}
	return failureorigin.CauseDNSUnclassified
}
