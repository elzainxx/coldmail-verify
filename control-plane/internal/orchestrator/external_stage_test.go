package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/external"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the Phase-6 end-to-end behavioral fail-verify suite (§15 acceptance criteria), driving a
// REAL orchestrator + MemStore + a controllable mock vendor through the full compute→settle path.
// Real behavioral tests, not mocks-asserting-mocks. The GAP-47 honesty holds: we test the MECHANICS
// (routing, anti-laundering, spend-once, breaker, cache-no-rebill, global-kill, privacy) against the
// mock — the vendor's real ACCURACY on the residue is PENDING-REAL-VENDOR (phase-6-status.md).

// gmailResolver routes gmail.com to Google's MX so the cheap path settles risky/provider_no_smtp — a
// TERMINAL residue class (b) escalatable inline WITHOUT an SMTP node (the cleanest end-to-end residue
// for a unit test: provider_routing produces provider_no_smtp from the cheap layers alone).
func gmailResolver() fixtureResolver {
	return fixtureResolver{mx: map[string][]layers.MXHost{
		"gmail.com":     {{Host: "gmail-smtp-in.l.google.com", Pref: 10}},
		"acmecorp.com":  {{Host: "mx.acmecorp.com", Pref: 10}},
		"disposdom.com": {{Host: "mx.disposdom.com", Pref: 10}},
	}}
}

// extSetup builds an orchestrator with a mock vendor wired as the fallback stage, plus a policy whose
// external block escalates all four residue classes (the shipped cold-email policy). It returns the
// orchestrator, the store (to assert LOG + external_calls), the policy, and the mock (to assert
// calls). budget/breaker default to permissive (unset caps, error-rate off) unless a test overrides.
func extSetup(t *testing.T, out external.MockOutcome) (*Orchestrator, *store.MemStore, *policy.Definition, *external.MockVendor) {
	t.Helper()
	h, err := hash.NewHasher([]byte("p6-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	mem := store.NewMem(func() time.Time { return now }) // same clock as the orchestrator (cache TTL)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	mv := external.NewMockVendor("millionverifier", out)
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: gmailResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:p6", Now: func() time.Time { return now },
		External: &ExternalDeps{
			Vendor:  mv,
			Budget:  external.Budget{},                                    // all caps unset (not enforced)
			Breaker: external.BreakerParams{Cooldown: 5 * time.Minute},    // error-rate off by default
			Now:     func() time.Time { return now },
		},
	})
	return o, mem, def, mv
}

