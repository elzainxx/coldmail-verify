package layers

import (
	"context"
	"errors"
	"net"
	"testing"
)

// fixtureResolver is an injectable Resolver: no network. It maps domain→MX or domain→error,
// so we can prove the harden-toward-unknown classification exactly.
type fixtureResolver struct {
	mx   map[string][]MXHost
	ips  map[string][]net.IP
	errs map[string]error // domain -> error to return from BOTH LookupMX and LookupIP
}

func (f fixtureResolver) LookupMX(_ context.Context, d string) ([]MXHost, error) {
	if e, ok := f.errs[d]; ok {
		return nil, e
	}
	if m, ok := f.mx[d]; ok {
		return m, nil
	}
	return nil, nil // no MX records, no error → triggers A/AAAA fallback
}
func (f fixtureResolver) LookupIP(_ context.Context, d string) ([]net.IP, error) {
	if e, ok := f.errs[d]; ok {
		return nil, e
	}
	return f.ips[d], nil
}

func notFoundErr() error   { return &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true} }
func timeoutErr() error    { return &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true, IsTemporary: true} }
func servfailErr() error   { return &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true} }

func ev(t *testing.T, email, local, domain string, r Resolver, l ListSource) *Evidence {
	t.Helper()
	return NewEvidence(email, local+"@"+domain, local, domain, r, l)
}

// THE centerpiece test: a transient DNS failure must yield Unknown, NEVER Fail (invalid).
func TestMXHardensTowardUnknownOnTransient(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		err    error
		want   Result
	}{
		{"timeout -> unknown", timeoutErr(), Unknown},
		{"servfail -> unknown", servfailErr(), Unknown},
		{"NXDOMAIN -> fail (definitive non-existence)", notFoundErr(), Fail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fixtureResolver{errs: map[string]error{"x.com": c.err}}
			out := MXLayer{}.Run(ctx, ev(t, "a@x.com", "a", "x.com", r, nil))
			if out.Result != c.want {
				t.Errorf("MX on %s = %s, want %s (harden-toward-unknown: transient is NEVER fail)", c.name, out.Result, c.want)
			}
		})
	}
}

func TestMXFoundAndImplicitMX(t *testing.T) {
	ctx := context.Background()
	// Explicit MX present.
	r1 := fixtureResolver{mx: map[string][]MXHost{"x.com": {{Host: "mx.x.com", Pref: 10}}}}
	if out := (MXLayer{}).Run(ctx, ev(t, "a@x.com", "a", "x.com", r1, nil)); out.Result != Pass {
		t.Errorf("explicit MX must Pass, got %s", out.Result)
	}
	// No MX but A record present → implicit MX (RFC 5321) → Pass.
	r2 := fixtureResolver{ips: map[string][]net.IP{"x.com": {net.ParseIP("1.2.3.4")}}}
	if out := (MXLayer{}).Run(ctx, ev(t, "a@x.com", "a", "x.com", r2, nil)); out.Result != Pass {
		t.Errorf("A-record implicit MX must Pass, got %s", out.Result)
	}
	// No MX, no A → definitive no mail route → Fail.
	r3 := fixtureResolver{}
	if out := (MXLayer{}).Run(ctx, ev(t, "a@x.com", "a", "x.com", r3, nil)); out.Result != Fail {
		t.Errorf("no MX and no A must Fail, got %s", out.Result)
	}
}

func TestNullMXDetection(t *testing.T) {
	ctx := context.Background()
	r := fixtureResolver{mx: map[string][]MXHost{"nomail.example": {{Host: ".", Pref: 0}}}}
	e := ev(t, "a@nomail.example", "a", "nomail.example", r, nil)
	if out := (NullMXLayer{}).Run(ctx, e); out.Result != Pass || out.Value != true {
		t.Errorf("null-MX must be detected (Result=Pass, Value=true), got %s/%v", out.Result, out.Value)
	}
	// And the MX layer reports the domain as having no usable route (Fail), sharing the lookup.
	if out := (MXLayer{}).Run(ctx, e); out.Result != Fail {
		t.Errorf("null-MX domain must be Fail at the MX layer, got %s", out.Result)
	}
}

