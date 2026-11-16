package verdict

import (
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/failureorigin"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// THE PROPERTY (A3): every `unknown` this assembler ever builds carries signals.failure_origin, in BOTH the response and the LOG row.
// It is asserted over the WHOLE reason vocabulary — not over the reasons someone remembers emit an unknown today — so a new reason, or a
// new code path that mints an unknown with a reason nobody mapped, fails here instead of reaching the platform without the key.

func testProv() Provenance {
	return Provenance{RequestID: "018f0000-0000-7000-8000-000000000001", CheckedAt: time.Unix(1700000000, 0), PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg"}
}

func unknownCore(reason vocab.Reason, signals map[string]any, cause failureorigin.Cause) Core {
	return Core{Status: vocab.StatusUnknown, Reason: reason, Confidence: 10, Recommendation: vocab.RecReview, Signals: signals, Cause: cause}
}

func originOf(t *testing.T, signals map[string]any) vocab.FailureOrigin {
	t.Helper()
	s, ok := signals[vocab.SignalFailureOrigin].(string)
	if !ok {
		t.Fatalf("signals has no string failure_origin: %v", signals)
	}
	o := vocab.FailureOrigin(s)
	if !vocab.ValidFailureOrigin(o) {
		t.Fatalf("failure_origin = %q is not own|remote|undetermined", s)
	}
	return o
}

func TestAssemble_EveryUnknownReasonCarriesAFailureOrigin(t *testing.T) {
	reasons := vocab.Reasons()
	if len(reasons) < 20 {
		t.Fatalf("the reason vocabulary has %d entries; the sweep would be vacuous", len(reasons))
	}
	for _, reason := range reasons {
		// With no signals, with an empty bag, with unrelated keys, and with NO recorded cause: the key must be there every time.
		for _, signals := range []map[string]any{nil, {}, {"mx_found": false, "retry_eligible": true}} {
			resp, row := Assemble(unknownCore(reason, signals, ""), testProv())
			got := originOf(t, resp.Signals)
			if row.Signals[vocab.SignalFailureOrigin] != string(got) {
				t.Errorf("reason %s: the LOG row's failure_origin (%v) differs from the response's (%s) — response and log must not drift", reason, row.Signals[vocab.SignalFailureOrigin], got)
			}
			if got == vocab.OriginRemote {
				t.Errorf("reason %s with no recorded cause came out remote: with no evidence an unknown is never billable", reason)
			}
			if resp.Status != vocab.StatusUnknown || resp.Reason != reason || resp.Confidence != 10 || resp.Recommendation != vocab.RecReview {
				t.Errorf("reason %s: assembling failure_origin changed the verdict: %+v", reason, resp)
			}
			if !resp.Valid() {
				t.Errorf("reason %s: the assembled unknown is not a valid Door-1 response", reason)
			}
		}
	}
}

// Every row of the mapping table, driven through the assembler: the (reason, cause) a stage reports is the origin the platform sees.
func TestAssemble_TheTableIsWhatTheResponseCarries(t *testing.T) {
	if len(failureorigin.Rows()) == 0 {
		t.Fatal("empty table")
	}
	for _, r := range failureorigin.Rows() {
		resp, row := Assemble(unknownCore(r.Reason, map[string]any{}, r.Cause), testProv())
		if got := originOf(t, resp.Signals); got != r.Origin {
			t.Errorf("%s/%s: response failure_origin = %s, table says %s", r.Reason, r.Cause, got, r.Origin)
		}
		if row.Signals[vocab.SignalFailureOrigin] != string(r.Origin) {
			t.Errorf("%s/%s: LOG row failure_origin = %v, table says %s", r.Reason, r.Cause, row.Signals[vocab.SignalFailureOrigin], r.Origin)
		}
	}
}

// A stage cannot assert its own origin through the signals bag: only the table decides. A forged or stale key is overwritten.
func TestAssemble_AStaleOrForgedFailureOriginIsOverwritten(t *testing.T) {
	resp, _ := Assemble(unknownCore(vocab.ReasonGreylisted, map[string]any{vocab.SignalFailureOrigin: "own"}, ""), testProv())
	if got := originOf(t, resp.Signals); got != vocab.OriginUndetermined {
		t.Errorf("a pre-set failure_origin=own with no cause must be recomputed to undetermined, got %s", got)
	}
	resp, _ = Assemble(unknownCore(vocab.ReasonDNSError, map[string]any{vocab.SignalFailureOrigin: "garbage"}, failureorigin.CauseDNSServFail), testProv())
	if got := originOf(t, resp.Signals); got != vocab.OriginRemote {
		t.Errorf("a garbage failure_origin must be replaced by the table's answer (remote for a SERVFAIL), got %s", got)
	}
}

// CONTROLS: the key is for failures. A normal answer carries none; a degraded cheap-only answer caused by us carries own; a settled
// routing outcome (provider_mx_routed) carries none; and the key never touches the verdict fields.
func TestAssemble_NonUnknownResultsCarryTheKeyOnlyForOwnDegradation(t *testing.T) {
	for _, status := range []vocab.Status{vocab.StatusValid, vocab.StatusInvalid, vocab.StatusRisky} {
		rec := vocab.RecReview
		if status == vocab.StatusInvalid {
			rec = vocab.RecSkip
		}
		for _, reason := range vocab.Reasons() {
			resp, _ := Assemble(Core{Status: status, Reason: reason, Confidence: 50, Recommendation: rec, Signals: map[string]any{}}, testProv())
			if _, has := resp.Signals[vocab.SignalFailureOrigin]; has {
				t.Fatalf("%s/%s with no skip token carries failure_origin: the key must be absent on a normal answer", status, reason)
			}
		}
	}
	for _, tok := range vocab.SkipTokens() {
		resp, row := Assemble(Core{Status: vocab.StatusRisky, Reason: vocab.ReasonPlausible, Confidence: 40, Recommendation: vocab.RecReview,
			Signals: map[string]any{vocab.SignalSMTPSkippedReason: tok}}, testProv())
		_, has := resp.Signals[vocab.SignalFailureOrigin]
		if want := vocab.SkipIsOwnFailure(tok); has != want {
			t.Errorf("risky/plausible with skip=%s: failure_origin present = %v, want %v", tok, has, want)
		}
		if has && originOf(t, resp.Signals) != vocab.OriginOwn {
			t.Errorf("skip=%s: the degraded answer must say own", tok)
		}
		if has && row.Signals[vocab.SignalFailureOrigin] != "own" {
			t.Errorf("skip=%s: the LOG row must carry the same key as the response", tok)
		}
		if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible || resp.Confidence != 40 || resp.Recommendation != vocab.RecReview {
			t.Errorf("skip=%s: labelling the origin changed the verdict: %+v", tok, resp)
		}
	}
}
