package layers

// Who failed when the mx layer could not resolve (signals.failure_origin; Docs/failure-origin.md), through the PRODUCTION resolver over
// the real DNS protocol: internal/dnsfixture runs a small DNS server in-process on loopback and NewNetResolverAt points Go's real DNS
// client at it — the same fixture ADR-0019/0020 use, not a fake Resolver. The fixture is the only thing standing in for the network.
//
// The two things asserted, independently: the OUTCOME is unchanged (mxError → unknown, never invalid: the causeRecorder only observes), and
// the CAUSE the layer reports maps to the origin the owner ruled — our resolver unreachable or timing out is `own`; a SERVFAIL relayed by a
// working resolver is `remote`; anything else is `undetermined`. Each failing case has a paired control that is not a failure.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/dnsfixture"
	"github.com/coldmail-verify/control-plane/internal/failureorigin"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

func dnsOrigin(c failureorigin.Cause) vocab.FailureOrigin {
	return failureorigin.OriginFor(vocab.ReasonDNSError, c)
}

func TestDNSFailureCause_RealDNS_WhoFailed(t *testing.T) {
	srv := dnsfixture.Start(t)
	for _, tc := range []struct {
		name       string
		domain     string
		budget     time.Duration
		reps       int // the A and AAAA lookups run concurrently: the cause must not depend on who finishes first
		wantCause  failureorigin.Cause
		wantOrigin vocab.FailureOrigin
	}{
		// remote: the resolver ANSWERED SERVFAIL — a working resolver reporting that the domain's name servers gave it nothing.
		{"SERVFAIL for every type", "servfail.test", 3 * time.Second, 5, failureorigin.CauseDNSServFail, vocab.OriginRemote},
		{"SERVFAIL on the MX query of a name that has an address", "mxservfail.test", 3 * time.Second, 5, failureorigin.CauseDNSServFail, vocab.OriginRemote},
		{"SERVFAIL on the MX query of a name with no address", "mxservfail-noaddr.test", 3 * time.Second, 5, failureorigin.CauseDNSServFail, vocab.OriginRemote},
		{"MX is NODATA, AAAA is SERVFAIL (the follow-up lookup's failure)", "aaaa-servfail.test", 3 * time.Second, 15, failureorigin.CauseDNSServFail, vocab.OriginRemote},
		{"MX is NODATA, A is SERVFAIL", "a-servfail.test", 3 * time.Second, 15, failureorigin.CauseDNSServFail, vocab.OriginRemote},
		// own: our resolver did not answer inside our budget.
		{"the resolver never answers the MX query (timeout)", "blackhole.test", 1200 * time.Millisecond, 1, failureorigin.CauseDNSResolverTimeout, vocab.OriginOwn},
		{"the resolver never answers the A follow-up (timeout)", "a-timeout.test", 1200 * time.Millisecond, 1, failureorigin.CauseDNSResolverTimeout, vocab.OriginOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < tc.reps; i++ {
				srv.Reset()
				ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
				ev := evidenceFor(tc.domain, NewNetResolverAt(srv.Addr()))
				out := MXLayer{}.Run(ctx, ev)
				_, outcome := ev.mx(ctx)
				cancel()

				if out.Result != Unknown || outcome != mxError {
					t.Fatalf("[run %d] result=%q outcome=%d, want unknown/mxError (the recorder must not change the outcome)", i, out.Result, outcome)
				}
				if out.Cause != tc.wantCause {
					t.Fatalf("[run %d] cause = %q, want %q", i, out.Cause, tc.wantCause)
				}
				if got := dnsOrigin(out.Cause); got != tc.wantOrigin {
					t.Fatalf("[run %d] origin = %s, want %s", i, got, tc.wantOrigin)
				}
			}
		})
	}
}

// CONTROLS: the same fixture, the cause removed. A published MX, an implicit MX (an A record), a null MX and a genuine NXDOMAIN are all
// ANSWERS — no unknown, so no cause and no origin.
func TestDNSFailureCause_RealDNS_ControlsAreNotFailures(t *testing.T) {
	srv := dnsfixture.Start(t)
	for _, tc := range []struct {
		domain  string
		outcome mxOutcome
	}{
		{"hasmx.test", mxFound}, {"aonly.test", mxFound}, {"nullmx.test", mxNullMX}, {"no-such-domain-xyz.test", mxNone}, {"norecords.test", mxNone},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ev := evidenceFor(tc.domain, NewNetResolverAt(srv.Addr()))
		out := MXLayer{}.Run(ctx, ev)
		_, outcome := ev.mx(ctx)
		cancel()
		if outcome != tc.outcome || out.Result == Unknown {
			t.Errorf("%s: outcome=%d result=%q, want a definitive outcome %d (this control must NOT be a failure)", tc.domain, outcome, out.Result, tc.outcome)
		}
		if out.Cause != "" || ev.mxCause != "" {
			t.Errorf("%s: an answered lookup carries cause %q: only a failure has a cause", tc.domain, out.Cause)
		}
	}
}

