package flywheel

import "testing"

// The correctness-heart tests (§4, GAP-49). These are the fail-verify anchors: the exclusion discipline
// (§4.3) is the single behavior whose corruption silently poisons every future verdict, so each test
// names what BREAKS if the classifier is neutered.

// TestClassify_BlockedNeverInvalidNumerator is THE fail-verify anchor (§4.3): a blocked bounce is about
// the SENDER, never the address — it must be zero-information and NEVER enter an invalid numerator.
// FAIL-VERIFY: change the EventBlocked case in Classify to return ClassInvalidSignal → this breaks.
func TestClassify_BlockedNeverInvalidNumerator(t *testing.T) {
	for _, ev := range []Event{EventBlocked, EventPolicyReject, EventSpamReject, EventGreylisted, EventDeferred, EventComplaint} {
		c := Classify(ev, false)
		if c.Class != ClassZeroInformation {
			t.Fatalf("%s: got class %q, want zero_information (exclusion is the discipline, §4.3)", ev, c.Class)
		}
		if c.InInvalidNumerator() {
			t.Fatalf("%s: entered the invalid numerator — a sender-fault bounce must NEVER count against the address (GAP-49)", ev)
		}
		if c.Calibrates() {
			t.Fatalf("%s: calibrates — zero-information is excluded from numerator AND denominator (§4.3)", ev)
		}
	}
}

// TestClassify_MailboxFullIsValidSignal is the counterintuitive rule (§4.2): a full mailbox EXISTS.
// FAIL-VERIFY: map EventMailboxFull → ClassInvalidSignal → this breaks (the classic poisoning error).
func TestClassify_MailboxFullIsValidSignal(t *testing.T) {
	c := Classify(EventMailboxFull, false)
	if c.Class != ClassValidSignal {
		t.Fatalf("mailbox_full: got %q, want valid_signal — a full mailbox is positive existence proof (§4.2, the 552 ruling)", c.Class)
	}
	if c.Strength != StrengthStrong {
		t.Fatalf("mailbox_full: got strength %d, want strong (near-proof of existence)", c.Strength)
	}
	if c.InInvalidNumerator() {
		t.Fatal("mailbox_full entered the invalid numerator — this is exactly the GAP-49 poisoning error")
	}
}

// TestClassify_MailboxNotFoundIsInvalidSignal — the gold negative (§4.2).
func TestClassify_MailboxNotFoundIsInvalidSignal(t *testing.T) {
	c := Classify(EventMailboxNotFound, false)
	if c.Class != ClassInvalidSignal || !c.InInvalidNumerator() {
		t.Fatalf("mailbox_not_found: got %q — want invalid_signal, the one outcome that proves invalid (§4.2)", c.Class)
	}
}

// TestClassify_DeliveredOnCatchAllIsNearZero (§4.5): delivery to a catch-all domain proves almost
// nothing about the specific mailbox (the domain 250s everything). It must be weight≈0 and NOT calibrate.
func TestClassify_DeliveredOnCatchAllIsNearZero(t *testing.T) {
	onCatch := Classify(EventDelivered, true)
	if onCatch.Strength != StrengthZero {
		t.Fatalf("delivered-on-catch-all: got strength %d, want zero (§4.5 — the catch-all trap)", onCatch.Strength)
	}
	if onCatch.Calibrates() {
		t.Fatal("delivered-on-catch-all calibrates — it is near-worthless and must not add denominator noise (§4.5)")
	}
	// Contrast: delivered on a DISCRIMINATING server is moderate evidence and DOES calibrate.
	off := Classify(EventDelivered, false)
	if off.Strength != StrengthModerate || !off.Calibrates() {
		t.Fatalf("delivered-on-discriminating: got strength %d calibrates=%v, want moderate+calibrates (§4.5)", off.Strength, off.Calibrates())
	}
	// The ORDERING is LOCKED: mailbox_full (strong) >= discriminating-delivery (moderate) > catch-all (zero).
	if !(Classify(EventMailboxFull, false).Strength >= off.Strength && off.Strength > onCatch.Strength) {
		t.Fatal("evidence-strength ordering violated (§4.5 LOCKED: mailbox_full ≥ discriminating-delivery > catch-all)")
	}
}

