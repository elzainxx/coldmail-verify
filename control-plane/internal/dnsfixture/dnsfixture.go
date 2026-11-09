// Package dnsfixture is a small AUTHORITATIVE DNS server that runs in-process on loopback, for tests only.
//
// WHY IT EXISTS (ADR-0019, ADR-0020). Module 1's DNS handling was twice wrong in ways no fixture could show, because the
// fixtures were fakes: a `Resolver` that returned whatever the test told it to. A fake cannot reproduce what a real
// resolver does — a search-list walk that overwrites a SERVFAIL, a NODATA that Go reports exactly like NXDOMAIN, an A/AAAA
// lookup that keeps whichever error arrived last. So the production `layers.NetResolver` is pointed at THIS server
// (`layers.NewNetResolverAt(srv.Addr())`) and what runs is Go's real DNS client over the real DNS protocol.
//
// Nothing here skips and nothing needs Docker or a network, so `go test ./...` executes it everywhere. Import it from
// _test.go files only: TestNotImportedByProductionCode fails the build of the test suite if a non-test file ever does.
//
// THE ZONE. Every name is under .test. Anything not listed is NXDOMAIN, including every search-suffixed name — that is
// what makes the search-list collapse reproducible. A name that IS listed answers NODATA (NOERROR, empty answer, SOA in the
// authority section — the shape a real authoritative server sends) for every record type it does not carry. This table is
// the only place the shapes are defined; the tests name them.
package dnsfixture

import (
	"net"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

// TB is the subset of testing.TB the server needs, so this package does not import "testing" into non-test code.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Server is the running fixture. Addr is "127.0.0.1:port" (UDP). Queries records "TYPE name" in arrival order.
type Server struct {
	pc    net.PacketConn
	mu    sync.Mutex
	asked []string
}

// Start listens on a loopback UDP port and serves the zone until the test ends.
func Start(t TB) *Server {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dnsfixture: cannot listen on loopback: %v", err)
		return nil
	}
	s := &Server{pc: pc}
	t.Cleanup(func() { _ = pc.Close() })
	go s.serve()
	return s
}

// Addr is the address to hand to layers.NewNetResolverAt.
func (s *Server) Addr() string { return s.pc.LocalAddr().String() }

// Reset forgets the recorded queries.
func (s *Server) Reset() {
	s.mu.Lock()
	s.asked = nil
	s.mu.Unlock()
}

// Queries returns every query received since Start/Reset as "TYPE name." (lower-cased, rooted), in arrival order.
func (s *Server) Queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
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

func (s *Server) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.pc.ReadFrom(buf)
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
		s.mu.Lock()
		s.asked = append(s.asked, typeName(q.Type)+" "+strings.ToLower(q.Name.String()))
		s.mu.Unlock()
		if msg, ok := reply(h.ID, q); ok {
			_, _ = s.pc.WriteTo(msg, from)
		} // else: deliberately NO reply — a server that never answers
	}
}

// ── the zone ───────────────────────────────────────────────────────────────────────────────────────────────────────────

// answer is what the zone says for one (name, type). Zero value = NODATA is NOT implied by it: a type absent from a
// name's map is NODATA, so an answer is always something positive, a failure, or silence.
type answer struct {
	rcode   dnsmessage.RCode // RCodeSuccess unless a failure
	silent  bool             // never reply (a timeout)
	records []rec
}

type rec struct {
	owner string // "" = the query name
	body  dnsmessage.ResourceBody
	typ   dnsmessage.Type
}

func recA(ip [4]byte) rec { return rec{typ: dnsmessage.TypeA, body: &dnsmessage.AResource{A: ip}} }
func recAAAA(ip [16]byte) rec {
	return rec{typ: dnsmessage.TypeAAAA, body: &dnsmessage.AAAAResource{AAAA: ip}}
}
func recAAt(owner string, ip [4]byte) rec {
	return rec{owner: owner, typ: dnsmessage.TypeA, body: &dnsmessage.AResource{A: ip}}
}
func recMX(pref uint16, host string) rec {
	return rec{typ: dnsmessage.TypeMX, body: &dnsmessage.MXResource{Pref: pref, MX: dnsmessage.MustNewName(host)}}
}
func recCNAME(target string) rec {
	return rec{typ: dnsmessage.TypeCNAME, body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)}}
}
func recTXT(s string) rec {
	return rec{typ: dnsmessage.TypeTXT, body: &dnsmessage.TXTResource{TXT: []string{s}}}
}

func rrs(r ...rec) answer            { return answer{records: r} }
func fail(c dnsmessage.RCode) answer { return answer{rcode: c} }

var (
	servfail = fail(dnsmessage.RCodeServerFailure)
	silent   = answer{silent: true}

	v6 = [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 0x50} // 2001:db8::50
)

// zone maps a lower-cased, rooted name to its record sets. A type not listed for a listed name is NODATA.
// `all` (when set) answers every type, ahead of the per-type map.
type entry struct {
	all   *answer
	types map[dnsmessage.Type]answer
}

