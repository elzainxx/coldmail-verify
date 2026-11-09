package layers

// Real-protocol tests for the production resolver (ADR-0019, ADR-0020). A small authoritative DNS server runs IN-PROCESS on
// loopback (internal/dnsfixture, golang.org/x/net/dns/dnsmessage), and the PRODUCTION NetResolver is pointed at it with
// NewNetResolverAt — so what runs is Go's real DNS client over the real DNS protocol, through the real layers. Nothing
// here skips and nothing needs Docker, so `go test ./...` executes it everywhere; the search-domain CI fixture
// (scripts/check-dns-search-domain.sh) runs this same compiled test binary inside a container whose resolv.conf carries a
// `search` line, which is what makes the search-list hazard bite.
//
// THE PROPERTY (the module's founding one, restated for DNS): an outage of OUR resolution is `unknown`, never `invalid`;
// and `invalid` for a missing route is stated only when non-existence is PROVABLE — no MX, no A, no AAAA, all three
// definitively absent. Two assertions per case, independently: the VERDICT and the WIRE (the only name ever asked for is
// the rooted one — a search-suffixed query is the defect itself — and the follow-up address queries happen only when
// they should).

import (
	"context"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/dnsfixture"
)

func evidenceFor(domain string, r Resolver) *Evidence {
	return NewEvidence("x@"+domain, "x@"+domain, "x", domain, r, nil)
}

// queryTypes returns the sorted record types the server was asked about, failing if any query was for a name other than the
// rooted domain (a search-suffixed name is the ADR-0019 defect) — the wire half of every case below.
func queryTypes(t *testing.T, srv *dnsfixture.Server, caseName, domain string) []string {
	t.Helper()
	var types []string
	for _, q := range srv.Queries() {
		f := strings.Fields(q)
		if len(f) != 2 || f[1] != domain+"." {
			t.Errorf("%s: the resolver asked %q; only the rooted name %q may ever be asked (a search-suffixed query is the defect)", caseName, q, domain+".")
			continue
		}
		types = append(types, f[0])
	}
	// A SET of record types, not a count: Go's client retries a SERVFAIL (attempts=2), which is its business, not ours.
	sort.Strings(types)
	uniq := types[:0]
	for i, ty := range types {
		if i == 0 || ty != types[i-1] {
			uniq = append(uniq, ty)
		}
	}
	return uniq
}

// The headline, through the PRODUCTION layers. Each row asserts the verdict-bearing layer result AND the wire.
func TestMXLayer_RealDNS_FailureIsUnknownNeverInvalid_AndOnlyTheRootedNameIsAsked(t *testing.T) {
	srv := dnsfixture.Start(t)
	for _, tc := range []struct {
		name        string
		domain      string
		want        Result
		wantOutcome mxOutcome
		budget      time.Duration
	}{
		{"a published MX passes", "hasmx.test", Pass, mxFound, 3 * time.Second},
		// Control: the strictness must not have weakened the genuine invalid. A real NXDOMAIN is still proof.
		{"a genuine NXDOMAIN is still proof of no mail route (fail)", "no-such-domain-xyz.test", Fail, mxNone, 3 * time.Second},
		// The defect: with a search domain this became NXDOMAIN → Fail → invalid/no_mx for a valid address.
		{"SERVFAIL is unknown, never invalid", "servfail.test", Unknown, mxError, 3 * time.Second},
		{"an MX query that SERVFAILs is unknown even though the name has an A record", "mxservfail.test", Unknown, mxError, 3 * time.Second},
		// The row the search-domain gate's mutant must turn into `Fail`: only here does a search-list collapse still reach the verdict.
		{"an MX query that SERVFAILs on a name with no address is unknown", "mxservfail-noaddr.test", Unknown, mxError, 3 * time.Second},
		{"a server that never answers (timeout) is unknown", "blackhole.test", Unknown, mxError, 1200 * time.Millisecond},
	} {
		srv.Reset()
		ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
		ev := evidenceFor(tc.domain, NewNetResolverAt(srv.Addr()))
		out := MXLayer{}.Run(ctx, ev)
		_, outcome := ev.mx(ctx)
		cancel()

		if out.Result != tc.want || outcome != tc.wantOutcome {
			t.Errorf("%s: MXLayer result=%q outcome=%d, want result=%q outcome=%d — a DNS failure of ours must never be stated as a fact about the address",
				tc.name, out.Result, outcome, tc.want, tc.wantOutcome)
		}
		if len(srv.Queries()) == 0 {
			t.Errorf("%s: the server saw no query at all — the resolver is not reaching the fixture, so this case proves nothing", tc.name)
		}
		queryTypes(t, srv, tc.name, tc.domain)
	}
}

