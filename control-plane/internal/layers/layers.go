// Package layers holds the brain-native cheap layers (P1 §6, OQ-6 — no engine, no port 25).
//
// Contract (P1 §6.0/§7.1): a layer reports a FINDING + a hint via LayerOutput; it NEVER
// decides the final verdict — the POLICY decides gate/flag/stop (Increment 5). An `unknown`
// from a layer (e.g. a DNS timeout) must propagate toward status `unknown`, never `invalid`
// (Door 4 invariant, the founding harden-toward-unknown discipline).
//
// Layers that need DNS (mx, null_mx, provider_classify) share ONE memoized lookup via the
// Evidence object, so a request resolves MX at most once. DNS + list lookups are behind
// interfaces (Resolver, ListSource) so unit tests inject fixtures — no network, no DB.
package layers

import (
	"context"
	"net"
	"sync"
)

// Result is a layer's finding (P1 §6.0).
type Result string

const (
	Pass    Result = "pass"
	Fail    Result = "fail"
	Unknown Result = "unknown"
	Flag    Result = "flag"
	Skip    Result = "skip"
)

// Severity is the layer's hint (the policy, not the layer, acts on it).
type Severity string

const (
	Info     Severity = "info"
	Warn     Severity = "warn"
	Critical Severity = "critical"
)

// LayerOutput is the LOCKED shape emitted into the signals bag (P1 §6.0).
type LayerOutput struct {
	LayerID      string
	SignalKey    string
	Result       Result
	SeverityHint Severity
	Value        any
	Detail       string
}

// Layer is the common interface each cheap layer implements. Run is pure w.r.t. the verdict:
// it returns a finding, never a status. It may read/populate the shared Evidence (e.g. MX).
type Layer interface {
	ID() string
	Run(ctx context.Context, ev *Evidence) LayerOutput
}

// MXHost is one resolved mail exchanger (host + preference).
type MXHost struct {
	Host string
	Pref uint16
}

// Resolver abstracts DNS so tests inject fixtures. LookupMX returns the domain's MX records
// (an RFC 7505 null-MX surfaces as a single host "." ); LookupIP is the A/AAAA fallback used
// to decide implicit-MX deliverability (RFC 5321 §5.1). Errors are classified by the MX layer.
type Resolver interface {
	LookupMX(ctx context.Context, domain string) ([]MXHost, error)
	LookupIP(ctx context.Context, domain string) ([]net.IP, error)
}

// ListSource is the subset of the store the list-backed layers need (disposable, spam-trap).
// Declared here (not imported from store) so layers depend on a capability, not the DB type.
type ListSource interface {
	IsDisposable(ctx context.Context, domain string) (bool, error)
	IsSpamTrap(ctx context.Context, domain string) (bool, error)
}

// mxOutcome is the classified result of the shared DNS lookup.
type mxOutcome int

const (
	mxUnresolved mxOutcome = iota // not yet looked up
	mxFound                       // has MX records (or A/AAAA implicit MX)
	mxNone                        // domain resolves but has NO mail route (positive proof → invalid)
	mxNullMX                      // RFC 7505 null-MX (explicit "accepts no mail" → invalid)
	mxError                       // transient DNS failure (timeout/SERVFAIL → unknown, NEVER invalid)
)

// Evidence is the per-request working set: the normalized address parts, the injected
// dependencies, and a memoized DNS result shared across the DNS-dependent layers.
type Evidence struct {
	Raw        string // caller's raw input (echoed; OQ-4)
	Normalized string // norm/v1 output
	LocalPart  string // normalized local part
	Domain     string // normalized domain (norm/v1: lowercase/punycode)

	resolver Resolver
	lists    ListSource

	mxOnce    sync.Once
	mxHosts   []MXHost  // MX records with a real target (null-MX sentinel removed)
	mxOutcome mxOutcome // classified once
}

// NewEvidence builds the per-request evidence. resolver/lists may be nil for layers that
// don't need them (the DNS/list layers guard on nil and emit Unknown rather than panic).
func NewEvidence(raw, normalized, localPart, domain string, resolver Resolver, lists ListSource) *Evidence {
	return &Evidence{
		Raw: raw, Normalized: normalized, LocalPart: localPart, Domain: domain,
		resolver: resolver, lists: lists,
	}
}

// mx resolves (once) and classifies the domain's mail route. Shared by the mx, null_mx and
// provider_classify layers so DNS is hit at most once per request.
func (ev *Evidence) mx(ctx context.Context) (hosts []MXHost, outcome mxOutcome) {
	ev.mxOnce.Do(func() {
		if ev.resolver == nil {
			ev.mxOutcome = mxError // no resolver wired → cannot prove anything → unknown
			return
		}
		ev.mxHosts, ev.mxOutcome = resolveMX(ctx, ev.resolver, ev.Domain)
	})
	return ev.mxHosts, ev.mxOutcome
}

// resolveMX performs the lookup and applies the harden-toward-unknown classification. It is
// the single most safety-critical function in the cheap path: a transient failure must
// become `unknown`, and only a definitive "this domain has no mail route" becomes a fail.
func resolveMX(ctx context.Context, r Resolver, domain string) ([]MXHost, mxOutcome) {
	mxs, err := r.LookupMX(ctx, domain)
	if err != nil {
		// NXDOMAIN / "no such host" is DEFINITIVE non-existence → the domain cannot receive
		// mail. Everything else (timeout, SERVFAIL, temporary) is NOT proof → unknown.
		var dnsErr *net.DNSError
		if ok := asDNSNotFound(err, &dnsErr); ok {
			return nil, mxNone
		}
		return nil, mxError
	}
	// RFC 7505 null-MX: exactly one MX whose target is root "." (or empty) → explicit no-mail.
	if len(mxs) == 1 && isNullMXTarget(mxs[0].Host) {
		return nil, mxNullMX
	}
	// Filter any stray null sentinels; keep real targets.
	real := mxs[:0]
	for _, m := range mxs {
		if !isNullMXTarget(m.Host) {
			real = append(real, m)
		}
	}
	if len(real) > 0 {
		return real, mxFound
	}
	// No MX records: fall back to A/AAAA (implicit MX, RFC 5321 §5.1).
	ips, ipErr := r.LookupIP(ctx, domain)
	if ipErr != nil {
		var dnsErr *net.DNSError
		if ok := asDNSNotFound(ipErr, &dnsErr); ok {
			return nil, mxNone
		}
		return nil, mxError
	}
	if len(ips) > 0 {
		// Implicit MX = the domain itself.
		return []MXHost{{Host: domain, Pref: 0}}, mxFound
	}
	return nil, mxNone
}

func isNullMXTarget(host string) bool {
	return host == "" || host == "."
}