var zone = map[string]entry{
	// ── failures of the resolution itself ──
	"blackhole.test.": {all: &silent},   // never answers
	"servfail.test.":  {all: &servfail}, // SERVFAIL for every type

	// ── an MX is published ──
	"hasmx.test.":    {types: map[dnsmessage.Type]answer{dnsmessage.TypeMX: rrs(recMX(10, "mx.hasmx.test."))}},
	"mx.hasmx.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 10}))}},

	// ── RFC 7505 null MX: "this domain accepts no mail". The second one ALSO has an A record, as real null-MX domains
	// often do (the apex serves a website) — the A record must not rescue it.
	"nullmx.test.":        {types: map[dnsmessage.Type]answer{dnsmessage.TypeMX: rrs(recMX(0, "."))}},
	"nullmx-with-a.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeMX: rrs(recMX(0, ".")), dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 60}))}},

	// ── RFC 5321 §5.1 implicit MX: no MX (NODATA), an address instead ──
	"aonly.test.":     {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 50}))}},
	"aaaaonly.test.":  {types: map[dnsmessage.Type]answer{dnsmessage.TypeAAAA: rrs(recAAAA(v6))}},
	"dualstack.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 51})), dnsmessage.TypeAAAA: rrs(recAAAA(v6))}},
	// An alias: the A answer is a CNAME chain ending in an address (the A lookup must follow it).
	"aliased.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: rrs(recCNAME("aonly.test."), recAAt("aonly.test.", [4]byte{192, 0, 2, 50}))}},

	// ── the domain EXISTS and carries no mail route at all: no MX, no A, no AAAA (only a TXT) ──
	"norecords.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeTXT: rrs(recTXT("v=spf1 -all"))}},

	// ── the MX query fails and the name has NO address records (A and AAAA are plain NODATA). This is the shape on which a
	// search-list collapse still reaches the VERDICT: unrooted, the SERVFAIL for the real name is overwritten by the suffixed
	// name's NXDOMAIN, the MX answer reads "not found", and the address follow-up then finds nothing → `invalid`. Rooted, it
	// stays SERVFAIL → unknown. (With an address — mxservfail.test. — the follow-up masks the collapse; this one does not.)
	"mxservfail-noaddr.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeMX: servfail}},

	// ── the MX query fails but the name has an address: still unknown (an outage of ours is not a fact about the address) ──
	"mxservfail.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeMX: servfail, dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 30}))}},

	// ── PARTIAL failures of the A/AAAA follow-up (MX is NODATA in all of them). Go's combined A+AAAA lookup keeps whichever
	// error arrives LAST — so these are the shapes that, asked as one lookup, answer "not found" or "SERVFAIL" at random.
	"aaaa-servfail.test.":      {types: map[dnsmessage.Type]answer{dnsmessage.TypeAAAA: servfail}},                                                      // A = NODATA, AAAA = SERVFAIL
	"a-servfail.test.":         {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: servfail}},                                                         // A = SERVFAIL, AAAA = NODATA
	"a-ok-aaaa-servfail.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: rrs(recA([4]byte{192, 0, 2, 52})), dnsmessage.TypeAAAA: servfail}}, // an address exists
	"aaaa-ok-a-servfail.test.": {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: servfail, dnsmessage.TypeAAAA: rrs(recAAAA(v6))}},                  // an address exists
	"a-timeout.test.":          {types: map[dnsmessage.Type]answer{dnsmessage.TypeA: silent}},                                                           // A never answers
}

// reply builds the response for q, or ok=false for "send nothing".
func reply(id uint16, q dnsmessage.Question) ([]byte, bool) {
	name := strings.ToLower(q.Name.String())
	e, listed := zone[name]
	if !listed {
		return build(id, q, dnsmessage.RCodeNameError, nil) // NXDOMAIN, with the SOA a real server adds
	}
	a := answerFor(e, q.Type)
	if a.silent {
		return nil, false
	}
	return build(id, q, a.rcode, a.records)
}

func answerFor(e entry, t dnsmessage.Type) answer {
	if e.all != nil {
		return *e.all
	}
	if a, ok := e.types[t]; ok {
		return a
	}
	return answer{} // NODATA: NOERROR, no records (the SOA goes into the authority section)
}

func build(id uint16, q dnsmessage.Question, rcode dnsmessage.RCode, recs []rec) ([]byte, bool) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, Authoritative: true, RecursionAvailable: true, RCode: rcode})
	if err := b.StartQuestions(); err != nil {
		return nil, false
	}
	if err := b.Question(q); err != nil {
		return nil, false
	}
	if err := b.StartAnswers(); err != nil {
		return nil, false
	}
	for _, r := range recs {
		owner := q.Name
		if r.owner != "" {
			owner = dnsmessage.MustNewName(r.owner)
		}
		h := dnsmessage.ResourceHeader{Name: owner, Type: r.typ, Class: dnsmessage.ClassINET, TTL: 60}
		var err error
		switch body := r.body.(type) {
		case *dnsmessage.AResource:
			err = b.AResource(h, *body)
		case *dnsmessage.AAAAResource:
			err = b.AAAAResource(h, *body)
		case *dnsmessage.MXResource:
			err = b.MXResource(h, *body)
		case *dnsmessage.CNAMEResource:
			err = b.CNAMEResource(h, *body)
		case *dnsmessage.TXTResource:
			err = b.TXTResource(h, *body)
		}
		if err != nil {
			return nil, false
		}
	}
	// A negative answer (NXDOMAIN or NODATA) carries the zone's SOA in the authority section, as a real server's does.
	if len(recs) == 0 && (rcode == dnsmessage.RCodeSuccess || rcode == dnsmessage.RCodeNameError) {
		if err := b.StartAuthorities(); err != nil {
			return nil, false
		}
		soa := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("test."), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: 60}
		if err := b.SOAResource(soa, dnsmessage.SOAResource{
			NS: dnsmessage.MustNewName("ns.test."), MBox: dnsmessage.MustNewName("hostmaster.test."),
			Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, MinTTL: 60,
		}); err != nil {
			return nil, false
		}
	}
	msg, err := b.Finish()
	return msg, err == nil
}