// AC-1 + AC-2 (the headline): a provider_no_smtp residue escalates, and a vendor "valid" settles
// risky/external_verified at ≤ cap — NEVER status:valid. This is the anti-laundering demo.
func TestExternal_ProviderResidueEscalates_VendorValidIsNeverValid(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid, StatusRaw: "ok", ConfidenceRaw: 99})
	resp, err := o.Verify(context.Background(), "someone@gmail.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status == vocab.StatusValid {
		t.Fatal("ANTI-LAUNDERING BROKEN: a vendor 'valid' produced status:valid (§3.4)")
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonExternalVerified {
		t.Fatalf("vendor valid on residue => risky/external_verified, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Confidence > 60 {
		t.Errorf("external confidence %d exceeds the cap 60 — a rented verdict is never SMTP-grade", resp.Confidence)
	}
	if mv.Calls() != 1 {
		t.Errorf("expected exactly one vendor call for the residue, got %d", mv.Calls())
	}
	// The provenance bag is populated (§6.2) and the vendor JSON rode the raw sidecar (§3.3).
	ext, _ := resp.Signals["external"].(map[string]any)
	if ext == nil || ext["vendor"] != "millionverifier" || ext["residue_class"] != "provider_no_smtp" {
		t.Errorf("signals.external provenance not populated: %+v", resp.Signals["external"])
	}
	// Exactly one settled verdict + exactly one external-call spend record (§6.1).
	if len(mem.Log) != 1 {
		t.Fatalf("exactly one LOG row, got %d", len(mem.Log))
	}
	if len(mem.ExternalCalls) != 1 {
		t.Fatalf("exactly one external-call spend record, got %d", len(mem.ExternalCalls))
	}
	if mem.ExternalCalls[0].Outcome != "ok" || mem.ExternalCalls[0].CostUnits != 1 {
		t.Errorf("spend record: %+v", mem.ExternalCalls[0])
	}
	if mem.ExternalCalls[0].VerificationID != mem.Log[0].ID {
		t.Error("spend record must point at the settled verdict (the rented population is joinable — §6.3)")
	}
}

// AC-2 FAIL-VERIFY (the anti-laundering test must BREAK if the cap is removed). We call the resolver
// directly with a would-be-laundering cap to prove the guard is the ONLY thing stopping SMTP-grade
// valid — and assert the SHIPPED path never does it. (external.Resolve is where the rule lives; this
// documents that removing the valid→risky mapping there is what a fail-verify neuters.)
func TestExternal_AntiLaunderingFailVerify_Documented(t *testing.T) {
	// The shipped resolver: a vendor valid is risky, capped — no matter the cap value.
	got := external.Resolve(external.VendorResult{Result: vocab.StatusValid, ConfidenceRaw: 100}, external.ResidueProviderNoSMTP, 60)
	if got.Status == vocab.StatusValid {
		t.Fatal("fail-verify anchor: external.Resolve mapped a vendor valid to status:valid — the anti-laundering rule is gone")
	}
	// Even with an absurd cap, valid never appears — the STATUS mapping (not the cap) forbids it.
	got = external.Resolve(external.VendorResult{Result: vocab.StatusValid, ConfidenceRaw: 100}, external.ResidueProviderNoSMTP, 100)
	if got.Status != vocab.StatusRisky {
		t.Fatalf("a vendor valid must be risky regardless of the cap value, got %s", got.Status)
	}
}

// AC-3 (the cost firewall): a clean corp residue that is NOT a route_when class, a hard invalid, and a
// disposable-risky are NEVER escalated — no vendor call, no charge. (risky/plausible without a residue
// reason is not in the closed set; syntax-invalid and disposable stop at a cheap gate.)
func TestExternal_NeverEscalatedSet(t *testing.T) {
	// A clean corp address settles risky/plausible — NOT a residue class → no escalation.
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid})
	if _, err := o.Verify(context.Background(), "jane@acmecorp.com", def); err != nil {
		t.Fatalf("Verify corp: %v", err)
	}
	// A hard invalid (bad syntax) stops at the syntax gate → no escalation.
	if _, err := o.Verify(context.Background(), "not-an-email", def); err != nil {
		t.Fatalf("Verify invalid: %v", err)
	}
	// A disposable domain settles risky/disposable (a cheap gate) → no escalation.
	mem2 := disposableSetup(t, mv)
	_ = mem2
	if mv.Calls() != 0 {
		t.Fatalf("COST FIREWALL BREACH: %d vendor call(s) on non-residue verdicts — none must escalate (§4.2)", mv.Calls())
	}
	if len(mem.ExternalCalls) != 0 {
		t.Fatalf("no spend record may exist for non-residue verdicts, got %d", len(mem.ExternalCalls))
	}
}

// disposableSetup seeds a disposable domain and verifies an address there settles risky/disposable
// (a cheap gate, never escalated). Shares the mock so the caller can assert zero calls.
func disposableSetup(t *testing.T, mv *external.MockVendor) *store.MemStore {
	t.Helper()
	h, _ := hash.NewHasher([]byte("p6-test-pepper"))
	mem := store.NewMem(nil)
	mem.SeedDisposable("disposdom.com")
	def, _ := policy.ColdEmailDefinition()
	now := time.Unix(1_700_000_000, 0)
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: gmailResolver(), CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:p6",
		Now: func() time.Time { return now },
		External: &ExternalDeps{Vendor: mv, Now: func() time.Time { return now }},
	})
	resp, err := o.Verify(context.Background(), "x@disposdom.com", def)
	if err != nil {
		t.Fatalf("Verify disposable: %v", err)
	}
	if resp.Reason != vocab.ReasonDisposable {
		t.Fatalf("disposable domain => risky/disposable, got %s/%s", resp.Status, resp.Reason)
	}
	return mem
}

