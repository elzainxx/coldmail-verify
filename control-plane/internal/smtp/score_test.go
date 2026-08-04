package smtp

import (
	"testing"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// These tests lock the §7.2 map + the §5.5 catch-all truth table + §6.5 cache-settled predicate.
// The single most important invariant (P2 Appendix D, "the single most important line in the
// spec"): valid is minted ONLY on target-250 AND probe-explicitly-rejected.

func acceptedTarget() Parsed {
	// A clean 250 on RCPT with no error → CleanSuccess.
	return Parsed{Reached: true, PortOpened: true, Code: "250", Stage: "rcptto", CleanSuccess: true}
}

// §5.5 row 4 — the ONLY valid path.
func TestScore_Valid_OnlyWhenProbeRejected(t *testing.T) {
	in := Input{
		Target:        acceptedTarget(),
		EngineSuccess: true,
		Catchall:      &CatchallBlock{Probed: true, Conclusion: ConclusionNotCatchAll},
	}
	r := Score(in)
	if r.Status != vocab.StatusValid || r.Reason != vocab.ReasonAccepted {
		t.Fatalf("target 250 + probe rejected → valid/accepted, got %s/%s", r.Status, r.Reason)
	}
	if !r.Cacheable {
		t.Fatal("a settled valid must be cacheable")
	}
	if r.Confidence != confCleanValid {
		t.Fatalf("clean valid confidence = %d, want %d", r.Confidence, confCleanValid)
	}
}

// §5.5 row 3 — target 250 + probe 250 → catch-all → risky, NEVER valid.
func TestScore_CatchAll_IsRiskyNeverValid(t *testing.T) {
	in := Input{
		Target:        acceptedTarget(),
		EngineSuccess: true,
		Catchall:      &CatchallBlock{Probed: true, Conclusion: ConclusionCatchAll},
	}
	r := Score(in)
	if r.Status == vocab.StatusValid {
		t.Fatal("catch-all domain must NEVER mint valid (§5.5 row 3)")
	}
	if r.Status != vocab.StatusRisky || r.Reason != vocab.ReasonCatchAll {
		t.Fatalf("want risky/catch_all, got %s/%s", r.Status, r.Reason)
	}
}

// §5.5 rows 5/7 — inconclusive probe → risky, never valid (harden toward risky).
func TestScore_InconclusiveProbe_IsRiskyNeverValid(t *testing.T) {
	in := Input{
		Target:        acceptedTarget(),
		EngineSuccess: true,
		Catchall:      &CatchallBlock{Probed: true, Conclusion: ConclusionInconclusive},
	}
	r := Score(in)
	if r.Status == vocab.StatusValid {
		t.Fatal("inconclusive probe must NEVER mint valid (§5.5 row 5)")
	}
	if r.Status != vocab.StatusRisky {
		t.Fatalf("want risky, got %s/%s", r.Status, r.Reason)
	}
}

// §5.5 row 6 — accepted but NOT probed → risky/plausible, never valid.
func TestScore_NotProbed_IsRiskyPlausibleNeverValid(t *testing.T) {
	in := Input{Target: acceptedTarget(), EngineSuccess: true, Catchall: nil}
	r := Score(in)
	if r.Status == vocab.StatusValid {
		t.Fatal("unprobed accepted must NEVER mint valid (§5.5 row 6)")
	}
	if r.Status != vocab.StatusRisky || r.Reason != vocab.ReasonPlausible {
		t.Fatalf("want risky/plausible, got %s/%s", r.Status, r.Reason)
	}
	if s, _ := r.Signals["catchall_conclusion"].(string); s != ConclusionNotProbed {
		t.Fatalf("catchall_conclusion must be not_probed, got %q", s)
	}
}

// §5.5 row 1 — address-fault on target is decisive; the probe is irrelevant.
func TestScore_AddressFault_DecisiveOverProbe(t *testing.T) {
	in := Input{
		Target:   Parsed{Reached: true, PortOpened: true, Code: "550", Stage: "rcptto", Body: "550 user unknown"},
		Catchall: &CatchallBlock{Probed: true, Conclusion: ConclusionCatchAll}, // even a catch-all probe
	}
	r := Score(in)
	if r.Status != vocab.StatusInvalid || r.Reason != vocab.ReasonMailboxNotFound {
		t.Fatalf("address-fault is decisive → invalid/mailbox_not_found, got %s/%s", r.Status, r.Reason)
	}
	if !r.Cacheable {
		t.Fatal("a settled invalid must be cacheable")
	}
}

// §6.8 — a safe-check pass is scored below a clean 250 and logged distinctly.
func TestScore_SafeCheckPass_ReducedConfidenceAndDistinct(t *testing.T) {
	in := Input{
		// success true but NOT a clean 250 (smtp_debug present) → safe pass.
		Target:        Parsed{Reached: true, PortOpened: true, Code: "250", Stage: "rcptto", CleanSuccess: false},
		EngineSuccess: true,
		SafeCheckOn:   true,
		Catchall:      &CatchallBlock{Probed: true, Conclusion: ConclusionNotCatchAll},
	}
	r := Score(in)
	if r.Status != vocab.StatusValid {
		t.Fatalf("safe-check pass + probe rejected is still valid, got %s", r.Status)
	}
	if r.Confidence != confSafePassValid || r.Confidence >= confCleanValid {
		t.Fatalf("safe-pass confidence %d must be capped below clean %d", r.Confidence, confCleanValid)
	}
	if r.Signals["smtp_safe_pass"] != true {
		t.Fatal("safe-pass must be logged distinctly via smtp_safe_pass signal")
	}
}

// §6.5 / OQ-P2-6 — transient/your-fault unknowns are NEVER cacheable.
func TestScore_TransientUnknowns_NeverCacheable(t *testing.T) {
	cases := []struct {
		name string
		p    Parsed
	}{
		{"greylist_450", Parsed{Reached: true, PortOpened: true, Code: "450", Stage: "rcptto", Body: "450 greylisted"}},
		{"timeout", Parsed{PortOpened: true, Timeout: true, Stage: "connection", Body: "server response timeout"}},
		{"block_554", Parsed{Reached: true, PortOpened: true, Code: "554", Stage: "rcptto", Body: "554 blocked by policy"}},
		{"conn_failed", Parsed{PortOpened: false, Stage: ""}},
		{"ambiguous_550", Parsed{Reached: true, PortOpened: true, Code: "550", Stage: "rcptto", Body: "550 rejected"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Score(Input{Target: c.p})
			if r.Status != vocab.StatusUnknown {
				t.Fatalf("%s must be unknown, got %s/%s", c.name, r.Status, r.Reason)
			}
			if r.Cacheable {
				t.Fatalf("%s is a transient unknown → must NOT be cacheable (§6.5)", c.name)
			}
		})
	}
}

// §6.5 greylist requeue shape — the retry metadata is present (nothing retries it in P2).
func TestScore_Greylist_CarriesRequeueShape(t *testing.T) {
	r := Score(Input{Target: Parsed{Reached: true, PortOpened: true, Code: "450", Stage: "rcptto", Body: "450 greylisted"}})
	if r.Signals["greylisted"] != true || r.Signals["retry_eligible"] != true {
		t.Fatalf("greylist must carry greylisted+retry_eligible signals, got %+v", r.Signals)
	}
	if _, ok := r.Signals["retry_after_hint_s"]; !ok {
		t.Fatal("greylist must carry retry_after_hint_s (queue seam)")
	}
}

// §6.6 — DNS divergence is logged; engine-MX is recorded as authoritative.
func TestScore_DNSDivergence_Logged(t *testing.T) {
	in := Input{
		Target:        acceptedTarget(),
		EngineSuccess: true,
		Catchall:      &CatchallBlock{Probed: true, Conclusion: ConclusionNotCatchAll},
		BrainProvider: "google",
		EngineMXHosts: []string{"1.2.3.4"},
		BrainSawMX:    false, // brain saw no MX, engine used one → divergence
	}
	r := Score(in)
	if r.Signals["dns_divergence"] != true {
		t.Fatalf("brain/engine MX mismatch must log dns_divergence, got %+v", r.Signals)
	}
	if r.Signals["brain_mx_provider"] != "google" {
		t.Fatal("advisory brain provider must be recorded")
	}
}

// The frozen invariant restated at the score level: no non-row-4 accepted path is ever valid.
func TestScore_ValidInvariant_Exhaustive(t *testing.T) {
	for _, concl := range []string{ConclusionCatchAll, ConclusionInconclusive, ConclusionNotProbed, ""} {
		var cb *CatchallBlock
		if concl != "" {
			cb = &CatchallBlock{Probed: true, Conclusion: concl}
		}
		r := Score(Input{Target: acceptedTarget(), EngineSuccess: true, Catchall: cb})
		if r.Status == vocab.StatusValid {
			t.Fatalf("conclusion %q produced valid — only not_catchall may (§5.5 row 4)", concl)
		}
	}
}

// CacheableReason predicate parity (§6.5) — a single testable predicate.
func TestCacheableReason_SettledOnly(t *testing.T) {
	if !CacheableReason(vocab.StatusValid, vocab.ReasonAccepted) {
		t.Fatal("valid is settled/cacheable")
	}
	if !CacheableReason(vocab.StatusInvalid, vocab.ReasonMailboxNotFound) {
		t.Fatal("invalid is settled/cacheable")
	}
	if !CacheableReason(vocab.StatusRisky, vocab.ReasonCatchAll) {
		t.Fatal("risky/catch_all is settled/cacheable")
	}
	if CacheableReason(vocab.StatusUnknown, vocab.ReasonGreylisted) {
		t.Fatal("any unknown must NOT be cacheable")
	}
}
