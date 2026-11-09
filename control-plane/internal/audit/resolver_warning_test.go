package audit

// ADR-0019 addendum: the verifier node's resolver hygiene rides the EXISTING host-audit gate. The fork's GET /v1/audit adds one
// more key — warnings.resolver — when the node's resolv.conf has a `search`/`domain` line, `ndots` > 1, or the hostname has a dot;
// nothing new is needed on this side because green is, and has always been, "warnings == {}". These tests pin that this is TRUE of
// the code as it stands, on the exact JSON the Ruby wrapper emits, so a later "tidy" of the warnings handling (a key allow-list, a
// "known warnings only" filter) cannot quietly make a node with a dirty resolver look clean.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/node"
)

// What the fork's wrapper serves for a node whose gem-level audit is clean but whose resolver is not. The box ALSO says
// audit_status "green" here, deliberately: the brain re-derives status itself and must never trust the box's word for it.
const wrapperBodyDirtyResolver = `{
  "wrapper_api_version": "v1",
  "current_host_ip": "203.0.113.10",
  "warnings": {"resolver": "node resolver is not clean: hostname \"verifier.example.com\" contains a dot (Ruby derives a search domain from it). The node must run with no search domain, no ` + "`domain`" + ` line, ndots <= 1 and a hostname with no dot (ADR-0019), or it does no SMTP."},
  "audit_status": "green",
  "audited_at": "2026-11-09T12:00:00Z"
}`

const wrapperBodyClean = `{
  "wrapper_api_version": "v1",
  "current_host_ip": "203.0.113.10",
  "warnings": {},
  "audit_status": "green",
  "audited_at": "2026-11-09T12:00:00Z"
}`

func serveAudit(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audit" {
			t.Errorf("expected /v1/audit, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolverWarning_OnTheWire_IsRedEvenWhenTheBoxClaimsGreen(t *testing.T) {
	c := node.NewHTTPClient(2 * time.Second)

	dirty, err := c.FetchAudit(context.Background(), serveAudit(t, wrapperBodyDirtyResolver).URL, "tok")
	if err != nil {
		t.Fatalf("FetchAudit: %v", err)
	}
	if _, ok := dirty.Warnings["resolver"]; !ok {
		t.Fatalf("the resolver warning did not survive decoding: %+v", dirty.Warnings)
	}
	if dirty.AuditStatus != "green" {
		t.Fatalf("test setup: the box was meant to claim green, got %q", dirty.AuditStatus)
	}
	if got := DeriveStatus(Result{CurrentHostIP: dirty.CurrentHostIP, Warnings: dirty.Warnings}); got != StatusRed {
		t.Fatalf("a node with a dirty resolver derived %q; green is ONLY warnings == {} and the brain never trusts the box's own audit_status", got)
	}

	// CONTROL: the identical request path with an empty warnings map is green.
	clean, err := c.FetchAudit(context.Background(), serveAudit(t, wrapperBodyClean).URL, "tok")
	if err != nil {
		t.Fatalf("FetchAudit: %v", err)
	}
	if got := DeriveStatus(Result{CurrentHostIP: clean.CurrentHostIP, Warnings: clean.Warnings}); got != StatusGreen {
		t.Fatalf("CONTROL FAILED: a clean audit derived %q, want green — the resolver check must add nothing when the resolver is clean", got)
	}
}

// A dirty resolver is quarantined on the first red, exactly like a PTR or DNS warning — it is not a special case, and a node that
// stops being dirty earns its way back through the same N consecutive greens.
func TestScheduler_ResolverWarningQuarantinesLikeAnyOtherRed_AndRecoversTheSameWay(t *testing.T) {
	s, _, sim := schedSetup(t, 2)
	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:00:00Z"))
	if out := s.AuditNode(context.Background(), nodeRef()); out.To != LifecycleActive {
		t.Fatalf("setup: green must promote, got %s", out.To)
	}

	sim.Set(schedURL, node.SimResolverWarning(schedEgressIP, "2026-07-17T12:05:00Z"))
	out := s.AuditNode(context.Background(), nodeRef())
	if out.Status != StatusRed || out.To != LifecycleQuarantined {
		t.Fatalf("a dirty resolver must quarantine on the first red, got status=%s to=%s", out.Status, out.To)
	}

	sim.Set(schedURL, node.SimGreen(schedEgressIP, "2026-07-17T12:10:00Z")) // the operator fixed the resolver
	if out := s.AuditNode(context.Background(), nodeRef()); out.To != LifecycleQuarantined {
		t.Fatalf("green #1 of 2 must stay quarantined, got %s", out.To)
	}
	if out := s.AuditNode(context.Background(), nodeRef()); out.To == LifecycleQuarantined {
		t.Fatalf("green #2 of 2 must leave quarantine, still %s", out.To)
	}
}
