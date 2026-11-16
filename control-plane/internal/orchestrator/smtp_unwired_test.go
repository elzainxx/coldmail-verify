package orchestrator

// M1-G6: with the SMTP stage unwired (SMTP disabled, or buildSMTP returned nil because no node / no config could be fetched) every residue
// address used to settle as risky/plausible with NO skip token — indistinguishable from a genuine cheap-layers answer — and be cached.
// It now carries signals.smtp_skipped_reason="smtp_unwired" and is NEVER cached (our own degraded state is not an answer).
//
// WHAT MUST NOT MOVE: the verdict (risky/plausible, confidence, recommendation) and, for a queued item, the retry decision: nothing in
// this process will ever make the stage appear, so the item must SETTLE (QSettled), never wait like a capacity gap would.
//
// THE CONTROL is the same address with the stage WIRED and the probe answering: its risky/plausible verdict is identical, carries no token,
// and IS cached — so the three differences below are caused by the missing stage, not by the verdict.

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// plausibleBody: a probe that ran, was accepted, and was NOT catch-all-probed: the scorer settles it risky/plausible (row 6 of the
// truth table, "not_probed"). The wired control for the unwired residue — same verdict, a real answer.
var plausibleBody = []byte(`{
  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":true,
  "smtp_debug":null,
  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)

// unwiredOrch is the orchestrator with the given (possibly half-built) SMTP deps.
func unwiredOrch(t *testing.T, smtpDeps *SMTPDeps) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("smtp-unwired-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	o := New(Deps{Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:test", SMTP: smtpDeps})
	return o, mem, def
}

func TestSMTPUnwired_TheTokenIsPresent_TheVerdictIsUnchanged_AndNothingIsCached(t *testing.T) {
	// Every way the stage can be "not wired": nothing at all, no client, no registry.
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	for _, tc := range []struct {
		name string
		deps *SMTPDeps
	}{
		{"SMTP is nil (disabled, or buildSMTP returned nil)", nil},
		{"SMTP deps with no client", &SMTPDeps{Registry: router.New(nodes, nil)}},
		{"SMTP deps with no registry", &SMTPDeps{Client: fakeNode{body: plausibleBody}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, mem, def := unwiredOrch(t, tc.deps)
			ctx := context.Background()
			resp, err := o.Verify(ctx, "jane@acmecorp.com", def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			// the verdict is exactly what an unwired residue always was
			if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible || resp.Confidence != 40 || resp.Recommendation != vocab.RecReview {
				t.Fatalf("verdict = %s/%s/%d/%s, want risky/plausible/40/review (the token must not move the verdict)", resp.Status, resp.Reason, resp.Confidence, resp.Recommendation)
			}
			// the token
			if got, _ := resp.Signals[vocab.SignalSMTPSkippedReason].(string); got != vocab.SkipSMTPUnwired {
				t.Fatalf("smtp_skipped_reason = %q, want %q", got, vocab.SkipSMTPUnwired)
			}
			if logged, _ := mem.Log[0].Signals[vocab.SignalSMTPSkippedReason].(string); logged != vocab.SkipSMTPUnwired {
				t.Errorf("the LOG row must carry the token too, got %q", logged)
			}
			// our own degraded state is labelled as ours
			if got, _ := resp.Signals[vocab.SignalFailureOrigin].(string); got != string(vocab.OriginOwn) {
				t.Errorf("failure_origin = %q, want own (a degraded cheap-only answer caused by us)", got)
			}
			// not cached: two identical requests both compute, neither is a cache hit, and nothing is in the cache
			if mem.CacheLen() != 0 {
				t.Fatalf("an unwired residue must NOT be cached, cache len = %d", mem.CacheLen())
			}
			again, err := o.Verify(ctx, "jane@acmecorp.com", def)
			if err != nil {
				t.Fatalf("second Verify: %v", err)
			}
			if again.Meta.FromCache || again.Flags["from_cache"] {
				t.Error("the second identical request was served from cache: the unwired residue was cached")
			}
			if len(mem.Log) != 2 {
				t.Errorf("both requests are logged (Door 2), got %d rows", len(mem.Log))
			}
		})
	}
}

// THE PAIRED CONTROL: the same address, the stage WIRED, the probe answering with the same risky/plausible verdict. No token, no
// failure_origin, and it IS cached and served from cache.
func TestSMTPUnwired_Control_AWiredPlausibleAnswerCarriesNoTokenAndIsCached(t *testing.T) {
	o, mem, def := smtpSetup(t, plausibleBody, nil, time.Hour)
	ctx := context.Background()
	resp, err := o.Verify(ctx, "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible || resp.Confidence != 40 || resp.Recommendation != vocab.RecReview {
		t.Fatalf("control verdict = %s/%s/%d/%s, want the SAME risky/plausible/40/review the unwired residue has", resp.Status, resp.Reason, resp.Confidence, resp.Recommendation)
	}
	if tok, has := resp.Signals[vocab.SignalSMTPSkippedReason]; has {
		t.Errorf("a wired, answered probe carries no skip token, got %v", tok)
	}
	if org, has := resp.Signals[vocab.SignalFailureOrigin]; has {
		t.Errorf("an answer carries no failure_origin, got %v", org)
	}
	if mem.CacheLen() != 1 {
		t.Fatalf("a wired plausible answer is a settled verdict and IS cached, cache len = %d", mem.CacheLen())
	}
	again, err := o.Verify(ctx, "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if !again.Meta.FromCache {
		t.Error("the second identical request must be a cache hit for the wired plausible answer")
	}
}

// The retry decision is untouched: an unwired item SETTLES in the queue (QSettled) — it must not become a capacity wait (QNoEligibleNode),
// which would requeue it forever because nothing in this process can ever wire the stage. Not cached either way. The red-node control shows
// the capacity-gap class IS what a node problem produces, so the assertion on the unwired class is not vacuous.
func TestSMTPUnwired_AQueuedItemSettlesAndDoesNotWait(t *testing.T) {
	ctx := context.Background()
	o, _, def := unwiredOrch(t, nil)
	out, err := o.ComputeForQueue(ctx, "jane@acmecorp.com", def, "")
	if err != nil {
		t.Fatalf("ComputeForQueue: %v", err)
	}
	if out.Class != QSettled {
		t.Fatalf("an unwired item must SETTLE (QSettled), got class %d: it would otherwise wait forever for a stage that can never appear", out.Class)
	}
	if out.Cache != nil {
		t.Error("an unwired item must not produce a cache row")
	}
	if out.LastReason != string(vocab.ReasonPlausible) {
		t.Errorf("last_reason = %q, want the verdict's own reason %q (unchanged)", out.LastReason, vocab.ReasonPlausible)
	}
	if got, _ := out.Row.Signals[vocab.SignalSMTPSkippedReason].(string); got != vocab.SkipSMTPUnwired {
		t.Errorf("the settled LOG row must carry the token, got %q", got)
	}

	// CONTROL: a wired stage whose only node is red is a CAPACITY gap: it waits (QNoEligibleNode), and is not cached either.
	ow, memw, defw := smtpSetup(t, plausibleBody, nil, time.Hour)
	memw.SeedNodeHealth(store.NodeHealth{NodeID: "n1", Status: "active", AuditStatus: "red", Port25Open: true, LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP})
	red, err := ow.ComputeForQueue(ctx, "jane@acmecorp.com", defw, "")
	if err != nil {
		t.Fatalf("ComputeForQueue (red): %v", err)
	}
	if red.Class != QNoEligibleNode || red.Cache != nil {
		t.Fatalf("control: a red node is a capacity wait, class=%d cache=%v, want class %d and no cache", red.Class, red.Cache, QNoEligibleNode)
	}

	// CONTROL: a wired, answering stage settles AND caches (the unwired item's missing cache row is the token's doing).
	oa, _, defa := smtpSetup(t, plausibleBody, nil, time.Hour)
	ans, err := oa.ComputeForQueue(ctx, "jane@acmecorp.com", defa, "")
	if err != nil {
		t.Fatalf("ComputeForQueue (answered): %v", err)
	}
	if ans.Class != QSettled || ans.Cache == nil {
		t.Fatalf("control: an answered plausible settles and caches, class=%d cache=%v", ans.Class, ans.Cache)
	}
}

// The token is declared once (vocab) and the orchestrator's handles are exactly those values — a rename on either side fails here.
func TestSMTPUnwired_TheTokenIsRegisteredWhereTheOthersAre(t *testing.T) {
	handles := map[string]string{
		"no_eligible_node": reasonNoEligibleNode, "audit_red": reasonAuditRed, "provider_mx_routed": reasonProviderMX,
		"warmup_cap": reasonWarmupCap, "rate_limited": reasonRateLimited, "smtp_unwired": reasonSMTPUnwired,
	}
	for want, got := range handles {
		if want != got {
			t.Errorf("the orchestrator's handle for %q is %q", want, got)
		}
	}
	if len(handles) != len(vocab.SkipTokens()) {
		t.Fatalf("the orchestrator writes %d tokens, the vocabulary lists %d: %v", len(handles), len(vocab.SkipTokens()), vocab.SkipTokens())
	}
	for _, tok := range vocab.SkipTokens() {
		if _, ok := handles[tok]; !ok {
			t.Errorf("vocab lists skip token %q but the orchestrator has no handle for it", tok)
		}
	}
}
