package dnsfixture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type shape struct {
	rcode       dnsmessage.RCode
	answers     int
	authorities int
	answered    bool
}

// ask sends one raw query and returns the response SHAPE — what a Go resolver flattens into a single error.
func ask(t *testing.T, s *Server, name string, qt dnsmessage.Type) shape {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(700 * time.Millisecond))
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return shape{answered: false}
	}
	var p dnsmessage.Parser
	h, err := p.Start(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	ans, err := p.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := p.AllAuthorities()
	if err != nil {
		t.Fatal(err)
	}
	return shape{rcode: h.RCode, answers: len(ans), authorities: len(auth), answered: true}
}

// The package doc claims "the shape a real authoritative server sends". This checks the claim on the wire. It matters
// because Go's resolver reports NXDOMAIN and NODATA identically (IsNotFound) — so only the raw protocol can show that the
// fixture really serves two different things, and that "A-only" really is NODATA-for-MX rather than a name that is absent.
func TestZoneShapes(t *testing.T) {
	s := Start(t)
	for _, tc := range []struct {
		name string
		qt   dnsmessage.Type
		want shape
	}{
		{"aonly.test. MX is NODATA: NOERROR, empty answer, SOA in authority", dnsmessage.TypeMX, shape{dnsmessage.RCodeSuccess, 0, 1, true}},
		{"aonly.test. A is a positive answer", dnsmessage.TypeA, shape{dnsmessage.RCodeSuccess, 1, 0, true}},
		{"aonly.test. AAAA is NODATA too", dnsmessage.TypeAAAA, shape{dnsmessage.RCodeSuccess, 0, 1, true}},
		{"a name that is not in the zone is NXDOMAIN with an SOA", dnsmessage.TypeMX, shape{dnsmessage.RCodeNameError, 0, 1, true}},
		{"nullmx-with-a.test. MX is one record (the null MX)", dnsmessage.TypeMX, shape{dnsmessage.RCodeSuccess, 1, 0, true}},
		{"aliased.test. A is a CNAME plus the address it points at", dnsmessage.TypeA, shape{dnsmessage.RCodeSuccess, 2, 0, true}},
		{"norecords.test. exists (TXT) but has no MX: NODATA", dnsmessage.TypeMX, shape{dnsmessage.RCodeSuccess, 0, 1, true}},
		{"aaaa-servfail.test. AAAA is SERVFAIL", dnsmessage.TypeAAAA, shape{dnsmessage.RCodeServerFailure, 0, 0, true}},
		{"aaaa-servfail.test. A is plain NODATA", dnsmessage.TypeA, shape{dnsmessage.RCodeSuccess, 0, 1, true}},
		{"a-timeout.test. A never answers", dnsmessage.TypeA, shape{answered: false}},
	} {
		// the query name is the first word of the case name
		name := strings.Fields(tc.name)[0]
		if strings.HasPrefix(tc.name, "a name that is not") {
			name = "no-such-name-xyz.test."
		}
		if got := ask(t, s, name, tc.qt); got != tc.want {
			t.Errorf("%s: wire shape = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The fixture is test support: nothing that ships may import it (importing it would also drag the "testing"-adjacent zone
// into a production binary). Walks the whole module and fails if a non-test file imports this package.
func TestNotImportedByProductionCode(t *testing.T) {
	const self = "github.com/coldmail-verify/control-plane/internal/dnsfixture"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	checked := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/internal/dnsfixture/") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("cannot parse %s: %v", path, perr)
			return nil
		}
		checked++
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == self {
				t.Errorf("%s imports the DNS test fixture: it is test-only and must not be linked into the product", path)
			}
		}
		return nil
	})
	if checked < 10 {
		t.Fatalf("only %d non-test files were inspected — the walk is not reaching the module, so this proves nothing", checked)
	}
}
