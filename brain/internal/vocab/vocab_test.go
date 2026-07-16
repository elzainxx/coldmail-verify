package vocab

import "testing"

// Door 4 is additive-only. These tests fail if a value is renamed/removed (the exact
// regression the door forbids) or if the composability invariant breaks.

func TestReservedValuesPresent(t *testing.T) {
	// Values later phases DEPEND on must exist NOW (reserved), or a future addition is
	// a breaking change. (pre-takeoff-checklist §1.2.)
	for _, r := range []Reason{ReasonHistoricalBounce, ReasonExternalVerified, ReasonMailboxFull, ReasonCatchAll} {
		if !ValidReason(r) {
			t.Errorf("reserved reason %q must be present in the frozen enum", r)
		}
	}
}

func TestVocabRejectsUnknown(t *testing.T) {
	if ValidStatus("bogus") || ValidReason("bogus") || ValidRecommendation("bogus") {
		t.Fatal("unknown vocabulary values must be rejected (Door 4 guard)")
	}
}

func TestComposabilityInvariants(t *testing.T) {
	// LOCKED (P1 §3.1.3): invalid⇒skip only; invalid+send is forbidden.
	cases := []struct {
		s   Status
		rec Recommendation
		ok  bool
	}{
		{StatusInvalid, RecSkip, true},
		{StatusInvalid, RecSend, false}, // the forbidden combination
		{StatusInvalid, RecReview, false},
		{StatusValid, RecSend, true},
		{StatusValid, RecReview, true},
		{StatusValid, RecSkip, false},
		{StatusRisky, RecReview, true},
		{StatusRisky, RecSkip, true},
		{StatusRisky, RecSend, false},
		{StatusUnknown, RecReview, true},
		{StatusUnknown, RecSend, false},
	}
	for _, c := range cases {
		if got := ComposabilityOK(c.s, c.rec); got != c.ok {
			t.Errorf("ComposabilityOK(%s,%s)=%v want %v", c.s, c.rec, got, c.ok)
		}
	}
}
