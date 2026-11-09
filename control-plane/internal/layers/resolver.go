package layers

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// fqdn returns name fully qualified: with a trailing dot, which makes the name ABSOLUTE.
//
// WHY THIS EXISTS (the M3-CI-OD-1 class, applied to module 1 — ADR-0019). Go's resolver, given a name
// without a trailing dot, walks the search list from /etc/resolv.conf — the name as written first, then
// name.<search-domain> for each search domain — and, unless Resolver.StrictErrors is set, returns the error
// of the LAST name it tried. So a SERVFAIL for the real domain was overwritten by the NXDOMAIN of the
// suffixed one, asDNSNotFound read "this domain does not exist", and resolveMX reported mxNone: a DNS outage
// of OURS became `invalid / no_mx` — our failure stated as a fact about the user's address, which this
// module exists never to do. (The same walk can also return another name's records: a NXDOMAIN for the real
// name followed by a wildcard under a search domain answers for a domain the user never typed.) A rooted
// name is looked up exactly as written and nothing else, so the error — and the data — belong to the query.
//
// An empty name is returned unchanged (never turned into "." — the root); a rooted one is idempotent.
func fqdn(name string) string {
	if name == "" || strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

// asDNSNotFound reports whether err is a DEFINITIVE "this name does not exist" DNS error
// (NXDOMAIN / no-such-host / no records for the type) — the only DNS condition that proves
// non-deliverability. It is the NARROW door: a *net.DNSError that is IsNotFound and is NEITHER a timeout
// NOR temporary. A timeout, SERVFAIL, refused or unreachable server, deadline, malformed reply or any
// non-DNS error returns false, so the caller keeps it as `unknown` (harden-toward-unknown; Door 4
// invariant). The earlier form returned IsNotFound alone, so an error that was both not-found and
// temporary counted as proof of non-existence. *out receives the parsed *net.DNSError when the error is
// one, for the caller's optional detail.
//
// What Go cannot tell us: IsNotFound is set for both NXDOMAIN and NODATA and the rcode is not exposed.
func asDNSNotFound(err error, out **net.DNSError) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		*out = dnsErr
		return dnsErr.IsNotFound && !dnsErr.IsTimeout && !dnsErr.IsTemporary
	}
	return false
}

// lookuper is the subset of *net.Resolver NetResolver uses, so a test can record exactly which names reach
// the DNS client (and prove every one is rooted) without any network.
//
// LookupIP takes the network ("ip4" asks ONLY for A, "ip6" ONLY for AAAA) because NetResolver.LookupIP must ask the two
// separately — see there (ADR-0020).
type lookuper interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// NetResolver is the production Resolver backed by the Go stdlib net.Resolver. It is used by
// the control-plane on the cheap path (OQ-6: the brain does its OWN MX/DNS resolution — the
// engine JSON drops mail_servers, GAP-2). Timeouts are the caller's context's responsibility.
// Every name is rooted (fqdn) before it reaches the DNS client.
type NetResolver struct {
	r lookuper
}

// NewNetResolver returns a NetResolver using the system default resolver.
func NewNetResolver() *NetResolver { return &NetResolver{r: net.DefaultResolver} }

// NewNetResolverAt returns a NetResolver that dials the DNS server at addr ("host:port") instead of the
// nameservers in /etc/resolv.conf. It replaces the SERVER only: the pure-Go resolver still reads the SEARCH
// LIST and ndots from /etc/resolv.conf, which is exactly why every name is rooted. Used by the tests that
// point the production resolver at a controllable DNS server (internal/layers/dns_realdns_test.go); no
// production code path uses it.
func NewNetResolverAt(addr string) *NetResolver {
	return &NetResolver{r: &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func (n *NetResolver) LookupMX(ctx context.Context, domain string) ([]MXHost, error) {
	recs, err := n.r.LookupMX(ctx, fqdn(domain))
	if err != nil {
		// net.LookupMX returns an error for a genuine null-MX ("."), but also surfaces the
		// record; we detect null-MX at the record level in resolveMX. Propagate the error so
		// NXDOMAIN vs timeout classification happens in one place (asDNSNotFound).
		return nil, err
	}
	out := make([]MXHost, 0, len(recs))
	for _, m := range recs {
		out = append(out, MXHost{Host: trimDot(m.Host), Pref: m.Pref})
	}
	// Deterministic order (by preference then host) so provider classification and any
	// signals are stable across calls.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pref != out[j].Pref {
			return out[i].Pref < out[j].Pref
		}
		return out[i].Host < out[j].Host
	})
	return out, nil
}

// LookupIP returns the domain's A and AAAA addresses. It asks for the two record types as TWO SEPARATE lookups and
// combines them itself, and that is deliberate (ADR-0020): Go's own combined lookup (LookupIPAddr, or LookupIP with
// network "ip") sends A and AAAA concurrently and, for a rooted name, keeps only the LAST error to arrive — so with A
// = NODATA and AAAA = SERVFAIL it returns "not found" or "server misbehaving" depending on which goroutine finished
// last. "Not found" is the one answer this module turns into `invalid`, so a partial outage of ours would sometimes be
// stated as a fact about the user's domain. Asked separately, each error is unambiguous and the rule is explicit:
//
//   - any address from either family    → the addresses (the other family's failure is irrelevant: the host exists);
//   - both families definitively absent → a not-found error (the one door resolveMX reads as "no A/AAAA");
//   - anything else                     → the non-not-found error, which resolveMX keeps as `unknown`.
//
// The two queries run in parallel, so the extra family costs no extra latency over Go's combined lookup.
func (n *NetResolver) LookupIP(ctx context.Context, domain string) ([]net.IP, error) {
	name := fqdn(domain)
	type answer struct {
		ips []net.IP
		err error
	}
	var v4, v6 answer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); v4.ips, v4.err = n.r.LookupIP(ctx, "ip4", name) }()
	go func() { defer wg.Done(); v6.ips, v6.err = n.r.LookupIP(ctx, "ip6", name) }()
	wg.Wait()

	if all := append(append([]net.IP{}, v4.ips...), v6.ips...); len(all) > 0 {
		return all, nil
	}
	var de *net.DNSError
	v4Absent := v4.err == nil || asDNSNotFound(v4.err, &de) // nil error + no addresses = answered, nothing there
	v6Absent := v6.err == nil || asDNSNotFound(v6.err, &de)
	switch {
	case v4Absent && v6Absent:
		if v4.err != nil {
			return nil, v4.err // a not-found error, so the caller's narrow door sees it
		}
		if v6.err != nil {
			return nil, v6.err
		}
		return nil, nil // both answered with nothing
	case !v4Absent:
		return nil, v4.err
	default:
		return nil, v6.err
	}
}

func trimDot(host string) string {
	if len(host) > 0 && host[len(host)-1] == '.' {
		return host[:len(host)-1]
	}
	return host
}