// own: the resolver we configured is unreachable. A UDP port that was bound and then closed: the kernel answers our datagram with an ICMP
// port-unreachable, which Go's DNS client reports as a socket-level error talking to that server; if a sandbox swallows the ICMP the query
// times out instead. Both are OUR resolver not answering — own — and neither may ever read as remote or as a fact about the domain.
func TestDNSFailureCause_RealDNS_AnUnreachableResolverIsOwn(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot bind a loopback UDP port: %v", err)
	}
	dead := pc.LocalAddr().String()
	_ = pc.Close() // nothing listens there any more

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	ev := evidenceFor("hasmx.test", NewNetResolverAt(dead))
	out := MXLayer{}.Run(ctx, ev)
	_, outcome := ev.mx(ctx)
	if out.Result != Unknown || outcome != mxError {
		t.Fatalf("an unreachable resolver must be unknown/mxError (never invalid), got result=%q outcome=%d", out.Result, outcome)
	}
	if out.Cause != failureorigin.CauseDNSResolverUnreachable && out.Cause != failureorigin.CauseDNSResolverTimeout {
		t.Fatalf("cause = %q, want resolver_unreachable (ICMP refused) or resolver_timeout (ICMP swallowed)", out.Cause)
	}
	if got := dnsOrigin(out.Cause); got != vocab.OriginOwn {
		t.Fatalf("origin = %s, want own", got)
	}
}

// own: no resolver at all is our configuration.
func TestDNSFailureCause_NoResolverWiredIsOwn(t *testing.T) {
	ev := NewEvidence("x@d.test", "x@d.test", "x", "d.test", nil, nil)
	out := MXLayer{}.Run(context.Background(), ev)
	if out.Result != Unknown || out.Cause != failureorigin.CauseDNSNoResolver {
		t.Fatalf("result=%q cause=%q, want unknown with %q", out.Result, out.Cause, failureorigin.CauseDNSNoResolver)
	}
	if got := dnsOrigin(out.Cause); got != vocab.OriginOwn {
		t.Fatalf("origin = %s, want own", got)
	}
}

// The classifier on its own, over the error shapes Go's DNS client produces (net/dnsclient_unix.go, net/net.go newDNSError) — including
// the ones that must NOT be billable: a bad rcode that is not SERVFAIL (REFUSED: our resolver refusing US), a lame REFERRAL (a server
// that is not recursive: our configuration), a cgo error with no server, a non-DNS error.
func TestDNSCause_ClassifiesTheErrorShapesGoProduces(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want failureorigin.Cause
	}{
		{"socket read timeout", &net.DNSError{Err: "read udp 127.0.0.1:1->127.0.0.1:2: i/o timeout", IsTimeout: true, IsTemporary: true, Server: "127.0.0.1:2"}, failureorigin.CauseDNSResolverTimeout},
		{"SERVFAIL reply", &net.DNSError{Err: "server misbehaving", IsTemporary: true, Server: "127.0.0.1:2"}, failureorigin.CauseDNSServFail},
		{"connection refused to the resolver", &net.DNSError{Err: "read udp 127.0.0.1:1->127.0.0.1:2: read: connection refused", IsTemporary: true, Server: "127.0.0.1:2"}, failureorigin.CauseDNSResolverUnreachable},
		{"another bad rcode (REFUSED/FORMERR): server misbehaving but NOT temporary", &net.DNSError{Err: "server misbehaving", Server: "127.0.0.1:2"}, failureorigin.CauseDNSUnclassified},
		{"lame referral from a non-recursive server", &net.DNSError{Err: "lame referral", Server: "127.0.0.1:2"}, failureorigin.CauseDNSUnclassified},
		{"malformed reply", &net.DNSError{Err: "cannot unmarshal DNS message", Server: "127.0.0.1:2"}, failureorigin.CauseDNSUnclassified},
		{"a cgo resolver error carries no server", &net.DNSError{Err: "temporary failure in name resolution", IsTemporary: true}, failureorigin.CauseDNSUnclassified},
		{"our own context deadline", context.DeadlineExceeded, failureorigin.CauseDNSResolverTimeout},
		{"our own context cancelled, wrapped", fmt.Errorf("lookup: %w", context.Canceled), failureorigin.CauseDNSResolverTimeout},
		{"a DNSError that wraps a context error", &net.DNSError{Err: "x", UnwrapErr: context.DeadlineExceeded}, failureorigin.CauseDNSResolverTimeout},
		{"an error that is not a DNS error", errors.New("boom"), failureorigin.CauseDNSUnclassified},
		{"nil", nil, failureorigin.CauseDNSUnclassified},
	} {
		if got := dnsCause(tc.err); got != tc.want {
			t.Errorf("%s: dnsCause = %q, want %q", tc.name, got, tc.want)
		}
	}
	// The direction of error, restated: of every shape above only the SERVFAIL reply is remote.
	for _, c := range []failureorigin.Cause{failureorigin.CauseDNSUnclassified, failureorigin.CauseDNSResolverUnreachable, failureorigin.CauseDNSResolverTimeout, failureorigin.CauseDNSNoResolver} {
		if dnsOrigin(c) == vocab.OriginRemote {
			t.Errorf("%q must never be remote", c)
		}
	}
}