// TestNormalizeEvent_DSNSemantics — the DIAL layer maps raw DSN strings to events without trusting a
// pre-classified label (§3.2). The most specific existence signals win over broad policy buckets.
func TestNormalizeEvent_DSNSemantics(t *testing.T) {
	cases := []struct {
		raw  string
		want Event
	}{
		{"550 5.1.1 user unknown", EventMailboxNotFound},
		{"552 mailbox full, over quota", EventMailboxFull},
		{"550 5.7.1 message rejected as spam", EventSpamReject},
		{"451 greylisted, try again later", EventGreylisted},
		{"550 blocked by reputation, listed on dnsbl", EventBlocked},
		{"250 2.0.0 delivered", EventDelivered},
		{"421 temporary failure, deferred", EventDeferred},
		{"complaint: feedback loop", EventComplaint},
		{"", EventUnknown},
		{"some novel unparseable dsn", EventUnknown},
	}
	for _, c := range cases {
		if got := NormalizeEvent(c.raw); got != c.want {
			t.Errorf("NormalizeEvent(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	// Harden-toward-exclusion (§4.2): an unrecognized DSN is zero-information, NEVER invalid-signal.
	if Classify(NormalizeEvent("garbage"), false).InInvalidNumerator() {
		t.Fatal("an unclassifiable DSN entered the invalid numerator — must harden toward exclusion (§4.2)")
	}
}

// TestNormalizeEvent_NoBareNumericSubstringPoisoning is the CRITICAL regression (§4.3/GAP-49): a bare
// numeric substring (551, 5.1.1) embedded in a queue-id, IP, or byte-count inside a TRANSIENT/POLICY
// DSN must NEVER be read as a reply code and pull the bounce into mailbox_not_found (invalid-signal).
// Codes are parsed structurally (front-of-line basic code; whole-token enhanced code). FAIL-VERIFY:
// revert NormalizeEvent to strings.Contains on "551"/"5.1.1" → these classify as invalid-signal → break.
func TestNormalizeEvent_NoBareNumericSubstringPoisoning(t *testing.T) {
	poison := []struct {
		raw  string
		want Event
	}{
		{"451 greylisted; try again later (queued as 3F551A2B9)", EventGreylisted},
		{"552-5.2.2 Message size 15510 exceeds policy", EventMailboxFull}, // real over-quota code wins; 15510 must not read as 551
		{"550 5.7.1 blocked; host mx.ex.com[125.1.10.7] said policy", EventBlocked}, // zero-info either way; the IP's 5.1.1-substring must not win it to invalid-signal
		{"421 4.7.0 temporary failure, message 551 deferred", EventDeferred},
		{"250 2.0.0 delivered ok, id=A5511B", EventDelivered},
	}
	for _, c := range poison {
		got := NormalizeEvent(c.raw)
		if got != c.want {
			t.Errorf("NormalizeEvent(%q) = %q, want %q (numeric-substring poisoning)", c.raw, got, c.want)
		}
		if Classify(got, false).InInvalidNumerator() {
			t.Fatalf("NormalizeEvent(%q) → %q entered the invalid numerator — the §4.3/GAP-49 poisoning the structural parse must prevent", c.raw, got)
		}
	}
	// A composite where the ADDRESS text IS authoritative still resolves to invalid-signal (the strong
	// address signal wins over a mere transient code) — the intended behavior, not poisoning.
	if NormalizeEvent("451 4.7.1 greylisted ... 550 user unknown") != EventMailboxNotFound {
		t.Fatal("an authoritative 'user unknown' text should still classify as mailbox_not_found")
	}
	// A bare generic 550 with NO address text is NOT mailbox_not_found (550 is generic permanent reject).
	if NormalizeEvent("550 message rejected") == EventMailboxNotFound {
		t.Fatal("a bare 550 with no not-found signal must NOT be invalid-signal (550 is generic)")
	}
}