func TestProviderClassification(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		domain string
		mxHost string
		want   string
	}{
		{"corp.com", "aspmx.l.google.com", "google"},
		{"corp.com", "corp-com.mail.protection.outlook.com", "o365"},
		{"corp.com", "mta5.am0.yahoodns.net", "yahoo"},
		{"corp.com", "mx.corp.com", "self"},
		{"corp.com", "mail.somerandomhost.net", "other"},
	}
	for _, c := range cases {
		r := fixtureResolver{mx: map[string][]MXHost{c.domain: {{Host: c.mxHost, Pref: 10}}}}
		out := ProviderClassifyLayer{}.Run(ctx, ev(t, "a@"+c.domain, "a", c.domain, r, nil))
		if out.Value != c.want {
			t.Errorf("classify %s (MX %s) = %v, want %s", c.domain, c.mxHost, out.Value, c.want)
		}
	}
	// Provider classification with no usable MX => Skip (no guess).
	out := ProviderClassifyLayer{}.Run(ctx, ev(t, "a@x.com", "a", "x.com", fixtureResolver{}, nil))
	if out.Result != Skip {
		t.Errorf("no-MX provider classify must Skip, got %s", out.Result)
	}
}

func TestSharedMXLookupOnce(t *testing.T) {
	// The three DNS layers must share ONE lookup (Evidence memoizes). Count calls.
	ctx := context.Background()
	counting := &countingResolver{inner: fixtureResolver{mx: map[string][]MXHost{"x.com": {{Host: "aspmx.l.google.com", Pref: 1}}}}}
	e := ev(t, "a@x.com", "a", "x.com", counting, nil)
	_ = MXLayer{}.Run(ctx, e)
	_ = NullMXLayer{}.Run(ctx, e)
	_ = ProviderClassifyLayer{}.Run(ctx, e)
	if counting.mxCalls != 1 {
		t.Errorf("MX lookup must happen exactly once across the DNS layers, got %d calls", counting.mxCalls)
	}
}

type countingResolver struct {
	inner   Resolver
	mxCalls int
}

func (c *countingResolver) LookupMX(ctx context.Context, d string) ([]MXHost, error) {
	c.mxCalls++
	return c.inner.LookupMX(ctx, d)
}
func (c *countingResolver) LookupIP(ctx context.Context, d string) ([]net.IP, error) {
	return c.inner.LookupIP(ctx, d)
}

// ── Pure / list layers ─────────────────────────────────────────────────────────────────

func TestSyntaxLayer(t *testing.T) {
	ctx := context.Background()
	if out := (SyntaxLayer{}).Run(ctx, ev(t, "a@b.co", "a", "b.co", nil, nil)); out.Result != Pass {
		t.Errorf("valid parts must Pass, got %s", out.Result)
	}
	if out := (SyntaxLayer{}).Run(ctx, ev(t, "a@localhost", "a", "localhost", nil, nil)); out.Result != Fail {
		t.Errorf("dotless domain must Fail, got %s", out.Result)
	}
}

func TestRoleLayer(t *testing.T) {
	ctx := context.Background()
	for _, lp := range []string{"info", "sales", "admin", "support+urgent"} {
		if out := (RoleLayer{}).Run(ctx, ev(t, lp+"@c.com", lp, "c.com", nil, nil)); out.Result != Flag {
			t.Errorf("%s@ must Flag as role, got %s", lp, out.Result)
		}
	}
	if out := (RoleLayer{}).Run(ctx, ev(t, "jane.doe@c.com", "jane.doe", "c.com", nil, nil)); out.Result != Pass {
		t.Errorf("personal local part must not flag as role, got %s", out.Result)
	}
}