// AC-9 (cache re-read does not re-bill — GAP-43): the second identical verify hits the cache and
// returns the stored external_verified verdict with NO second vendor call and NO second charge.
func TestExternal_CacheReReadDoesNotReBill(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid, StatusRaw: "ok"})
	if _, err := o.Verify(context.Background(), "dup@gmail.com", def); err != nil {
		t.Fatalf("Verify #1: %v", err)
	}
	if mv.Calls() != 1 {
		t.Fatalf("first verify must call the vendor once, got %d", mv.Calls())
	}
	// Second identical verify → cache hit → the escalation was decided ONCE at verdict-mint (§4.3).
	resp2, err := o.Verify(context.Background(), "dup@gmail.com", def)
	if err != nil {
		t.Fatalf("Verify #2: %v", err)
	}
	if !resp2.Meta.FromCache {
		t.Fatal("second identical verify must be a cache hit")
	}
	if resp2.Reason != vocab.ReasonExternalVerified {
		t.Errorf("cached rented verdict must return external_verified, got %s", resp2.Reason)
	}
	if mv.Calls() != 1 {
		t.Fatalf("RE-BILL: cache re-read triggered a second vendor call (%d) — escalate-once at mint is broken (GAP-43)", mv.Calls())
	}
	if len(mem.ExternalCalls) != 1 {
		t.Fatalf("cache re-read must not add a spend record, got %d", len(mem.ExternalCalls))
	}
}

// AC-5 (breaker open → keep_residue): with the per-day cap already exhausted the breaker is open, so
// the residue settles AS-IS (risky/provider_no_smtp) — never a fabricated verdict, never a vendor call.
func TestExternal_BreakerOpen_KeepsResidue(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid})
	// Pre-load the day's spend at the cap by wiring a tiny daily cap and a pre-existing spend record.
	o.external.Budget = external.Budget{DailyCap: 1}
	now := o.extNow()
	_ = mem.AppendExternalCall(context.Background(), store.ExternalCallRow{
		ID: "018f0000-0000-7000-8000-0000000000c1", Vendor: "millionverifier", Outcome: "ok",
		CostUnits: 5, CalledAt: now.Add(-time.Minute),
	})
	resp, err := o.Verify(context.Background(), "budget@gmail.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Reason != vocab.ReasonProviderNoSMTP {
		t.Fatalf("breaker open must keep_residue (risky/provider_no_smtp), got %s/%s", resp.Status, resp.Reason)
	}
	if mv.Calls() != 0 {
		t.Fatalf("breaker open must make NO vendor call, got %d", mv.Calls())
	}
}

// AC-10 + global-fallback-kill (AC via §5.5): a policy WITHOUT an external block never escalates
// (the mechanical opt-out lever), and the global kill stops all egress in one action.
func TestExternal_OptOut_And_GlobalKill(t *testing.T) {
	// Opt-out: a policy with NO external block → the layer never runs (more residue, full privacy).
	o, _, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid})
	optOut := *def
	optOut.External = nil // the opt-out lever (§7.5): a policy without the block
	if _, err := o.Verify(context.Background(), "optout@gmail.com", &optOut); err != nil {
		t.Fatalf("Verify opt-out: %v", err)
	}
	if mv.Calls() != 0 {
		t.Fatalf("a policy WITHOUT an external block must never escalate, got %d call(s)", mv.Calls())
	}

	// Global-fallback-kill: even with the opted-in policy, GloballyDisabled stops all egress.
	o2, _, def2, mv2 := extSetup(t, external.MockOutcome{Result: vocab.StatusValid})
	o2.external.GloballyDisabled = true
	resp, err := o2.Verify(context.Background(), "killed@gmail.com", def2)
	if err != nil {
		t.Fatalf("Verify global-kill: %v", err)
	}
	if mv2.Calls() != 0 {
		t.Fatalf("GLOBAL KILL BREACH: %d vendor call(s) with the global-fallback-kill on — it must stop ALL egress", mv2.Calls())
	}
	if resp.Reason != vocab.ReasonProviderNoSMTP {
		t.Errorf("global-kill must keep_residue, got %s/%s", resp.Status, resp.Reason)
	}
}

// AC (vendor error → keep_residue + error recorded): a vendor-down error keeps the residue and
// records an error row (Outcome=error, cost 0) so the breaker's error-rate window is measurable.
func TestExternal_VendorError_KeepsResidue_RecordsError(t *testing.T) {
	o, mem, def, _ := extSetup(t, external.MockOutcome{Err: external.ErrVendorDown})
	resp, err := o.Verify(context.Background(), "down@gmail.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Reason != vocab.ReasonProviderNoSMTP {
		t.Fatalf("vendor error must keep_residue, got %s/%s", resp.Status, resp.Reason)
	}
	if len(mem.ExternalCalls) != 1 || mem.ExternalCalls[0].Outcome != "error" || mem.ExternalCalls[0].CostUnits != 0 {
		t.Fatalf("a vendor error must record an error row (cost 0), got %+v", mem.ExternalCalls)
	}
}

