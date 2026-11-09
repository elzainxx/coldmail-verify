package smtp

// ADR-0021: the verifier node's destination guard reports `probe_refused` (a fact, no transcript); the brain maps it to
// risky/non_public_mx. The decode is tested against the GOLDEN fixture the Ruby side's RSpec also asserts byte-for-byte
// (verifier-node/truemail-rack/spec/fixtures/probe_refused.json), so the two languages cannot drift apart on the wire shape.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

func goldenProbeRefused(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join("..", "..", "..", "verifier-node", "truemail-rack", "spec", "fixtures", "probe_refused.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("cannot read the golden fixture shared with the Ruby wrapper (%s): %v", p, err)
	}
	return b
}

func TestProbeRefused_GoldenFixtureDecodesAndMapsToRiskyNonPublicMX(t *testing.T) {
	var wr WrapperResponse
	if err := json.Unmarshal(goldenProbeRefused(t), &wr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wr.ProbeRefused == nil {
		t.Fatal("probe_refused did not decode: the wire field name drifted from the wrapper's")
	}
	if wr.ProbeRefused.Reason != "non_public_target" || wr.ProbeRefused.Count != 2 || len(wr.ProbeRefused.Targets) != 2 {
		t.Fatalf("decoded refusal = %+v", wr.ProbeRefused)
	}
	if wr.Success || len(wr.SMTPDebug) != 0 {
		t.Fatal("a refusal carries no success and no transcript")
	}
	res := ProbeRefusedResult(wr.ProbeRefused)
	if res.Status != vocab.StatusRisky || res.Reason != vocab.ReasonNonPublicMX {
		t.Fatalf("got %s/%s, want risky/non_public_mx", res.Status, res.Reason)
	}
	if res.Status == vocab.StatusInvalid {
		t.Fatal("a destination refusal is a fact about DNS, never about the address: it must not be invalid")
	}
	if res.Cacheable || CacheableReason(res.Status, res.Reason) {
		t.Fatal("non_public_mx is derived from current DNS state and must not be cached")
	}
	if got := res.Signals["probe_refused_ranges"]; !reflect.DeepEqual(got, []string{"link-local/cloud-metadata", "private-rfc1918"}) {
		t.Fatalf("signals.probe_refused_ranges = %v (sorted, unique range labels expected)", got)
	}
	if !vocab.ValidReason(res.Reason) {
		t.Fatal("non_public_mx is not in the frozen vocabulary")
	}
}

// CONTROL: the same bytes through the pre-ADR-0021 path (no ProbeRefused handling: ParseTranscript on an empty transcript) are an
// unknown / connection_failed — so the mapping above is what makes the difference, and it is not a coincidence of the fixture.
func TestProbeRefused_Control_WithoutTheMappingItIsAnUnknownNotARefusal(t *testing.T) {
	var wr WrapperResponse
	if err := json.Unmarshal(goldenProbeRefused(t), &wr); err != nil {
		t.Fatal(err)
	}
	res := Score(Input{Target: ParseTranscript(wr.SMTPDebug, wr.Success), EngineSuccess: wr.Success})
	if res.Status != vocab.StatusUnknown || res.Reason != vocab.ReasonConnectionFailed {
		t.Fatalf("control: the unmapped body scored %s/%s, want unknown/connection_failed — the test would not detect a missing mapping", res.Status, res.Reason)
	}
}

func TestCacheableReason_OnlyNonPublicMXIsTheRiskyThatIsNotCached(t *testing.T) {
	for _, r := range []vocab.Reason{vocab.ReasonCatchAll, vocab.ReasonPlausible, vocab.ReasonDisposable, vocab.ReasonExternalVerified} {
		if !CacheableReason(vocab.StatusRisky, r) {
			t.Errorf("risky/%s must stay cacheable (settled)", r)
		}
	}
	if CacheableReason(vocab.StatusRisky, vocab.ReasonNonPublicMX) {
		t.Error("risky/non_public_mx must not be cacheable")
	}
	if strings.TrimSpace(string(goldenProbeRefused(t))) == "" {
		t.Error("golden fixture is empty")
	}
}
