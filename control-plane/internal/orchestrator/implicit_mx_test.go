package orchestrator

// End-to-end for ADR-0020 (RFC 5321 §5.1 implicit MX): the whole Verify path — normalisation, the cheap layers, the policy,
// the SMTP stage, the log and the cache — driven through the PRODUCTION resolver over the real DNS protocol
// (internal/dnsfixture), not through the fake `fixtureResolver` every other orchestrator test uses. That fake answered "no
// MX" with an empty list and no error, a shape real Go never produces, which is how the implicit-MX fallback stayed dead
// code while every test passed.
//
// THE RULING, as assertions: no MX + an A/AAAA record → that host is the mail host and the SMTP probe runs; if the probe
// cannot run → risky, never invalid; null MX stays invalid (even with an A record); no MX and no A/AAAA stays invalid; and an
// outage of ours is unknown. The paired controls are the point: the rule must not have become "everything passes".

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/dnsfixture"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// countingNode is a node.Client that records whether the SMTP probe was ever reached.
type countingNode struct {
	body  []byte
	err   error
	calls *atomic.Int32
}

func (c countingNode) Verify(_ context.Context, _, _, _ string) (node.RawEngineResult, error) {
	c.calls.Add(1)
	return c.body, c.err
}

// realDNSOrch builds an orchestrator whose resolver is the production NetResolver pointed at the in-process DNS server.
// withSMTP=false leaves the SMTP stage unwired; otherwise the node health is as given.
func realDNSOrch(t *testing.T, withSMTP bool, health store.NodeHealth, body []byte, nodeErr error) (*Orchestrator, *store.MemStore, *policy.Definition, *atomic.Int32) {
	t.Helper()
	srv := dnsfixture.Start(t)
	h, err := hash.NewHasher([]byte("implicit-mx-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(health)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	calls := &atomic.Int32{}
	deps := Deps{
		Hasher: h, Store: mem, Resolver: layers.NewNetResolverAt(srv.Addr()), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:test",
	}
	if withSMTP {
		nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
		deps.SMTP = &SMTPDeps{
			Client:   countingNode{body: body, err: nodeErr, calls: calls},
			Registry: router.New(nodes, nil), TokenFor: func(string) string { return "tok" },
			Health: mem, MaxAuditAge: 0,
		}
	}
	return New(deps), mem, def, calls
}

var greenNode = store.NodeHealth{
	NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
	LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
}

// What the node reports when truemail itself reached the domain's implicit-MX host (it does its own RFC 5321 lookup: the
// fork leaves not_rfc_mx_lookup_flow at its default, so mx → cname → A).
var implicitMXValidBody = []byte(`{
  "wrapper_api_version":"v1","email":"jane@aonly.test","validation_type":"smtp","success":true,
  "smtp_debug":null,
  "catchall":{"probed":true,"probe_local_part":"nx-seed","accepted":false,"conclusion":"not_catchall"},
  "mx":{"domain":"aonly.test","mail_servers":["192.0.2.50"]}}`)

// THE RULING, headline: A-only is not invalid; the cheap path hands it to the SMTP probe, which decides.
func TestImplicitMX_ADomainWithOnlyAnARecord_ContinuesToTheSMTPProbe(t *testing.T) {
	for _, d := range []string{"aonly.test", "aaaaonly.test", "dualstack.test", "aliased.test"} {
		t.Run(d, func(t *testing.T) {
			o, mem, def, calls := realDNSOrch(t, true, greenNode, implicitMXValidBody, nil)
			resp, err := o.Verify(context.Background(), "jane@"+d, def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Status == vocab.StatusInvalid {
				t.Fatalf("%s: a domain with an A/AAAA record and no MX was reported invalid/%s (RFC 5321 §5.1: it receives mail at that host)", d, resp.Reason)
			}
			if calls.Load() != 1 {
				t.Fatalf("%s: the SMTP probe was reached %d times, want exactly 1 (the implicit MX must CONTINUE to the probe, not stop at the cheap layer)", d, calls.Load())
			}
			if resp.Status != vocab.StatusValid || resp.Reason != vocab.ReasonAccepted {
				t.Fatalf("%s: probe accepted → valid/accepted, got %s/%s", d, resp.Status, resp.Reason)
			}
			if len(mem.Raw) != 1 {
				t.Fatalf("%s: a probe that ran must leave its raw sidecar row, got %d", d, len(mem.Raw))
			}
		})
	}
}

// "if the probe can't run → risky, never invalid". Every way the probe can fail to run, on an implicit-MX domain.
func TestImplicitMX_WhenTheProbeCannotRun_IsRiskyNeverInvalid(t *testing.T) {
	redNode := store.NodeHealth{NodeID: "n1", Status: "active", AuditStatus: "red", Port25Open: true, LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP}
	for _, tc := range []struct {
		name     string
		withSMTP bool
		health   store.NodeHealth
		skip     string
	}{
		{"the only node's audit is red", true, redNode, "audit_red"},
		// M1-G6: an unwired stage used to leave NO token, indistinguishable from a genuine cheap answer; it now says smtp_unwired.
		{"no SMTP stage is wired at all", false, store.NodeHealth{}, "smtp_unwired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, mem, def, calls := realDNSOrch(t, tc.withSMTP, tc.health, implicitMXValidBody, nil)
			resp, err := o.Verify(context.Background(), "jane@aonly.test", def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Status == vocab.StatusInvalid {
				t.Fatal("RULING VIOLATION: an implicit-MX domain whose probe could not run was reported invalid")
			}
			if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible {
				t.Fatalf("probe cannot run → the cheap residue stands, risky/plausible; got %s/%s", resp.Status, resp.Reason)
			}
			if got, _ := resp.Signals["smtp_skipped_reason"].(string); got != tc.skip {
				t.Fatalf("smtp_skipped_reason = %q, want %q", got, tc.skip)
			}
			if calls.Load() != 0 {
				t.Fatalf("the node was called %d times although the probe was meant not to run", calls.Load())
			}
			if mem.CacheLen() != 0 {
				// PB1 for the capacity gaps; and since M1-G6 the unwired stage is our own degraded state, not an answer: never cached either.
				t.Fatalf("a fallback for a probe that could not run must not be cached, got cache len %d", mem.CacheLen())
			}
		})
	}
}

// The probe ran but the node found nothing to connect to (truemail: "target host(s) not found", no transcript). The ruling
// is "never invalid"; the existing SMTP stage already says unknown/connection_failed. Pinned here so it cannot drift to invalid.
func TestImplicitMX_ProbeRanButNothingToConnectTo_IsUnknownNeverInvalid(t *testing.T) {
	body := []byte(`{"wrapper_api_version":"v1","email":"jane@aonly.test","validation_type":"smtp","success":false,
	  "smtp_debug":null,"errors":{"mx":"target host(s) not found"},"mx":{"domain":"aonly.test","mail_servers":[]}}`)
	o, _, def, calls := realDNSOrch(t, true, greenNode, body, nil)
	resp, err := o.Verify(context.Background(), "jane@aonly.test", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("the probe should have run once, ran %d", calls.Load())
	}
	if resp.Status == vocab.StatusInvalid {
		t.Fatalf("a node that found no host to connect to must never make the address invalid, got invalid/%s", resp.Reason)
	}
	if resp.Status != vocab.StatusUnknown {
		t.Fatalf("probe ran, no transcript → unknown, got %s/%s", resp.Status, resp.Reason)
	}
}

// THE PAIRED CONTROLS. The ruling must not have turned into "every domain passes the cheap layer".
func TestImplicitMX_Controls_ProvableAbsenceIsStillInvalid_AndNeverReachesTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		domain     string
		wantStatus vocab.Status
		wantReason vocab.Reason
	}{
		{"truly no records (the name exists: TXT only, no MX, no A, no AAAA)", "norecords.test", vocab.StatusInvalid, vocab.ReasonNoMX},
		{"NXDOMAIN", "no-such-domain-xyz.test", vocab.StatusInvalid, vocab.ReasonNoMX},
		{"null MX (RFC 7505)", "nullmx.test", vocab.StatusInvalid, vocab.ReasonNullMX},
		{"null MX that also has an A record — the A record must not rescue it", "nullmx-with-a.test", vocab.StatusInvalid, vocab.ReasonNullMX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, def, calls := realDNSOrch(t, true, greenNode, implicitMXValidBody, nil)
			resp, err := o.Verify(context.Background(), "jane@"+tc.domain, def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Status != tc.wantStatus || resp.Reason != tc.wantReason {
				t.Fatalf("%s → %s/%s, want %s/%s", tc.domain, resp.Status, resp.Reason, tc.wantStatus, tc.wantReason)
			}
			if calls.Load() != 0 {
				t.Fatalf("%s: a provably mail-less domain reached the SMTP probe (%d calls)", tc.domain, calls.Load())
			}
		})
	}
}

// An outage of OURS at the A/AAAA follow-up is unknown — never invalid, and never a probe either (we do not know the host).
func TestImplicitMX_AnOutageAtTheAddressFollowUp_IsUnknownNeverInvalid(t *testing.T) {
	for _, d := range []string{"aaaa-servfail.test", "a-servfail.test", "mxservfail.test", "mxservfail-noaddr.test", "servfail.test"} {
		t.Run(d, func(t *testing.T) {
			// repeated: the A and AAAA lookups run concurrently, and the old combined lookup answered at random
			for i := 0; i < 15; i++ {
				o, _, def, calls := realDNSOrch(t, true, greenNode, implicitMXValidBody, nil)
				resp, err := o.Verify(context.Background(), "jane@"+d, def)
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if resp.Status == vocab.StatusInvalid {
					t.Fatalf("%s [run %d]: our DNS outage was reported invalid/%s", d, i, resp.Reason)
				}
				if resp.Status != vocab.StatusUnknown || resp.Reason != vocab.ReasonDNSError {
					t.Fatalf("%s [run %d]: want unknown/dns_error, got %s/%s", d, i, resp.Status, resp.Reason)
				}
				if calls.Load() != 0 {
					t.Fatalf("%s: the probe ran against a domain whose DNS we could not resolve", d)
				}
			}
		})
	}
}
