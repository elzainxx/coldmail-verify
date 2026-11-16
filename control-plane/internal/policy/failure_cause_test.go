package policy

import (
	"context"
	"net"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/failureorigin"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// signals.failure_origin: the policy carries the mx layer's evidence on the `unknown/dns_error` verdict it stops at, and ONLY there. It
// changes nothing else about the verdict. (The origin itself is decided by failureorigin.OriginFor and written by verdict.Assemble; this
// pins the hand-off between them.)

func TestEvaluate_DNSUnknownCarriesTheLayersCause_AndNothingElseDoes(t *testing.T) {
	servfail := &net.DNSError{Err: "server misbehaving", IsTemporary: true, Server: "127.0.0.1:53"}
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true, Server: "127.0.0.1:53"}
	for _, tc := range []struct {
		name string
		r    layers.Resolver
		want failureorigin.Cause
	}{
		{"a SERVFAIL reply", fakeResolver{err: map[string]error{"d.example": servfail}}, failureorigin.CauseDNSServFail},
		{"a resolver timeout", fakeResolver{err: map[string]error{"d.example": timeout}}, failureorigin.CauseDNSResolverTimeout},
		{"no resolver wired", nil, failureorigin.CauseDNSNoResolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := evalEmail(t, "a", "d.example", tc.r, nil)
			if v.Status != vocab.StatusUnknown || v.Reason != vocab.ReasonDNSError || v.Confidence != 10 || v.Recommendation != vocab.RecSkip {
				t.Fatalf("verdict = %s/%s/%d/%s, want unknown/dns_error/10/skip (the cause must not move it)", v.Status, v.Reason, v.Confidence, v.Recommendation)
			}
			if v.Cause != tc.want {
				t.Fatalf("Cause = %q, want %q", v.Cause, tc.want)
			}
			if _, has := v.Signals[vocab.SignalFailureOrigin]; has {
				t.Error("the policy must not write failure_origin itself: only verdict.Assemble does")
			}
		})
	}

	// CONTROLS: every verdict that is not that unknown carries no cause.
	for name, v := range map[string]Verdict{
		"a clean corp address (risky/plausible)": evalEmail(t, "jane", "acmecorp.com", mxCorp("acmecorp.com"), nil),
		"no MX at all (invalid/no_mx)":           evalEmail(t, "a", "nomx.example", fakeResolver{}, nil),
		"a null MX (invalid/null_mx)":            evalEmail(t, "a", "nullmx.example", fakeResolver{mx: map[string][]layers.MXHost{"nullmx.example": {{Host: ".", Pref: 0}}}}, nil),
	} {
		if v.Cause != "" {
			t.Errorf("%s: Cause = %q, want none (only an unknown has a cause)", name, v.Cause)
		}
	}
}

// A nil resolver is its own case above; keep the direct layer-level guarantee that the policy engine itself tolerates it without panicking.
func TestEvaluate_NilResolverIsUnknownNotAPanic(t *testing.T) {
	def, err := ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	ev := layers.NewEvidence("a@d.example", "a@d.example", "a", "d.example", nil, nil)
	v, err := NewEngine().Evaluate(context.Background(), def, ev, LayerSet())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Status != vocab.StatusUnknown {
		t.Fatalf("no resolver → unknown, got %s/%s", v.Status, v.Reason)
	}
}
