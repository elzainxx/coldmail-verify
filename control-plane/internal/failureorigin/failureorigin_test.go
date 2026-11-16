package failureorigin

import (
	"os"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// The mapping is the one thing the platform bills on, so these tests are about the direction of error: a false `remote` charges a customer
// for OUR failure. They pin every row, then sweep every (reason × cause) pair NOT in the table and require it never to come out remote.

func TestTable_RowsAreLegalAndUnique(t *testing.T) {
	seen := map[[2]string]bool{}
	if len(Rows()) == 0 {
		t.Fatal("the mapping table is empty: this test would pass over nothing")
	}
	for _, r := range Rows() {
		if !vocab.ValidReason(r.Reason) {
			t.Errorf("row %+v: reason is not in the frozen vocabulary", r)
		}
		if !vocab.ValidFailureOrigin(r.Origin) {
			t.Errorf("row %+v: origin is not own|remote|undetermined", r)
		}
		k := [2]string{string(r.Reason), string(r.Cause)}
		if seen[k] {
			t.Errorf("row %+v is duplicated: one (reason, cause) pair has exactly one origin", r)
		}
		seen[k] = true
		if got := OriginFor(r.Reason, r.Cause); got != r.Origin {
			t.Errorf("OriginFor(%s, %s) = %s, the table says %s", r.Reason, r.Cause, got, r.Origin)
		}
	}
}

// Each origin is reachable and each is reachable for the reason the owner's rulings name.
func TestOriginFor_OwnerRulings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason vocab.Reason
		cause  Cause
		want   vocab.FailureOrigin
	}{
		{"smtp_blocked is ALWAYS own (a policy or reputation block is about OUR IP)", vocab.ReasonSMTPBlocked, CauseSMTPReply, vocab.OriginOwn},
		{"greylisted is a 4xx reply from the recipient's server: remote", vocab.ReasonGreylisted, CauseSMTPReply, vocab.OriginRemote},
		{"our resolver timing out is own", vocab.ReasonDNSError, CauseDNSResolverTimeout, vocab.OriginOwn},
		{"our resolver unreachable is own", vocab.ReasonDNSError, CauseDNSResolverUnreachable, vocab.OriginOwn},
		{"no resolver wired is own", vocab.ReasonDNSError, CauseDNSNoResolver, vocab.OriginOwn},
		{"a SERVFAIL relayed by a working resolver is remote", vocab.ReasonDNSError, CauseDNSServFail, vocab.OriginRemote},
		{"a node transport error is own", vocab.ReasonSMTPTimeout, CauseNodeTransport, vocab.OriginOwn},
		{"a node HTTP 400/401 is own", vocab.ReasonSMTPError, CauseNodeRejected, vocab.OriginOwn},
		{"a node non-2xx is own", vocab.ReasonConnectionFailed, CauseNodeHTTPStatus, vocab.OriginOwn},
		{"an SMTP reply code on an ambiguous 550 is remote", vocab.ReasonSMTPError, CauseSMTPReply, vocab.OriginRemote},
		{"silence after the TCP session was proven open is remote", vocab.ReasonSMTPTimeout, CauseSMTPSilentAfterConnect, vocab.OriginRemote},
		{"a connection that never opened cannot tell our egress from their refusal: undetermined", vocab.ReasonConnectionFailed, CauseSMTPNoSession, vocab.OriginUndetermined},
		{"a reply at the greeting or MAIL FROM answers the connecting client, not the mailbox: undetermined", vocab.ReasonConnectionFailed, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
		{"a 4xx at MAIL FROM is a deferral aimed at us, not a recipient greylist: undetermined", vocab.ReasonGreylisted, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
		{"an ambiguous 550 at MAIL FROM is about our sender: undetermined", vocab.ReasonSMTPError, CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
		{"a dropped session with no reply and no timeout is undetermined", vocab.ReasonSMTPError, CauseSMTPSessionLost, vocab.OriginUndetermined},
		{"an unclassified DNS error is undetermined", vocab.ReasonDNSError, CauseDNSUnclassified, vocab.OriginUndetermined},
		{"the drainer's error cap is own", vocab.ReasonUnknownError, CauseErrorCapped, vocab.OriginOwn},
	} {
		if got := OriginFor(tc.reason, tc.cause); got != tc.want {
			t.Errorf("%s: OriginFor(%s, %s) = %s, want %s", tc.name, tc.reason, tc.cause, got, tc.want)
		}
	}
}

// THE DIRECTION OF ERROR. Every reason × every cause (and "no cause"): the answer is `remote` ONLY for a table row that says so. Everything
// else is own (smtp_blocked) or undetermined. This is what makes "a new reason, a new cause, or a forgotten branch can never silently become
// billable" a property instead of a hope.
func TestOriginFor_RemoteOnlyWhereTheTableProvesIt(t *testing.T) {
	remote := map[[2]string]bool{}
	for _, r := range Rows() {
		if r.Origin == vocab.OriginRemote {
			remote[[2]string{string(r.Reason), string(r.Cause)}] = true
		}
	}
	if len(remote) == 0 {
		t.Fatal("no remote rows at all: the sweep below would assert nothing")
	}
	causes := append(Causes(), "", "made_up_cause")
	checked := 0
	for _, reason := range vocab.Reasons() {
		for _, cause := range causes {
			got := OriginFor(reason, cause)
			if !vocab.ValidFailureOrigin(got) {
				t.Fatalf("OriginFor(%s, %q) = %q, not a legal origin", reason, cause, got)
			}
			if got == vocab.OriginRemote && !remote[[2]string{string(reason), string(cause)}] {
				t.Errorf("OriginFor(%s, %q) = remote but no table row proves it: a customer would be charged on a guess", reason, cause)
			}
			if reason == vocab.ReasonSMTPBlocked && got != vocab.OriginOwn {
				t.Errorf("OriginFor(smtp_blocked, %q) = %s: a block is always own", cause, got)
			}
			if cause == "" && got == vocab.OriginRemote {
				t.Errorf("OriginFor(%s, no cause) = remote: with no evidence the answer is never remote", reason)
			}
			checked++
		}
	}
	if checked < len(vocab.Reasons()) {
		t.Fatalf("swept only %d pairs", checked)
	}
}

// Of: every unknown gets an origin, whatever its reason; a non-unknown gets one only for an own-failure skip token.
func TestOf_EveryUnknownHasAnOrigin_AndOnlyOwnSkipsLabelOtherStatuses(t *testing.T) {
	for _, reason := range vocab.Reasons() {
		o, ok := Of(vocab.StatusUnknown, reason, "", nil)
		if !ok || !vocab.ValidFailureOrigin(o) {
			t.Errorf("Of(unknown, %s, no cause) = (%q, %v): every unknown must carry a legal origin", reason, o, ok)
		}
	}
	for _, status := range []vocab.Status{vocab.StatusValid, vocab.StatusInvalid, vocab.StatusRisky} {
		// CONTROL: no skip token → the key does not apply.
		if o, ok := Of(status, vocab.ReasonPlausible, "", map[string]any{}); ok {
			t.Errorf("Of(%s, no skip token) = (%q, true): the key must not label a normal answer", status, o)
		}
		for _, tok := range vocab.SkipTokens() {
			o, ok := Of(status, vocab.ReasonPlausible, "", map[string]any{vocab.SignalSMTPSkippedReason: tok})
			want := vocab.SkipIsOwnFailure(tok)
			if ok != want || (ok && o != vocab.OriginOwn) {
				t.Errorf("Of(%s, skip=%s) = (%q, %v), want ok=%v origin=own", status, tok, o, ok, want)
			}
		}
	}
}

// THE DOC CANNOT DRIFT FROM THE CODE. Docs/failure-origin.md section 3 is the mapping the owner reads and the platform bills on; this reads
// it back and requires it to equal the function in both directions: every row of Rows() appears in the doc with the same origin, and every
// doc row gives exactly the origin OriginFor gives for its (reason, cause) — a "(none)" cause being no evidence at all.
func TestDoc_MappingTableMatchesTheCode(t *testing.T) {
	const docPath = "../../../Docs/failure-origin.md"
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	text := strings.ReplaceAll(string(raw), "\r", "")
	begin, end := strings.Index(text, "<!-- origin-table:begin -->"), strings.Index(text, "<!-- origin-table:end -->")
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("%s lost its <!-- origin-table:begin/end --> markers", docPath)
	}
	tick := func(cell string) string { return strings.Trim(strings.TrimSpace(cell), "`") }

	type key struct{ reason, cause string }
	docOrigin := map[key]string{}
	docRows := 0
	for _, line := range strings.Split(text[begin:end], "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue // header, separator, blank
		}
		cells := strings.Split(line, "|")
		// "| a | b | c | d | e | f |" splits to ["", a, b, c, d, e, f, ""]
		if len(cells) != 8 {
			t.Fatalf("a mapping row must have 6 cells (reason, condition, cause, origin, branch, test), got %d: %.120s", len(cells)-2, line)
		}
		docRows++
		reason, cause, origin := tick(cells[1]), tick(cells[3]), tick(cells[4])
		if cause == "(none)" {
			cause = ""
		}
		if !vocab.ValidReason(vocab.Reason(reason)) {
			t.Errorf("doc row names an unknown reason %q", reason)
		}
		if want := OriginFor(vocab.Reason(reason), Cause(cause)); string(want) != origin {
			t.Errorf("doc says %s/%q → %s, the code says %s", reason, cause, origin, want)
		}
		docOrigin[key{reason, cause}] = origin
	}
	if docRows < len(Rows()) {
		t.Fatalf("the doc table has %d rows, fewer than the code's %d", docRows, len(Rows()))
	}
	for _, r := range Rows() {
		got, ok := docOrigin[key{string(r.Reason), string(r.Cause)}]
		if !ok {
			t.Errorf("Rows() has %s/%s → %s but the doc table has no such row", r.Reason, r.Cause, r.Origin)
		} else if got != string(r.Origin) {
			t.Errorf("%s/%s: doc says %s, code says %s", r.Reason, r.Cause, got, r.Origin)
		}
	}
}
