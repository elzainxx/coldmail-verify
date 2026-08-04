package policy

import (
	"context"
	"net"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// ── test doubles: a resolver + lists we fully control (no network, no DB) ────────────────

type fakeResolver struct {
	mx  map[string][]layers.MXHost
	ips map[string][]net.IP
	err map[string]error
}

func (f fakeResolver) LookupMX(_ context.Context, d string) ([]layers.MXHost, error) {
	if e, ok := f.err[d]; ok {
		return nil, e
	}
	return f.mx[d], nil
}
func (f fakeResolver) LookupIP(_ context.Context, d string) ([]net.IP, error) {
	if e, ok := f.err[d]; ok {
		return nil, e
	}
	return f.ips[d], nil
}

type fakeLists struct {
	disp map[string]bool
	trap map[string]bool
}

func (f fakeLists) IsDisposable(_ context.Context, d string) (bool, error) { return f.disp[d], nil }
func (f fakeLists) IsSpamTrap(_ context.Context, d string) (bool, error)   { return f.trap[d], nil }

// evalEmail runs the shipped cold-email policy over one address with the given fakes.
func evalEmail(t *testing.T, local, domain string, r layers.Resolver, l layers.ListSource) Verdict {
	t.Helper()
	def, err := ColdEmailDefinition()
	if err != nil {
		t.Fatalf("ColdEmailDefinition: %v", err)
	}
	ev := layers.NewEvidence(local+"@"+domain, local+"@"+domain, local, domain, r, l)
	v, err := NewEngine().Evaluate(context.Background(), def, ev, LayerSet())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// EVERY verdict the engine emits must be vocab-valid and composability-correct (Door 1/4).
	if !vocab.ValidStatus(v.Status) || !vocab.ValidReason(v.Reason) || !vocab.ValidRecommendation(v.Recommendation) {
		t.Fatalf("engine emitted out-of-vocab verdict: %+v", v)
	}
	if !vocab.ComposabilityOK(v.Status, v.Recommendation) {
		t.Fatalf("engine emitted a forbidden status/recommendation pair: %s/%s", v.Status, v.Recommendation)
	}
	if v.Confidence < 0 || v.Confidence > 100 {
		t.Fatalf("confidence out of range: %d", v.Confidence)
	}
	return v
}

func mxGoogle(domain string) fakeResolver {
	return fakeResolver{mx: map[string][]layers.MXHost{domain: {{Host: "aspmx.l.google.com", Pref: 1}}}}
}
func mxCorp(domain string) fakeResolver {
	return fakeResolver{mx: map[string][]layers.MXHost{domain: {{Host: "mx." + domain, Pref: 10}}}}
}

// THE Door-1 invariant: an `invalid` verdict can NEVER carry recommendation=send.
func TestInvalidNeverSaysSend(t *testing.T) {
	cases := []struct {
		name         string
		local, dom   string
		r            layers.Resolver
		l            layers.ListSource
		wantStatus   vocab.Status
		wantReason   vocab.Reason
	}{
		{"no MX", "a", "nomx.example", fakeResolver{}, nil, vocab.StatusInvalid, vocab.ReasonNoMX},
		{"null MX", "a", "nullmx.example", fakeResolver{mx: map[string][]layers.MXHost{"nullmx.example": {{Host: ".", Pref: 0}}}}, nil, vocab.StatusInvalid, vocab.ReasonNullMX},
		{"spam trap", "a", "trap.example", mxCorp("trap.example"), fakeLists{trap: map[string]bool{"trap.example": true}}, vocab.StatusInvalid, vocab.ReasonSpamTrap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := evalEmail(t, c.local, c.dom, c.r, c.l)
			if v.Status != c.wantStatus || v.Reason != c.wantReason {
				t.Errorf("got %s/%s, want %s/%s", v.Status, v.Reason, c.wantStatus, c.wantReason)
			}
			if v.Recommendation == vocab.RecSend {
				t.Errorf("FORBIDDEN: invalid verdict recommended send (Door 1) — %+v", v)
			}
			if v.Recommendation != vocab.RecSkip {
				t.Errorf("invalid must recommend skip, got %s", v.Recommendation)
			}
		})
	}
}

// Stop-early: a syntax fail must stop BEFORE any DNS layer runs (cheapest-sufficient §7.3).
func TestStopEarlySkipsLaterLayers(t *testing.T) {
	// A resolver that FAILS the test if it is ever called — proves MX never ran.
	failing := explodingResolver{t: t}
	// "a@localhost" fails syntax (no dot) → must stop at syntax, never reach MX.
	v := evalEmail(t, "a", "localhost", failing, nil)
	if v.Status != vocab.StatusInvalid || v.Reason != vocab.ReasonInvalidSyntax {
		t.Fatalf("syntax fail must stop at invalid/invalid_syntax, got %s/%s", v.Status, v.Reason)
	}
	if v.StoppedAt != "syntax" {
		t.Errorf("must stop at the syntax layer, stopped at %q", v.StoppedAt)
	}
}