func TestFreeCorpLayer(t *testing.T) {
	ctx := context.Background()
	if out := (FreeCorpLayer{}).Run(ctx, ev(t, "a@gmail.com", "a", "gmail.com", nil, nil)); out.Result != Flag {
		t.Errorf("gmail must flag as free provider, got %s", out.Result)
	}
	if out := (FreeCorpLayer{}).Run(ctx, ev(t, "a@acmecorp.com", "a", "acmecorp.com", nil, nil)); out.Result != Pass {
		t.Errorf("corp domain must not flag free, got %s", out.Result)
	}
}

func TestTypoLayer(t *testing.T) {
	ctx := context.Background()
	// A near-miss of gmail.com should suggest a correction.
	out := TypoLayer{}.Run(ctx, ev(t, "a@gmial.com", "a", "gmial.com", nil, nil))
	if out.Result != Flag || out.Detail != "a@gmail.com" {
		t.Errorf("gmial.com must suggest a@gmail.com, got result=%s detail=%q", out.Result, out.Detail)
	}
	// An exact known-good must NOT be flagged.
	if out := (TypoLayer{}).Run(ctx, ev(t, "a@gmail.com", "a", "gmail.com", nil, nil)); out.Result != Pass {
		t.Errorf("exact gmail.com must not flag typo, got %s", out.Result)
	}
	// A totally unrelated domain must not get a spurious suggestion.
	if out := (TypoLayer{}).Run(ctx, ev(t, "a@my-unique-corp.io", "a", "my-unique-corp.io", nil, nil)); out.Result != Pass {
		t.Errorf("unrelated domain must not flag typo, got %s (detail %q)", out.Result, out.Detail)
	}
	_ = knownGoodSorted() // touch the helper
}

// A fake ListSource for the disposable/spam-trap layers.
type fakeLists struct {
	disposable map[string]bool
	spamTrap   map[string]bool
	err        error
}

func (f fakeLists) IsDisposable(_ context.Context, d string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.disposable[d], nil
}
func (f fakeLists) IsSpamTrap(_ context.Context, d string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.spamTrap[d], nil
}

func TestDisposableLayer(t *testing.T) {
	ctx := context.Background()
	lists := fakeLists{disposable: map[string]bool{"mailinator.com": true}}
	// Detector convention: Pass = IS disposable (fires disposable:pass → risky, OQ-7),
	// Value=true sets is_disposable. It is NEVER a gate to invalid — that is the policy's job.
	out := DisposableLayer{}.Run(ctx, ev(t, "a@mailinator.com", "a", "mailinator.com", nil, lists))
	if out.Result != Pass || out.Value != true {
		t.Errorf("disposable must be Pass/true (detector present; policy makes it risky), got %s/%v", out.Result, out.Value)
	}
	if out := (DisposableLayer{}).Run(ctx, ev(t, "a@corp.com", "a", "corp.com", nil, lists)); out.Result != Fail || out.Value != false {
		t.Errorf("non-disposable must be Fail/false, got %s/%v", out.Result, out.Value)
	}
	// A list error must yield Unknown, never a false detection.
	errLists := fakeLists{err: errors.New("db down")}
	if out := (DisposableLayer{}).Run(ctx, ev(t, "a@corp.com", "a", "corp.com", nil, errLists)); out.Result != Unknown {
		t.Errorf("list error must yield Unknown, got %s", out.Result)
	}
}

func TestSpamTrapLayer(t *testing.T) {
	ctx := context.Background()
	lists := fakeLists{spamTrap: map[string]bool{"trap.example": true}}
	// A trap fires: Result=Pass (grammar: spam_trap:pass → invalid), Value=true.
	if out := (SpamTrapLayer{}).Run(ctx, ev(t, "a@trap.example", "a", "trap.example", nil, lists)); out.Result != Pass || out.Value != true {
		t.Errorf("known trap must be Pass/true, got %s/%v", out.Result, out.Value)
	}
	// Not a trap: Result=Fail (nothing to gate).
	if out := (SpamTrapLayer{}).Run(ctx, ev(t, "a@corp.com", "a", "corp.com", nil, lists)); out.Result != Fail {
		t.Errorf("non-trap must be Fail, got %s", out.Result)
	}
}
