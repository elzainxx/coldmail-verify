package layers

// Real-protocol tests for the production resolver (ADR-0019). A small authoritative DNS server runs IN-PROCESS on
// loopback (golang.org/x/net/dns/dnsmessage, already a dependency), and the PRODUCTION NetResolver is pointed at it
// with NewNetResolverAt — so what runs is Go's real DNS client over the real DNS protocol, through the real layers.
// Nothing here skips and nothing needs Docker, so `go test ./...` executes it everywhere; the search-domain CI
// fixture (scripts/check-dns-search-domain.sh) runs this same compiled test binary inside a container whose
// resolv.conf carries a `search` line, which is what makes the search-list hazard bite.
//
// THE PROPERTY (the module's founding one, restated for DNS): an outage of OUR resolution is `unknown`, never
// `invalid`. Two assertions per case, independently: the VERDICT (SERVFAIL → Unknown, never Fail) and the WIRE
// (the only name ever asked for is the rooted one — a search-suffixed query is the defect itself).

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type fixtureDNS struct {
	pc    net.PacketConn
	mu    sync.Mutex
	asked []string // "TYPE name" in arrival order
}

func startFixtureDNS(t *testing.T) *fixtureDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fixture DNS: cannot listen on loopback: %v", err)
	}
	f := &fixtureDNS{pc: pc}
	t.Cleanup(func() { _ = pc.Close() })
	go f.serve()
	return f
}

func (f *fixtureDNS) addr() string { return f.pc.LocalAddr().String() }

func (f *fixtureDNS) reset() {
	f.mu.Lock()
	f.asked = nil
	f.mu.Unlock()
}

func (f *fixtureDNS) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func typeName(t dnsmessage.Type) string {
	switch t {
	case dnsmessage.TypeA:
		return "A"
	case dnsmessage.TypeAAAA:
		return "AAAA"
	case dnsmessage.TypeMX:
		return "MX"
	case dnsmessage.TypeTXT:
		return "TXT"
	}
	return t.String()
}

func (f *fixtureDNS) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.asked = append(f.asked, typeName(q.Type)+" "+strings.ToLower(q.Name.String()))
		f.mu.Unlock()
		if msg, ok := fixtureReply(h.ID, q); ok {
			_, _ = f.pc.WriteTo(msg, from)
		} // else: deliberately NO reply — a server that never answers
	}
}

// fixtureReply is the zone. Anything not listed is NXDOMAIN — including every search-suffixed name, which is what
// makes the collapse reproducible: servfail.test. SERVFAILs, servfail.test.search-domain.test. is NXDOMAIN.
func fixtureReply(id uint16, q dnsmessage.Question) ([]byte, bool) {
	name := strings.ToLower(q.Name.String())
	rr := func(typ dnsmessage.Type, body dnsmessage.ResourceBody) dnsmessage.Resource {
		return dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: typ, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   body,
		}
	}
	build := func(rcode dnsmessage.RCode, answers ...dnsmessage.Resource) ([]byte, bool) {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, Authoritative: true, RecursionAvailable: true, RCode: rcode})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		for _, a := range answers {
			switch body := a.Body.(type) {
			case *dnsmessage.MXResource:
				_ = b.MXResource(a.Header, *body)
			case *dnsmessage.AResource:
				_ = b.AResource(a.Header, *body)
			}
		}
		msg, err := b.Finish()
		return msg, err == nil
	}
	mx := func(pref uint16, host string) dnsmessage.Resource {
		return rr(dnsmessage.TypeMX, &dnsmessage.MXResource{Pref: pref, MX: dnsmessage.MustNewName(host)})
	}
	a := func(ip [4]byte) dnsmessage.Resource { return rr(dnsmessage.TypeA, &dnsmessage.AResource{A: ip}) }

	switch name {
	case "blackhole.test.":
		return nil, false // never answers
	case "servfail.test.":
		return build(dnsmessage.RCodeServerFailure)
	case "mxservfail.test.": // the MX query fails; the name itself has an A record
		switch q.Type {
		case dnsmessage.TypeMX:
			return build(dnsmessage.RCodeServerFailure)
		case dnsmessage.TypeA:
			return build(dnsmessage.RCodeSuccess, a([4]byte{192, 0, 2, 30}))
		}
		return build(dnsmessage.RCodeSuccess)
	case "hasmx.test.":
		if q.Type == dnsmessage.TypeMX {
			return build(dnsmessage.RCodeSuccess, mx(10, "mx.hasmx.test."))
		}
		return build(dnsmessage.RCodeSuccess) // NODATA for A/AAAA
	case "mx.hasmx.test.":
		if q.Type == dnsmessage.TypeA {
			return build(dnsmessage.RCodeSuccess, a([4]byte{192, 0, 2, 10}))
		}
		return build(dnsmessage.RCodeSuccess)
	case "nullmx.test.": // RFC 7505
		if q.Type == dnsmessage.TypeMX {
			return build(dnsmessage.RCodeSuccess, mx(0, "."))
		}
		return build(dnsmessage.RCodeSuccess)
	}
	return build(dnsmessage.RCodeNameError) // NXDOMAIN
}