type explodingResolver struct{ t *testing.T }

func (e explodingResolver) LookupMX(context.Context, string) ([]layers.MXHost, error) {
	e.t.Fatal("MX lookup ran after a syntax-fail stop — stop-early was not honored")
	return nil, nil
}
func (e explodingResolver) LookupIP(context.Context, string) ([]net.IP, error) {
	e.t.Fatal("IP lookup ran after a syntax-fail stop")
	return nil, nil
}

// OQ-7: a disposable address is RISKY + skip, never invalid — and the is_disposable flag is set.
func TestDisposableIsRiskySkipNeverInvalid(t *testing.T) {
	lists := fakeLists{disp: map[string]bool{"mailinator.com": true}}
	v := evalEmail(t, "a", "mailinator.com", mxCorp("mailinator.com"), lists)
	if v.Status != vocab.StatusRisky {
		t.Errorf("disposable must be risky (OQ-7), got %s", v.Status)
	}
	if v.Status == vocab.StatusInvalid {
		t.Fatal("disposable must NEVER be invalid (OQ-7 — poisons P7 calibration)")
	}
	if v.Reason != vocab.ReasonDisposable {
		t.Errorf("reason must be disposable, got %s", v.Reason)
	}
	if v.Recommendation != vocab.RecSkip {
		t.Errorf("cold-email skips disposables, got %s", v.Recommendation)
	}
	if !v.Flags["is_disposable"] {
		t.Error("is_disposable flag must be set")
	}
}

// A transient DNS failure must yield unknown/dns_error — never invalid (harden-toward-unknown).
func TestTransientDNSIsUnknownNotInvalid(t *testing.T) {
	r := fakeResolver{err: map[string]error{"x.com": &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}}}
	v := evalEmail(t, "a", "x.com", r, nil)
	if v.Status != vocab.StatusUnknown || v.Reason != vocab.ReasonDNSError {
		t.Errorf("transient DNS must be unknown/dns_error, got %s/%s", v.Status, v.Reason)
	}
	if v.Status == vocab.StatusInvalid {
		t.Fatal("a transient DNS failure must NEVER be invalid (Door 4 invariant)")
	}
}

// A big provider (routed to :mx) cannot be SMTP-confirmed in P1 → risky/provider_no_smtp (OQ-14).
func TestProviderRoutedToMXIsRiskyNotValid(t *testing.T) {
	v := evalEmail(t, "jane", "gmail.com", mxGoogle("gmail.com"), nil)
	if v.Status != vocab.StatusRisky || v.Reason != vocab.ReasonProviderNoSMTP {
		t.Errorf("gmail (routed :mx) must be risky/provider_no_smtp, got %s/%s", v.Status, v.Reason)
	}
	if v.Status == vocab.StatusValid {
		t.Fatal("P1 must NOT claim valid without SMTP")
	}
	if v.Recommendation == vocab.RecSend {
		t.Error("risky must not recommend send")
	}
}

// A clean corporate domain that passes all cheap layers is the honest P1 maybe: risky/plausible,
// NEVER valid (no SMTP). This is the harden-toward-unknown ceiling of Phase 1.
func TestCleanCorpIsPlausibleNotValid(t *testing.T) {
	v := evalEmail(t, "jane.doe", "acmecorp.com", mxCorp("acmecorp.com"), fakeLists{})
	if v.Status == vocab.StatusValid {
		t.Fatal("P1 must never mint valid — SMTP (P2) is required for a confident yes")
	}
	if v.Status != vocab.StatusRisky || v.Reason != vocab.ReasonPlausible {
		t.Errorf("clean corp must be risky/plausible in P1, got %s/%s", v.Status, v.Reason)
	}
}

func TestConfidenceInvalidsHighUnprovenLow(t *testing.T) {
	// Invalids carry high confidence (positive proof); plausible is capped low (§7.4).
	inv := evalEmail(t, "a", "nomx.example", fakeResolver{}, nil)
	if inv.Confidence < 90 {
		t.Errorf("proven invalid (no_mx) must be high-confidence, got %d", inv.Confidence)
	}
	plausible := evalEmail(t, "jane.doe", "acmecorp.com", mxCorp("acmecorp.com"), fakeLists{})
	if plausible.Confidence > 40 {
		t.Errorf("unproven plausible must be capped low (≤40, §7.4), got %d", plausible.Confidence)
	}
}

func TestParseRejectsBadAction(t *testing.T) {
	// A gate action outside the vocab must be rejected at evaluate time (Door 4 guard).
	def, _ := ColdEmailDefinition()
	def.Gates["mx:fail"] = "invalid:not_a_real_reason:stop"
	ev := layers.NewEvidence("a@x.com", "a@x.com", "a", "x.com", fakeResolver{}, nil)
	if _, err := NewEngine().Evaluate(context.Background(), def, ev, LayerSet()); err == nil {
		t.Error("an action with an out-of-vocab reason must error, not emit it")
	}
}
