package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/external"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
	"github.com/coldmail-verify/control-plane/internal/zbtest"
)

// The ZeroBounce adapter behind the REAL external stage: a real orchestrator, a MemStore and the real HTTP
// adapter pointed at the stand-in (control-plane/test/zbstub; no real vendor, no real key). These prove the
// mechanics the owner's ruling lists — anti-laundering on every table row, the kill switch, the spend cap,
// spend-once, "disabled sends nothing", non-answers leaving M1's verdict standing, and that the key never
// reaches stored state or a log — each beside a paired control that changes exactly one thing. Every test
// counts only requests carrying its OWN tag at the stand-in, so "nothing was sent" is a count of zero.

const zbStageKey = "ZBKEY-STAGE-0000-0000"

type zbHarness struct {
	o      *Orchestrator
	mem    *store.MemStore
	def    *policy.Definition
	hasher *hash.Hasher
	zb     *external.ZeroBounce
}

// zbCapped is the budget buildExternal builds: the hard vendor cap enforced, everything else unset.
func zbCapped(n int) external.Budget {
	return external.Budget{VendorDailyCap: n, VendorDailyCapEnforced: true}
}

func zbStage(t *testing.T, budget external.Budget, mutate func(*ExternalDeps)) zbHarness {
	t.Helper()
	base := zbtest.URL(t)
	hasher, err := hash.NewHasher([]byte("zb-stage-test-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	mem := store.NewMem(func() time.Time { return now })
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	zb, err := external.NewZeroBounce(external.ZeroBounceConfig{APIKey: zbStageKey, BaseURL: base, Timeout: 5 * time.Second, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatalf("zerobounce: %v", err)
	}
	deps := &ExternalDeps{Vendor: zb, Budget: budget, Breaker: external.BreakerParams{Cooldown: 5 * time.Minute}, Now: func() time.Time { return now }}
	if mutate != nil {
		mutate(deps)
	}
	o := New(Deps{
		Hasher: hasher, Store: mem, Resolver: gmailResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:zb", Now: func() time.Time { return now }, External: deps,
	})
	return zbHarness{o: o, mem: mem, def: def, hasher: hasher, zb: zb}
}

// stageWant is the expected M1 verdict for one answered pair, per residue-class group — typed here from
// Docs/vendor-zerobounce.md, not derived from the adapter's table.
type stageWant struct {
	status, sub              string
	atCatchAllOrProvider     vocab.Status // OQ-P6-3: a vendor "no" is downgraded to risky on accept-all / :mx residue
	atRetryExhaustedOrGaveUp vocab.Status // ... and accepted as invalid where non-existence is plausible
}

var stageRows = []stageWant{
	{"valid", "", vocab.StatusRisky, vocab.StatusRisky},
	{"valid", "leading_period_removed", vocab.StatusRisky, vocab.StatusRisky},
	{"valid", "gold", vocab.StatusRisky, vocab.StatusRisky},
	{"valid", "role_based_accept_all", vocab.StatusRisky, vocab.StatusRisky},
	{"valid", "ai_agent_mailbox", vocab.StatusRisky, vocab.StatusRisky},
	{"catch-all", "", vocab.StatusRisky, vocab.StatusRisky},
	{"catch-all", "ai_agent_mailbox", vocab.StatusRisky, vocab.StatusRisky},
	{"invalid", "mailbox_not_found", vocab.StatusRisky, vocab.StatusInvalid},
	{"invalid", "no_dns_entries", vocab.StatusRisky, vocab.StatusInvalid},
	{"invalid", "failed_syntax_check", vocab.StatusRisky, vocab.StatusInvalid},
	{"invalid", "does_not_accept_mail", vocab.StatusRisky, vocab.StatusInvalid},
	{"invalid", "mailbox_quota_exceeded", vocab.StatusRisky, vocab.StatusRisky},
	{"invalid", "possible_typo", vocab.StatusRisky, vocab.StatusRisky},
	{"invalid", "unroutable_ip_address", vocab.StatusRisky, vocab.StatusRisky},
	{"spamtrap", "", vocab.StatusRisky, vocab.StatusInvalid},
	{"abuse", "", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "disposable", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "role_based", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "role_based_catch_all", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "toxic", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "global_suppression", vocab.StatusRisky, vocab.StatusRisky},
	{"do_not_mail", "possible_trap", vocab.StatusRisky, vocab.StatusRisky},
}

func (w stageWant) behaviour() string {
	if w.sub == "" {
		return w.status
	}
	return w.status + "--" + w.sub
}

// residue builds the settled residue verdict the stage is handed for a class.
func residue(class external.ResidueClass) policy.Verdict {
	switch class {
	case external.ResidueRetryExhausted:
		return policy.Verdict{Status: vocab.StatusUnknown, Reason: vocab.ReasonGreylisted, Confidence: 10, Recommendation: vocab.RecReview, Signals: map[string]any{}}
	case external.ResidueGaveUpRateLimited:
		return policy.Verdict{Status: vocab.StatusRisky, Reason: vocab.ReasonPlausible, Confidence: 40, Recommendation: vocab.RecReview, Signals: map[string]any{"smtp_skipped_reason": "rate_limited"}}
	case external.ResidueProviderNoSMTP:
		return policy.Verdict{Status: vocab.StatusRisky, Reason: vocab.ReasonProviderNoSMTP, Confidence: 35, Recommendation: vocab.RecReview, Signals: map[string]any{}}
	default:
		return policy.Verdict{Status: vocab.StatusRisky, Reason: vocab.ReasonCatchAll, Confidence: 40, Recommendation: vocab.RecReview, Signals: map[string]any{}}
	}
}

// EVERY answered row through the real stage, on a class from each OQ-P6-3 group. The vendor's "yes" is never
// valid; the vendor's "no" is invalid only where non-existence is plausible; the reason is always
// external_verified and the confidence is the policy cap; exactly one request and one 1-credit ok record.
func TestZB_Stage_EveryAnsweredRow_AntiLaundering(t *testing.T) {
	zbtest.URL(t)
	hn := zbStage(t, zbCapped(1000), nil)
	groups := []struct {
		name  string
		class external.ResidueClass
		want  func(stageWant) vocab.Status
	}{
		{"catch_all", external.ResidueCatchAll, func(w stageWant) vocab.Status { return w.atCatchAllOrProvider }},
		{"provider_no_smtp", external.ResidueProviderNoSMTP, func(w stageWant) vocab.Status { return w.atCatchAllOrProvider }},
		{"retry_exhausted", external.ResidueRetryExhausted, func(w stageWant) vocab.Status { return w.atRetryExhaustedOrGaveUp }},
		{"gave_up_rate_limited", external.ResidueGaveUpRateLimited, func(w stageWant) vocab.Status { return w.atRetryExhaustedOrGaveUp }},
	}
	invalids := map[string]int{}
	for _, g := range groups {
		for _, row := range stageRows {
			t.Run(g.name+"/"+row.behaviour(), func(t *testing.T) {
				tag := zbtest.Tag(t)
				addr := zbtest.Addr(row.behaviour(), tag, "gmail.com")
				out := hn.o.runExternal(context.Background(), addr, residue(g.class), g.class, hn.def, "", "", hn.hasher.EmailHash(addr))
				if !out.Escalated {
					t.Fatalf("an answered row did not escalate: %+v", out)
				}
				v := out.Verdict
				if v.Status == vocab.StatusValid {
					t.Fatalf("ANTI-LAUNDERING BROKEN: ZeroBounce %s resolved to status:valid", row.behaviour())
				}
				if want := g.want(row); v.Status != want {
					t.Errorf("status = %s, want %s", v.Status, want)
				}
				if v.Reason != vocab.ReasonExternalVerified {
					t.Errorf("reason = %s, want external_verified", v.Reason)
				}
				if v.Confidence != 60 {
					t.Errorf("confidence = %d, want the policy cap 60 (ZeroBounce reports none of its own)", v.Confidence)
				}
				if !vocab.ComposabilityOK(v.Status, v.Recommendation) {
					t.Errorf("forbidden combination %s + %s", v.Status, v.Recommendation)
				}
				if v.Status == vocab.StatusInvalid {
					invalids[g.name]++
					if v.Recommendation != vocab.RecSkip {
						t.Errorf("an invalid verdict must recommend skip, got %s", v.Recommendation)
					}
				}
				c := out.Call
				if c == nil || c.Outcome != "ok" || c.CostUnits != 1 || c.Vendor != "zerobounce" {
					t.Fatalf("spend record = %+v, want an ok, 1-credit zerobounce record", c)
				}
				wantRaw := row.status
				if row.sub != "" {
					wantRaw += "/" + row.sub
				}
				if c.VendorStatusRaw != wantRaw {
					t.Errorf("vendor_status_raw = %q, want %q", c.VendorStatusRaw, wantRaw)
				}
				ext, _ := v.Signals["external"].(map[string]any)
				if ext == nil || ext["vendor"] != "zerobounce" || ext["residue_class"] != string(g.class) {
					t.Errorf("signals.external provenance = %+v", v.Signals["external"])
				}
				if bytes.Contains(out.RawBlob, []byte(addr)) || bytes.Contains(out.RawBlob, []byte("West Palm Beach")) {
					t.Errorf("the raw sidecar kept the address or personal data: %s", out.RawBlob)
				}
				if got := zbtest.Calls(t, tag); got != 1 {
					t.Errorf("the stand-in saw %d requests, want 1", got)
				}
			})
		}
	}
	// The loop must have reached the asymmetric rows on both sides (a vendor 'no' accepted only where plausible).
	if invalids["catch_all"] != 0 || invalids["provider_no_smtp"] != 0 {
		t.Errorf("a vendor 'no' became invalid on accept-all / :mx residue: %v (OQ-P6-3)", invalids)
	}
	if invalids["retry_exhausted"] != 5 || invalids["gave_up_rate_limited"] != 5 {
		t.Errorf("hard negatives accepted as invalid on the plausible classes = %v, want 5 each (4 invalid + spamtrap)", invalids)
	}
}

// A non-answer — a vendor `unknown`, a pair nobody documented, a status nobody has seen, and every failure of
// transport or body — is NOT a vendor outcome: the residue is not replaced, the call is recorded as an ERROR
// (so the breaker's window sees it) with cost 0 and no vendor status (no vendor-charged marker), and a full
// Verify returns M1's own verdict unchanged. The control is an answered row beside them.
func TestZB_Stage_NonAnswers_ResidueStands(t *testing.T) {
	zbtest.URL(t)
	behaviours := []string{
		"unknown", "unknown--greylisted", "unknown--mail_server_temporary_error", "unknown--timeout_exceeded", "unknown--antispam_system",
		"valid--accept_all", "invalid--blocked", "do_not_mail--allowed", "totally_new_status", "valid--some_future_sub_status",
		"fail--error_body_key", "fail--error_body_credits", "fail--error_body_echo_key", "fail--http_429", "fail--http_500",
		"fail--http_403_html", "fail--http_302", "fail--malformed", "fail--truncated", "fail--message_only", "fail--unknown_status",
	}
	for _, b := range behaviours {
		t.Run(b, func(t *testing.T) {
			hn := zbStage(t, zbCapped(1000), nil) // a fresh client per row: a 429 pauses the client that saw it
			tag := zbtest.Tag(t)
			// (1) the stage on its own: nothing escalates, and the spend record is an uncharged error.
			addr := zbtest.Addr(b, tag, "gmail.com")
			cheap := residue(external.ResidueCatchAll)
			out := hn.o.runExternal(context.Background(), addr, cheap, external.ResidueCatchAll, hn.def, "", "", hn.hasher.EmailHash(addr))
			if out.Escalated || out.Verdict.Reason != "" || len(out.RawBlob) != 0 {
				t.Fatalf("a non-answer escalated: %+v", out)
			}
			c := out.Call
			if c == nil || c.Outcome != "error" || c.CostUnits != 0 || c.VendorStatusRaw != "" || c.RequestID != "" {
				t.Fatalf("spend record = %+v, want an uncharged error with no vendor status", c)
			}
			if got := zbtest.Calls(t, tag); got != 1 {
				t.Errorf("the stand-in saw %d requests, want 1", got)
			}

			// (2) a full Verify of a fresh address of the same behaviour returns M1's OWN verdict.
			tag2 := zbtest.Tag(t)
			resp, err := hn.o.Verify(context.Background(), zbtest.Addr(b, tag2, "gmail.com"), hn.def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonProviderNoSMTP {
				t.Errorf("M1's residue verdict must stand, got %s/%s", resp.Status, resp.Reason)
			}
			if _, has := resp.Signals["external"]; has {
				t.Errorf("a non-answer left an external provenance bag on the verdict: %+v", resp.Signals["external"])
			}
			// (3) the record the settler appended for that Verify is an uncharged error.
			if len(hn.mem.ExternalCalls) != 1 {
				t.Fatalf("%d spend records after one Verify, want 1", len(hn.mem.ExternalCalls))
			}
			if c := hn.mem.ExternalCalls[0]; c.Outcome != "error" || c.CostUnits != 0 || c.VendorStatusRaw != "" {
				t.Errorf("the stored record carries a vendor-charged marker: %+v", c)
			}
		})
	}
	// Control: an answered row escalates through an identical harness and IS recorded ok with its cost.
	hn := zbStage(t, zbCapped(1000), nil)
	tag := zbtest.Tag(t)
	resp, err := hn.o.Verify(context.Background(), zbtest.Addr("valid", tag, "gmail.com"), hn.def)
	if err != nil || resp.Reason != vocab.ReasonExternalVerified {
		t.Fatalf("control: an answered row must escalate, got %+v / %v", resp, err)
	}
	if len(hn.mem.ExternalCalls) != 1 || hn.mem.ExternalCalls[0].Outcome != "ok" || hn.mem.ExternalCalls[0].CostUnits != 1 {
		t.Errorf("control: an answered row must be recorded ok with its cost, got %+v", hn.mem.ExternalCalls)
	}
}

// The kill switch: with the global-fallback-kill pulled the vendor receives NOTHING, however many residues
// arrive; with it released the same address is sent. (buildExternal also returns nil when the rung is off —
// pinned in cmd/control-plane.)
func TestZB_Stage_KillSwitch_StopsAllCalls(t *testing.T) {
	zbtest.URL(t)
	tag := zbtest.Tag(t)
	killed := zbStage(t, zbCapped(1000), func(d *ExternalDeps) { d.GloballyDisabled = true })
	for i := 0; i < 3; i++ {
		resp, err := killed.o.Verify(context.Background(), fmt.Sprintf("valid+%s+n%d@gmail.com", tag, i), killed.def)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Reason != vocab.ReasonProviderNoSMTP {
			t.Errorf("the kill switch must keep_residue, got %s/%s", resp.Status, resp.Reason)
		}
	}
	if got := zbtest.Calls(t, tag); got != 0 {
		t.Fatalf("KILL SWITCH BREACH: %d request(s) reached the vendor with the global-fallback-kill pulled", got)
	}
	// Paired control: the same addresses with the switch released DO reach the vendor.
	live := zbStage(t, zbCapped(1000), nil)
	for i := 0; i < 3; i++ {
		resp, err := live.o.Verify(context.Background(), fmt.Sprintf("valid+%s+n%d@gmail.com", tag, i), live.def)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Reason != vocab.ReasonExternalVerified {
			t.Errorf("control: with the switch released the residue must escalate, got %s/%s", resp.Status, resp.Reason)
		}
	}
	if got := zbtest.Calls(t, tag); got != 3 {
		t.Fatalf("control: expected 3 requests with the switch released, got %d", got)
	}
}

// The hard daily spend cap: the Nth call is made, the N+1th is REFUSED and the stand-in's counter proves it;
// a cap of 0 (the default) sends nothing at all. Controls: a larger cap lets all five through.
func TestZB_Stage_DailySpendCap_NPlusOneRefused(t *testing.T) {
	zbtest.URL(t)
	run := func(t *testing.T, limit, n int) (escalated, calls int, mem *store.MemStore) {
		t.Helper()
		hn := zbStage(t, zbCapped(limit), nil)
		tag := zbtest.Tag(t)
		for i := 0; i < n; i++ {
			resp, err := hn.o.Verify(context.Background(), fmt.Sprintf("valid+%s+n%d@gmail.com", tag, i), hn.def)
			if err != nil {
				t.Fatal(err)
			}
			switch resp.Reason {
			case vocab.ReasonExternalVerified:
				escalated++
			case vocab.ReasonProviderNoSMTP: // refused: M1's own verdict stands
			default:
				t.Fatalf("unexpected verdict %s/%s", resp.Status, resp.Reason)
			}
		}
		return escalated, zbtest.Calls(t, tag), hn.mem
	}
	t.Run("cap 3: the 4th and 5th are refused", func(t *testing.T) {
		esc, calls, mem := run(t, 3, 5)
		if esc != 3 || calls != 3 {
			t.Fatalf("escalated=%d requests=%d, want 3 and 3: the N+1th call must be refused before it reaches the vendor", esc, calls)
		}
		if len(mem.ExternalCalls) != 3 {
			t.Errorf("%d spend records, want 3 (a refused call is not a spend)", len(mem.ExternalCalls))
		}
	})
	t.Run("cap 0 (the default): nothing is sent", func(t *testing.T) {
		if esc, calls, _ := run(t, 0, 3); esc != 0 || calls != 0 {
			t.Fatalf("escalated=%d requests=%d, want 0 and 0: enabling the vendor without choosing a cap sends nothing", esc, calls)
		}
	})
	t.Run("control: cap 100 lets all five through", func(t *testing.T) {
		if esc, calls, _ := run(t, 100, 5); esc != 5 || calls != 5 {
			t.Fatalf("escalated=%d requests=%d, want 5 and 5", esc, calls)
		}
	})
}

// Spend-once end to end: two CONCURRENT verifies of the same address cost one vendor request and one credit,
// and both callers get the rented verdict. Controls: two DISTINCT addresses cost two of each.
func TestZB_Stage_SpendOnce_ConcurrentIdenticalResidues(t *testing.T) {
	zbtest.URL(t)
	run := func(t *testing.T, same bool) (calls, credits, records int) {
		t.Helper()
		hn := zbStage(t, zbCapped(1000), nil)
		tag := zbtest.Tag(t)
		addrs := []string{"valid+dly400+" + tag + "@gmail.com", "valid+dly400+" + tag + "@gmail.com"}
		if !same {
			addrs[1] = "valid+dly400+" + tag + "+n2@gmail.com"
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		reasons := make([]vocab.Reason, 2)
		errs := make([]error, 2)
		for i := range addrs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, err := hn.o.Verify(context.Background(), addrs[i], hn.def)
				reasons[i], errs[i] = resp.Reason, err
			}()
		}
		close(start)
		wg.Wait()
		for i := range addrs {
			if errs[i] != nil || reasons[i] != vocab.ReasonExternalVerified {
				t.Fatalf("caller %d: reason=%s err=%v, want external_verified", i, reasons[i], errs[i])
			}
		}
		for _, c := range hn.mem.ExternalCalls {
			if c.Outcome != "ok" {
				t.Errorf("a record was not ok: %+v", c)
			}
			credits += c.CostUnits
		}
		return zbtest.Calls(t, tag), credits, len(hn.mem.ExternalCalls)
	}
	if calls, credits, _ := run(t, true); calls != 1 || credits != 1 {
		t.Fatalf("identical concurrent residues: requests=%d credits=%d, want 1 and 1 (spend-once)", calls, credits)
	}
	if calls, credits, records := run(t, false); calls != 2 || credits != 2 || records != 2 {
		t.Fatalf("control: distinct concurrent residues: requests=%d credits=%d records=%d, want 2, 2 and 2", calls, credits, records)
	}
}

// "Disabled sends nothing": every way the layer can be off leaves the stand-in's counter at zero, and the
// control — the same address with everything on — is sent. A residue must be one the policy opts into AND the
// layer must be wired AND the cap must allow it.
func TestZB_Stage_DisabledSendsNothing(t *testing.T) {
	zbtest.URL(t)
	addrFor := func(tag string) string { return "valid+" + tag + "@gmail.com" }
	check := func(name string, hn zbHarness, def *policy.Definition, wantCalls int) {
		t.Helper()
		tag := zbtest.Tag(t)
		resp, err := hn.o.Verify(context.Background(), addrFor(tag), def)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := zbtest.Calls(t, tag); got != wantCalls {
			t.Errorf("%s: the stand-in saw %d requests, want %d", name, got, wantCalls)
		}
		if wantCalls == 0 && resp.Reason != vocab.ReasonProviderNoSMTP {
			t.Errorf("%s: with no call M1's own verdict must stand, got %s/%s", name, resp.Status, resp.Reason)
		}
	}
	on := zbStage(t, zbCapped(10), nil)
	check("control: everything on", on, on.def, 1)

	// the layer is not wired at all (ExternalDeps nil: the P1-P5 posture, and what buildExternal returns by default)
	bare := zbStage(t, zbCapped(10), nil)
	bare.o.external = nil
	check("layer not wired", bare, bare.def, 0)

	// the policy has no external block (the mechanical privacy opt-out)
	optOut := *on.def
	optOut.External = nil
	check("policy without an external block", on, &optOut, 0)

	// the policy disables the layer, or does not opt into this residue class
	disabled := *on.def
	ext := *on.def.External
	ext.Enabled = false
	disabled.External = &ext
	check("policy external.enabled=false", on, &disabled, 0)

	narrow := *on.def
	ext2 := *on.def.External
	ext2.RouteWhen = []string{"catch_all"} // provider_no_smtp is not opted into
	narrow.External = &ext2
	check("policy route_when omits provider_no_smtp", on, &narrow, 0)

	// the vendor cap is its default of 0
	zero := zbStage(t, zbCapped(0), nil)
	check("daily spend cap 0", zero, zero.def, 0)

	// the kill switch
	killed := zbStage(t, zbCapped(10), func(d *ExternalDeps) { d.GloballyDisabled = true })
	check("kill switch", killed, killed.def, 0)

	// the breaker is open on error rate (a vendor that is failing is not called)
	tripped := zbStage(t, zbCapped(10), func(d *ExternalDeps) {
		d.Breaker = external.BreakerParams{ErrorRateThreshold: 0.5, MinSamples: 2, Cooldown: 5 * time.Minute}
	})
	for i := 0; i < 3; i++ {
		_ = tripped.mem.AppendExternalCall(context.Background(), store.ExternalCallRow{
			ID: fmt.Sprintf("018f0000-0000-7000-8000-00000000a%03d", i), Vendor: "zerobounce", Outcome: "error",
			CalledAt: time.Unix(1_700_000_000, 0).Add(-time.Minute),
		})
	}
	check("breaker open on error rate", tripped, tripped.def, 0)
}

// The key reaches NOTHING stored or logged: not the LOG, not the raw sidecar, not the spend records, not the
// response, not the process log — including when the vendor itself echoes it in an error body. The dump is
// proven non-empty first, so the absence of the key is a finding and not a blank.
func TestZB_Stage_KeyNeverInStoredStateOrLogs(t *testing.T) {
	zbtest.URL(t)
	var logBuf bytes.Buffer
	origLog := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origLog) })

	hn := zbStage(t, zbCapped(100), nil)
	var dump strings.Builder
	for _, b := range []string{"valid", "do_not_mail--toxic", "fail--error_body_echo_key", "fail--http_500", "fail--truncated", "unknown"} {
		tag := zbtest.Tag(t)
		resp, err := hn.o.Verify(context.Background(), zbtest.Addr(b, tag, "gmail.com"), hn.def)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		fmt.Fprintf(&dump, "%+v %#v\n", resp, resp)
	}
	fmt.Fprintf(&dump, "%+v\n%+v\n%+v\n", hn.mem.Log, hn.mem.Raw, hn.mem.ExternalCalls)
	fmt.Fprintf(&dump, "%v %+v %#v\n", hn.zb, hn.zb, hn.zb)
	all := dump.String() + logBuf.String()
	if !strings.Contains(all, "external_verified") || !strings.Contains(all, "zerobounce") || len(hn.mem.ExternalCalls) < 5 {
		t.Fatalf("control: the dump must contain the escalated verdicts and the spend records (else the grep proves nothing); calls=%d", len(hn.mem.ExternalCalls))
	}
	if strings.Contains(all, zbStageKey) {
		t.Fatal("the API key appeared in stored state, a response, or a log line")
	}
	// Control that the grep CAN see the key: a stored row would show it if the adapter put it there.
	if !strings.Contains(fmt.Sprintf("%+v", struct{ K string }{zbStageKey}), zbStageKey) {
		t.Fatal("control: the key literal must be visible to the check")
	}
}