// AC-11 (unrecognized vendor status → unknown → keep_residue): the mock returns an unknown result
// (as §8.2 normalizes an unrecognized status), so the residue stays first-class.
func TestExternal_VendorUnknown_KeepsResidue(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusUnknown, StatusRaw: "weird_new"})
	resp, err := o.Verify(context.Background(), "weird@gmail.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Reason != vocab.ReasonProviderNoSMTP {
		t.Fatalf("vendor unknown must keep_residue (principle 2), got %s/%s", resp.Status, resp.Reason)
	}
	// A call WAS made (we paid), so the provenance record exists (ok, real cost) — but the verdict is
	// unchanged. The vendor charging for its own "unknown" is real; P7 measures it.
	if mv.Calls() != 1 || len(mem.ExternalCalls) != 1 || mem.ExternalCalls[0].Outcome != "ok" {
		t.Errorf("vendor unknown: one ok call recorded, verdict unchanged; got calls=%d records=%d", mv.Calls(), len(mem.ExternalCalls))
	}
}

// Per-job spend cap enforced on the INLINE escalation path for a QUEUED bulk item (the review's
// HIGH finding): a queued provider_no_smtp residue escalates inline via ComputeForQueue(jobID);
// once the job's per_job_cap is reached, further escalations must keep_residue (no vendor call), and
// every recorded spend row must carry the real job_id. Regression guard for the closed hole.
func TestExternal_PerJobCap_EnforcedOnInlineQueuedEscalation(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid, CostUnits: 1})
	o.external.Budget = external.Budget{PerJobCap: 2} // this bulk job may spend at most 2 units
	jobID := "018f0000-0000-7000-8000-0000000j0b01"

	// Simulate the drainer computing 4 queued items of the SAME job (all @gmail.com → provider_no_smtp,
	// escalated inline). Each settled QueueOutcome's spend record would be recorded by the drainer's CAS;
	// here we record it directly (mirroring the settle) so the NEXT compute's budget read sees it.
	escalated := 0
	for i := 0; i < 4; i++ {
		out, err := o.ComputeForQueue(context.Background(), "user@gmail.com", def, jobID)
		if err != nil {
			t.Fatalf("ComputeForQueue #%d: %v", i, err)
		}
		if out.ExternalCall != nil {
			if out.ExternalCall.JobID != jobID {
				t.Fatalf("spend record must carry the real job_id, got %q", out.ExternalCall.JobID)
			}
			if out.ExternalCall.Outcome == "ok" && out.Row.Reason == string(vocab.ReasonExternalVerified) {
				escalated++
				// Record it so the next iteration's per-job budget read sees the spend (mirrors settle).
				_ = mem.AppendExternalCall(context.Background(), *out.ExternalCall)
			}
		}
	}
	// The cap is 2 → at most 2 escalations bill; the rest keep_residue (no vendor call past the cap).
	if escalated != 2 {
		t.Fatalf("per_job_cap=2 must bound the INLINE escalations to 2, got %d (the cost firewall hole is open)", escalated)
	}
	if mv.Calls() != 2 {
		t.Fatalf("per_job_cap=2 must bound vendor calls to 2, got %d", mv.Calls())
	}
}

// AC-8 + AC-9 (privacy: address-only egress): the vendor receives the plaintext ADDRESS and nothing
// else — no caller, no job, no other member — and the spend record stores email_hash, never plaintext.
func TestExternal_AddressOnlyEgress_NoPlaintextPersisted(t *testing.T) {
	o, mem, def, mv := extSetup(t, external.MockOutcome{Result: vocab.StatusValid})
	if _, err := o.Verify(context.Background(), "Privacy.Test@gmail.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The vendor saw exactly one address (normalized), nothing else.
	if len(mv.AddrsSeen) != 1 || mv.AddrsSeen[0] != "privacy.test@gmail.com" {
		t.Errorf("the vendor must receive the address ONLY (normalized), got %v", mv.AddrsSeen)
	}
	// The spend record stores email_hash (32 bytes), NEVER plaintext.
	rec := mem.ExternalCalls[0]
	if len(rec.EmailHash) != 32 {
		t.Errorf("spend record must store the 32-byte email_hash (Door 3), got %d bytes", len(rec.EmailHash))
	}
	// No field on the record holds the raw address (Door 3 — nothing plaintext persisted by P6).
	if rec.RequestID == "privacy.test@gmail.com" || rec.VendorStatusRaw == "privacy.test@gmail.com" {
		t.Error("no external-call field may hold the plaintext address (§7.3)")
	}
}
