package layers

import (
	"context"
	"errors"
	"net"
	"sort"
)

// asDNSNotFound reports whether err is a DEFINITIVE "this name does not exist" DNS error
// (NXDOMAIN / no-such-host / no records for the type) — the only DNS condition that proves
// non-deliverability. A timeout or temporary/SERVFAIL failure returns false, so the caller
// keeps it as `unknown` (harden-toward-unknown; Door 4 invariant). *out receives the parsed
// *net.DNSError when the error is one, for the caller's optional detail.
func asDNSNotFound(err error, out **net.DNSError) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		*out = dnsErr
		// IsNotFound is set for NXDOMAIN / no records. IsTemporary/IsTimeout are NOT
		// non-existence — those stay unknown.
		return dnsErr.IsNotFound
	}
	return false
}

// NetResolver is the production Resolver backed by the Go stdlib net.Resolver. It is used by
// the control-plane on the cheap path (OQ-6: the brain does its OWN MX/DNS resolution — the
// engine JSON drops mail_servers, GAP-2). Timeouts are the caller's context's responsibility.
type NetResolver struct {
	r *net.Resolver
}

// NewNetResolver returns a NetResolver using the system default resolver.
func NewNetResolver() *NetResolver { return &NetResolver{r: net.DefaultResolver} }

func (n *NetResolver) LookupMX(ctx context.Context, domain string) ([]MXHost, error) {
	recs, err := n.r.LookupMX(ctx, domain)
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

func (n *NetResolver) LookupIP(ctx context.Context, domain string) ([]net.IP, error) {
	addrs, err := n.r.LookupIPAddr(ctx, domain)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

func trimDot(host string) string {
	if len(host) > 0 && host[len(host)-1] == '.' {
		return host[:len(host)-1]
	}
	return host
}
