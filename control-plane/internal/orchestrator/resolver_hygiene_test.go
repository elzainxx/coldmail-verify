package orchestrator

// ADR-0019 addendum, END TO END: a node whose resolver is not clean does no SMTP. The REAL scheduler records the audit into the
// store (the same path production takes), and then Verify is asked to route a residue domain: the node client must never be called,
// and the verdict is the cheap one — risky/plausible with smtp_skipped_reason, never invalid (OQ-P3-6). The control is the same
// setup with a clean audit, where the probe DOES run: without it a "node never called" assertion could pass for any reason at all.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/audit"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// routedAfterAudit audits node n1 with `result` through the real scheduler, then verifies jane@acmecorp.com through an orchestrator
// wired to the same store. It returns the verdict and how many times the SMTP node was actually called.
func routedAfterAudit(t *testing.T, result node.AuditResult) (vocab.Status, vocab.Reason, string, int) {
	t.Helper()
	ctx := context.Background()
	mem := store.NewMem(nil)
	if err := mem.UpsertNodeState(ctx, "n1", "http://mock", "provisioning", true); err != nil {
		t.Fatalf("register node: %v", err)
	}
	sim := node.NewSimAuditSurface()
	sim.Set("http://mock", result)
	sched := audit.NewScheduler(audit.Deps{Client: sim, Store: mem, Now: func() time.Time { return time.Now() }}, 1)
	sched.AuditNode(ctx, audit.NodeRef{ID: "n1", BaseURL: "http://mock", Token: "tok", PublicEgressIP: testEgressIP})

	h, err := hash.NewHasher([]byte("resolver-hygiene-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	def, _ := policy.ColdEmailDefinition()
	calls := &atomic.Int32{}
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client: countingNode{body: validBody, calls: calls}, Registry: router.New(nodes, nil),
			TokenFor: func(string) string { return "tok" }, Health: mem, MaxAuditAge: 0,
		},
	})
	resp, err := o.Verify(ctx, "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	skip, _ := resp.Signals["smtp_skipped_reason"].(string)
	return resp.Status, resp.Reason, skip, int(calls.Load())
}

func TestResolverHygiene_ADirtyResolverMeansTheNodeDoesNoSMTP(t *testing.T) {
	status, reason, skip, called := routedAfterAudit(t, node.SimResolverWarning(testEgressIP, time.Now().UTC().Format(time.RFC3339)))
	if called != 0 {
		t.Fatalf("the SMTP node was called %d time(s) although its audit carried a resolver warning — a dirty resolver must mean NO SMTP", called)
	}
	if status == vocab.StatusInvalid {
		t.Fatal("OQ-P3-6 VIOLATION: a node's dirty resolver must never make the address invalid")
	}
	if status != vocab.StatusRisky || reason != vocab.ReasonPlausible {
		t.Fatalf("no probe → the cheap residue stands, risky/plausible; got %s/%s", status, reason)
	}
	if skip == "" {
		t.Fatal("the skipped probe must say why (signals.smtp_skipped_reason), got none")
	}
	t.Logf("dirty resolver → node not called, verdict %s/%s, smtp_skipped_reason=%s", status, reason, skip)
}

func TestResolverHygiene_Control_ACleanAuditRunsTheProbe(t *testing.T) {
	status, reason, skip, called := routedAfterAudit(t, node.SimGreen(testEgressIP, time.Now().UTC().Format(time.RFC3339)))
	if called != 1 || skip != "" {
		t.Fatalf("CONTROL FAILED: with a clean audit the probe should run exactly once with no skip reason; called=%d skip=%q (%s/%s) — so 'node never called' above proves nothing", called, skip, status, reason)
	}
	if status != vocab.StatusValid {
		t.Fatalf("clean audit + accepting node → valid, got %s/%s", status, reason)
	}
}