func evidenceFor(domain string, r Resolver) *Evidence {
	return NewEvidence("x@"+domain, "x@"+domain, "x", domain, r, nil)
}

// The headline, through the PRODUCTION layers. Each row asserts the verdict-bearing layer result AND the wire.
func TestMXLayer_RealDNS_FailureIsUnknownNeverInvalid_AndOnlyTheRootedNameIsAsked(t *testing.T) {
	srv := startFixtureDNS(t)
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
		{"a server that never answers (timeout) is unknown", "blackhole.test", Unknown, mxError, 1200 * time.Millisecond},
	} {
		srv.reset()
		ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
		ev := evidenceFor(tc.domain, NewNetResolverAt(srv.addr()))
		out := MXLayer{}.Run(ctx, ev)
		_, outcome := ev.mx(ctx)
		cancel()

		if out.Result != tc.want || outcome != tc.wantOutcome {
			t.Errorf("%s: MXLayer result=%q outcome=%d, want result=%q outcome=%d — a DNS failure of ours must never be stated as a fact about the address",
				tc.name, out.Result, outcome, tc.want, tc.wantOutcome)
		}
		if len(srv.queries()) == 0 {
			t.Errorf("%s: the server saw no query at all — the resolver is not reaching the fixture, so this case proves nothing", tc.name)
		}
		for _, q := range srv.queries() {
			if f := strings.Fields(q); len(f) != 2 || f[1] != tc.domain+"." {
				t.Errorf("%s: the resolver asked %q; only the rooted name %q may ever be asked (a search-suffixed query is the defect)", tc.name, q, tc.domain+".")
			}
		}
	}
}

// Every name reaching the DNS client is rooted — the unit-level pin, no network and no resolv.conf involved.
type recordingLookuper struct{ names []string }

func (r *recordingLookuper) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.names = append(r.names, "MX "+name)
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (r *recordingLookuper) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.names = append(r.names, "IP "+host)
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestNetResolver_RootsEveryName(t *testing.T) {
	rec := &recordingLookuper{}
	n := &NetResolver{r: rec}
	_, _ = n.LookupMX(context.Background(), "example.com")
	_, _ = n.LookupIP(context.Background(), "example.com")
	_, _ = n.LookupMX(context.Background(), "already.rooted.")
	want := []string{"MX example.com.", "IP example.com.", "MX already.rooted."}
	if strings.Join(rec.names, "|") != strings.Join(want, "|") {
		t.Fatalf("names reaching the DNS client = %v, want %v (an unrooted name regains the search-list collapse)", rec.names, want)
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
	srv := startFixtureDNS(t)
	raw := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, srv.addr())
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
