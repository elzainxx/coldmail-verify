package vocab

import (
	"reflect"
	"sort"
	"testing"
)

// The signals vocabulary is additive-only like the enums beside it: these fail if a token is renamed or removed, or if the own-failure
// classification drifts (that classification decides which cheap-only answers are labelled failure_origin=own).

func TestFailureOrigin_ExactlyThreeValues(t *testing.T) {
	for _, o := range []FailureOrigin{OriginOwn, OriginRemote, OriginUndetermined} {
		if !ValidFailureOrigin(o) {
			t.Errorf("%q must be a legal failure origin", o)
		}
	}
	for _, bad := range []FailureOrigin{"", "OWN", "us", "them", "unknown", "remote "} {
		if ValidFailureOrigin(bad) {
			t.Errorf("%q must NOT be a legal failure origin (the vocabulary is exactly own|remote|undetermined)", bad)
		}
	}
	// The wire spelling is what the platform codes against.
	if string(OriginOwn) != "own" || string(OriginRemote) != "remote" || string(OriginUndetermined) != "undetermined" || SignalFailureOrigin != "failure_origin" {
		t.Fatal("FROZEN spelling changed: failure_origin / own / remote / undetermined")
	}
}

// The six skip tokens. The first five existed before the P2-research close-out and are frozen; smtp_unwired is the addition.
func TestSkipTokens_AreTheSix_AndSmtpUnwiredIsRegistered(t *testing.T) {
	want := []string{"audit_red", "no_eligible_node", "provider_mx_routed", "rate_limited", "smtp_unwired", "warmup_cap"}
	got := SkipTokens()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("SkipTokens must be sorted, got %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkipTokens = %v, want %v (additive-only: a value is never renamed or removed)", got, want)
	}
	if SkipSMTPUnwired != "smtp_unwired" || SignalSMTPSkippedReason != "smtp_skipped_reason" {
		t.Fatal("FROZEN spelling changed: smtp_unwired / smtp_skipped_reason")
	}
}

func TestSkipIsOwnFailure_OnlyProviderRoutingIsASettledAnswer(t *testing.T) {
	for _, tok := range SkipTokens() {
		want := tok != SkipProviderMX // provider_mx_routed is a settled routing outcome; every other token is our own degradation
		if got := SkipIsOwnFailure(tok); got != want {
			t.Errorf("SkipIsOwnFailure(%q) = %v, want %v", tok, got, want)
		}
	}
	// Control: a token that is not in the vocabulary is not an own failure (an unrecognised token must never label an answer).
	for _, tok := range []string{"", "bogus", "SMTP_UNWIRED"} {
		if SkipIsOwnFailure(tok) {
			t.Errorf("SkipIsOwnFailure(%q) = true for a token outside the vocabulary", tok)
		}
	}
}