// ADR-0020 — RFC 5321 §5.1 implicit MX, against REAL DNS shapes. A domain with no MX but an A/AAAA record receives its
// mail at that host; calling it `invalid / no_mx` breaks the founding rule (invalid only when non-existence is provable).
// The pairs: A-only → NOT invalid; truly no records → invalid; null MX → invalid (even with an A record). Plus every
// partial failure of the follow-up, which must be `unknown`.
func TestMXLayer_RealDNS_ImplicitMX(t *testing.T) {
	srv := dnsfixture.Start(t)
	host := func(d string) []MXHost { return []MXHost{{Host: d, Pref: 0}} }
	const reps = 40 // the partial-failure cases raced two goroutines; repeat so a regression to a combined lookup cannot hide
	for _, tc := range []struct {
		name        string
		domain      string
		want        Result
		wantOutcome mxOutcome
		wantHosts   []MXHost
		wantTypes   []string // sorted record types the server must have been asked, for the rooted name only
		budget      time.Duration
		reps        int
	}{
		// ── the ruling: an address and no MX is a mail host ──
		{"A-only: NODATA for MX, an A record → implicit MX, passes", "aonly.test", Pass, mxFound, host("aonly.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"AAAA-only: passes", "aaaaonly.test", Pass, mxFound, host("aaaaonly.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"dual-stack, no MX: passes", "dualstack.test", Pass, mxFound, host("dualstack.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"an alias (CNAME chain to an address) is followed: passes", "aliased.test", Pass, mxFound, host("aliased.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"a published MX needs no follow-up: nothing but the MX is asked", "hasmx.test", Pass, mxFound, []MXHost{{Host: "mx.hasmx.test", Pref: 10}}, []string{"MX"}, 3 * time.Second, 1},

		// ── the controls that keep the rule honest: it must not become "everything passes" ──
		{"CONTROL truly no records: the name exists (TXT only), no MX, no A, no AAAA → invalid", "norecords.test", Fail, mxNone, nil, []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"CONTROL NXDOMAIN → invalid", "no-such-domain-xyz.test", Fail, mxNone, nil, []string{"A", "AAAA", "MX"}, 3 * time.Second, 1},
		{"CONTROL null MX (RFC 7505) → invalid, and the A record is never even asked for", "nullmx.test", Fail, mxNullMX, nil, []string{"MX"}, 3 * time.Second, 1},
		{"CONTROL null MX that ALSO has an A record is still null MX — the A record must not rescue it", "nullmx-with-a.test", Fail, mxNullMX, nil, []string{"MX"}, 3 * time.Second, 1},

		// ── a failure of ours at any step is unknown, never invalid ──
		{"MX SERVFAIL (the name has an A record) → unknown; the A record is not consulted", "mxservfail.test", Unknown, mxError, nil, []string{"MX"}, 3 * time.Second, 1},
		{"A = NODATA, AAAA = SERVFAIL → unknown (Go's combined lookup answered 'not found' or 'SERVFAIL' at random)", "aaaa-servfail.test", Unknown, mxError, nil, []string{"A", "AAAA", "MX"}, 3 * time.Second, reps},
		{"A = SERVFAIL, AAAA = NODATA → unknown", "a-servfail.test", Unknown, mxError, nil, []string{"A", "AAAA", "MX"}, 3 * time.Second, reps},
		{"A never answers (timeout), AAAA = NODATA → unknown", "a-timeout.test", Unknown, mxError, nil, []string{"A", "AAAA", "MX"}, 1200 * time.Millisecond, 1},

		// ── an address that exists outweighs the other family's failure: the host is real ──
		{"A present, AAAA = SERVFAIL → passes", "a-ok-aaaa-servfail.test", Pass, mxFound, host("a-ok-aaaa-servfail.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, reps},
		{"AAAA present, A = SERVFAIL → passes", "aaaa-ok-a-servfail.test", Pass, mxFound, host("aaaa-ok-a-servfail.test"), []string{"A", "AAAA", "MX"}, 3 * time.Second, reps},
	} {
		for i := 0; i < tc.reps; i++ {
			srv.Reset()
			ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
			ev := evidenceFor(tc.domain, NewNetResolverAt(srv.Addr()))
			out := MXLayer{}.Run(ctx, ev)
			hosts, outcome := ev.mx(ctx)
			cancel()

			if out.Result != tc.want || outcome != tc.wantOutcome {
				t.Errorf("%s [run %d]: MXLayer result=%q outcome=%d, want result=%q outcome=%d", tc.name, i, out.Result, outcome, tc.want, tc.wantOutcome)
			}
			if out.Result == Fail && tc.want != Fail {
				t.Errorf("%s [run %d]: reported Fail (invalid) for a domain that is not provably without a mail route", tc.name, i)
			}
			if len(hosts) != len(tc.wantHosts) || (len(hosts) > 0 && hosts[0] != tc.wantHosts[0]) {
				t.Errorf("%s [run %d]: mail hosts = %v, want %v", tc.name, i, hosts, tc.wantHosts)
			}
			if got := queryTypes(t, srv, tc.name, tc.domain); strings.Join(got, ",") != strings.Join(tc.wantTypes, ",") {
				t.Errorf("%s [run %d]: record types asked = %v, want %v", tc.name, i, got, tc.wantTypes)
			}
		}
	}
}

// The CONTROL that makes the test above mean something. legacyResolveMX is a frozen copy of resolveMX from before ADR-0020:
// a not-found MX returned mxNone on the spot, so the A/AAAA fallback below it ran only when LookupMX returned an EMPTY LIST
// with no error — a shape the real Go resolver never produces. Run against the real protocol it must (1) call the A-only
// domains invalid, and (2) never ask for an address at all. If it ever does not, the fixture has stopped reproducing the
// defect and the green above would be vacuous. (scripts/check-implicit-mx.sh --mutant is the same control on the REAL code.)
func legacyResolveMX(ctx context.Context, r Resolver, domain string) ([]MXHost, mxOutcome) {
	mxs, err := r.LookupMX(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if ok := asDNSNotFound(err, &dnsErr); ok {
			return nil, mxNone
		}
		return nil, mxError
	}
	if len(mxs) == 1 && isNullMXTarget(mxs[0].Host) {
		return nil, mxNullMX
	}
	real := mxs[:0]
	for _, m := range mxs {
		if !isNullMXTarget(m.Host) {
			real = append(real, m)
		}
	}
	if len(real) > 0 {
		return real, mxFound
	}
	ips, ipErr := r.LookupIP(ctx, domain)
	if ipErr != nil {
		var dnsErr *net.DNSError
		if ok := asDNSNotFound(ipErr, &dnsErr); ok {
			return nil, mxNone
		}
		return nil, mxError
	}
	if len(ips) > 0 {
		return []MXHost{{Host: domain, Pref: 0}}, mxFound
	}
	return nil, mxNone
}

func TestMXLayer_RealDNS_ImplicitMX_LegacyLogicFailsTheFixture(t *testing.T) {
	srv := dnsfixture.Start(t)
	for _, d := range []string{"aonly.test", "aaaaonly.test", "dualstack.test", "aliased.test"} {
		srv.Reset()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, outcome := legacyResolveMX(ctx, NewNetResolverAt(srv.Addr()), d)
		cancel()
		if outcome != mxNone {
			t.Errorf("legacy logic on %s = outcome %d, want mxNone (invalid/no_mx): the fixture no longer reproduces the defect, so TestMXLayer_RealDNS_ImplicitMX proves nothing", d, outcome)
		}
		if got := queryTypes(t, srv, "legacy "+d, d); strings.Join(got, ",") != "MX" {
			t.Errorf("legacy logic on %s asked %v; it should have stopped at the MX query — its A/AAAA fallback was unreachable against real DNS", d, got)
		}
	}
}

// ── NetResolver.LookupIP: the A/AAAA combination rule, exhaustively, with no network ───────────────────────────────────────

type stubLookuper struct {
	v4, v6 func() ([]net.IP, error)
}

func (s *stubLookuper) LookupMX(context.Context, string) ([]*net.MX, error) {
	return nil, notFoundErr()
}
func (s *stubLookuper) LookupIP(_ context.Context, network, host string) ([]net.IP, error) {
	switch network {
	case "ip4":
		return s.v4()
	case "ip6":
		return s.v6()
	}
	return nil, &net.DNSError{Err: "unexpected network " + network, IsTemporary: true}
}

func TestNetResolver_LookupIP_CombinesTheTwoFamiliesExplicitly(t *testing.T) {
	addr4 := func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.0.2.1")}, nil }
	addr6 := func() ([]net.IP, error) { return []net.IP{net.ParseIP("2001:db8::1")}, nil }
	nf := func() ([]net.IP, error) { return nil, notFoundErr() }
	sf := func() ([]net.IP, error) { return nil, servfailErr() }
	to := func() ([]net.IP, error) { return nil, timeoutErr() }
	empty := func() ([]net.IP, error) { return nil, nil }

	type outcome int
	const (
		gotAddrs outcome = iota
		gotNotFound
		gotFailure // an error that is NOT a definitive not-found → the caller keeps it as unknown
		gotNothing // answered, nothing there, no error
	)
	for _, tc := range []struct {
		name   string
		v4, v6 func() ([]net.IP, error)
		want   outcome
	}{
		{"A and AAAA", addr4, addr6, gotAddrs},
		{"A only (AAAA not found)", addr4, nf, gotAddrs},
		{"AAAA only (A not found)", nf, addr6, gotAddrs},
		{"an address outweighs the other family's SERVFAIL (v4)", addr4, sf, gotAddrs},
		{"an address outweighs the other family's SERVFAIL (v6)", sf, addr6, gotAddrs},
		{"an address outweighs the other family's timeout", to, addr6, gotAddrs},
		{"both definitively absent → not found (the only route to `invalid`)", nf, nf, gotNotFound},
		{"A not found, AAAA SERVFAIL → failure, NOT not-found", nf, sf, gotFailure},
		{"A SERVFAIL, AAAA not found → failure, NOT not-found", sf, nf, gotFailure},
		{"A not found, AAAA timeout → failure", nf, to, gotFailure},
		{"both SERVFAIL → failure", sf, sf, gotFailure},
		{"A answered-empty, AAAA SERVFAIL → failure (cannot prove AAAA is absent)", empty, sf, gotFailure},
		{"both answered-empty → nothing, no error", empty, empty, gotNothing},
	} {
		n := &NetResolver{r: &stubLookuper{v4: tc.v4, v6: tc.v6}}
		for i := 0; i < 20; i++ { // the two lookups run concurrently; the result must not depend on who finishes first
			ips, err := n.LookupIP(context.Background(), "example.test")
			var de *net.DNSError
			var got outcome
			switch {
			case len(ips) > 0 && err == nil:
				got = gotAddrs
			case err == nil:
				got = gotNothing
			case asDNSNotFound(err, &de):
				got = gotNotFound
			default:
				got = gotFailure
			}
			if got != tc.want {
				t.Fatalf("%s [run %d]: got outcome %d (ips=%v err=%v), want %d", tc.name, i, got, ips, err, tc.want)
			}
		}
	}
}

// Every name reaching the DNS client is rooted — the unit-level pin, no network and no resolv.conf involved — and the
// address lookup is always TWO lookups ("ip4" then "ip6"), never the combined "ip" that races its own errors.
type recordingLookuper struct {
	names chan string
}

func (r *recordingLookuper) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.names <- "MX " + name
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (r *recordingLookuper) LookupIP(_ context.Context, network, host string) ([]net.IP, error) {
	r.names <- strings.ToUpper(network) + " " + host
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestNetResolver_RootsEveryName(t *testing.T) {
	rec := &recordingLookuper{names: make(chan string, 16)}
	n := &NetResolver{r: rec}
	_, _ = n.LookupMX(context.Background(), "example.com")
	_, _ = n.LookupIP(context.Background(), "example.com")
	_, _ = n.LookupMX(context.Background(), "already.rooted.")
	close(rec.names)
	var got []string
	for s := range rec.names {
		got = append(got, s)
	}
	sort.Strings(got)
	want := []string{"IP4 example.com.", "IP6 example.com.", "MX already.rooted.", "MX example.com."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("names reaching the DNS client = %v, want %v (an unrooted name regains the search-list collapse; a combined \"ip\" lookup regains the A/AAAA error race)", got, want)
	}
}

func TestFQDN(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"example.com", "example.com."}, {"example.com.", "example.com."}, {"xn--mnchen-3ya.de", "xn--mnchen-3ya.de."}, {"", ""},
	} {
		if got := fqdn(tc.in); got != tc.want {
			t.Errorf("fqdn(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := fqdn(fqdn(tc.in)); got != tc.want {
			t.Errorf("fqdn not idempotent for %q: %q", tc.in, got)
		}
	}
}

// The strict door. OLD-BUG rows are the ones the previous `return dnsErr.IsNotFound` got wrong.
func TestAsDNSNotFound_IsTheNarrowDoor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"NXDOMAIN/NODATA (IsNotFound)", &net.DNSError{Err: "no such host", IsNotFound: true}, true},
		{"SERVFAIL (IsTemporary)", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, false},
		{"timeout (IsTimeout)", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, false},
		{"OLD-BUG: not-found AND temporary", &net.DNSError{Err: "x", IsNotFound: true, IsTemporary: true}, false},
		{"OLD-BUG: not-found AND timeout", &net.DNSError{Err: "x", IsNotFound: true, IsTimeout: true}, false},
		{"unclassified DNSError", &net.DNSError{Err: "malformed"}, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"nil", nil, false},
	} {
		var de *net.DNSError
		if got := asDNSNotFound(tc.err, &de); got != tc.want {
			t.Errorf("%s: asDNSNotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func resolvConfSearchList() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && (f[0] == "search" || f[0] == "domain") {
			out = append(out, f[1:]...)
		}
	}
	return out
}

// TestSearchDomainCanary is what makes the search-domain CI fixture a CONTROL: where /etc/resolv.conf has a search
// list, Go's UNROOTED lookup of servfail.test really does collapse SERVFAIL into not-found, and the rooted one does
// not. It never skips; where there is no search list it asserts only the rooted half and says so. If the fixture
// claims a search domain it does not have (LAYERS_EXPECT_SEARCH_DOMAIN=1), or the hazard does not reproduce under
// it, that is a failure — a green under a fixture that exercises nothing would be the vacuous kind.
func TestSearchDomainCanary(t *testing.T) {
	srv := dnsfixture.Start(t)
	raw := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, srv.Addr())
	}} // deliberately NOT NetResolver: this is Go's resolver as it behaves without the fix
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	var de *net.DNSError
	_, rooted := raw.LookupMX(ctx, "servfail.test.")
	if asDNSNotFound(rooted, &de) {
		t.Fatalf("a ROOTED lookup of servfail.test. came back as not-found (%v); a SERVFAIL must never read as non-existence", rooted)
	}
	search := resolvConfSearchList()
	if os.Getenv("LAYERS_EXPECT_SEARCH_DOMAIN") == "1" && len(search) == 0 {
		t.Fatalf("LAYERS_EXPECT_SEARCH_DOMAIN=1 but /etc/resolv.conf has no search/domain line: the fixture did not inject its search domain, so a green here would prove nothing")
	}
	if len(search) == 0 {
		t.Logf("canary: no search list in /etc/resolv.conf — the collapse is not exercised in THIS run (the CI search-domain fixture exercises it)")
		return
	}
	_, unrooted := raw.LookupMX(ctx, "servfail.test")
	if !asDNSNotFound(unrooted, &de) {
		t.Fatalf("search list %v is present but an UNROOTED lookup of servfail.test did not collapse into not-found (got %v): the fixture is not reproducing the hazard", search, unrooted)
	}
	t.Logf("canary: search list %v is live — unrooted servfail.test collapsed SERVFAIL into not-found (the bug); the rooted lookup stayed SERVFAIL (the fix)", search)
}
